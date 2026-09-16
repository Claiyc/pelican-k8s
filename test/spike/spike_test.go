//go:build spike

// Package spike is milestone M0 from ARCHITECTURE.md: the agent (Wings as a
// library) and the shim run the Paper egg without Kubernetes. The shim runs in
// a Docker yolk container, the agent runs in-process against the fake Panel.
//
//	go test -tags spike -run TestSpike ./test/spike -v -timeout 20m
package spike

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"

	"github.com/Claiyc/pelican-k8s/internal/agent/app"
	"github.com/Claiyc/pelican-k8s/internal/shim/prepare"
	"github.com/Claiyc/pelican-k8s/test/fakepanel"
)

const (
	uuid     = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	tokenID  = "spike"
	token    = "spiketoken"
	gameImg  = "ghcr.io/pelican-eggs/yolks:java_25"
	instImg  = "ghcr.io/pelican-eggs/installers:alpine"
	ctrName  = "pelican-k8s-spike"
	gamePort = 25565
)

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out.String())
	}
	return out.String()
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func installScript(t *testing.T) (script, entrypoint string) {
	b, err := os.ReadFile("testdata/egg-paper.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var egg struct {
		Scripts struct {
			Installation struct {
				Script     string `yaml:"script"`
				Entrypoint string `yaml:"entrypoint"`
			} `yaml:"installation"`
		} `yaml:"scripts"`
	}
	if err := yaml.Unmarshal(b, &egg); err != nil {
		t.Fatal(err)
	}
	return egg.Scripts.Installation.Script, egg.Scripts.Installation.Entrypoint
}

func TestSpike(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	base := os.Getenv("PELICAN_SPIKE_DIR")
	if base == "" {
		base = filepath.Join(os.TempDir(), "pelican-k8s-spike")
	}
	root := filepath.Join(base, "root")
	shared := filepath.Join(base, "pelican")
	uid, gid := os.Getuid(), os.Getgid()
	if err := (prepare.Layout{Shared: shared, Data: root, UUID: uuid}).Run(); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", filepath.Join(shared, "bin", "shim"), "../../cmd/shim")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build shim: %v\n%s", err, out)
	}
	volume := filepath.Join(root, "volumes", uuid)
	exec.Command("docker", "rm", "-f", ctrName).Run()

	// --- install (the Job) ---
	script, entrypoint := installScript(t)
	if _, err := os.Stat(filepath.Join(volume, "server.jar")); err != nil {
		t.Log("running Paper install script in the installer image (this downloads Paper)")
		instDir := filepath.Join(base, "install")
		os.MkdirAll(instDir, 0o755)
		os.WriteFile(filepath.Join(instDir, "install.sh"), []byte(script), 0o644)
		out := run(t, "docker", "run", "--rm",
			"-v", volume+":/mnt/server", "-v", instDir+":/pelican/install", "-v", filepath.Join(shared, "bin")+":/pelican/bin:ro",
			"-e", "MINECRAFT_VERSION=latest", "-e", "BUILD_NUMBER=latest", "-e", "SERVER_JARFILE=server.jar",
			instImg, "/pelican/bin/shim", "install-run", "--log", "/pelican/install/output.log", "--exit", "/pelican/install/exit-code",
			"--chown", fmt.Sprintf("%d:%d", uid, gid), "--", entrypoint, "/pelican/install/install.sh")
		t.Logf("installer: %s", lastLines(out, 5))
		ec, _ := os.ReadFile(filepath.Join(instDir, "exit-code"))
		if strings.TrimSpace(string(ec)) != "0" {
			t.Fatalf("install exit code %q", ec)
		}
	}
	os.WriteFile(filepath.Join(volume, "eula.txt"), []byte("eula=true\n"), 0o644)

	// --- fake Panel ---
	panel := fakepanel.New(tokenID, token)
	panel.Add(&fakepanel.Server{UUID: uuid, Settings: fakepanel.PaperSettings(uuid, gamePort, 1024), ProcessConfiguration: fakepanel.PaperProcessConfiguration()})
	ps := httptest.NewServer(panel.Handler())
	defer ps.Close()

	// --- game container with the shim as PID 1 ---
	run(t, "docker", "run", "-d", "--name", ctrName, "-u", fmt.Sprintf("%d:%d", uid, gid), "-m", "1400m",
		"-v", volume+":/home/container", "-v", shared+":/pelican", "-e", "HOME=/home/container", "--tmpfs", "/tmp:rw,exec,size=100m",
		gameImg, "/pelican/bin/shim", "run", "--socket", "/pelican/run/shim.sock", "--", "/bin/bash", "/entrypoint.sh")
	defer func() {
		if t.Failed() {
			t.Logf("container logs:\n%s", lastLines(run(t, "docker", "logs", ctrName), 40))
		}
		exec.Command("docker", "rm", "-f", ctrName).Run()
	}()

	// --- agent in-process ---
	apiPort, sftpPort := freePort(t), freePort(t)
	cfgPath := filepath.Join(base, "config.yml")
	cfg := fmt.Sprintf(`token_id: %s
token: %s
remote: %s
api:
  host: 127.0.0.1
  port: %d
system:
  root_directory: %s
  data: %s/volumes
  log_directory: %s/logs
  archive_directory: %s/scratch/archives
  backup_directory: %s/scratch/backups
  tmp_directory: %s/scratch/tmp
  user:
    uid: %d
    gid: %d
    rootless:
      enabled: true
    passwd:
      enable: false
      directory: %s/.pelican/passwd
  machine_id:
    enable: false
    directory: %s/.pelican/machine-id
  check_permissions_on_boot: false
  enable_log_rotate: false
  sftp:
    bind_address: 127.0.0.1
    bind_port: %d
docker:
  network:
    interface: 0.0.0.0
`, tokenID, token, ps.URL, apiPort, root, root, root, root, root, root, uid, gid, root, root, sftpPort)
	os.WriteFile(cfgPath, []byte(cfg), 0o600)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	agentErr := make(chan error, 1)
	go func() {
		agentErr <- app.Run(ctx, app.Options{ConfigPath: cfgPath, ShimSocket: filepath.Join(shared, "run", "shim.sock"), Logger: slog.Default(), Ready: ready})
	}()
	select {
	case <-ready:
	case err := <-agentErr:
		t.Fatalf("agent exited: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("agent did not become ready")
	}
	api := fmt.Sprintf("http://127.0.0.1:%d", apiPort)

	// --- start via the Wings API, watch the Panel see starting -> running ---
	if code, body := call(t, api, "GET", "/api/servers/"+uuid, nil); code != 200 || !strings.Contains(body, `"state":"offline"`) {
		t.Fatalf("GET server: %d %s", code, body)
	}
	if code, body := call(t, api, "POST", "/api/servers/"+uuid+"/power", `{"action":"start"}`); code != 202 {
		t.Fatalf("power start: %d %s", code, body)
	}
	if !panel.WaitForState(uuid, "running", 4*time.Minute) {
		t.Fatalf("server did not reach running; states=%v", panel.StateChanges)
	}
	t.Logf("state changes so far: %v", panel.StateChanges)
	time.Sleep(5 * time.Second)
	_, body := call(t, api, "GET", "/api/servers/"+uuid, nil)
	var resp struct {
		State       string `json:"state"`
		Utilization struct {
			Memory uint64 `json:"memory_bytes"`
			Uptime int64  `json:"uptime"`
		} `json:"utilization"`
	}
	json.Unmarshal([]byte(body), &resp)
	if resp.State != "running" || resp.Utilization.Memory == 0 || resp.Utilization.Uptime == 0 {
		t.Fatalf("api response: %s", body)
	}
	t.Logf("running: memory=%d uptime=%dms", resp.Utilization.Memory, resp.Utilization.Uptime)

	// --- websocket with a JWT signed by the agent token ---
	wsCheck(t, apiPort, ps.URL)

	// --- console command + stop via the stop command ---
	if code, body := call(t, api, "POST", "/api/servers/"+uuid+"/commands", `{"commands":["list"]}`); code != 204 {
		t.Fatalf("commands: %d %s", code, body)
	}
	if code, body := call(t, api, "POST", "/api/servers/"+uuid+"/power", `{"action":"stop"}`); code != 202 {
		t.Fatalf("power stop: %d %s", code, body)
	}
	if !panel.WaitForState(uuid, "offline", 2*time.Minute) {
		t.Fatalf("server did not stop; states=%v", panel.StateChanges)
	}
	time.Sleep(3 * time.Second) // a crash restart would flip it back to starting
	if s := panel.Get(uuid); s.State != "offline" {
		t.Fatalf("server restarted after a clean stop: %v", panel.StateChanges)
	}
	var seq []string
	for _, sc := range panel.StateChanges {
		seq = append(seq, sc["previous_state"]+">"+sc["new_state"])
	}
	t.Logf("state changes: %v", seq)
	want := "offline>starting starting>running running>stopping stopping>offline"
	if strings.Join(seq, " ") != want {
		t.Fatalf("unexpected state sequence %v", seq)
	}
	_, body = call(t, api, "GET", "/api/servers/"+uuid+"/logs?size=20", nil)
	if !strings.Contains(body, "Stopping server") && !strings.Contains(body, "Saving") {
		t.Fatalf("logs missing shutdown lines: %s", body)
	}

	// --- crash detection: kill the process from the outside, Wings restarts it ---
	if code, _ := call(t, api, "POST", "/api/servers/"+uuid+"/power", `{"action":"start"}`); code != 202 {
		t.Fatal("restart")
	}
	if !panel.WaitForState(uuid, "running", 4*time.Minute) {
		t.Fatalf("second start failed: %v", panel.StateChanges)
	}
	run(t, "docker", "exec", ctrName, "sh", "-c", "kill -9 $(pgrep -f 'java' | head -1)")
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if len(panel.CallsMatching("container/status")) > 0 && panel.Get(uuid).State == "starting" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !panel.WaitForState(uuid, "running", 4*time.Minute) {
		t.Fatalf("crash restart did not happen: %v", panel.StateChanges)
	}
	t.Logf("crash restart observed: %v", panel.StateChanges[len(panel.StateChanges)-3:])
	call(t, api, "POST", "/api/servers/"+uuid+"/power", `{"action":"kill"}`)
	panel.WaitForState(uuid, "offline", time.Minute)
	cancel()
	<-agentErr
}

func call(t *testing.T, base, method, path string, body any) (int, string) {
	t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		r = strings.NewReader(b)
	}
	req, _ := http.NewRequest(method, base+path, r)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if ua := res.Header.Get("User-Agent"); !strings.HasPrefix(ua, "Pelican Wings/v") {
		t.Fatalf("missing Wings User-Agent header on %s %s: %q", method, path, ua)
	}
	return res.StatusCode, string(b)
}

type wsClaims struct {
	jwt.Payload
	UserUUID    string   `json:"user_uuid"`
	ServerUUID  string   `json:"server_uuid"`
	Permissions []string `json:"permissions"`
	Scope       string   `json:"scope"`
	UniqueID    string   `json:"unique_id"`
}

func wsCheck(t *testing.T, port int, origin string) {
	t.Helper()
	now := time.Now()
	claims := wsClaims{
		Payload:     jwt.Payload{IssuedAt: jwt.NumericDate(now), ExpirationTime: jwt.NumericDate(now.Add(10 * time.Minute)), JWTID: "abc"},
		UserUUID:    "u-1", ServerUUID: uuid, Permissions: []string{"websocket.connect", "control.console", "control.start", "control.stop", "admin.websocket.errors"}, Scope: "websocket", UniqueID: "x1",
	}
	tok, err := jwt.Sign(claims, jwt.NewHS256([]byte(token)))
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{"Origin": []string{origin}}
	c, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://127.0.0.1:%d/api/servers/%s/ws", port, uuid), h)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	send := func(ev string, args ...string) {
		if err := c.WriteJSON(map[string]any{"event": ev, "args": args}); err != nil {
			t.Fatal(err)
		}
	}
	send("auth", string(tok))
	seen := map[string]int{}
	var console []string
	c.SetReadDeadline(time.Now().Add(20 * time.Second))
	send("send logs")
	for len(console) < 5 || seen["auth success"] == 0 || seen["status"] == 0 {
		var m struct {
			Event string   `json:"event"`
			Args  []string `json:"args"`
		}
		if err := c.ReadJSON(&m); err != nil {
			t.Fatalf("websocket read: %v (seen %v, console %d lines)", err, seen, len(console))
		}
		seen[m.Event]++
		if m.Event == "console output" {
			console = append(console, m.Args...)
		}
		if m.Event == "jwt error" || m.Event == "daemon error" {
			t.Fatalf("websocket error: %v", m.Args)
		}
	}
	t.Logf("websocket events: %v; sample console line: %q", seen, console[len(console)-1])
	if seen["stats"] == 0 {
		send("send stats")
		var m struct {
			Event string `json:"event"`
		}
		for m.Event != "stats" {
			if err := c.ReadJSON(&m); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

var _ = strconv.Itoa
