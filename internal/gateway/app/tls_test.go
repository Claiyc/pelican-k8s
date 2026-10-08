package app

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Claiyc/pelican-k8s/internal/pki"
	"github.com/Claiyc/pelican-k8s/test/fakepanel"
)

// writeGatewayCerts writes a gateway certificate directory for "localhost".
func writeGatewayCerts(t *testing.T) (string, *pki.CA) {
	t.Helper()
	ca, _, err := pki.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := ca.Issue(pki.GatewayName, []string{"localhost"}, pki.Server|pki.Client, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, b := range map[string][]byte{pki.CertFile: certPEM, pki.KeyFile: keyPEM, pki.CAFile: ca.CertPEM} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, ca
}

func TestNewTLS(t *testing.T) {
	api := newFakeAPIServer(t)
	cfg := testConfig("http://panel.invalid")
	cfg.TLSDir, _ = writeGatewayCerts(t)
	g, err := New(context.Background(), cfg, api.restConfig(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if g.certs == nil || g.Agents.TLSConfig() == nil {
		t.Fatal("TLS not wired")
	}

	cfg.TLSDir = t.TempDir()
	if _, err := New(context.Background(), cfg, api.restConfig(), slog.Default()); err == nil || !strings.Contains(err.Error(), "tls:") {
		t.Fatalf("empty certificate directory: %v", err)
	}
}

func TestRemoteAPIServesTLS(t *testing.T) {
	api := newFakeAPIServer(t)
	useRestConfig(t, api.restConfig())
	fp := fakepanel.New(nodeID, nodeToken)
	ps := httptest.NewServer(fp.Handler())
	defer ps.Close()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cfg := testConfig(ps.URL)
	cfg.ListenRemote = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	dir, ca := writeGatewayCerts(t)
	certs, err := pki.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	g := newGateway(t, cfg)
	g.certs = certs
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- g.Run(ctx) }()
	defer func() {
		cancel()
		<-errc
	}()

	// An agent trusts the internal CA and reaches the gateway by its name.
	agent := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: ca.Pool()}}, Timeout: time.Second}
	url := "https://" + net.JoinHostPort("localhost", strconv.Itoa(port)) + "/api/remote/servers"
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := agent.Get(url)
		if err == nil {
			res.Body.Close()
			if res.TLS == nil || res.TLS.PeerCertificates[0].Subject.CommonName != pki.GatewayName {
				t.Fatalf("remote API TLS %+v", res.TLS)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("remote API not serving TLS: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if res, err := http.Get(strings.Replace(url, "https:", "http:", 1)); err == nil {
		res.Body.Close()
		if res.StatusCode < 400 {
			t.Fatalf("plain HTTP served: %d", res.StatusCode)
		}
	}
}
