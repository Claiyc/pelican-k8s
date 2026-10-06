package app

import (
	"context"
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

func newGateway(t *testing.T, withServer bool) *gateway {
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
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(g.Close)
	return g
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
	if err := os.WriteFile(filepath.Join(dir, "root"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err = Run(ctx, Options{ConfigPath: writeConfig(t, dir, c)})
	if err == nil || !strings.Contains(err.Error(), "configure directories") {
		t.Fatalf("uncreatable root directory: %v", err)
	}
}

// TestRunServesAndShutsDown boots the whole agent against a fake gateway. It
// must be the only test that gets past the cron scheduler, which Wings allows
// once per process.
func TestRunServesAndShutsDown(t *testing.T) {
	if fullBootDone.Swap(true) {
		t.Skip("Wings initialises its database once per process; run without -count>1")
	}
	gw := newGateway(t, true)
	dir, c := baseConfig(t, gw.URL)
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			ConfigPath: writeConfig(t, dir, c),
			ShimSocket: filepath.Join(dir, "shim.sock"),
			ShimToken:  "s",
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

	base := fmt.Sprintf("http://127.0.0.1:%d", c.apiPort)
	res, err := http.Get(base + "/internal/v1/healthz")
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

	// The game is not running: not ready, and the agent never auto-starts it.
	res, err = http.Get(base + "/internal/v1/ready")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("ready: %d", res.StatusCode)
	}

	// The Wings API is reachable behind the same listener and requires the token.
	res, err = http.Get(base + "/api/servers/" + serverUUID)
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
	if _, err := http.Get(base + "/internal/v1/healthz"); err == nil {
		t.Fatal("the HTTP server is still serving")
	}
}
