// Package certs keeps the internal CA and the certificate Secrets the
// operator issues from it (ARCHITECTURE.md 12.5, 12.6).
package certs

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/internal/pki"
)

var partOf = map[string]string{"app.kubernetes.io/part-of": "pelican-k8s"}

// The CA Secret holds the CA (pki.CertFile, pki.KeyFile) and the bundle every
// peer trusts (pki.CAFile). During a rotation it also holds the next CA, and
// two annotations record the stage and when it began.
const (
	NextCertFile = "next.crt"
	NextKeyFile  = "next.key"

	AnnotationRotation      = "pelican-k8s.io/ca-rotation"
	AnnotationRotationSince = "pelican-k8s.io/ca-rotation-since"

	// StageNext: the next CA is trusted everywhere but signs nothing yet.
	StageNext = "next"
	// StagePromoted: the next CA signs; the previous one is still trusted.
	StagePromoted = "promoted"
)

// EnsureCA loads the CA and its trust bundle from the CA Secret, creating
// the Secret first, with a CA valid for lifetime, when it does not exist.
// Replicas that start together agree on the one that was created first.
func EnsureCA(ctx context.Context, c client.Client, key types.NamespacedName, now time.Time, lifetime time.Duration) (*pki.CA, []byte, error) {
	sec := &corev1.Secret{}
	err := c.Get(ctx, key, sec)
	if apierrors.IsNotFound(err) {
		ca, keyPEM, nerr := pki.NewCAFor(now, lifetime)
		if nerr != nil {
			return nil, nil, nerr
		}
		sec = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Labels: partOf},
			Type:       corev1.SecretTypeTLS,
			Data:       map[string][]byte{pki.CertFile: ca.CertPEM, pki.KeyFile: keyPEM, pki.CAFile: ca.CertPEM},
		}
		err = c.Create(ctx, sec)
		if err == nil {
			return ca, ca.CertPEM, nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return nil, nil, err
		}
		// Another replica created it first.
		err = c.Get(ctx, key, sec)
	}
	if err != nil {
		return nil, nil, err
	}
	ca, err := pki.ParseCA(sec.Data[pki.CertFile], sec.Data[pki.KeyFile])
	if err != nil {
		return nil, nil, fmt.Errorf("secret %s: %w", key, err)
	}
	return ca, trustOf(sec.Data, ca), nil
}

// trustOf returns the Secret's trust bundle; a CA Secret written before
// rotation existed has none and trusts its CA alone.
func trustOf(data map[string][]byte, ca *pki.CA) []byte {
	if len(data[pki.CAFile]) > 0 {
		return data[pki.CAFile]
	}
	return ca.CertPEM
}

// Leaf describes a certificate Secret issued from the CA.
type Leaf struct {
	CommonName string
	DNSNames   []string
	Usage      pki.Usage
}

// Data returns the Secret data for the leaf: existing when it is still
// valid for this CA and these names and carries the trust bundle, otherwise
// a freshly issued certificate. changed reports whether the data must be
// written.
func (l Leaf) Data(ca *pki.CA, trust []byte, existing map[string][]byte, now time.Time) (data map[string][]byte, changed bool, err error) {
	if existing != nil && len(existing[pki.KeyFile]) > 0 && !ca.NeedsRenewal(existing[pki.CertFile], l.DNSNames, now) {
		if bytes.Equal(existing[pki.CAFile], trust) {
			return existing, false, nil
		}
		// Only the bundle moved (a rotation began or ended): keep the key pair.
		return map[string][]byte{pki.CertFile: existing[pki.CertFile], pki.KeyFile: existing[pki.KeyFile], pki.CAFile: trust}, true, nil
	}
	certPEM, keyPEM, err := ca.Issue(l.CommonName, l.DNSNames, l.Usage, now)
	if err != nil {
		return nil, false, err
	}
	return map[string][]byte{pki.CertFile: certPEM, pki.KeyFile: keyPEM, pki.CAFile: trust}, true, nil
}

// leafOf describes an issued certificate by its own names and usages.
func leafOf(certPEM []byte) (Leaf, error) {
	cert, err := pki.ParseCertificate(certPEM)
	if err != nil {
		return Leaf{}, err
	}
	l := Leaf{CommonName: cert.Subject.CommonName, DNSNames: cert.DNSNames}
	for _, u := range cert.ExtKeyUsage {
		switch u {
		case x509.ExtKeyUsageServerAuth:
			l.Usage |= pki.Server
		case x509.ExtKeyUsageClientAuth:
			l.Usage |= pki.Client
		}
	}
	return l, nil
}

// Manager keeps the internal CA and the certificates issued from it
// current. It runs on the leader only. On every pass it
//   - reloads the CA Secret into the Issuer, so a new leader starts from the
//     latest CA,
//   - moves a CA rotation forward when one is due (Rotate),
//   - creates or renews the gateway's certificate Secret, a server
//     certificate for the remote API that is also its client certificate
//     toward agents,
//   - renews agent certificate Secrets and gives them the current bundle.
//     The reconciler creates them and renews them as it passes; the manager
//     covers servers it does not visit and drives a rotation through. An
//     agent's names come from its server UUID label, as in the reconciler.
type Manager struct {
	Client client.Client
	CAKey  types.NamespacedName
	Issuer *pki.Issuer
	// GatewayKey and GatewayNames are the gateway's Secret and DNS names.
	GatewayKey   types.NamespacedName
	GatewayNames []string
	// AgentNamespace holds the agents' certificate Secrets.
	AgentNamespace string

	// Rotate replaces the CA once it enters the last third of its lifetime;
	// the new one is valid for Lifetime. Overlap is how long each step of a
	// rotation waits for mounted Secrets to reach every pod.
	Rotate   bool
	Lifetime time.Duration
	Overlap  time.Duration

	Interval time.Duration
	Now      func() time.Time
	Log      logr.Logger
}

// Start implements manager.Runnable.
func (m *Manager) Start(ctx context.Context) error {
	interval := m.Interval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	for {
		if err := m.Sync(ctx); err != nil {
			m.Log.Error(err, "certificates")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (m *Manager) NeedLeaderElection() bool { return true }

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Sync makes one pass.
func (m *Manager) Sync(ctx context.Context) error {
	now := m.now()
	sec := &corev1.Secret{}
	if err := m.Client.Get(ctx, m.CAKey, sec); err != nil {
		return err
	}
	ca, err := pki.ParseCA(sec.Data[pki.CertFile], sec.Data[pki.KeyFile])
	if err != nil {
		return fmt.Errorf("secret %s: %w", m.CAKey, err)
	}
	leaves, err := m.agentLeaves(ctx)
	if err != nil {
		return err
	}
	gw := &corev1.Secret{}
	switch err := m.Client.Get(ctx, m.GatewayKey, gw); {
	case err == nil:
		leaves = append(leaves, gw)
	case apierrors.IsNotFound(err):
		gw = nil
	default:
		return err
	}
	if ca, err = m.rotate(ctx, sec, ca, leaves, now); err != nil {
		return err
	}
	trust := trustOf(sec.Data, ca)
	if err := m.Issuer.SetCA(ca, trust); err != nil {
		return err
	}
	if err := m.ensureGateway(ctx, gw, ca, trust, now); err != nil {
		return err
	}
	var errs []error
	for _, s := range leaves {
		if s == gw {
			continue
		}
		if err := m.renew(ctx, s, ca, trust, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// agentLeaves lists the agents' certificate Secrets.
func (m *Manager) agentLeaves(ctx context.Context) ([]*corev1.Secret, error) {
	if m.AgentNamespace == "" {
		return nil, nil
	}
	list := &corev1.SecretList{}
	if err := m.Client.List(ctx, list, client.InNamespace(m.AgentNamespace), client.MatchingLabels{v1alpha1.LabelComponent: "agent"}); err != nil {
		return nil, err
	}
	var out []*corev1.Secret
	for i := range list.Items {
		s := &list.Items[i]
		if uuid := s.Labels[v1alpha1.LabelServerUUID]; uuid != "" && s.Name == names.TLSSecret(uuid) {
			out = append(out, s)
		}
	}
	return out, nil
}

// rotate moves a CA rotation one step forward when its conditions hold and
// returns the CA that signs from now on. A rotation goes
//
//	(none) -> next:      a new CA joins the bundle; the old one still signs
//	next -> promoted:    once every certificate Secret carries that bundle
//	                     and Overlap has passed, the new CA signs
//	promoted -> (none):  once every certificate is signed by the new CA and
//	                     Overlap has passed, the old CA leaves the bundle
//
// so that at every moment each peer trusts the CA of every certificate it
// may be shown.
func (m *Manager) rotate(ctx context.Context, sec *corev1.Secret, ca *pki.CA, leaves []*corev1.Secret, now time.Time) (*pki.CA, error) {
	stage := sec.Annotations[AnnotationRotation]
	since, _ := time.Parse(time.RFC3339, sec.Annotations[AnnotationRotationSince])
	settled := !now.Before(since.Add(m.Overlap))
	trust := trustOf(sec.Data, ca)
	data := map[string][]byte{}
	for k, v := range sec.Data {
		data[k] = v
	}
	var next string
	switch {
	case stage == "" && m.Rotate && ca.NeedsRotation(now):
		lifetime := m.Lifetime
		if lifetime <= 0 {
			lifetime = pki.CALifetime
		}
		n, keyPEM, err := pki.NewCAFor(now, lifetime)
		if err != nil {
			return nil, err
		}
		data[NextCertFile], data[NextKeyFile] = n.CertPEM, keyPEM
		data[pki.CAFile] = joinPEM(trust, n.CertPEM)
		next = StageNext
	case stage == StageNext && settled && allCarry(leaves, trust):
		n, err := pki.ParseCA(data[NextCertFile], data[NextKeyFile])
		if err != nil {
			return nil, fmt.Errorf("secret %s: next CA: %w", m.CAKey, err)
		}
		data[pki.CertFile], data[pki.KeyFile] = data[NextCertFile], data[NextKeyFile]
		delete(data, NextCertFile)
		delete(data, NextKeyFile)
		ca = n
		next = StagePromoted
	case stage == StagePromoted && settled && allCarry(leaves, trust) && allSignedBy(leaves, ca, now):
		data[pki.CAFile] = ca.CertPEM
	default:
		return ca, nil
	}
	sec.Data = data
	if sec.Annotations == nil {
		sec.Annotations = map[string]string{}
	}
	if next == "" {
		delete(sec.Annotations, AnnotationRotation)
		delete(sec.Annotations, AnnotationRotationSince)
	} else {
		sec.Annotations[AnnotationRotation] = next
		sec.Annotations[AnnotationRotationSince] = now.UTC().Format(time.RFC3339)
	}
	if err := m.Client.Update(ctx, sec); err != nil {
		return nil, err
	}
	switch next {
	case StageNext:
		m.Log.Info("CA rotation: next CA added to the trust bundle", "secret", m.CAKey.String())
	case StagePromoted:
		m.Log.Info("CA rotation: next CA promoted, reissuing certificates", "secret", m.CAKey.String())
	default:
		m.Log.Info("CA rotation: previous CA removed from the trust bundle", "secret", m.CAKey.String())
	}
	return ca, nil
}

func joinPEM(a, b []byte) []byte {
	out := slices.Clone(a)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return append(out, b...)
}

func allCarry(leaves []*corev1.Secret, trust []byte) bool {
	for _, s := range leaves {
		if !bytes.Equal(s.Data[pki.CAFile], trust) {
			return false
		}
	}
	return true
}

func allSignedBy(leaves []*corev1.Secret, ca *pki.CA, now time.Time) bool {
	for _, s := range leaves {
		l, err := leafOf(s.Data[pki.CertFile])
		if err != nil || ca.NeedsRenewal(s.Data[pki.CertFile], l.DNSNames, now) {
			return false
		}
	}
	return true
}

// ensureGateway creates or renews the gateway's certificate Secret.
func (m *Manager) ensureGateway(ctx context.Context, existing *corev1.Secret, ca *pki.CA, trust []byte, now time.Time) error {
	leaf := Leaf{CommonName: pki.GatewayName, DNSNames: m.GatewayNames, Usage: pki.Server | pki.Client}
	if existing == nil {
		data, _, err := leaf.Data(ca, trust, nil, now)
		if err != nil {
			return err
		}
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: m.GatewayKey.Namespace, Name: m.GatewayKey.Name, Labels: partOf},
			Type:       corev1.SecretTypeTLS,
			Data:       data,
		}
		if err := m.Client.Create(ctx, sec); err != nil {
			return err
		}
		m.Log.Info("issued gateway certificate", "secret", m.GatewayKey.String())
		return nil
	}
	data, changed, err := leaf.Data(ca, trust, existing.Data, now)
	if err != nil || !changed {
		return err
	}
	existing.Data = data
	if err := m.Client.Update(ctx, existing); err != nil {
		return err
	}
	m.Log.Info("renewed gateway certificate", "secret", m.GatewayKey.String())
	return nil
}

// renew renews an agent's certificate Secret.
func (m *Manager) renew(ctx context.Context, s *corev1.Secret, ca *pki.CA, trust []byte, now time.Time) error {
	uuid := s.Labels[v1alpha1.LabelServerUUID]
	leaf := Leaf{CommonName: names.AgentService(uuid), DNSNames: pki.AgentDNSNames(uuid, s.Namespace), Usage: pki.Server}
	data, changed, err := leaf.Data(ca, trust, s.Data, now)
	if err != nil || !changed {
		return err
	}
	s.Data = data
	if err := m.Client.Update(ctx, s); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	return nil
}
