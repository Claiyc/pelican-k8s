package certs

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/Claiyc/pelican-k8s/internal/pki"
)

// CertificateGVK is cert-manager's Certificate.
var CertificateGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}

// CertManager issues certificates through a cert-manager issuer instead of
// the internal CA (ARCHITECTURE.md 12.6): the operator writes a Certificate
// per agent and cert-manager fills and renews its Secret.
type CertManager struct {
	// IssuerName, IssuerKind (Issuer or ClusterIssuer) and IssuerGroup name
	// the issuer, as a Certificate's spec.issuerRef does.
	IssuerName  string
	IssuerKind  string
	IssuerGroup string
}

// Certificate returns the Certificate whose Secret secretName carries the
// leaf: same lifetime, renewal window and key type as the internal CA's.
func (m *CertManager) Certificate(meta metav1.ObjectMeta, secretName string, leaf Leaf) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(CertificateGVK)
	u.SetNamespace(meta.Namespace)
	u.SetName(meta.Name)
	u.SetLabels(meta.Labels)
	u.Object["spec"] = m.spec(secretName, leaf, meta.Labels)
	return u
}

func (m *CertManager) spec(secretName string, leaf Leaf, labels map[string]string) map[string]any {
	usages := []any{"digital signature"}
	if leaf.Usage&pki.Server != 0 {
		usages = append(usages, "server auth")
	}
	if leaf.Usage&pki.Client != 0 {
		usages = append(usages, "client auth")
	}
	dns := make([]any, 0, len(leaf.DNSNames))
	for _, n := range leaf.DNSNames {
		dns = append(dns, n)
	}
	secretLabels := map[string]any{}
	for k, v := range labels {
		secretLabels[k] = v
	}
	group := m.IssuerGroup
	if group == "" {
		group = CertificateGVK.Group
	}
	kind := m.IssuerKind
	if kind == "" {
		kind = "Issuer"
	}
	return map[string]any{
		"secretName":     secretName,
		"secretTemplate": map[string]any{"labels": secretLabels},
		"commonName":     leaf.CommonName,
		"dnsNames":       dns,
		"usages":         usages,
		"duration":       pki.LeafLifetime.String(),
		"renewBefore":    pki.RenewBefore.String(),
		"privateKey":     map[string]any{"algorithm": "ECDSA", "size": int64(256), "rotationPolicy": "Always"},
		// cert-manager's default, explicit so that a stored spec carrying it
		// still compares equal to this one.
		"encodeUsagesInRequest": true,
		"issuerRef":             map[string]any{"name": m.IssuerName, "kind": kind, "group": group},
	}
}
