package supervisor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Claiyc/pelican-k8s/internal/shim/protocol"
)

type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var testToken = []byte("shim-token")

func startSupervisor(t *testing.T, argv []string, reap bool) (*protocol.Client, *safeBuf, context.CancelFunc) {
	t.Helper()
	c, out, cancel, _ := startSupervisorListener(t, argv, reap)
	return c, out, cancel
}

// startSupervisorListener plays the agent: it listens, lets the supervisor
// connect and authenticate, and subscribes to its events.
func startSupervisorListener(t *testing.T, argv []string, reap bool) (*protocol.Client, *safeBuf, context.CancelFunc, *protocol.Listener) {
	t.Helper()
	return startWith(t, Options{Argv: argv, ReapOrphans: reap})
}

// startWith starts a supervisor with o, filling in the agent address, the
// token, the directory, the output and short shutdown timings.
func startWith(t *testing.T, o Options) (*protocol.Client, *safeBuf, context.CancelFunc, *protocol.Listener) {
	t.Helper()
	dir := t.TempDir()
	ln, err := protocol.Listen("127.0.0.1:0", testToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := &safeBuf{}
	o.Agent, o.Token, o.Dir, o.RingSize, o.Stdout = ln.Addr(), testToken, dir, 64, out
	if o.KillGrace == 0 {
		o.KillGrace = time.Second
	}
	if o.GracePeriod == 0 {
		o.GracePeriod, o.TermLead = 3*time.Second, 2*time.Second
	}
	s := New(o)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	c := acceptShim(t, ln)
	t.Cleanup(func() {
		c.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("supervisor did not stop")
		}
		ln.Close()
	})
	return c, out, cancel, ln
}

func acceptShim(t *testing.T, ln *protocol.Listener) *protocol.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := ln.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Subscribe(ctx, false); err != nil {
		t.Fatal(err)
	}
	return c
}

func waitEvent(t *testing.T, c *protocol.Client, typ string, timeout time.Duration) *protocol.Message {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-c.Events:
			if !ok {
				t.Fatalf("events closed while waiting for %s", typ)
			}
			if ev.Type == typ {
				return ev
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %s event", typ)
		}
	}
}

func TestLifecycle(t *testing.T) {
	for _, reap := range []bool{false, true} {
		t.Run(map[bool]string{false: "wait", true: "reaper"}[reap], func(t *testing.T) {
			c, out, _ := startSupervisor(t, []string{"/bin/sh", "-c", `echo "hello $GREETING"; read line; echo "got:$line"; exit 3`}, reap)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			st, err := c.Status(ctx)
			if err != nil || st.Running {
				t.Fatalf("initial status %+v %v", st, err)
			}
			st, err = c.Start(ctx, []string{"GREETING=world"}, nil)
			if err != nil || !st.Running || st.PID == 0 {
				t.Fatalf("start %+v %v", st, err)
			}
			if _, err := c.Start(ctx, nil, nil); err == nil {
				t.Fatal("second start should fail")
			}
			waitEvent(t, c, protocol.TypeStarted, 2*time.Second)
			var collected string
			for !strings.Contains(collected, "hello world") {
				ev := waitEvent(t, c, protocol.TypeOutput, 3*time.Second)
				collected += string(ev.Data)
			}
			if err := c.Stdin(ctx, []byte("ping\n")); err != nil {
				t.Fatal(err)
			}
			ev := waitEvent(t, c, protocol.TypeExited, 5*time.Second)
			if ev.Exit == nil || ev.Exit.Code != 3 {
				t.Fatalf("exit %+v", ev.Exit)
			}
			// PTY echo plus the program's own output.
			if s := out.String(); !strings.Contains(s, "got:ping") {
				t.Fatalf("stdout tee missing output: %q", s)
			}
			st, err = c.Status(ctx)
			if err != nil || st.Running || st.LastExit == nil || st.LastExit.Code != 3 {
				t.Fatalf("final status %+v %v", st, err)
			}

			// Restart is possible after exit, replay returns the ring buffer tail.
			if _, err := c.Start(ctx, nil, nil); err != nil {
				t.Fatal(err)
			}
			waitEvent(t, c, protocol.TypeStarted, 2*time.Second)
			if err := c.Kill(ctx); err != nil {
				t.Fatal(err)
			}
			ev = waitEvent(t, c, protocol.TypeExited, 5*time.Second)
			if ev.Exit.Code != 137 || ev.Exit.Signal != "SIGKILL" {
				t.Fatalf("kill exit %+v", ev.Exit)
			}
		})
	}
}

func TestSignalAndReplay(t *testing.T) {
	c, _, _, ln := startSupervisorListener(t, []string{"/bin/sh", "-c", `trap 'echo term; exit 0' TERM; echo ready; while true; do sleep 0.1; done`}, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Start(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	var collected string
	for !strings.Contains(collected, "ready") {
		ev := waitEvent(t, c, protocol.TypeOutput, 3*time.Second)
		collected += string(ev.Data)
	}
	if err := c.Signal(ctx, "bogus"); err == nil {
		t.Fatal("bogus signal accepted")
	}
	// An agent restart: the shim reconnects while the process keeps running,
	// and the new connection sees the replayed ring buffer.
	c.Close()
	c2 := acceptShim(t, ln)
	defer c2.Close()
	if st, err := c2.Subscribe(ctx, true); err != nil || !st.Running {
		t.Fatalf("resubscribe: %+v %v", st, err)
	}
	ev := waitEvent(t, c2, protocol.TypeOutput, 2*time.Second)
	if !strings.Contains(string(ev.Data), "ready") {
		t.Fatalf("replay %q", ev.Data)
	}
	c = c2
	if err := c.Signal(ctx, "SIGTERM"); err != nil {
		t.Fatal(err)
	}
	ev = waitEvent(t, c, protocol.TypeExited, 5*time.Second)
	if ev.Exit.Code != 0 {
		t.Fatalf("exit %+v", ev.Exit)
	}
	if err := c.Signal(ctx, "SIGTERM"); err == nil {
		t.Fatal("signal on stopped process should fail")
	}
}

// TestHelperProcess is re-executed as the supervised process by other tests.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	fmt.Println("ignoring TERM")
	time.Sleep(time.Minute)
	os.Exit(0)
}

func TestShutdownTerminatesProcess(t *testing.T) {
	c, _, cancel := startSupervisor(t, []string{os.Args[0], "-test.run=TestHelperProcess", "--"}, false)
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	ctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ccancel()
	if _, err := c.Start(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, c, protocol.TypeStarted, 2*time.Second)
	var collected string
	for !strings.Contains(collected, "ignoring TERM") {
		ev := waitEvent(t, c, protocol.TypeOutput, 5*time.Second)
		collected += string(ev.Data)
	}
	start := time.Now()
	// Without a stop configuration Run sends SIGTERM, again TermLead before
	// the grace period ends (after 1s), then SIGKILL after KillGrace (1s).
	cancel()
	waitEvent(t, c, protocol.TypeTerminating, 2*time.Second)
	ev := waitEvent(t, c, protocol.TypeExited, 10*time.Second)
	if ev.Exit.Signal != "SIGKILL" {
		t.Fatalf("expected SIGKILL after grace, got %+v", ev.Exit)
	}
	if time.Since(start) < 1900*time.Millisecond {
		t.Fatal("killed before grace period")
	}
}

// On termination the shim stops the process with the stop command it holds,
// without any help from the agent.
func TestShutdownRunsStopCommand(t *testing.T) {
	c, _, cancel, _ := startWith(t, Options{Argv: []string{"/bin/sh", "-c", `echo ready; while read line; do if [ "$line" = "halt" ]; then echo bye; exit 0; fi; done`}, GracePeriod: time.Minute, TermLead: 20 * time.Second})
	ctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ccancel()
	if _, err := c.Start(ctx, nil, &protocol.StopConfig{Type: protocol.StopCommand, Value: "halt"}); err != nil {
		t.Fatal(err)
	}
	var collected string
	for !strings.Contains(collected, "ready") {
		collected += string(waitEvent(t, c, protocol.TypeOutput, 3*time.Second).Data)
	}
	start := time.Now()
	cancel()
	waitEvent(t, c, protocol.TypeTerminating, 2*time.Second)
	ev := waitEvent(t, c, protocol.TypeExited, 5*time.Second)
	if ev.Exit.Code != 0 || ev.Exit.Signal != "" {
		t.Fatalf("exit %+v", ev.Exit)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("the stop command did not end the process; it waited for the grace period")
	}
}

// configure replaces the stop configuration; a signal configuration is sent
// to the process group.
func TestShutdownRunsConfiguredStopSignal(t *testing.T) {
	c, _, cancel, _ := startWith(t, Options{Argv: []string{"/bin/sh", "-c", `trap 'echo int; exit 7' INT; trap '' TERM; echo ready; while true; do sleep 0.1; done`}, GracePeriod: time.Minute, TermLead: 20 * time.Second})
	ctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ccancel()
	if _, err := c.Start(ctx, nil, &protocol.StopConfig{Type: protocol.StopCommand, Value: "stop"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Configure(ctx, protocol.StopConfig{Type: protocol.StopSignal, Value: "SIGINT"}); err != nil {
		t.Fatal(err)
	}
	var collected string
	for !strings.Contains(collected, "ready") {
		collected += string(waitEvent(t, c, protocol.TypeOutput, 3*time.Second).Data)
	}
	cancel()
	ev := waitEvent(t, c, protocol.TypeExited, 5*time.Second)
	if ev.Exit.Code != 7 {
		t.Fatalf("exit %+v", ev.Exit)
	}
}

// A shim with no process exits at once on termination.
func TestShutdownWithoutProcessIsImmediate(t *testing.T) {
	c, _, cancel, _ := startWith(t, Options{Argv: []string{"/bin/true"}, GracePeriod: time.Minute, TermLead: 20 * time.Second})
	start := time.Now()
	cancel()
	waitEvent(t, c, protocol.TypeTerminating, 2*time.Second)
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the shim kept running")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("shutdown waited without a process")
	}
}

// The readiness file follows the state the agent pushes, and only while a
// process runs.
func TestReadyFile(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	c, _, _, _ := startWith(t, Options{Argv: []string{"/bin/sh", "-c", `read line; exit 0`}, ReadyFile: ready, PodUID: "pod-7"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exists := func() bool { _, err := os.Stat(ready); return err == nil }

	if err := c.State(ctx, "running"); err != nil {
		t.Fatal(err)
	}
	if exists() {
		t.Fatal("ready without a process")
	}
	st, err := c.Start(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.PodUID != "pod-7" {
		t.Fatalf("status pod UID %q", st.PodUID)
	}
	if err := c.State(ctx, "starting"); err != nil {
		t.Fatal(err)
	}
	if exists() {
		t.Fatal("ready while starting")
	}
	if err := c.State(ctx, "running"); err != nil {
		t.Fatal(err)
	}
	if !exists() {
		t.Fatal("not ready while running")
	}
	if err := c.Stdin(ctx, []byte("x\n")); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, c, protocol.TypeExited, 5*time.Second)
	if exists() {
		t.Fatal("ready after the process exited")
	}
}

func TestStopSignal(t *testing.T) {
	for in, want := range map[string]syscall.Signal{"SIGABRT": syscall.SIGABRT, "sigint": syscall.SIGINT, "C": syscall.SIGINT, "SIGTERM": syscall.SIGTERM, "SIGHUP": syscall.SIGKILL, "": syscall.SIGKILL} {
		if got := StopSignal(in); got != want {
			t.Errorf("StopSignal(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMergeEnv(t *testing.T) {
	got := MergeEnv([]string{"PATH=/bin", "HOME=/root", "X=1"}, []string{"HOME=/home/container", "Y=2"})
	want := []string{"PATH=/bin", "HOME=/home/container", "X=1", "Y=2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v", got)
	}
}

// TestReaperKeepsReapingWhileIdle covers a stopped server: orphans (for
// example from kubectl exec) must not pile up as zombies, and they must not
// disturb the exit status of a process started afterwards.
func TestReaperKeepsReapingWhileIdle(t *testing.T) {
	c, _, _ := startSupervisor(t, []string{"/bin/sh", "-c", "exit 7"}, true)
	var pids []int
	for i := 0; i < 40; i++ { // more than the exit channel holds
		cmd := exec.Command("/bin/true")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		pids = append(pids, cmd.Process.Pid)
	}
	deadline := time.Now().Add(10 * time.Second)
	for _, pid := range pids {
		for {
			if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); os.IsNotExist(err) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("pid %d was never reaped", pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Start(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	if m := waitEvent(t, c, protocol.TypeExited, 5*time.Second); m.Exit == nil || m.Exit.Code != 7 {
		t.Fatalf("exit after idle orphans: %+v", m.Exit)
	}
}

func TestStragglerPIDs(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"1", "7", "42", "self", "net", "cpuinfo"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A regular file whose name is numeric must not be taken for a process.
	if err := os.WriteFile(filepath.Join(dir, "99"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got := stragglerPIDs(dir, 1)
	sort.Ints(got)
	if len(got) != 2 || got[0] != 7 || got[1] != 42 {
		t.Fatalf("got %v, want [7 42]", got)
	}
	// Excluding self is what keeps the shim from killing itself.
	for _, p := range got {
		if p == 1 {
			t.Fatal("self must be excluded")
		}
	}
}

func TestStragglerPIDsMissingProc(t *testing.T) {
	if got := stragglerPIDs(filepath.Join(t.TempDir(), "absent"), 1); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

// The sweep is guarded on the real PID, not on a flag: outside a PID namespace
// it would kill unrelated processes on the host.
func TestKillStragglersOnlyRunsAsPID1(t *testing.T) {
	if os.Getpid() == 1 {
		t.Skip("test process is PID 1")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	s := &Supervisor{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.killStragglers()

	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("unrelated process was killed: %v", err)
	}
}

// The shim token reaches neither the game process nor anything it starts,
// whether it comes from the shim's environment or the agent's start request.
func TestTokenNotInProcessEnvironment(t *testing.T) {
	t.Setenv(protocol.TokenEnv, "from-container-env")
	c, _, _ := startSupervisor(t, []string{"/bin/sh", "-c", `echo "token:${` + protocol.TokenEnv + `:-none}:$GREETING"`}, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Start(ctx, []string{protocol.TokenEnv + "=from-agent", "GREETING=hi"}, nil); err != nil {
		t.Fatal(err)
	}
	var collected string
	for !strings.Contains(collected, "token:") || !strings.Contains(collected, "\n") {
		ev := waitEvent(t, c, protocol.TypeOutput, 3*time.Second)
		collected += string(ev.Data)
	}
	if !strings.Contains(collected, "token:none:hi") {
		t.Fatalf("process saw %q", collected)
	}
}

func TestWithoutKey(t *testing.T) {
	got := WithoutKey([]string{"A=1", "TOKEN=x", "TOKENX=y", "TOKEN=z", "B"}, "TOKEN")
	if strings.Join(got, ",") != "A=1,TOKENX=y,B" {
		t.Fatalf("got %v", got)
	}
}

// Without a token the shim refuses to run rather than serve an
// unauthenticated channel.
func TestRunRequiresToken(t *testing.T) {
	s := New(Options{Agent: "127.0.0.1:1", Argv: []string{"/bin/true"}})
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("Run without a token succeeded")
	}
}

// A process that is not the shim (the game, same UID) cannot pass the
// agent's handshake: the agent never hands its connection out.
func TestImpostorIsNotAccepted(t *testing.T) {
	dir := t.TempDir()
	ln, err := protocol.Listen("127.0.0.1:0", testToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	impostor := New(Options{Agent: ln.Addr(), Token: []byte("guessed"), Argv: []string{"/bin/true"}, Dir: dir})
	go func() { _ = impostor.Run(ctx) }()
	actx, acancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer acancel()
	if c, err := ln.Accept(actx); err == nil {
		t.Fatalf("impostor accepted: %v", c)
	}
}
