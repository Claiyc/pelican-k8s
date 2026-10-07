package controller

import (
	"bytes"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/internal/pki"
)

func TestTLSSecretAndAgentURL(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	ca, _, err := pki.NewCA(h.now)
	if err != nil {
		t.Fatal(err)
	}
	h.r.PKI = &pki.Issuer{CA: ca, ClientName: pki.OperatorName}
	var bases []string
	h.r.NewAgent = func(base, token string) AgentAPI {
		bases = append(bases, base)
		return h.agent
	}
	h.reconcile(3)

	sec := &corev1.Secret{}
	if !h.get(sec, names.TLSSecret(uuid)) {
		t.Fatal("tls secret missing")
	}
	if sec.Type != corev1.SecretTypeTLS || len(sec.OwnerReferences) != 1 || !bytes.Equal(sec.Data[pki.CAFile], ca.CertPEM) || len(sec.Data[pki.KeyFile]) == 0 {
		t.Fatalf("tls secret %+v", sec)
	}
	cert, err := pki.ParseCertificate(sec.Data[pki.CertFile])
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.VerifyHostname(pki.AgentHost("10.0.0.5", uuid, ns)); err != nil {
		t.Fatal(err)
	}
	first := string(sec.Data[pki.CertFile])

	if sts := h.agentStatefulSetArgs(); !strings.Contains(sts, "--tls-dir") {
		t.Fatalf("agent args %q", sts)
	}

	h.createPod(true)
	h.reconcile(1)
	want := "https://" + pki.AgentHost("10.0.0.5", uuid, ns) + ":8080"
	if len(bases) == 0 || bases[len(bases)-1] != want {
		t.Fatalf("agent base %q, want %q", bases, want)
	}

	// A fresh certificate is kept; one in its renewal window is replaced.
	h.reconcile(1)
	h.get(sec, names.TLSSecret(uuid))
	if string(sec.Data[pki.CertFile]) != first {
		t.Fatal("fresh certificate replaced")
	}
	h.now = h.now.Add(pki.LeafLifetime - pki.RenewBefore + 1)
	h.reconcile(1)
	h.get(sec, names.TLSSecret(uuid))
	if string(sec.Data[pki.CertFile]) == first || h.r.PKI.CA.NeedsRenewal(sec.Data[pki.CertFile], pki.AgentDNSNames(uuid, ns), h.now) {
		t.Fatal("certificate not renewed in its window")
	}
}

func TestPlainAgentURL(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	var bases []string
	h.r.NewAgent = func(base, token string) AgentAPI {
		bases = append(bases, base)
		return h.agent
	}
	h.reconcile(3)
	if h.get(&corev1.Secret{}, names.TLSSecret(uuid)) {
		t.Fatal("tls secret without TLS")
	}
	h.createPod(true)
	h.reconcile(1)
	if len(bases) == 0 || bases[len(bases)-1] != "http://10.0.0.5:8080" {
		t.Fatalf("agent base %q", bases)
	}
}

func (h *harness) agentStatefulSetArgs() string {
	h.t.Helper()
	sts := &appsv1.StatefulSet{}
	if !h.get(sts, names.AgentStatefulSet(uuid)) {
		h.t.Fatal("agent statefulset missing")
	}
	return strings.Join(sts.Spec.Template.Spec.Containers[0].Args, " ")
}
