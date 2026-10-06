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

	sock := filepath.Join(dir, "shim.sock")
	ln, err := protocol.Listen(sock, []byte(shimToken), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	sup := supervisor.New(supervisor.Options{Socket: sock, Token: []byte(shimToken), Argv: argv, Dir: dir, KillGrace: time.Second})
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

	wings := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	return &fixture{handler: Handler(wings, m, reg), manager: m, srv: s, env: env}
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

func TestReady(t *testing.T) {
	f := newFixture(t, sleepArgv, remote.ProcessStopConfiguration{})
	for _, tc := range []struct {
		state string
		code  int
	}{
		{environment.ProcessOfflineState, http.StatusServiceUnavailable},
		{environment.ProcessStartingState, http.StatusServiceUnavailable},
		{environment.ProcessRunningState, http.StatusOK},
	} {
		f.env.SetState(tc.state)
		rec := f.do("GET", "/internal/v1/ready", "", "")
		got := decode(t, rec)
		if rec.Code != tc.code || got["ready"] != (tc.code == http.StatusOK) {
			t.Errorf("state %s: %d %v", tc.state, rec.Code, got)
		}
		if states, _ := got["states"].([]any); len(states) != 1 || states[0] != tc.state {
			t.Errorf("state %s: reported %v", tc.state, got["states"])
		}
	}

	empty := Handler(http.NotFoundHandler(), server.NewEmptyManager(nil), nil)
	rec := httptest.NewRecorder()
	empty.ServeHTTP(rec, httptest.NewRequest("GET", "/internal/v1/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no server loaded must not be ready, got %d", rec.Code)
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

func TestPrestopStopsRunningServer(t *testing.T) {
	f := newFixture(t, sleepArgv, remote.ProcessStopConfiguration{Type: remote.ProcessStopSignal, Value: "SIGTERM"})
	if err := f.env.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.env.SetState(environment.ProcessRunningState)

	rec := f.do("POST", "/internal/v1/prestop", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("prestop: %d", rec.Code)
	}
	// prestop returns once the process is gone.
	if got := f.env.State(); got != environment.ProcessOfflineState {
		t.Fatalf("state after prestop: %s", got)
	}
}

func TestPrestopSkipsServersBusyWithLifecycleOperations(t *testing.T) {
	for name, set := range map[string]func(*server.Server){
		"installing":   func(s *server.Server) { s.SetInstalling(true) },
		"transferring": func(s *server.Server) { s.SetTransferring(true) },
		"restoring":    func(s *server.Server) { s.SetRestoring(true) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, sleepArgv, remote.ProcessStopConfiguration{Type: remote.ProcessStopSignal, Value: "SIGTERM"})
			if err := f.env.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.env.SetState(environment.ProcessRunningState)
			set(f.srv)

			rec := f.do("POST", "/internal/v1/prestop", "", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("prestop: %d", rec.Code)
			}
			if got := f.env.State(); got != environment.ProcessRunningState {
				t.Fatalf("a busy server must not be stopped, state %s", got)
			}
		})
	}
}
