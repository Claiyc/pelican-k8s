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

func startSupervisor(t *testing.T, argv []string, reap bool) (*protocol.Client, *safeBuf, context.CancelFunc) {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "shim.sock")
	out := &safeBuf{}
	s := New(Options{Socket: sock, Argv: argv, Dir: dir, RingSize: 64, Stdout: out, KillGrace: time.Second, ReapOrphans: reap})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
	defer dcancel()
	c, err := protocol.WaitReady(dctx, sock, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("supervisor did not stop")
		}
	})
	return c, out, cancel
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
			st, err = c.Start(ctx, []string{"GREETING=world"})
			if err != nil || !st.Running || st.PID == 0 {
				t.Fatalf("start %+v %v", st, err)
			}
			if _, err := c.Start(ctx, nil); err == nil {
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
			if _, err := c.Start(ctx, nil); err != nil {
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
	c, _, _ := startSupervisor(t, []string{"/bin/sh", "-c", `trap 'echo term; exit 0' TERM; echo ready; while true; do sleep 0.1; done`}, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Start(ctx, nil); err != nil {
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
	// A second client sees the replayed ring buffer.
	c2, err := protocol.Dial(ctx, c2path(t, c))
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if _, err := c2.Subscribe(ctx, true); err != nil {
		t.Fatal(err)
	}
	ev := waitEvent(t, c2, protocol.TypeOutput, 2*time.Second)
	if !strings.Contains(string(ev.Data), "ready") {
		t.Fatalf("replay %q", ev.Data)
	}
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

// c2path finds the socket path from the first client's connection.
func c2path(t *testing.T, c *protocol.Client) string {
	t.Helper()
	return c.RemoteAddr()
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
	if _, err := c.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, c, protocol.TypeStarted, 2*time.Second)
	var collected string
	for !strings.Contains(collected, "ignoring TERM") {
		ev := waitEvent(t, c, protocol.TypeOutput, 5*time.Second)
		collected += string(ev.Data)
	}
	start := time.Now()
	cancel() // Run must SIGTERM, wait KillGrace (1s), then SIGKILL
	ev := waitEvent(t, c, protocol.TypeExited, 10*time.Second)
	if ev.Exit.Signal != "SIGKILL" {
		t.Fatalf("expected SIGKILL after grace, got %+v", ev.Exit)
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatal("killed before grace period")
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
	if _, err := c.Start(ctx, nil); err != nil {
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
