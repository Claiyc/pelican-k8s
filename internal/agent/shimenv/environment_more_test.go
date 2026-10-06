package shimenv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/server"

	"github.com/Claiyc/pelican-k8s/internal/shim/protocol"
	"github.com/Claiyc/pelican-k8s/internal/shim/supervisor"
)

// idleEnv is an environment whose shim never connects.
func idleEnv(t *testing.T, dialTimeout time.Duration) *Environment {
	t.Helper()
	dir := t.TempDir()
	cfg := environment.NewConfiguration(environment.Settings{}, nil)
	e := New("idle", cfg, Options{
		Connect:     func(ctx context.Context) (*protocol.Client, error) { <-ctx.Done(); return nil, ctx.Err() },
		SocketPath:  filepath.Join(dir, "shim.sock"),
		RunLog:      filepath.Join(dir, "console.log"),
		DialTimeout: dialTimeout,
	})
	t.Cleanup(e.Close)
	return e
}

func TestStaticAccessors(t *testing.T) {
	e := idleEnv(t, time.Second)
	if e.Type() != "shim" {
		t.Fatalf("type %q", e.Type())
	}
	if e.Config() == nil || e.Events() == nil {
		t.Fatal("config and events must be set")
	}
	if e.Image() != "" {
		t.Fatalf("image %q", e.Image())
	}
	e.SetImage("example/image:1")
	if e.Image() != "example/image:1" {
		t.Fatalf("image %q", e.Image())
	}
	if ok, err := e.Exists(); !ok || err != nil {
		t.Fatalf("Exists = %v, %v", ok, err)
	}
	if e.Create() != nil || e.InSituUpdate() != nil {
		t.Fatal("Create and InSituUpdate are no-ops")
	}
	if e.State() != environment.ProcessOfflineState {
		t.Fatalf("initial state %s", e.State())
	}
}

func TestSetStateValidation(t *testing.T) {
	e := idleEnv(t, time.Second)
	defer func() {
		if recover() == nil {
			t.Fatal("an invalid state must panic like Docker's environment")
		}
	}()
	e.SetState("bogus")
}

func TestWithoutShim(t *testing.T) {
	ctx := context.Background()
	e := idleEnv(t, 100*time.Millisecond)

	if running, err := e.IsRunning(ctx); running || err != nil {
		t.Fatalf("IsRunning = %v, %v", running, err)
	}
	if up, err := e.Uptime(ctx); up != 0 || err != nil {
		t.Fatalf("Uptime = %d, %v", up, err)
	}
	if code, oom, _ := e.ExitState(); code != 1 || oom {
		t.Fatalf("a never-started process reports exit 1, got %d %v", code, oom)
	}
	if err := e.SendCommand("x"); !errors.Is(err, ErrNotAttached) {
		t.Fatalf("SendCommand: %v", err)
	}
	for name, f := range map[string]func() error{
		"Attach":        func() error { return e.Attach(ctx) },
		"OnBeforeStart": func() error { return e.OnBeforeStart(ctx) },
		"Start":         func() error { return e.Start(ctx) },
	} {
		err := f()
		if err == nil || !strings.Contains(err.Error(), "has not connected") {
			t.Errorf("%s without a shim: %v", name, err)
		}
	}
	if e.State() != environment.ProcessOfflineState || e.IsAttached() {
		t.Fatal("failed attempts must leave the environment offline and detached")
	}

	// Stopping something that cannot be running ends offline, for every stop type.
	for _, stop := range []remote.ProcessStopConfiguration{
		{Type: remote.ProcessStopCommand, Value: "stop"},
		{Type: remote.ProcessStopNativeStop},
		{Type: remote.ProcessStopSignal, Value: "SIGTERM"},
	} {
		e.SetStopConfiguration(stop)
		e.SetState(environment.ProcessRunningState)
		if err := e.Stop(ctx); err != nil {
			t.Fatalf("Stop(%v): %v", stop, err)
		}
		if e.State() != environment.ProcessOfflineState {
			t.Fatalf("Stop(%v) left state %s", stop, e.State())
		}
	}
	e.SetState(environment.ProcessRunningState)
	if err := e.Terminate(ctx, "SIGKILL"); err != nil || e.State() != environment.ProcessOfflineState {
		t.Fatalf("Terminate: %v, state %s", err, e.State())
	}
	e.SetState(environment.ProcessRunningState)
	if err := e.Destroy(); err != nil || e.State() != environment.ProcessOfflineState {
		t.Fatalf("Destroy: %v, state %s", err, e.State())
	}
}

func TestIdleShimStopAndTerminate(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newTestEnv(t, []string{"/bin/sh", "-c", "sleep 30"})
	if err := e.OnBeforeStart(ctx); err != nil {
		t.Fatal(err)
	}
	// Connected, but nothing runs.
	if running, err := e.IsRunning(ctx); running || err != nil {
		t.Fatalf("IsRunning = %v, %v", running, err)
	}
	if up, err := e.Uptime(ctx); up != 0 || err != nil {
		t.Fatalf("Uptime = %d, %v", up, err)
	}
	e.SetStopConfiguration(remote.ProcessStopConfiguration{Type: remote.ProcessStopNativeStop})
	e.SetState(environment.ProcessRunningState)
	if err := e.Stop(ctx); err != nil {
		t.Fatalf("Stop of an idle shim: %v", err)
	}
	if e.State() != environment.ProcessOfflineState {
		t.Fatalf("state %s", e.State())
	}
	e.SetState(environment.ProcessRunningState)
	if err := e.Terminate(ctx, "SIGTERM"); err != nil || e.State() != environment.ProcessOfflineState {
		t.Fatalf("Terminate of an idle shim: %v, state %s", err, e.State())
	}
	if err := e.Destroy(); err != nil || e.State() != environment.ProcessOfflineState {
		t.Fatalf("Destroy: %v, state %s", err, e.State())
	}
}

func TestSignalStopMapsTheConfiguredSignal(t *testing.T) {
	script := `trap 'echo got:ABRT; exit 10' ABRT; trap 'echo got:INT; exit 11' INT; trap 'echo got:TERM; exit 12' TERM; echo ready; while true; do sleep 0.05; done`
	for _, tc := range []struct {
		value string
		code  uint32
	}{
		{"SIGABRT", 10},
		{"sigint", 11},
		{"C", 11},
		{"SIGTERM", 12},
		{"", 137},        // empty type and value: SIGKILL
		{"SIGUSR1", 137}, // unsupported signals escalate to SIGKILL
	} {
		t.Run(tc.value, func(t *testing.T) {
			e, _, _ := newTestEnv(t, []string{"/bin/sh", "-c", script})
			e.SetStopConfiguration(remote.ProcessStopConfiguration{Type: remote.ProcessStopSignal, Value: tc.value})
			ctx := context.Background()
			if err := e.Start(ctx); err != nil {
				t.Fatal(err)
			}
			waitLogLine(t, e, "ready")
			if err := e.Stop(ctx); err != nil {
				t.Fatal(err)
			}
			waitState(t, e, environment.ProcessOfflineState, 5*time.Second)
			if code, _, _ := e.ExitState(); code != tc.code {
				t.Fatalf("exit code %d, want %d", code, tc.code)
			}
		})
	}
}

func waitLogLine(t *testing.T, e *Environment, want string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if lines, _ := e.Readlog(50); strings.Contains(strings.Join(lines, "\n"), want) {
			return
		}
	}
	t.Fatalf("log line %q never appeared", want)
}

func TestStopWhileDetachedSignalsTheProcess(t *testing.T) {
	e, _, _ := newTestEnv(t, []string{"/bin/sh", "-c", `trap 'exit 12' TERM; echo ready; while true; do sleep 0.05; done`})
	e.SetStopConfiguration(remote.ProcessStopConfiguration{Type: remote.ProcessStopNativeStop})
	ctx := context.Background()
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitLogLine(t, e, "ready")
	e.mu.Lock()
	e.attached = false
	e.mu.Unlock()
	if err := e.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if e.State() != environment.ProcessStoppingState && e.State() != environment.ProcessOfflineState {
		t.Fatalf("state %s", e.State())
	}
	waitState(t, e, environment.ProcessOfflineState, 5*time.Second)
	if code, _, _ := e.ExitState(); code != 12 {
		t.Fatalf("exit code %d", code)
	}
}

// A process that ignores SIGTERM and its stop command is only ended by the
// escalation to SIGKILL once the wait expires.
func TestWaitForStopEscalates(t *testing.T) {
	stubborn := []string{"/bin/sh", "-c", `trap '' TERM; echo ready; while true; do sleep 0.05; done`}

	for name, stop := range map[string]remote.ProcessStopConfiguration{
		"signal stop": {Type: remote.ProcessStopSignal, Value: "SIGTERM"},
		"stdin stop":  {Type: remote.ProcessStopCommand, Value: "stop"},
	} {
		t.Run(name+" terminates", func(t *testing.T) {
			e, _, _ := newTestEnv(t, stubborn)
			e.SetStopConfiguration(stop)
			if err := e.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			waitLogLine(t, e, "ready")
			if err := e.WaitForStop(context.Background(), 300*time.Millisecond, true); err != nil {
				t.Fatal(err)
			}
			waitState(t, e, environment.ProcessOfflineState, 5*time.Second)
			if code, _, _ := e.ExitState(); code != 137 {
				t.Fatalf("exit code %d, want 137", code)
			}
		})
	}

	t.Run("without terminate it reports the timeout", func(t *testing.T) {
		e, _, _ := newTestEnv(t, stubborn)
		e.SetStopConfiguration(remote.ProcessStopConfiguration{Type: remote.ProcessStopCommand, Value: "stop"})
		if err := e.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitLogLine(t, e, "ready")
		err := e.WaitForStop(context.Background(), 300*time.Millisecond, false)
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("err = %v", err)
		}
		if e.State() == environment.ProcessOfflineState {
			t.Fatal("the process must not have been killed")
		}
	})

	t.Run("parent context cancellation without terminate", func(t *testing.T) {
		e, _, _ := newTestEnv(t, stubborn)
		e.SetStopConfiguration(remote.ProcessStopConfiguration{Type: remote.ProcessStopCommand, Value: "stop"})
		if err := e.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitLogLine(t, e, "ready")
		cctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		if err := e.WaitForStop(cctx, time.Minute, false); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestTerminateHonoursContext(t *testing.T) {
	e, _, _ := newTestEnv(t, []string{"/bin/sh", "-c", `trap '' TERM; echo ready; while true; do sleep 0.05; done`})
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitLogLine(t, e, "ready")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := e.Terminate(ctx, "SIGTERM"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if e.State() != environment.ProcessStoppingState {
		t.Fatalf("state %s", e.State())
	}
	// SIGKILL still works afterwards and the exit state is the kill.
	if err := e.Terminate(context.Background(), "SIGKILL"); err != nil {
		t.Fatal(err)
	}
	if e.State() != environment.ProcessOfflineState {
		t.Fatalf("state %s", e.State())
	}
}

func TestDestroyKillsTheProcess(t *testing.T) {
	e, _, _ := newTestEnv(t, []string{"/bin/sh", "-c", "sleep 30"})
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.Destroy(); err != nil {
		t.Fatal(err)
	}
	if e.State() != environment.ProcessOfflineState {
		t.Fatalf("state %s", e.State())
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if running, _ := e.IsRunning(context.Background()); !running {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("process still running after Destroy")
}

func TestStartWhenAlreadyRunningAttaches(t *testing.T) {
	e, _, _ := newTestEnv(t, []string{"/bin/sh", "-c", "sleep 30"})
	ctx := context.Background()
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	e.SetState(environment.ProcessOfflineState) // the agent lost track of it
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if e.State() != environment.ProcessRunningState || !e.IsAttached() {
		t.Fatalf("state %s attached %v", e.State(), e.IsAttached())
	}
	if up, err := e.Uptime(ctx); err != nil || up < 0 {
		t.Fatalf("uptime %d, %v", up, err)
	}
}

func TestInjectExitBranches(t *testing.T) {
	e := idleEnv(t, time.Second)

	// Already offline without a recorded exit: the injected details are kept.
	e.InjectExit(137, true)
	if code, oom, _ := e.ExitState(); code != 137 || !oom {
		t.Fatalf("exit %d %v", code, oom)
	}
	// Already offline with a placeholder: the authoritative values replace it.
	e.InjectExit(2, false)
	if code, oom, _ := e.ExitState(); code != 2 || oom {
		t.Fatalf("exit %d %v", code, oom)
	}

	// A process restarted after the connection loss: a late relay is ignored.
	e.SetState(environment.ProcessRunningState)
	e.mu.Lock()
	e.disconnectedAt = time.Now().Add(-time.Minute)
	e.startedAt = time.Now()
	e.mu.Unlock()
	e.InjectExit(9, true)
	if e.State() != environment.ProcessRunningState {
		t.Fatalf("state %s", e.State())
	}
	if code, _, _ := e.ExitState(); code != 2 {
		t.Fatalf("a stale relay must not overwrite the exit state, got %d", code)
	}
}

func TestIsConnError(t *testing.T) {
	for msg, want := range map[string]bool{
		"write: broken pipe":                 true,
		"connection closed":                  true,
		"read: connection reset by peer":     true,
		"use of closed network connection":   true,
		"permission denied":                  false,
		"process is not running":             false,
		"context deadline exceeded":          false,
		"wrapped: connection closed by peer": true,
	} {
		if got := isConnError(errors.New(msg)); got != want {
			t.Errorf("isConnError(%q) = %v, want %v", msg, got, want)
		}
	}
	if isConnError(nil) {
		t.Error("nil is not a connection error")
	}
}

func TestConnectLoopRetriesWithBackoff(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "shim.sock")
	ln := listenShim(t, sock)
	var calls atomic.Int32
	connect := func(ctx context.Context) (*protocol.Client, error) {
		if calls.Add(1) <= 2 {
			return nil, fmt.Errorf("listener not ready")
		}
		return ln.Accept(ctx)
	}
	e := New("retry", environment.NewConfiguration(environment.Settings{}, nil), Options{
		Connect: connect, SocketPath: sock, RunLog: filepath.Join(dir, "console.log"), DialTimeout: 5 * time.Second,
	})
	defer e.Close()
	// A shim that connects only after the failed attempts proves that the loop
	// kept trying.
	sup := supervisor.New(supervisor.Options{Socket: sock, Token: testToken, Argv: []string{"/bin/sh", "-c", "sleep 30"}, Dir: dir, KillGrace: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sup.Run(ctx) }()
	if err := e.OnBeforeStart(context.Background()); err != nil {
		t.Fatalf("environment never connected after retries: %v", err)
	}
	if calls.Load() < 3 {
		t.Fatalf("connect calls: %d", calls.Load())
	}
}

func TestRunLogHelpers(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := environment.NewConfiguration(environment.Settings{}, nil)
	e := New("log", cfg, Options{
		Connect: func(ctx context.Context) (*protocol.Client, error) { <-ctx.Done(); return nil, ctx.Err() },
		RunLog:  filepath.Join(blocker, "sub", "console.log"), // its parent is a file
	})
	defer e.Close()

	// An unwritable run log must not break output handling.
	e.appendRunLog([]byte("lost"))
	e.truncateRunLog()
	if _, err := e.runLogEmpty(); err == nil {
		t.Fatal("a stat error other than not-exist must be reported")
	}
	if lines, err := e.Readlog(10); err == nil && len(lines) != 0 {
		t.Fatalf("lines %v", lines)
	}

	// A working run log: append, truncate (closing the open file), empty check.
	e2 := New("log2", cfg, Options{
		Connect: func(ctx context.Context) (*protocol.Client, error) { <-ctx.Done(); return nil, ctx.Err() },
		RunLog:  filepath.Join(dir, "logs", "console.log"),
	})
	defer e2.Close()
	if empty, err := e2.runLogEmpty(); !empty || err != nil {
		t.Fatalf("missing log: empty=%v err=%v", empty, err)
	}
	e2.appendRunLog([]byte("one"))
	e2.appendRunLog([]byte("two"))
	if empty, _ := e2.runLogEmpty(); empty {
		t.Fatal("log has content")
	}
	if lines, _ := e2.Readlog(1); len(lines) != 1 || lines[0] != "two" {
		t.Fatalf("lines %v", lines)
	}
	e2.truncateRunLog()
	if empty, _ := e2.runLogEmpty(); !empty {
		t.Fatal("truncate must empty the log")
	}
	e2.appendRunLog([]byte("three"))
	if lines, _ := e2.Readlog(10); len(lines) != 1 || lines[0] != "three" {
		t.Fatalf("lines after truncate: %v", lines)
	}
}

func TestRegistryFactory(t *testing.T) {
	dir := t.TempDir()
	config.Update(func(c *config.Configuration) {
		c.System.Data = filepath.Join(dir, "data")
		c.System.LogDirectory = filepath.Join(dir, "log")
		c.System.CrashDetection.CrashDetectionEnabled = false
	})
	ln := listenShim(t, filepath.Join(dir, "shim.sock"))
	reg := NewRegistry(ln, []string{"INTERNAL_IP=10.1.2.3"}, nil)
	if _, ok := reg.Get("nope"); ok {
		t.Fatal("unknown server")
	}

	m := server.NewEmptyManager(nopPanel{}, server.WithEnvironmentFactory(reg.Factory))
	settings := `{"uuid":"srv-1","invocation":"x","container":{"image":"example/yolk:7"},"allocations":{"default":{"ip":"0.0.0.0","port":25565}}}`
	pc := &remote.ProcessConfiguration{}
	pc.Stop = remote.ProcessStopConfiguration{Type: remote.ProcessStopCommand, Value: "end"}
	data := remote.ServerConfigurationResponse{Settings: []byte(settings), ProcessConfiguration: pc}
	if _, err := m.InitServer(data); err != nil {
		t.Fatal(err)
	}
	e1, ok := reg.Get("srv-1")
	if !ok {
		t.Fatal("environment not registered")
	}
	if e1.Image() != "example/yolk:7" {
		t.Fatalf("image %q", e1.Image())
	}
	if got := e1.stopConfig(); got != pc.Stop {
		t.Fatalf("stop config %+v", got)
	}
	if e1.o.SocketPath != ln.Path() || e1.o.RunLog != filepath.Join(dir, "log", "console", "srv-1.log") {
		t.Fatalf("options %+v", e1.o)
	}
	if len(e1.o.ExtraEnv) != 1 || e1.o.ExtraEnv[0] != "INTERNAL_IP=10.1.2.3" {
		t.Fatalf("extra env %v", e1.o.ExtraEnv)
	}

	// Initialising the same server again replaces and closes the old environment.
	if _, err := m.InitServer(data); err != nil {
		t.Fatal(err)
	}
	e2, _ := reg.Get("srv-1")
	if e2 == e1 {
		t.Fatal("the environment must be replaced")
	}
	if e1.ctx.Err() == nil {
		t.Fatal("the replaced environment must be closed")
	}
	if e2.ctx.Err() != nil {
		t.Fatal("the new environment must be live")
	}
}

type nopPanel struct{ remote.Client }

func (nopPanel) PushServerStateChange(context.Context, string, remote.ServerStateChange) error {
	return nil
}
