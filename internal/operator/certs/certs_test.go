package certs

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/pki"
)

var (
	t0      = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	caKey   = types.NamespacedName{Namespace: "pelican-system", Name: "pelican-ca"}
	gwKey   = types.NamespacedName{Namespace: "pelican-system", Name: "pelican-gateway-tls"}
	gwNames = []string{"pelican-gateway", "pelican-gateway.pelican-system", "pelican-gateway.pelican-system.svc"}
)

func newClient(funcs interceptor.Funcs, objs ...client.Object) client.Client {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
}

func TestEnsureCA(t *testing.T) {
	ctx := context.Background()
	c := newClient(interceptor.Funcs{})
	ca, trust, err := EnsureCA(ctx, c, caKey, t0, pki.CALifetime)
	if err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{}
	if err := c.Get(ctx, caKey, sec); err != nil {
		t.Fatal(err)
	}
	if sec.Type != corev1.SecretTypeTLS || !bytes.Equal(sec.Data[pki.CertFile], ca.CertPEM) || !bytes.Equal(sec.Data[pki.CAFile], ca.CertPEM) || !bytes.Equal(trust, ca.CertPEM) {
		t.Fatalf("CA secret %+v", sec)
	}
	again, _, err := EnsureCA(ctx, c, caKey, t0.Add(time.Hour), pki.CALifetime)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Cert.Equal(ca.Cert) {
		t.Fatal("existing CA replaced")
	}
}

// TestEnsureCARace covers two replicas starting at once: the one that loses
// the create uses the winner's CA.
func TestEnsureCARace(t *testing.T) {
	ctx := context.Background()
	winner, key, err := pki.NewCA(t0)
	if err != nil {
		t.Fatal(err)
	}
	gets := 0
	c := newClient(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			gets++
			if gets == 1 {
				return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, k.Name)
			}
			return c.Get(ctx, k, obj, opts...)
		},
	}, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: caKey.Namespace, Name: caKey.Name}, Data: map[string][]byte{pki.CertFile: winner.CertPEM, pki.KeyFile: key}})
	got, trust, err := EnsureCA(ctx, c, caKey, t0, pki.CALifetime)
	if err != nil {
		t.Fatal(err)
	}
	// The winner's Secret predates trust bundles: it trusts its CA alone.
	if !got.Cert.Equal(winner.Cert) || !bytes.Equal(trust, winner.CertPEM) {
		t.Fatal("lost the race but kept its own CA")
	}
}

func TestEnsureCAErrors(t *testing.T) {
	ctx := context.Background()
	broken := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: caKey.Namespace, Name: caKey.Name}, Data: map[string][]byte{pki.CertFile: []byte("x")}}
	if _, _, err := EnsureCA(ctx, newClient(interceptor.Funcs{}, broken), caKey, t0, pki.CALifetime); err == nil {
		t.Error("broken CA secret: want error")
	}
	boom := errors.New("boom")
	failGet := interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return boom
	}}
	if _, _, err := EnsureCA(ctx, newClient(failGet), caKey, t0, pki.CALifetime); !errors.Is(err, boom) {
		t.Errorf("get error: %v", err)
	}
	failCreate := interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error { return boom }}
	if _, _, err := EnsureCA(ctx, newClient(failCreate), caKey, t0, pki.CALifetime); !errors.Is(err, boom) {
		t.Errorf("create error: %v", err)
	}
}

const agentNS = "pelican-servers"

type rig struct {
	t   *testing.T
	ctx context.Context
	c   client.Client
	m   *Manager
	now time.Time
}

// newRig starts from a CA valid for lifetime and one agent certificate.
func newRig(t *testing.T, lifetime time.Duration, funcs interceptor.Funcs) *rig {
	t.Helper()
	r := &rig{t: t, ctx: context.Background(), now: t0}
	r.c = newClient(funcs)
	ca, trust, err := EnsureCA(r.ctx, r.c, caKey, t0, lifetime)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := pki.NewIssuer(ca, trust, pki.OperatorName)
	if err != nil {
		t.Fatal(err)
	}
	issuer.Now = func() time.Time { return r.now }
	r.m = &Manager{
		Client: r.c, CAKey: caKey, Issuer: issuer, GatewayKey: gwKey, GatewayNames: gwNames,
		AgentNamespace: agentNS, Lifetime: lifetime, Overlap: time.Hour,
		Now: func() time.Time { return r.now }, Log: logr.Discard(),
	}
	data, _, err := Leaf{CommonName: "gs-u1-agent", DNSNames: pki.AgentDNSNames("u1", agentNS), Usage: pki.Server}.Data(ca, trust, nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Namespace: agentNS, Name: "gs-u1-tls", Labels: map[string]string{v1alpha1.LabelComponent: "agent", v1alpha1.LabelServerUUID: "u1"}}, Data: data},
		// Another agent Secret is left alone.
		{ObjectMeta: metav1.ObjectMeta{Namespace: agentNS, Name: "gs-u1-shim", Labels: map[string]string{v1alpha1.LabelComponent: "agent", v1alpha1.LabelServerUUID: "u1"}}, Data: map[string][]byte{"token": []byte("x")}},
	} {
		if err := r.c.Create(r.ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func (r *rig) secret(key types.NamespacedName) *corev1.Secret {
	r.t.Helper()
	sec := &corev1.Secret{}
	if err := r.c.Get(r.ctx, key, sec); err != nil {
		r.t.Fatal(err)
	}
	return sec
}

func (r *rig) sync() {
	r.t.Helper()
	if err := r.m.Sync(r.ctx); err != nil {
		r.t.Fatal(err)
	}
}

var agentKey = types.NamespacedName{Namespace: agentNS, Name: "gs-u1-tls"}

func (r *rig) stage() string { return r.secret(caKey).Annotations[AnnotationRotation] }

// verifies reports whether the certificate in leaf verifies against the
// bundle in peer, as that peer would check it.
func verifies(leaf, peer *corev1.Secret, usage x509.ExtKeyUsage) bool {
	cert, err := pki.ParseCertificate(leaf.Data[pki.CertFile])
	if err != nil {
		return false
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(peer.Data[pki.CAFile])
	_, err = cert.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: cert.NotBefore.Add(time.Hour), KeyUsages: []x509.ExtKeyUsage{usage}})
	return err == nil
}

func TestManagerGateway(t *testing.T) {
	r := newRig(t, pki.CALifetime, interceptor.Funcs{})
	r.sync()
	sec := r.secret(gwKey)
	cert, err := pki.ParseCertificate(sec.Data[pki.CertFile])
	if err != nil {
		t.Fatal(err)
	}
	ca := r.m.Issuer.CA()
	if cert.Subject.CommonName != pki.GatewayName || len(cert.ExtKeyUsage) != 2 || cert.VerifyHostname("pelican-gateway.pelican-system.svc") != nil || !bytes.Equal(sec.Data[pki.CAFile], ca.CertPEM) {
		t.Fatalf("gateway certificate %+v", cert)
	}
	first := string(sec.Data[pki.CertFile])
	r.sync()
	if string(r.secret(gwKey).Data[pki.CertFile]) != first {
		t.Fatal("fresh certificate replaced")
	}
	r.now = r.now.Add(pki.LeafLifetime - pki.RenewBefore + time.Hour)
	r.sync()
	second := string(r.secret(gwKey).Data[pki.CertFile])
	if second == first {
		t.Fatal("gateway certificate not renewed")
	}
	if ca.NeedsRenewal(r.secret(agentKey).Data[pki.CertFile], pki.AgentDNSNames("u1", agentNS), r.now) {
		t.Fatal("agent certificate not renewed")
	}
	r.m.GatewayNames = append(r.m.GatewayNames, "gateway.example.internal")
	r.sync()
	if string(r.secret(gwKey).Data[pki.CertFile]) == second {
		t.Fatal("certificate not reissued for new names")
	}
	if r.stage() != "" {
		t.Fatal("rotation started while disabled")
	}
}

func TestManagerStart(t *testing.T) {
	r := newRig(t, pki.CALifetime, interceptor.Funcs{})
	r.m.Now, r.m.Interval = nil, 10*time.Millisecond
	if !r.m.NeedLeaderElection() {
		t.Error("certificates must be written by the leader only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := r.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r.secret(gwKey)
}

// TestRotation walks a CA rotation through and checks at each step that
// every certificate verifies against every peer's bundle.
func TestRotation(t *testing.T) {
	const life = 30 * 24 * time.Hour
	failAgent := false
	r := newRig(t, life, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if failAgent && obj.GetName() == agentKey.Name {
			return errors.New("boom")
		}
		return c.Update(ctx, obj, opts...)
	}})
	r.m.Rotate = true
	old := r.m.Issuer.CA()
	r.sync()
	if r.stage() != "" {
		t.Fatal("rotation started early")
	}
	consistent := func(step string) {
		t.Helper()
		gw, agent := r.secret(gwKey), r.secret(agentKey)
		if !verifies(agent, gw, x509.ExtKeyUsageServerAuth) || !verifies(gw, agent, x509.ExtKeyUsageClientAuth) {
			t.Fatalf("%s: gateway and agent do not trust each other", step)
		}
		op, err := r.m.Issuer.GetClientCertificate(nil)
		if err != nil {
			t.Fatal(err)
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(agent.Data[pki.CAFile])
		if _, err := op.Leaf.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: r.now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
			t.Fatalf("%s: agent does not trust the operator: %v", step, err)
		}
	}
	consistent("start")

	// Last third: the next CA joins every bundle.
	r.now = t0.Add(21 * 24 * time.Hour)
	r.sync()
	ca := r.secret(caKey)
	if r.stage() != StageNext || len(ca.Data[NextCertFile]) == 0 || !bytes.Equal(r.secret(agentKey).Data[pki.CAFile], ca.Data[pki.CAFile]) {
		t.Fatalf("next stage: %v", ca.Annotations)
	}
	if !r.m.Issuer.CA().Cert.Equal(old.Cert) {
		t.Fatal("next CA signs before it is trusted everywhere")
	}
	consistent("next")

	// An agent Secret that missed the bundle holds the promotion.
	r.now = r.now.Add(2 * time.Hour)
	stale := r.secret(agentKey)
	stale.Data[pki.CAFile] = old.CertPEM
	if err := r.c.Update(r.ctx, stale); err != nil {
		t.Fatal(err)
	}
	failAgent = true
	if err := r.m.Sync(r.ctx); err == nil {
		t.Fatal("failed agent update not reported")
	}
	if r.stage() != StageNext {
		t.Fatal("promoted while an agent lacked the bundle")
	}
	failAgent = false
	r.sync() // the agent gets the bundle
	r.sync() // and the next CA is promoted
	if r.stage() != StagePromoted || r.m.Issuer.CA().Cert.Equal(old.Cert) {
		t.Fatalf("promotion: %v", r.secret(caKey).Annotations)
	}
	consistent("promoted")
	for _, k := range []types.NamespacedName{gwKey, agentKey} {
		l, _ := leafOf(r.secret(k).Data[pki.CertFile])
		if r.m.Issuer.CA().NeedsRenewal(r.secret(k).Data[pki.CertFile], l.DNSNames, r.now) {
			t.Fatalf("%s not reissued by the new CA", k.Name)
		}
	}

	// Within the overlap the old CA stays trusted.
	r.now = r.now.Add(30 * time.Minute)
	r.sync()
	if r.stage() != StagePromoted {
		t.Fatal("old CA dropped within the overlap")
	}
	r.now = r.now.Add(time.Hour)
	r.sync()
	final := r.secret(caKey)
	if r.stage() != "" || final.Annotations[AnnotationRotationSince] != "" || len(final.Data[NextCertFile]) != 0 || !bytes.Equal(final.Data[pki.CAFile], final.Data[pki.CertFile]) {
		t.Fatalf("rotation not finished: %v", final.Annotations)
	}
	if !bytes.Equal(r.secret(agentKey).Data[pki.CAFile], final.Data[pki.CertFile]) {
		t.Fatal("agent still trusts the old CA")
	}
	consistent("done")
	r.sync()
	if r.stage() != "" {
		t.Fatal("a fresh CA was rotated again")
	}
}

func TestLeafData(t *testing.T) {
	ca, _, _ := pki.NewCA(t0)
	other, _, _ := pki.NewCA(t0)
	leaf := Leaf{CommonName: "x", DNSNames: []string{"x"}, Usage: pki.Server}
	data, changed, err := leaf.Data(ca, ca.CertPEM, nil, t0)
	if err != nil || !changed {
		t.Fatal("new leaf not issued")
	}
	if _, changed, _ := leaf.Data(ca, ca.CertPEM, data, t0); changed {
		t.Error("valid leaf reissued")
	}
	// A new bundle keeps the key pair.
	both := joinPEM(ca.CertPEM, other.CertPEM)
	moved, changed, _ := leaf.Data(ca, both, data, t0)
	if !changed || !bytes.Equal(moved[pki.CertFile], data[pki.CertFile]) || !bytes.Equal(moved[pki.CAFile], both) {
		t.Error("bundle change reissued the certificate or kept the old bundle")
	}
	// A new CA reissues the leaf, and so does a missing key.
	if _, changed, _ := leaf.Data(other, other.CertPEM, data, t0); !changed {
		t.Error("leaf of another CA kept")
	}
	cp := map[string][]byte{pki.CertFile: data[pki.CertFile], pki.CAFile: data[pki.CAFile]}
	if _, changed, _ := leaf.Data(ca, ca.CertPEM, cp, t0); !changed {
		t.Error("leaf without a key kept")
	}
}

func TestCertManagerCertificate(t *testing.T) {
	m := &CertManager{IssuerName: "corp-ca"}
	labels := map[string]string{"a": "b"}
	u := m.Certificate(metav1.ObjectMeta{Namespace: agentNS, Name: "gs-u1-tls", Labels: labels}, "gs-u1-tls",
		Leaf{CommonName: "gs-u1-agent", DNSNames: []string{"gs-u1-agent"}, Usage: pki.Server | pki.Client})
	if u.GroupVersionKind() != CertificateGVK || u.GetNamespace() != agentNS || u.GetLabels()["a"] != "b" {
		t.Fatalf("certificate %v", u.Object)
	}
	spec := u.Object["spec"].(map[string]any)
	ref := spec["issuerRef"].(map[string]any)
	if spec["secretName"] != "gs-u1-tls" || ref["name"] != "corp-ca" || ref["kind"] != "Issuer" || ref["group"] != "cert-manager.io" {
		t.Fatalf("spec %v", spec)
	}
	if got := spec["usages"].([]any); len(got) != 3 || got[1] != "server auth" || got[2] != "client auth" {
		t.Fatalf("usages %v", got)
	}
	if spec["duration"] != "2160h0m0s" || spec["renewBefore"] != "720h0m0s" {
		t.Fatalf("lifetime %v %v", spec["duration"], spec["renewBefore"])
	}
}
