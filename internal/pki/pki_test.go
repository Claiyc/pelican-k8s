package pki

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

const (
	testUUID = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	testNS   = "pelican-servers"
)

var t0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func newCA(t *testing.T) (*CA, []byte) {
	t.Helper()
	ca, key, err := NewCA(t0)
	if err != nil {
		t.Fatal(err)
	}
	return ca, key
}

func TestCARoundTrip(t *testing.T) {
	ca, key := newCA(t)
	if !ca.Cert.IsCA || ca.Cert.NotAfter.Sub(t0) != CALifetime {
		t.Fatalf("CA: isCA=%v notAfter=%v", ca.Cert.IsCA, ca.Cert.NotAfter)
	}
	parsed, err := ParseCA(ca.CertPEM, key)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Cert.Equal(ca.Cert) {
		t.Fatal("parsed CA differs")
	}

	leaf, leafKey, err := ca.Issue("x", nil, Server, t0)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ cert, key []byte }{
		"no certificate": {nil, key},
		"no key":         {ca.CertPEM, nil},
		"bad key":        {ca.CertPEM, []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")},
		"not a CA":       {leaf, leafKey},
	} {
		if _, err := ParseCA(tc.cert, tc.key); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestIssue(t *testing.T) {
	ca, _ := newCA(t)
	cases := map[string]struct {
		usage Usage
		want  []x509.ExtKeyUsage
	}{
		"server":        {Server, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}},
		"client":        {Client, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}},
		"server+client": {Server | Client, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			certPEM, keyPEM, err := ca.Issue("cn", []string{"a.example"}, tc.usage, t0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
				t.Fatal(err)
			}
			cert, err := ParseCertificate(certPEM)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(cert.ExtKeyUsage, tc.want) || cert.Subject.CommonName != "cn" || cert.IsCA {
				t.Errorf("usage %v cn %q isCA %v", cert.ExtKeyUsage, cert.Subject.CommonName, cert.IsCA)
			}
			if cert.NotAfter.Sub(t0) != LeafLifetime {
				t.Errorf("lifetime %v", cert.NotAfter.Sub(t0))
			}
			if _, err := cert.Verify(x509.VerifyOptions{Roots: ca.Pool(), CurrentTime: t0, KeyUsages: tc.want}); err != nil {
				t.Errorf("verify: %v", err)
			}
		})
	}

	t.Run("capped at the CA's expiry", func(t *testing.T) {
		late := ca.Cert.NotAfter.Add(-24 * time.Hour)
		certPEM, _, err := ca.Issue("cn", nil, Server, late)
		if err != nil {
			t.Fatal(err)
		}
		cert, _ := ParseCertificate(certPEM)
		if !cert.NotAfter.Equal(ca.Cert.NotAfter) {
			t.Errorf("notAfter %v, CA %v", cert.NotAfter, ca.Cert.NotAfter)
		}
		// A short certificate renews in the last third of its life.
		if ca.NeedsRenewal(certPEM, nil, late) {
			t.Error("fresh short certificate needs renewal")
		}
		if !ca.NeedsRenewal(certPEM, nil, late.Add(17*time.Hour)) {
			t.Error("short certificate past two thirds does not need renewal")
		}
	})
}

func TestNeedsRenewal(t *testing.T) {
	ca, _ := newCA(t)
	other, _ := newCA(t)
	dns := []string{"a", "b"}
	certPEM, _, err := ca.Issue("cn", dns, Server, t0)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		ca   *CA
		cert []byte
		dns  []string
		now  time.Time
		want bool
	}{
		"fresh":                  {ca, certPEM, dns, t0, false},
		"just before the window": {ca, certPEM, dns, t0.Add(LeafLifetime - RenewBefore - time.Hour), false},
		"in the window":          {ca, certPEM, dns, t0.Add(LeafLifetime - RenewBefore + time.Hour), true},
		"expired":                {ca, certPEM, dns, t0.Add(LeafLifetime + time.Hour), true},
		"other names":            {ca, certPEM, []string{"a"}, t0, true},
		"other CA":               {other, certPEM, dns, t0, true},
		"garbage":                {ca, []byte("nope"), dns, t0, true},
	}
	for name, tc := range cases {
		if got := tc.ca.NeedsRenewal(tc.cert, tc.dns, tc.now); got != tc.want {
			t.Errorf("%s: NeedsRenewal = %v, want %v", name, got, tc.want)
		}
	}
}

func TestAgentHost(t *testing.T) {
	cases := []struct{ ip, host string }{
		{"10.1.2.3", "10-1-2-3.gs-" + testUUID + "-agent." + testNS + ".svc"},
		{"fd00::5", "fd00--5.gs-" + testUUID + "-agent." + testNS + ".svc"},
		{"fd00:1:2:3:4:5:6:7", "fd00-1-2-3-4-5-6-7.gs-" + testUUID + "-agent." + testNS + ".svc"},
	}
	ca, _ := newCA(t)
	certPEM, _, err := ca.Issue("agent", AgentDNSNames(testUUID, testNS), Server, t0)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := ParseCertificate(certPEM)
	for _, tc := range cases {
		host := AgentHost(tc.ip, testUUID, testNS)
		if host != tc.host {
			t.Errorf("AgentHost(%s) = %q, want %q", tc.ip, host, tc.host)
		}
		ip, ok := agentHostIP(host)
		if !ok || ip != tc.ip {
			t.Errorf("agentHostIP(%q) = %q, %v", host, ip, ok)
		}
		// The agent certificate covers the name, and only for its own server.
		if err := cert.VerifyHostname(host); err != nil {
			t.Errorf("certificate does not cover %q: %v", host, err)
		}
		if err := cert.VerifyHostname(AgentHost(tc.ip, "other", testNS)); err == nil {
			t.Errorf("certificate covers another server's name")
		}
	}
	// The shim dials the short Service name.
	if err := cert.VerifyHostname("gs-" + testUUID + "-agent"); err != nil {
		t.Error(err)
	}
	for _, host := range []string{"pelican-gateway.pelican-system.svc", "10-1-2-3.example.com", "nope.gs-x-agent.ns.svc", "gs-x-agent", "10-1-2.gs-x-agent.ns.svc"} {
		if ip, ok := agentHostIP(host); ok {
			t.Errorf("agentHostIP(%q) = %q, want no match", host, ip)
		}
	}
}

func TestAgentDialer(t *testing.T) {
	var got []string
	dial := AgentDialer(func(_ context.Context, _, addr string) (net.Conn, error) {
		got = append(got, addr)
		return nil, errors.New("not dialing")
	})
	for _, addr := range []string{
		AgentHost("10.0.0.5", testUUID, testNS) + ":8080",
		"[" + AgentHost("fd00::5", testUUID, testNS) + "]:8080",
		"pelican-gateway.pelican-system.svc:8081",
		"no-port",
	} {
		_, _ = dial(context.Background(), "tcp", addr)
	}
	want := []string{"10.0.0.5:8080", "[fd00::5]:8080", "pelican-gateway.pelican-system.svc:8081", "no-port"}
	if !slices.Equal(got, want) {
		t.Errorf("dialed %q, want %q", got, want)
	}
}

func writeDir(t *testing.T, dir string, ca *CA, cn string, dns []string, usage Usage) {
	t.Helper()
	certPEM, keyPEM, err := ca.Issue(cn, dns, usage, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{CertFile: certPEM, KeyFile: keyPEM, CAFile: ca.CertPEM} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestKeyPairReload(t *testing.T) {
	ca, _ := newCA(t)
	dir := t.TempDir()
	writeDir(t, dir, ca, "first", nil, Server)
	kp, err := LoadKeyPair(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	kp.now = func() time.Time { return now }
	cn := func() string {
		t.Helper()
		c, err := kp.Certificate()
		if err != nil {
			t.Fatal(err)
		}
		return c.Leaf.Subject.CommonName
	}
	if cn() != "first" {
		t.Fatal("first certificate not loaded")
	}

	writeDir(t, dir, ca, "second", nil, Server)
	if cn() != "first" {
		t.Error("reloaded before reloadEvery")
	}
	now = now.Add(reloadEvery)
	if cn() != "second" {
		t.Error("renewed certificate not picked up")
	}

	// A broken pair keeps the previous one in use.
	if err := os.WriteFile(filepath.Join(dir, KeyFile), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(reloadEvery)
	if cn() != "second" {
		t.Error("broken pair replaced the working one")
	}
	if err := os.Remove(filepath.Join(dir, CertFile)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(reloadEvery)
	if cn() != "second" {
		t.Error("missing file replaced the working pair")
	}

	if _, err := LoadKeyPair(t.TempDir()); err == nil {
		t.Error("LoadKeyPair of an empty directory: want error")
	}
}

func TestLoadDirErrors(t *testing.T) {
	ca, _ := newCA(t)
	dir := t.TempDir()
	writeDir(t, dir, ca, "x", nil, Server)
	if err := os.WriteFile(filepath.Join(dir, CAFile), []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(dir); err == nil {
		t.Error("bad CA bundle: want error")
	}
	if err := os.Remove(filepath.Join(dir, CAFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(dir); err == nil {
		t.Error("missing CA bundle: want error")
	}
	if _, err := LoadDir(t.TempDir()); err == nil {
		t.Error("empty directory: want error")
	}
}

// TestMutualTLS runs a server with an agent's directory and clients with the
// gateway's directory, the operator's issuer, and neither.
func TestMutualTLS(t *testing.T) {
	ca, _ := newCA(t)
	agentDir, gatewayDir := t.TempDir(), t.TempDir()
	writeDir(t, agentDir, ca, "agent", AgentDNSNames(testUUID, testNS), Server)
	writeDir(t, gatewayDir, ca, GatewayName, []string{"gw"}, Server|Client)
	agent, err := LoadDir(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := LoadDir(gatewayDir)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := "none"
		if len(r.TLS.VerifiedChains) > 0 {
			name = r.TLS.VerifiedChains[0][0].Subject.CommonName
		}
		_, _ = io.WriteString(w, name)
	}))
	srv.TLS = agent.ServerConfig(true)
	srv.StartTLS()
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	get := func(c *tls.Config, uuid string) (string, error) {
		tr := &http.Transport{TLSClientConfig: c, DialContext: AgentDialer(func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		})}
		defer tr.CloseIdleConnections()
		res, err := (&http.Client{Transport: tr}).Get("https://" + AgentHost("10.0.0.5", uuid, testNS) + ":8080/")
		if err != nil {
			return "", err
		}
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		return string(b), err
	}

	issuer := &Issuer{CA: ca, ClientName: OperatorName}
	for name, tc := range map[string]struct {
		cfg  *tls.Config
		want string
	}{
		"gateway":     {gateway.ClientConfig(), GatewayName},
		"operator":    {issuer.ClientConfig(), OperatorName},
		"no identity": {&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: ca.Pool()}, "none"},
	} {
		got, err := get(tc.cfg, testUUID)
		if err != nil || got != tc.want {
			t.Errorf("%s: %q, %v; want %q", name, got, err, tc.want)
		}
	}

	if _, err := get(gateway.ClientConfig(), "another-server"); err == nil {
		t.Error("the certificate of one server was accepted for another")
	}
	if _, err := get(&tls.Config{MinVersion: tls.VersionTLS13}, testUUID); err == nil {
		t.Error("a client without the CA accepted the agent")
	}

	// A client certificate from another CA is refused in the handshake.
	other, _ := newCA(t)
	otherDir := t.TempDir()
	writeDir(t, otherDir, other, GatewayName, nil, Client)
	impostor, err := LoadDir(otherDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := impostor.ClientConfig()
	cfg.RootCAs = ca.Pool()
	if _, err := get(cfg, testUUID); err == nil {
		t.Error("a client certificate from another CA was accepted")
	}
	// An agent's own certificate is not a client certificate.
	cfg = agent.ClientConfig()
	if got, err := get(cfg, testUUID); err == nil && got != "none" {
		t.Errorf("an agent certificate authenticated as a client: %q", got)
	}
}

func TestIssuerRenews(t *testing.T) {
	ca, _ := newCA(t)
	now := t0
	i := &Issuer{CA: ca, ClientName: OperatorName, Now: func() time.Time { return now }}
	first, err := i.GetClientCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := i.GetClientCertificate(nil); again != first {
		t.Error("certificate reissued while fresh")
	}
	now = now.Add(LeafLifetime - RenewBefore + time.Hour)
	renewed, err := i.GetClientCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if renewed == first || renewed.Leaf.Subject.CommonName != OperatorName {
		t.Error("certificate not renewed in its window")
	}
	if !slices.Equal(renewed.Leaf.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}) {
		t.Errorf("usage %v", renewed.Leaf.ExtKeyUsage)
	}
}
