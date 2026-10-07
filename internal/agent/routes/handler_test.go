package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/server"

	"github.com/Claiyc/pelican-k8s/internal/agent/shimenv"
	"github.com/Claiyc/pelican-k8s/internal/shim/protocol"
	"github.com/Claiyc/pelican-k8s/internal/shim/supervisor"
)

const (
	apiToken  = "agent-token"
	shimToken = "shim-token"
	serverID  = "11111111-2222-3333-4444-555555555555"
)

type fixture struct {
	handler http.Handler
	manager *server.Manager
	srv     *server.Server
	env     *shimenv.Environment
	// terminate plays the deletion of the game pod: the shim gets SIGTERM.
	terminate context.CancelFunc
	// wings, when set, serves the requests that fall through to Wings.
	wings http.HandlerFunc
}

// newFixture wires the handler to a real Wings manager holding one server
// whose environment is the shim environment; a supervisor in the same process
// plays the shim running argv.
func newFixture(t *testing.T, argv []string, stop remote.ProcessStopConfiguration) *fixture {
	t.Helper()
	dir := t.TempDir()
	c, err := config.NewAtPath("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	c.AuthenticationToken = apiToken
	c.System.Data = filepath.Join(dir, "data")
	c.System.LogDirectory = filepath.Join(dir, "log")
	c.System.RootDirectory = dir
	c.System.CrashDetection.CrashDetectionEnabled = false
	config.Set(c)

	ln, err := protocol.Listen("127.0.0.1:0", []byte(shimToken), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	sup := supervisor.New(supervisor.Options{Agent: ln.Addr(), Token: []byte(shimToken), PodUID: "game-pod", Argv: argv, Dir: dir, KillGrace: time.Second, GracePeriod: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = sup.Run(ctx) }()
	t.Cleanup(cancel)

	reg := shimenv.NewRegistry(ln, nil, nil)
	m := server.NewEmptyManager(stubPanel{}, server.WithEnvironmentFactory(reg.Factory))
	settings, _ := json.Marshal(map[string]any{
		"uuid":       serverID,
		"invocation": "sleep",
		"container":  map[string]string{"image": "example/image:1"},
		"build":      map[string]any{"memory_limit": 0},
		"allocations": map[string]any{
			"default": map[string]any{"ip": "0.0.0.0", "port": 25565},
		},
	})
	pc := &remote.ProcessConfiguration{}
	pc.Stop = stop
	s, err := m.InitServer(remote.ServerConfigurationResponse{Settings: settings, ProcessConfiguration: pc})
	if err != nil {
		t.Fatal(err)
	}
	m.Add(s)
	env, ok := reg.Get(serverID)
	if !ok {
		t.Fatal("environment not registered")
	}
	t.Cleanup(env.Close)
	// Wait for the shim connection: a connect that finds no process would
	// otherwise reset a state the test sets in the meantime.
	if err := env.OnBeforeStart(ctx); err != nil {
		t.Fatal(err)
	}

	f := &fixture{manager: m, srv: s, env: env, terminate: cancel}
	wings := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.wings != nil {
			f.wings(w, r)
			return
		}
		w.WriteHeader(http.StatusTeapot)
	})
	f.handler = Handler(wings, m, reg)
	return f
}

func (f *fixture) do(method, path, auth, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// stubPanel accepts state pushes; every other Panel call would panic, which
// the tests must never trigger.
type stubPanel struct{ remote.Client }

func (stubPanel) PushServerStateChange(context.Context, string, remote.ServerStateChange) error {
	return nil
}

var sleepArgv = []string{"/bin/sh", "-c", "sleep 60"}

func TestHealthzAndFallthrough(t *testing.T) {
	f := newFixture(t, sleepArgv, remote.ProcessStopConfiguration{})
	rec := f.do("GET", "/internal/v1/healthz", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz %d", rec.Code)
	}
	if got := decode(t, rec); got["ok"] != true || got["servers"] != float64(1) {
		t.Fatalf("healthz body %v", got)
	}
	if rec := f.do("POST", "/internal/v1/healthz", "", ""); rec.Code == http.StatusOK {
		t.Fatal("healthz is GET only")
	}
	if rec := f.do("GET", "/api/servers", "", ""); rec.Code != http.StatusTeapot {
		t.Fatalf("other paths must reach wings, got %d", rec.Code)
	}
}

func TestExitStateAuthAndBody(t *testing.T) {
	f := newFixture(t, sleepArgv, remote.ProcessStopConfiguration{})
	for name, auth := range map[string]string{"missing": "", "wrong": "Bearer nope", "no prefix": "nope"} {
		if rec := f.do("POST", "/internal/v1/exit-state", auth, `{"code":1}`); rec.Code != http.StatusForbidden {
			t.Errorf("%s token: %d", name, rec.Code)
		}
	}
	rec := f.do("POST", "/internal/v1/exit-state", "Bearer "+apiToken, `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: %d", rec.Code)
	}
	if rec := f.do("GET", "/internal/v1/exit-state", "Bearer "+apiToken, ""); rec.Code == http.StatusNoContent {
		t.Fatal("exit-state is POST only")
	}
}

func TestExitStateInjectsIntoRunningServer(t *testing.T) {
	f := newFixture(t, sleepArgv, remote.ProcessStopConfiguration{})
	if err := f.env.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.env.SetState(environment.ProcessRunningState)

	rec := f.do("POST", "/internal/v1/exit-state", "Bearer "+apiToken, `{"code":137,"oomKilled":true}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("exit-state: %d %s", rec.Code, rec.Body.String())
	}
	waitFor(t, "offline", func() bool { return f.env.State() == environment.ProcessOfflineState })
	code, oom, err := f.env.ExitState()
	if err != nil || code != 137 || !oom {
		t.Fatalf("exit state = %d %v %v", code, oom, err)
	}
}

func TestPrestopOfflineServerIsLeftAlone(t *testing.T) {
	f := newFixture(t, sleepArgv, remote.ProcessStopConfiguration{})
	rec := f.do("POST", "/internal/v1/prestop", "", "")
	if rec.Code != http.StatusOK || decode(t, rec)["ok"] != true {
		t.Fatalf("prestop: %d %s", rec.Code, rec.Body.String())
	}
	if f.env.State() != environment.ProcessOfflineState {
		t.Fatalf("state %s", f.env.State())
	}
}

// The agent pod alone is deleted: the shim does not report terminating, so the
// hook returns after PrestopShimWait and the process keeps running under the
// shim for the next agent.
func TestPrestopLeavesProcessRunningWhenOnlyTheAgentGoes(t *testing.T) {
	defer func(d time.Duration) { PrestopShimWait = d }(PrestopShimWait)
	PrestopShimWait = 300 * time.Millisecond
	f := newFixture(t, sleepArgv, remote.ProcessStopConfiguration{Type: remote.ProcessStopSignal, Value: "SIGTERM"})
	if err := f.env.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.env.SetState(environment.ProcessRunningState)

	start := time.Now()
	rec := f.do("POST", "/internal/v1/prestop", "", "")
	if rec.Code != http.StatusOK || decode(t, rec)["waited"] != false {
		t.Fatalf("prestop: %d %s", rec.Code, rec.Body.String())
	}
	if time.Since(start) < PrestopShimWait {
		t.Fatal("prestop did not give the shim time to report terminating")
	}
	if got := f.env.State(); got != environment.ProcessRunningState {
		t.Fatalf("the agent's preStop must not stop the process, state %s", got)
	}
}

// A drain takes both pods: the shim reports terminating and stops the process
// by itself; the hook returns once the process is offline.
func TestPrestopWaitsWhileTheShimStopsTheProcess(t *testing.T) {
	f := newFixture(t, sleepArgv, remote.ProcessStopConfiguration{Type: remote.ProcessStopSignal, Value: "SIGTERM"})
	if err := f.env.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.env.SetState(environment.ProcessRunningState)

	go func() {
		time.Sleep(200 * time.Millisecond)
		f.terminate()
	}()
	rec := f.do("POST", "/internal/v1/prestop", "", "")
	if rec.Code != http.StatusOK || decode(t, rec)["waited"] != true {
		t.Fatalf("prestop: %d %s", rec.Code, rec.Body.String())
	}
	if got := f.env.State(); got != environment.ProcessOfflineState {
		t.Fatalf("state after prestop: %s", got)
	}
}

func TestShimRoute(t *testing.T) {
	f := newFixture(t, sleepArgv, remote.ProcessStopConfiguration{})
	if rec := f.do("GET", "/internal/v1/shim", "Bearer nope", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong token: %d", rec.Code)
	}
	got := decode(t, f.do("GET", "/internal/v1/shim", "Bearer "+apiToken, ""))
	if got["attached"] != true || got["podUID"] != "game-pod" || got["running"] != false {
		t.Fatalf("idle shim: %v", got)
	}
	if err := f.env.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := decode(t, f.do("GET", "/internal/v1/shim", "Bearer "+apiToken, "")); got["running"] != true {
		t.Fatalf("running shim: %v", got)
	}
}

func TestActivityRoute(t *testing.T) {
	f := newFixture(t, sleepArgv, remote.ProcessStopConfiguration{})
	if rec := f.do("GET", "/internal/v1/activity", "", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("missing token: %d", rec.Code)
	}
	reasons := func() (bool, string) {
		got := decode(t, f.do("GET", "/internal/v1/activity", "Bearer "+apiToken, ""))
		var rs []string
		for _, r := range got["reasons"].([]any) {
			rs = append(rs, r.(string))
		}
		return got["busy"].(bool), strings.Join(rs, ",")
	}
	if busy, rs := reasons(); busy || rs != "" {
		t.Fatalf("idle agent: %v %q", busy, rs)
	}

	// A file request in flight.
	release := make(chan struct{})
	entered := make(chan struct{})
	f.wings = func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
	}
	done := make(chan struct{})
	go func() {
		f.do("POST", "/api/servers/"+serverID+"/files/decompress", "", "")
		close(done)
	}()
	<-entered
	if busy, rs := reasons(); !busy || rs != "files" {
		t.Fatalf("decompress in flight: %v %q", busy, rs)
	}
	close(release)
	<-done
	if busy, _ := reasons(); busy {
		t.Fatal("still busy after the request ended")
	}

	f.srv.SetRestoring(true)
	f.srv.SetInstalling(true)
	if busy, rs := reasons(); !busy || rs != "install,restore" {
		t.Fatalf("restore and install: %v %q", busy, rs)
	}
}

func TestIsFileRequest(t *testing.T) {
	for path, want := range map[string]bool{
		"/download/file":                      true,
		"/download/backup":                    true,
		"/upload/file":                        true,
		"/api/servers/x/files/compress":       true,
		"/api/servers/x/files/list-directory": true,
		"/api/servers/x/ws":                   false,
		"/api/servers/x/power":                false,
		"/api/servers/x/backup":               false,
		"/api/servers":                        false,
		"/api/servers/files/x":                false,
		"/internal/v1/healthz":                false,
	} {
		if got := isFileRequest(path); got != want {
			t.Errorf("isFileRequest(%q) = %v", path, got)
		}
	}
}
