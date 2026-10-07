package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Claiyc/pelican-k8s/internal/pki"
)

var fullBootDone atomic.Bool

const serverUUID = "11111111-2222-3333-4444-555555555555"

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// gateway is a stand-in for the pelican-k8s gateway's Wings remote API.
type gateway struct {
	*httptest.Server
	servers  atomic.Pointer[[]map[string]any]
	requests atomic.Int32
	// listCalls counts server list requests; the first one fails so that the
	// agent's retry loop is exercised.
	listCalls atomic.Int32
}

func newGateway(t *testing.T, withServer bool, tlsConfig *tls.Config) *gateway {
	t.Helper()
	g := &gateway{}
	list := []map[string]any{}
	if withServer {
		list = append(list, map[string]any{
			"uuid": serverUUID,
			"settings": map[string]any{
				"uuid":       serverUUID,
				"invocation": "sleep 60",
				"container":  map[string]string{"image": "example/image:1"},
				"allocations": map[string]any{
					"default": map[string]any{"ip": "0.0.0.0", "port": 25565},
				},
			},
			"process_configuration": map[string]any{"stop": map[string]string{"type": "signal", "value": "SIGTERM"}},
		})
	}
	g.servers.Store(&list)
	g.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/remote/servers" {
			if g.listCalls.Add(1) == 1 {
				http.Error(w, "gateway not ready", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": *g.servers.Load(),
				"meta": map[string]any{"current_page": 1, "last_page": 1},
			})
			return
		}
		_, _ = io.WriteString(w, "{}")
	}))
	if tlsConfig != nil {
		g.TLS = tlsConfig
		g.StartTLS()
	} else {
		g.Start()
	}
	t.Cleanup(g.Close)
	return g
}

// testPKI writes the agent's certificate directory and returns a gateway
// server configuration for "localhost" and the gateway's client configuration
// toward the agent, all from one CA.
func testPKI(t *testing.T) (agentDir string, gatewayServer, gatewayClient *tls.Config) {
	t.Helper()
	now := time.Now()
	ca, _, err := pki.NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	agentDir = t.TempDir()
	certPEM, keyPEM, err := ca.Issue("agent", []string{"localhost"}, pki.Server, now)
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{pki.CertFile: certPEM, pki.KeyFile: keyPEM, pki.CAFile: ca.CertPEM} {
		if err := os.WriteFile(filepath.Join(agentDir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	certPEM, keyPEM, err = ca.Issue(pki.GatewayName, []string{"localhost"}, pki.Server|pki.Client, now)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	gatewayServer = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{gw}}
	gatewayClient = &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: ca.Pool(), Certificates: []tls.Certificate{gw}}
	return agentDir, gatewayServer, gatewayClient
}

type agentConfig struct {
	remote, tokenID, token string
	timezone               string
	// logDir overrides the log directory (relative paths are not supported).
	logDir            string
	apiPort, sftpPort int
}

func writeConfig(t *testing.T, dir string, c agentConfig) string {
	t.Helper()
	if c.logDir == "" {
		c.logDir = dir + "/log"
	}
	body := strings.NewReplacer(
		"@DIR@", dir, "@TID@", c.tokenID, "@TOKEN@", c.token, "@REMOTE@", c.remote, "@TZ@", c.timezone,
		"@LOG@", c.logDir, "@API@", fmt.Sprint(c.apiPort), "@SFTP@", fmt.Sprint(c.sftpPort),
	).Replace(`token_id: "@TID@"
token: "@TOKEN@"
remote: "@REMOTE@"
api:
  host: 127.0.0.1
  port: @API@
system:
  root_directory: @DIR@/root
  log_directory: @LOG@
  data: @DIR@/data
  archive_directory: @DIR@/archives
  backup_directory: @DIR@/backups
  tmp_directory: @DIR@/tmp
  timezone: "@TZ@"
  user:
    passwd:
      directory: @DIR@/passwd
  machine_id:
    directory: @DIR@/machine-id
  sftp:
    bind_address: 127.0.0.1
    bind_port: @SFTP@
`)
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func baseConfig(t *testing.T, remote string) (string, agentConfig) {
	dir := t.TempDir()
	c := agentConfig{remote: remote, tokenID: "tid", token: "ttoken", timezone: "UTC", apiPort: freePort(t), sftpPort: freePort(t)}
	return dir, c
}

// Wings can initialise its activity database once per process, so only
// failures before that point can be tested more than once; the single full
// boot is TestRunServesAndShutsDown, which must stay last in this file.
func TestRunConfigErrors(t *testing.T) {
	ctx := context.Background()

	if err := Run(ctx, Options{ConfigPath: filepath.Join(t.TempDir(), "missing.yml")}); err == nil || !strings.Contains(err.Error(), "load config") {
		t.Fatalf("missing config: %v", err)
	}

	// (An empty token is rejected by Wings' own config loader with a panic.)
	dir, c := baseConfig(t, "http://127.0.0.1:1")
	c.tokenID = ""
	err := Run(ctx, Options{ConfigPath: writeConfig(t, dir, c)})
	if err == nil || !strings.Contains(err.Error(), "WINGS_TOKEN_ID and WINGS_TOKEN must be set") {
		t.Fatalf("missing token id: %v", err)
	}

	dir, c = baseConfig(t, "http://127.0.0.1:1")
	c.timezone = "Not/AZone"
	if err := Run(ctx, Options{ConfigPath: writeConfig(t, dir, c)}); err == nil {
		t.Fatal("an unknown timezone must fail")
	}

	// A directory that cannot be created (its parent is a file).
	dir, c = baseConfig(t, "http://127.0.0.1:1")
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c.logDir = filepath.Join(blocker, "log")
	if err := Run(ctx, Options{ConfigPath: writeConfig(t, dir, c)}); err == nil {
		t.Fatal("an uncreatable log directory must fail")
	}

	dir, c = baseConfig(t, "http://127.0.0.1:1")
	err = Run(ctx, Options{ConfigPath: writeConfig(t, dir, c), TLSDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "tls:") {
		t.Fatalf("empty certificate directory: %v", err)
	}

	dir, c = baseConfig(t, "http://127.0.0.1:1")
	if err := os.WriteFile(filepath.Join(dir, "root"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err = Run(ctx, Options{ConfigPath: writeConfig(t, dir, c)})
	if err == nil || !strings.Contains(err.Error(), "configure directories") {
		t.Fatalf("uncreatable root directory: %v", err)
	}
}

// TestRunServesAndShutsDown boots the whole agent against a fake gateway,
// with TLS on both sides (ARCHITECTURE.md 12.5). It must be the only test that
// gets past the cron scheduler, which Wings allows once per process.
func TestRunServesAndShutsDown(t *testing.T) {
	if fullBootDone.Swap(true) {
		t.Skip("Wings initialises its database once per process; run without -count>1")
	}
	tlsDir, gwServer, gwClient := testPKI(t)
	gw := newGateway(t, true, gwServer)
	// The fake gateway's certificate is for localhost, not its address.
	dir, c := baseConfig(t, strings.Replace(gw.URL, "127.0.0.1", "localhost", 1))
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			ConfigPath: writeConfig(t, dir, c),
			ShimListen: "127.0.0.1:0",
			ShimToken:  "s",
			TLSDir:     tlsDir,
			Ready:      ready,
		})
	}()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Run returned before listening: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("agent did not start listening")
	}

	// Wings' SFTP server starts in its own goroutine and writes its host key
	// into the data directory first. Wait for it to listen, so it does not
	// write into the directory while the test removes it.
	sftpAddr := fmt.Sprintf("127.0.0.1:%d", c.sftpPort)
	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", sftpAddr, time.Second)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the SFTP server is not listening: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	base := fmt.Sprintf("https://localhost:%d", c.apiPort)
	gateway := &http.Client{Transport: &http.Transport{TLSClientConfig: gwClient}}
	// Kubelet's probes: no client certificate. (Kubelet does not verify the
	// server either; the test does, so it needs no insecure configuration.)
	kubelet := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: gwClient.RootCAs}}}
	res, err := kubelet.Get(base + "/internal/v1/healthz")
	if err != nil {
		t.Fatal(err)
	}
	var health struct {
		OK      bool `json:"ok"`
		Servers int  `json:"servers"`
	}
	if err := json.NewDecoder(res.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !health.OK || health.Servers != 1 {
		t.Fatalf("healthz: %d %+v", res.StatusCode, health)
	}

	// Everything but kubelet's paths needs a client certificate, even with the token.
	req, _ := http.NewRequest("GET", base+"/internal/v1/shim", nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err = kubelet.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("call without a client certificate: %d", res.StatusCode)
	}
	// Plain HTTP is not served.
	if res, err := http.Get(strings.Replace(base, "https:", "http:", 1) + "/internal/v1/healthz"); err == nil {
		res.Body.Close()
		if res.StatusCode == http.StatusOK {
			t.Fatal("plain HTTP served")
		}
	}

	// No shim has connected, and the agent never auto-starts the game.
	res, err = gateway.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var shim struct {
		Attached bool `json:"attached"`
	}
	if err := json.NewDecoder(res.Body).Decode(&shim); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK || shim.Attached {
		t.Fatalf("shim: %d %+v", res.StatusCode, shim)
	}

	// The Wings API is reachable behind the same listener and requires the token.
	res, err = gateway.Get(base + "/api/servers/" + serverUUID)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized && res.StatusCode != http.StatusForbidden {
		t.Fatalf("unauthenticated wings call: %d", res.StatusCode)
	}
	if gw.listCalls.Load() < 2 {
		t.Fatalf("the agent must retry the failed server fetch, list calls: %d", gw.listCalls.Load())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("clean shutdown returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	// The listener is closed after shutdown.
	if res, err := kubelet.Get(base + "/internal/v1/healthz"); err == nil {
		res.Body.Close()
		t.Fatal("the HTTP server is still serving")
	}
}
