package agents

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
	"github.com/Claiyc/pelican-k8s/internal/pki"
)

func TestTLS(t *testing.T) {
	now := time.Now()
	ca, _, err := pki.NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	pair := func(cn string, dns []string, usage pki.Usage) tls.Certificate {
		certPEM, keyPEM, err := ca.Issue(cn, dns, usage, now)
		if err != nil {
			t.Fatal(err)
		}
		c, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	var gotHost, gotClient string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotClient = r.Host, r.TLS.VerifiedChains[0][0].Subject.CommonName
		_, _ = io.WriteString(w, "ok")
	}))
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair("agent", pki.AgentDNSNames(testUUID, testNS), pki.Server)}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: ca.Pool()}
	srv.StartTLS()
	defer srv.Close()

	st := store.New(newClient(t, agentPod("10.0.0.9", true), agentSecret()), testNS, "default")
	r := NewResolver(st, time.Second)
	var dialed []string
	addr := srv.Listener.Addr().String()
	r.Transport.DialContext = func(ctx context.Context, network, a string) (net.Conn, error) {
		dialed = append(dialed, a)
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	gw := pair(pki.GatewayName, nil, pki.Client)
	r.EnableTLS(func() *tls.Config {
		return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: ca.Pool(), Certificates: []tls.Certificate{gw}}
	})
	if r.TLSConfig() == nil {
		t.Fatal("TLSConfig nil after EnableTLS")
	}

	target, err := r.Resolve(context.Background(), testUUID)
	if err != nil {
		t.Fatal(err)
	}
	if !target.TLS || target.Namespace != testNS {
		t.Fatalf("target %+v", target)
	}
	status, body, err := r.Do(context.Background(), target, http.MethodGet, "/api/servers/"+testUUID, nil, "")
	if err != nil || status != http.StatusOK || string(body) != "ok" {
		t.Fatalf("Do: %d %q %v", status, body, err)
	}
	if gotClient != pki.GatewayName || gotHost != pki.AgentHost("10.0.0.9", testUUID, testNS)+":8080" {
		t.Errorf("agent saw client %q host %q", gotClient, gotHost)
	}
	if len(dialed) != 1 || dialed[0] != "10.0.0.9:8080" {
		t.Errorf("dialed %q", dialed)
	}

	// The certificate of one server does not pass for another.
	other := *target
	other.UUID = "another"
	if _, _, err := r.Do(context.Background(), &other, http.MethodGet, "/", nil, ""); err == nil {
		t.Error("agent certificate accepted for another server")
	}
}
