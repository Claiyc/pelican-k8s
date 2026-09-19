// Package supervisor is the shim's process supervisor: it spawns the egg's
// entrypoint in a PTY, relays its output, forwards stdin and signals, samples
// resource usage and reports exits over the unix socket protocol.
package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/Claiyc/pelican-k8s/internal/shim/cgroup"
	"github.com/Claiyc/pelican-k8s/internal/shim/protocol"
	"github.com/Claiyc/pelican-k8s/internal/shim/ringbuf"
)

// Options configure a Supervisor.
type Options struct {
	// Socket is the unix socket path to listen on.
	Socket string
	// Argv is the command to run. When empty, ArgvFile is read (a JSON array).
	Argv []string
	// ArgvFile holds the argv written by the probe init container.
	ArgvFile string
	// Dir is the working directory of the process (the server files).
	Dir string
	// TmpDir is emptied before every start; empty disables the cleanup.
	TmpDir string
	// RingSize is the size of the output ring buffer in bytes.
	RingSize int
	// StatsInterval is the resource sampling period while the process runs.
	StatsInterval time.Duration
	// KillGrace is the time between SIGTERM and SIGKILL when the shim itself is terminated.
	KillGrace time.Duration
	// Stdout receives a copy of all PTY output (container logs).
	Stdout io.Writer
	// Cgroup samples resource usage; nil disables stats.
	Cgroup *cgroup.Reader
	// Logger receives structured logs.
	Logger *slog.Logger
	// ReapOrphans enables the PID 1 reaper. Always on when running as PID 1.
	ReapOrphans bool
}

// Supervisor runs one process at a time and serves the shim protocol.
type Supervisor struct {
	o   Options
	log *slog.Logger

	mu        sync.Mutex
	cmd       *exec.Cmd
	ptmx      *os.File
	pid       int
	startedAt time.Time
	lastExit  *protocol.ExitState
	oomBase   uint64
	stopping  bool
	exited    chan struct{} // closed when the current process has exited

	ring  *ringbuf.Buffer
	conns map[*conn]struct{}

	childExit chan childExit
	// spawnMu makes "spawn the child and record its pid" atomic towards the
	// reaper, which must know the supervised pid before it may reap it.
	spawnMu sync.Mutex
}

type childExit struct {
	pid int
	ws  unix.WaitStatus
}

type conn struct {
	enc        *protocol.Encoder
	c          net.Conn
	subscribed bool
}

// New returns a supervisor for the given options.
func New(o Options) *Supervisor {
	if o.RingSize <= 0 {
		o.RingSize = 1 << 20
	}
	if o.StatsInterval <= 0 {
		o.StatsInterval = 2 * time.Second
	}
	if o.KillGrace <= 0 {
		o.KillGrace = 10 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Stdout == nil {
		o.Stdout = io.Discard
	}
	return &Supervisor{
		o:         o,
		log:       o.Logger,
		ring:      ringbuf.New(o.RingSize),
		conns:     map[*conn]struct{}{},
		childExit: make(chan childExit, 16),
	}
}

// Run serves the socket until ctx is cancelled or SIGTERM/SIGINT arrives. On
// termination a running process is stopped (SIGTERM, then SIGKILL after
// KillGrace) before Run returns.
func (s *Supervisor) Run(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.o.Socket), 0o770); err != nil {
		return err
	}
	_ = os.Remove(s.o.Socket)
	ln, err := net.Listen("unix", s.o.Socket)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.o.Socket, err)
	}
	defer ln.Close()
	if err := os.Chmod(s.o.Socket, 0o600); err != nil {
		return err
	}
	s.log.Info("shim listening", "socket", s.o.Socket, "pid", os.Getpid())

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)

	if s.o.ReapOrphans || os.Getpid() == 1 {
		go s.reaper(ctx)
	}

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()

	select {
	case <-ctx.Done():
	case sig := <-sigs:
		s.log.Info("shim received signal, shutting down", "signal", sig.String())
	}
	// Stop accepting connections first so a reconnecting agent does not latch
	// onto a shim that is about to exit.
	ln.Close()
	_ = os.Remove(s.o.Socket)
	s.shutdown()
	// Drop the remaining agent connections so they reconnect to the successor.
	s.mu.Lock()
	for c := range s.conns {
		c.c.Close()
	}
	s.mu.Unlock()
	return nil
}

func (s *Supervisor) shutdown() {
	s.mu.Lock()
	s.stopping = true
	running := s.pid != 0
	pid := s.pid
	exited := s.exited
	s.mu.Unlock()
	if !running {
		return
	}
	s.log.Info("terminating process group", "pid", pid)
	_ = unix.Kill(-pid, unix.SIGTERM)
	select {
	case <-exited:
		return
	case <-time.After(s.o.KillGrace):
	}
	s.log.Warn("process did not exit in time, sending SIGKILL", "pid", pid)
	_ = unix.Kill(-pid, unix.SIGKILL)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		s.log.Error("process still alive after SIGKILL")
	}
}

// reaper collects exit statuses of every child, including orphans re-parented
// to PID 1, and forwards the supervised child's status to waitLoop.
func (s *Supervisor) reaper(ctx context.Context) {
	sigs := make(chan os.Signal, 64)
	signal.Notify(sigs, syscall.SIGCHLD)
	defer signal.Stop(sigs)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sigs:
		case <-t.C:
		}
		s.reapAll()
	}
}

func (s *Supervisor) serve(c net.Conn) {
	cn := &conn{enc: protocol.NewEncoder(c), c: c, subscribed: true}
	s.mu.Lock()
	s.conns[cn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, cn)
		s.mu.Unlock()
		c.Close()
	}()
	dec := protocol.NewDecoder(c)
	for {
		var m protocol.Message
		if err := dec.Decode(&m); err != nil {
			if !errors.Is(err, protocol.ErrClosed) && !errors.Is(err, io.EOF) {
				s.log.Debug("connection ended", "error", err)
			}
			return
		}
		reply := s.handle(cn, &m)
		reply.Type = protocol.TypeReply
		reply.ID = m.ID
		if err := cn.enc.Encode(reply); err != nil {
			return
		}
	}
}

func (s *Supervisor) handle(cn *conn, m *protocol.Message) *protocol.Message {
	switch m.Type {
	case protocol.TypeStatus:
		return &protocol.Message{OK: true, Status: s.status()}
	case protocol.TypeSubscribe:
		s.mu.Lock()
		cn.subscribed = true
		s.mu.Unlock()
		if m.Replay {
			data := s.ring.Bytes()
			for len(data) > 0 {
				n := min(len(data), 64*1024)
				_ = cn.enc.Encode(&protocol.Message{Type: protocol.TypeOutput, Data: data[:n]})
				data = data[n:]
			}
		}
		return &protocol.Message{OK: true, Status: s.status()}
	case protocol.TypeStart:
		if err := s.start(m.Env); err != nil {
			return &protocol.Message{Error: err.Error(), Status: s.status()}
		}
		return &protocol.Message{OK: true, Status: s.status()}
	case protocol.TypeStdin:
		s.mu.Lock()
		ptmx := s.ptmx
		s.mu.Unlock()
		if ptmx == nil {
			return &protocol.Message{Error: "process is not running"}
		}
		if _, err := ptmx.Write(m.Data); err != nil {
			return &protocol.Message{Error: err.Error()}
		}
		return &protocol.Message{OK: true}
	case protocol.TypeSignal:
		sig, err := parseSignal(m.Signal)
		if err != nil {
			return &protocol.Message{Error: err.Error()}
		}
		if err := s.signal(sig); err != nil {
			return &protocol.Message{Error: err.Error()}
		}
		return &protocol.Message{OK: true, Status: s.status()}
	case protocol.TypeKill:
		if err := s.signal(unix.SIGKILL); err != nil {
			return &protocol.Message{Error: err.Error()}
		}
		return &protocol.Message{OK: true, Status: s.status()}
	default:
		return &protocol.Message{Error: "unknown message type " + m.Type}
	}
}

func (s *Supervisor) status() *protocol.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &protocol.Status{Version: protocol.Version, Running: s.pid != 0, PID: s.pid, LastExit: s.lastExit, Stopping: s.stopping}
	if s.pid != 0 {
		t := s.startedAt
		st.StartedAt = &t
	}
	return st
}

func (s *Supervisor) signal(sig unix.Signal) error {
	s.mu.Lock()
	pid := s.pid
	s.mu.Unlock()
	if pid == 0 {
		return errors.New("process is not running")
	}
	if err := unix.Kill(-pid, sig); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	return nil
}

// ErrAlreadyRunning is returned by start when a process is alive.
var ErrAlreadyRunning = errors.New("process is already running")

func (s *Supervisor) resolveArgv() ([]string, error) {
	if len(s.o.Argv) > 0 {
		return s.o.Argv, nil
	}
	if s.o.ArgvFile == "" {
		return nil, errors.New("no argv configured")
	}
	b, err := os.ReadFile(s.o.ArgvFile)
	if err != nil {
		return nil, fmt.Errorf("read argv file: %w", err)
	}
	var argv []string
	if err := json.Unmarshal(b, &argv); err != nil {
		return nil, fmt.Errorf("parse argv file %s: %w", s.o.ArgvFile, err)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("argv file %s is empty", s.o.ArgvFile)
	}
	return argv, nil
}

func (s *Supervisor) start(env []string) error {
	s.mu.Lock()
	if s.pid != 0 {
		s.mu.Unlock()
		return ErrAlreadyRunning
	}
	if s.stopping {
		s.mu.Unlock()
		return errors.New("shim is shutting down")
	}
	s.mu.Unlock()

	argv, err := s.resolveArgv()
	if err != nil {
		return err
	}
	if s.o.TmpDir != "" {
		cleanDir(s.o.TmpDir)
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = s.o.Dir
	cmd.Env = MergeEnv(os.Environ(), env)
	// Detach the game from the shim's own signals: the process gets its own
	// session and process group (pty.Start sets Setsid and Setctty).
	s.spawnMu.Lock()
	ptmx, err := pty.Start(cmd)
	if err != nil {
		s.spawnMu.Unlock()
		return fmt.Errorf("spawn %v: %w", argv, err)
	}

	s.mu.Lock()
	s.cmd = cmd
	s.ptmx = ptmx
	s.pid = cmd.Process.Pid
	s.startedAt = time.Now()
	s.exited = make(chan struct{})
	if s.o.Cgroup != nil {
		s.oomBase = s.o.Cgroup.OOMKills()
	}
	pid := s.pid
	exited := s.exited
	s.mu.Unlock()
	s.spawnMu.Unlock()
	s.ring.Reset()
	s.log.Info("process started", "pid", pid, "argv", argv)
	s.broadcast(&protocol.Message{Type: protocol.TypeStarted, PID: pid})

	go s.readOutput(ptmx)
	go s.waitLoop(pid, cmd, ptmx, exited)
	if s.o.Cgroup != nil {
		go s.statsLoop(exited)
	}
	return nil
}

func (s *Supervisor) readOutput(ptmx *os.File) {
	buf := make([]byte, 32*1024)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			s.ring.Write(chunk)
			_, _ = s.o.Stdout.Write(chunk)
			s.broadcast(&protocol.Message{Type: protocol.TypeOutput, Data: chunk})
		}
		if err != nil {
			// EIO is the normal end: the slave side was closed by the process.
			return
		}
	}
}

// reapAll collects every exited child. Only the supervised process is handed
// to waitLoop; orphans are dropped here. Forwarding them all filled the channel
// while no process ran (nothing drains it then), which stopped the reaper and
// left every later orphan a zombie, and a stale entry could be taken for the
// exit of a later process that reused the pid.
func (s *Supervisor) reapAll() {
	s.spawnMu.Lock()
	defer s.spawnMu.Unlock()
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
		if err != nil || pid <= 0 {
			return
		}
		s.mu.Lock()
		supervised := pid == s.pid
		s.mu.Unlock()
		if supervised {
			s.childExit <- childExit{pid: pid, ws: ws}
		} else {
			s.log.Debug("reaped orphan", "pid", pid, "status", int(ws))
		}
	}
}

// waitLoop waits for the child's exit status. When a reaper is active the
// status arrives through childExit; otherwise cmd.Wait is used directly.
func (s *Supervisor) waitLoop(pid int, cmd *exec.Cmd, ptmx *os.File, exited chan struct{}) {
	var ws unix.WaitStatus
	if s.o.ReapOrphans || os.Getpid() == 1 {
		for ce := range s.childExit {
			if ce.pid == pid {
				ws = ce.ws
				break
			}
		}
	} else {
		err := cmd.Wait()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			ws = unix.WaitStatus(ee.Sys().(syscall.WaitStatus))
		} else if err != nil {
			s.log.Warn("wait failed", "error", err)
		}
	}

	// The direct child is gone: make sure nothing of its session survives,
	// which is what a container teardown would do, then release the PTY.
	_ = unix.Kill(-pid, unix.SIGKILL)
	// Give the output reader a moment to drain what is still buffered.
	time.Sleep(50 * time.Millisecond)
	_ = ptmx.Close()

	ex := &protocol.ExitState{At: time.Now()}
	switch {
	case ws.Exited():
		ex.Code = ws.ExitStatus()
	case ws.Signaled():
		ex.Code = 128 + int(ws.Signal())
		ex.Signal = unix.SignalName(ws.Signal())
	}
	if s.o.Cgroup != nil {
		s.mu.Lock()
		base := s.oomBase
		s.mu.Unlock()
		if s.o.Cgroup.OOMKills() > base {
			ex.OOMKilled = true
		}
	}

	s.mu.Lock()
	s.pid = 0
	s.cmd = nil
	s.ptmx = nil
	s.lastExit = ex
	s.mu.Unlock()
	s.log.Info("process exited", "pid", pid, "code", ex.Code, "signal", ex.Signal, "oom", ex.OOMKilled)
	// Tell the agents before unblocking shutdown, which may close their connections.
	s.broadcast(&protocol.Message{Type: protocol.TypeExited, Exit: ex})
	close(exited)
}

func (s *Supervisor) statsLoop(exited chan struct{}) {
	t := time.NewTicker(s.o.StatsInterval)
	defer t.Stop()
	for {
		select {
		case <-exited:
			return
		case <-t.C:
			s.mu.Lock()
			started := s.startedAt
			s.mu.Unlock()
			st := s.o.Cgroup.Sample(started)
			s.broadcast(&protocol.Message{Type: protocol.TypeStats, Stats: st})
		}
	}
}

func (s *Supervisor) broadcast(m *protocol.Message) {
	s.mu.Lock()
	targets := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		if c.subscribed {
			targets = append(targets, c)
		}
	}
	s.mu.Unlock()
	for _, c := range targets {
		if err := c.enc.Encode(m); err != nil {
			c.c.Close()
		}
	}
}

// MergeEnv overlays override onto base by key; later entries win.
func MergeEnv(base, override []string) []string {
	idx := map[string]int{}
	out := make([]string, 0, len(base)+len(override))
	add := func(kv string) {
		k, _, _ := strings.Cut(kv, "=")
		if i, ok := idx[k]; ok {
			out[i] = kv
			return
		}
		idx[k] = len(out)
		out = append(out, kv)
	}
	for _, kv := range base {
		add(kv)
	}
	for _, kv := range override {
		add(kv)
	}
	return out
}

func cleanDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

func parseSignal(name string) (unix.Signal, error) {
	name = strings.ToUpper(strings.TrimSpace(name))
	if n, err := strconv.Atoi(name); err == nil {
		return unix.Signal(n), nil
	}
	if !strings.HasPrefix(name, "SIG") {
		name = "SIG" + name
	}
	sig := unix.SignalNum(name)
	if sig == 0 {
		return 0, fmt.Errorf("unknown signal %q", name)
	}
	return sig, nil
}
