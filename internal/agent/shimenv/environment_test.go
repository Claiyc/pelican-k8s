package shimenv

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/events"
	"github.com/pelican/wings/remote"

	"github.com/Claiyc/pelican-k8s/internal/shim/supervisor"
)

func init() {
	c, _ := config.NewAtPath("/dev/null")
	c.Docker.Network.Interface = "0.0.0.0"
	c.AuthenticationToken = "test-token"
	config.Set(c)
}

func newTestEnv(t *testing.T, argv []string) (*Environment, *supervisor.Supervisor, context.CancelFunc) {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "shim.sock")
	sup := supervisor.New(supervisor.Options{Socket: sock, Argv: argv, Dir: dir, KillGrace: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = sup.Run(ctx) }()
	cfg := environment.NewConfiguration(environment.Settings{Allocations: environment.Allocations{DefaultMapping: &environment.DefaultAllocationMapping{Ip: "127.0.0.1", Port: 25565}}}, []string{"SERVER_IP=127.0.0.1", "GREETING=hi"})
	e := New("test-server", cfg, Options{SocketPath: sock, RunLog: filepath.Join(dir, "logs", "console.log"), ExtraEnv: []string{"INTERNAL_IP=10.0.0.5"}, DialTimeout: 5 * time.Second})
	e.SetStopConfiguration(remote.ProcessStopConfiguration{Type: remote.ProcessStopCommand, Value: "stop"})
	t.Cleanup(func() { e.Close(); cancel() })
	return e, sup, cancel
}

func collectStates(t *testing.T, e *Environment) func() []string {
	t.Helper()
	ch := make(chan []byte, 64)
	e.Events().On(ch)
	var states []string
	return func() []string {
		for {
			select {
			case v := <-ch:
				var ev events.Event
				if err := events.DecodeTo(v, &ev); err == nil && ev.Topic == environment.StateChangeEvent {
					states = append(states, ev.Data.(string))
				}
			default:
				return states
			}
		}
	}
}

func waitState(t *testing.T, e *Environment, want string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if e.State() == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("state %q, want %q", e.State(), want)
}

func TestStartCommandStopAndReadlog(t *testing.T) {
	e, _, _ := newTestEnv(t, []string{"/bin/sh", "-c", `echo "env:$SERVER_IP:$INTERNAL_IP:$GREETING"; while read l; do echo "cmd:$l"; [ "$l" = stop ] && exit 0; done`})
	ctx := context.Background()
	var mu sync.Mutex
	var lines []string
	joined := func() string { mu.Lock(); defer mu.Unlock(); return strings.Join(lines, "\n") }
	e.SetLogCallback(func(b []byte) { mu.Lock(); lines = append(lines, string(b)); mu.Unlock() })
	states := collectStates(t, e)

	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if e.State() != environment.ProcessStartingState {
		t.Fatalf("state %s", e.State())
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(joined(), "env:0.0.0.0:10.0.0.5:hi") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(joined(), "env:0.0.0.0:10.0.0.5:hi") {
		t.Fatalf("no output: %v", joined())
	}
	if running, _ := e.IsRunning(ctx); !running {
		t.Fatal("expected running")
	}
	if up, _ := e.Uptime(ctx); up < 0 {
		t.Fatal("uptime")
	}
	e.SetState(environment.ProcessRunningState) // done detection would do this
	if err := e.SendCommand("hello"); err != nil {
		t.Fatal(err)
	}
	// The stop command moves the state to stopping before the process exits.
	if err := e.WaitForStop(ctx, 5*time.Second, true); err != nil {
		t.Fatal(err)
	}
	waitState(t, e, environment.ProcessOfflineState, 3*time.Second)
	code, oom, _ := e.ExitState()
	if code != 0 || oom {
		t.Fatalf("exit %d %v", code, oom)
	}
	got := states()
	want := []string{"starting", "running", "stopping", "offline"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("states %v", got)
	}
	log, err := e.Readlog(100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(log, "\n"), "cmd:hello") {
		t.Fatalf("readlog %v", log)
	}
	if err := e.SendCommand("x"); err == nil {
		t.Fatal("send on stopped process should fail")
	} else if !strings.Contains(err.Error(), "not running") && !strings.Contains(err.Error(), "not attached") {
		t.Fatal(err)
	}
}

func TestCrashSetsOfflineFromRunning(t *testing.T) {
	e, _, _ := newTestEnv(t, []string{"/bin/sh", "-c", `sleep 0.2; exit 5`})
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.SetState(environment.ProcessRunningState)
	waitState(t, e, environment.ProcessOfflineState, 5*time.Second)
	code, _, _ := e.ExitState()
	if code != 5 {
		t.Fatalf("code %d", code)
	}
}

func TestTerminateAndSignalStop(t *testing.T) {
	e, _, _ := newTestEnv(t, []string{"/bin/sh", "-c", `trap 'exit 0' INT; while true; do sleep 0.1; done`})
	e.SetStopConfiguration(remote.ProcessStopConfiguration{Type: remote.ProcessStopSignal, Value: "C"})
	ctx := context.Background()
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := e.WaitForStop(ctx, 5*time.Second, true); err != nil {
		t.Fatal(err)
	}
	waitState(t, e, environment.ProcessOfflineState, 3*time.Second)
	if code, _, _ := e.ExitState(); code != 0 {
		t.Fatalf("code %d", code)
	}

	// SIGKILL path via Terminate on a process that ignores everything else.
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := e.Terminate(ctx, "SIGKILL"); err != nil {
		t.Fatal(err)
	}
	if e.State() != environment.ProcessOfflineState {
		t.Fatalf("state %s", e.State())
	}
	if code, _, _ := e.ExitState(); code != 137 {
		t.Fatalf("code %d", code)
	}
}

func TestInjectExit(t *testing.T) {
	e, _, _ := newTestEnv(t, []string{"/bin/sh", "-c", `sleep 30`})
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.SetState(environment.ProcessRunningState)
	e.InjectExit(137, true)
	if e.State() != environment.ProcessOfflineState {
		t.Fatal("expected offline")
	}
	code, oom, _ := e.ExitState()
	if code != 137 || !oom {
		t.Fatalf("exit %d %v", code, oom)
	}
}

// TestContainerRestartHandledOnce: the shim goes away while the process is
// running, comes back with nothing running (kubelet restarted the container),
// the agent restarts the process, and a late exit-state relay from the
// operator must not kill the fresh start.
func TestContainerRestartHandledOnce(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "shim.sock")
	ctx1, cancel1 := context.WithCancel(context.Background())
	sup1 := supervisor.New(supervisor.Options{Socket: sock, Argv: []string{"/bin/sh", "-c", "sleep 30"}, Dir: dir, KillGrace: time.Second})
	go func() { _ = sup1.Run(ctx1) }()
	cfg := environment.NewConfiguration(environment.Settings{}, nil)
	e := New("s", cfg, Options{SocketPath: sock, RunLog: filepath.Join(dir, "console.log"), DialTimeout: 5 * time.Second})
	defer e.Close()
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.SetState(environment.ProcessRunningState)
	states := collectStates(t, e)
	// Container "restarts": the old shim dies, a new one comes up idle.
	cancel1()
	time.Sleep(300 * time.Millisecond)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	sup2 := supervisor.New(supervisor.Options{Socket: sock, Argv: []string{"/bin/sh", "-c", "sleep 30"}, Dir: dir, KillGrace: time.Second})
	go func() { _ = sup2.Run(ctx2) }()
	waitState(t, e, environment.ProcessOfflineState, 5*time.Second)
	// Either the old shim reported the SIGTERM exit (143) before dying or the
	// reconnect recorded the placeholder (137).
	if code, _, _ := e.ExitState(); code != 137 && code != 143 {
		t.Fatalf("exit code %d", code)
	}
	// Wings' crash handler would now restart; emulate it.
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The operator's relay arrives late with the real exit state.
	e.InjectExit(137, true)
	if e.State() != environment.ProcessStartingState {
		t.Fatalf("late relay must not change state, got %s", e.State())
	}
	if running, _ := e.IsRunning(context.Background()); !running {
		t.Fatal("process should still be running")
	}
	got := states()
	if strings.Join(got, ",") != "offline,starting" {
		t.Fatalf("states %v", got)
	}
}

func TestReattachAfterAgentRestart(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "shim.sock")
	sup := supervisor.New(supervisor.Options{Socket: sock, Argv: []string{"/bin/sh", "-c", "echo booted; sleep 30"}, Dir: dir, KillGrace: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sup.Run(ctx) }()
	cfg := environment.NewConfiguration(environment.Settings{}, nil)
	runLog := filepath.Join(dir, "console.log")
	e1 := New("s", cfg, Options{SocketPath: sock, RunLog: runLog, DialTimeout: 5 * time.Second})
	if err := e1.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	e1.Close()
	os.Remove(runLog) // simulate a lost run log so the ring buffer replay is exercised

	e2 := New("s", cfg, Options{SocketPath: sock, RunLog: runLog, DialTimeout: 5 * time.Second})
	defer e2.Close()
	waitState(t, e2, environment.ProcessRunningState, 5*time.Second)
	if !e2.IsAttached() {
		t.Fatal("expected attached")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if l, _ := e2.Readlog(10); len(l) > 0 && strings.Contains(l[0], "booted") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	l, _ := e2.Readlog(10)
	t.Fatalf("replayed log missing: %v", l)
}

func TestTailLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log")
	if l, err := TailLines(p, 5); err != nil || len(l) != 0 {
		t.Fatalf("%v %v", l, err)
	}
	var b strings.Builder
	for i := 0; i < 1000; i++ {
		b.WriteString(strings.Repeat("x", 1000))
		b.WriteString("\n")
	}
	b.WriteString("last\n")
	os.WriteFile(p, []byte(b.String()), 0o600)
	l, err := TailLines(p, 3)
	if err != nil || len(l) != 3 || l[2] != "last" {
		t.Fatalf("%d %v", len(l), err)
	}
}
