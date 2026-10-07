// Package certs keeps the internal CA and the certificate Secrets the
// operator issues from it (ARCHITECTURE.md 12.5).
package certs

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Claiyc/pelican-k8s/internal/pki"
)

var partOf = map[string]string{"app.kubernetes.io/part-of": "pelican-k8s"}

// EnsureCA loads the CA from its Secret, creating the Secret first when it
// does not exist. Replicas that start together agree on the one that was
// created first.
func EnsureCA(ctx context.Context, c client.Client, key types.NamespacedName, now time.Time) (*pki.CA, error) {
	sec := &corev1.Secret{}
	err := c.Get(ctx, key, sec)
	if apierrors.IsNotFound(err) {
		ca, keyPEM, err := pki.NewCA(now)
		if err != nil {
			return nil, err
		}
		sec = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Labels: partOf},
			Type:       corev1.SecretTypeTLS,
			Data:       map[string][]byte{pki.CertFile: ca.CertPEM, pki.KeyFile: keyPEM},
		}
		if err := c.Create(ctx, sec); err == nil {
			return ca, nil
		} else if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		err = c.Get(ctx, key, sec)
	}
	if err != nil {
		return nil, err
	}
	ca, err := pki.ParseCA(sec.Data[pki.CertFile], sec.Data[pki.KeyFile])
	if err != nil {
		return nil, fmt.Errorf("secret %s: %w", key, err)
	}
	return ca, nil
}

// Leaf describes a certificate Secret issued from the CA.
type Leaf struct {
	CommonName string
	DNSNames   []string
	Usage      pki.Usage
}

// Data returns the Secret data for the leaf: existing when it is still
// valid for this CA and these names, otherwise a freshly issued certificate.
// changed reports whether the data must be written.
func (l Leaf) Data(ca *pki.CA, existing map[string][]byte, now time.Time) (data map[string][]byte, changed bool, err error) {
	if existing != nil && !ca.NeedsRenewal(existing[pki.CertFile], l.DNSNames, now) && string(existing[pki.CAFile]) == string(ca.CertPEM) && len(existing[pki.KeyFile]) > 0 {
		return existing, false, nil
	}
	certPEM, keyPEM, err := ca.Issue(l.CommonName, l.DNSNames, l.Usage, now)
	if err != nil {
		return nil, false, err
	}
	return map[string][]byte{pki.CertFile: certPEM, pki.KeyFile: keyPEM, pki.CAFile: ca.CertPEM}, true, nil
}

// Gateway keeps the gateway's certificate Secret current: a server
// certificate for the remote API that is also its client certificate toward
// agents. It runs on the leader only.
type Gateway struct {
	Client   client.Client
	Key      types.NamespacedName
	CA       *pki.CA
	DNSNames []string
	Interval time.Duration
	Now      func() time.Time
	Log      logr.Logger
}

// Start implements manager.Runnable.
func (g *Gateway) Start(ctx context.Context) error {
	interval := g.Interval
	if interval <= 0 {
		interval = time.Hour
	}
	for {
		if err := g.Ensure(ctx); err != nil {
			g.Log.Error(err, "gateway certificate", "secret", g.Key.String())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// Ensure creates or renews the gateway's certificate Secret.
func (g *Gateway) Ensure(ctx context.Context) error {
	now := time.Now()
	if g.Now != nil {
		now = g.Now()
	}
	leaf := Leaf{CommonName: pki.GatewayName, DNSNames: g.DNSNames, Usage: pki.Server | pki.Client}
	sec := &corev1.Secret{}
	err := g.Client.Get(ctx, g.Key, sec)
	if apierrors.IsNotFound(err) {
		data, _, err := leaf.Data(g.CA, nil, now)
		if err != nil {
			return err
		}
		sec = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: g.Key.Namespace, Name: g.Key.Name, Labels: partOf},
			Type:       corev1.SecretTypeTLS,
			Data:       data,
		}
		if err := g.Client.Create(ctx, sec); err != nil {
			return err
		}
		g.Log.Info("issued gateway certificate", "secret", g.Key.String())
		return nil
	}
	if err != nil {
		return err
	}
	data, changed, err := leaf.Data(g.CA, sec.Data, now)
	if err != nil || !changed {
		return err
	}
	sec.Data = data
	if err := g.Client.Update(ctx, sec); err != nil {
		return err
	}
	g.Log.Info("renewed gateway certificate", "secret", g.Key.String())
	return nil
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (g *Gateway) NeedLeaderElection() bool { return true }
