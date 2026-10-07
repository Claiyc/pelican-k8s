package certs

import (
	"bytes"
	"context"
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
	ca, err := EnsureCA(ctx, c, caKey, t0)
	if err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{}
	if err := c.Get(ctx, caKey, sec); err != nil {
		t.Fatal(err)
	}
	if sec.Type != corev1.SecretTypeTLS || !bytes.Equal(sec.Data[pki.CertFile], ca.CertPEM) {
		t.Fatalf("CA secret %+v", sec)
	}
	again, err := EnsureCA(ctx, c, caKey, t0.Add(time.Hour))
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
	got, err := EnsureCA(ctx, c, caKey, t0)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Cert.Equal(winner.Cert) {
		t.Fatal("lost the race but kept its own CA")
	}
}

func TestEnsureCAErrors(t *testing.T) {
	ctx := context.Background()
	broken := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: caKey.Namespace, Name: caKey.Name}, Data: map[string][]byte{pki.CertFile: []byte("x")}}
	if _, err := EnsureCA(ctx, newClient(interceptor.Funcs{}, broken), caKey, t0); err == nil {
		t.Error("broken CA secret: want error")
	}
	boom := errors.New("boom")
	failGet := interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return boom
	}}
	if _, err := EnsureCA(ctx, newClient(failGet), caKey, t0); !errors.Is(err, boom) {
		t.Errorf("get error: %v", err)
	}
	failCreate := interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error { return boom }}
	if _, err := EnsureCA(ctx, newClient(failCreate), caKey, t0); !errors.Is(err, boom) {
		t.Errorf("create error: %v", err)
	}
}

func TestGatewayEnsure(t *testing.T) {
	ctx := context.Background()
	ca, _, err := pki.NewCA(t0)
	if err != nil {
		t.Fatal(err)
	}
	now := t0
	c := newClient(interceptor.Funcs{})
	g := &Gateway{Client: c, Key: gwKey, CA: ca, DNSNames: gwNames, Now: func() time.Time { return now }, Log: logr.Discard()}
	read := func() *corev1.Secret {
		t.Helper()
		sec := &corev1.Secret{}
		if err := c.Get(ctx, gwKey, sec); err != nil {
			t.Fatal(err)
		}
		return sec
	}

	if err := g.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	sec := read()
	cert, err := pki.ParseCertificate(sec.Data[pki.CertFile])
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != pki.GatewayName || len(cert.ExtKeyUsage) != 2 || cert.VerifyHostname("pelican-gateway.pelican-system.svc") != nil || !bytes.Equal(sec.Data[pki.CAFile], ca.CertPEM) {
		t.Fatalf("gateway certificate %+v", cert)
	}
	first := string(sec.Data[pki.CertFile])

	if err := g.Ensure(ctx); err != nil || string(read().Data[pki.CertFile]) != first {
		t.Fatalf("fresh certificate replaced (%v)", err)
	}
	now = now.Add(pki.LeafLifetime - pki.RenewBefore + time.Hour)
	if err := g.Ensure(ctx); err != nil || string(read().Data[pki.CertFile]) == first {
		t.Fatalf("certificate not renewed (%v)", err)
	}
	second := string(read().Data[pki.CertFile])
	g.DNSNames = append(g.DNSNames, "gateway.example.internal")
	if err := g.Ensure(ctx); err != nil || string(read().Data[pki.CertFile]) == second {
		t.Fatalf("certificate not reissued for new names (%v)", err)
	}
}

func TestGatewayStart(t *testing.T) {
	ca, _, err := pki.NewCA(t0)
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(interceptor.Funcs{})
	g := &Gateway{Client: c, Key: gwKey, CA: ca, DNSNames: gwNames, Interval: 10 * time.Millisecond, Log: logr.Discard()}
	if !g.NeedLeaderElection() {
		t.Error("the gateway certificate must be written by the leader only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := g.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), gwKey, &corev1.Secret{}); err != nil {
		t.Fatal(err)
	}
}

func TestLeafData(t *testing.T) {
	ca, _, _ := pki.NewCA(t0)
	other, _, _ := pki.NewCA(t0)
	leaf := Leaf{CommonName: "x", DNSNames: []string{"x"}, Usage: pki.Server}
	data, changed, err := leaf.Data(ca, nil, t0)
	if err != nil || !changed {
		t.Fatal("new leaf not issued")
	}
	if _, changed, _ := leaf.Data(ca, data, t0); changed {
		t.Error("valid leaf reissued")
	}
	// A new CA reissues every leaf, and so does a missing key or bundle.
	if _, changed, _ := leaf.Data(other, data, t0); !changed {
		t.Error("leaf of another CA kept")
	}
	for _, k := range []string{pki.KeyFile, pki.CAFile} {
		cp := map[string][]byte{}
		for kk, v := range data {
			if kk != k {
				cp[kk] = v
			}
		}
		if _, changed, _ := leaf.Data(ca, cp, t0); !changed {
			t.Errorf("leaf without %s kept", k)
		}
	}
}
