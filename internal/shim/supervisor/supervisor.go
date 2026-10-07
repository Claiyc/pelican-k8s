// Package supervisor is the shim's process supervisor: it spawns the egg's
// entrypoint in a PTY, relays its output, forwards stdin and signals, samples
// resource usage and reports exits over the shim protocol. The shim dials the
// agent over TCP and both sides authenticate with the shared token before it
// serves requests. When its container is terminated, the shim stops the
// process by itself with the stop configuration the agent gave it.
package supervisor

import (
	"context"
	"crypto/tls"
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

	"github.com/Claiyc/pelican-k8s/internal/pki"
	"github.com/Claiyc/pelican-k8s/internal/shim/cgroup"
	"github.com/Claiyc/pelican-k8s/internal/shim/protocol"
	"github.com/Claiyc/pelican-k8s/internal/shim/ringbuf"
)

// Options configure a Supervisor.
type Options struct {
	// Agent is the agent's shim address (host:port) the shim dials.
	Agent string
	// Token is the shared secret both sides prove on every connection. It is
	// never passed to the game process.
	Token []byte
	// PodUID is sent to the agent in the handshake.
	PodUID string
	// AgentCA, when set, is the CA bundle the agent's certificate must chain
	// to: the connection is TLS, verified against the host name in Agent. It
	// is read on every dial, so a renewed bundle needs no restart.
	AgentCA string
	// ReadyFile is present exactly while the agent reports the process as
	// running; the container's readiness probe checks it. Empty disables it.
	ReadyFile string
	// GracePeriod is the pod's termination grace period. On SIGTERM the shim
	// applies the stop configuration and sends SIGTERM to the process group
	// TermLead before the grace period ends, then SIGKILL after KillGrace.
	GracePeriod time.Duration
	// TermLead is how long before the end of GracePeriod the process group
	// gets SIGTERM when the stop configuration has not ended it.
	TermLead time.Duration
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
	// Stop is the initial stop configuration; the agent replaces it with
	// start and configure. Without one, termination sends SIGTERM.
	Stop *protocol.StopConfig
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
	stop      *protocol.StopConfig
	state     string        // the agent's process state
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
	if o.TermLead <= 0 {
		o.TermLead = 20 * time.Second
	}
	if o.GracePeriod <= 0 {
		o.GracePeriod = 660 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Stdout == nil {
		o.Stdout = io.Discard
	}
	return &Supervisor{
		o:         o,
		stop:      o.Stop,
		log:       o.Logger,
		ring:      ringbuf.New(o.RingSize),
		conns:     map[*conn]struct{}{},
		childExit: make(chan childExit, 16),
	}
}

// Run keeps a connection to the agent and serves it until ctx is cancelled or
// SIGTERM/SIGINT arrives. On termination a running process is stopped with
// the stop configuration, then SIGTERM and SIGKILL (see Options.GracePeriod),
// before Run returns.
func (s *Supervisor) Run(ctx context.Context) error {
	if len(s.o.Token) == 0 {
		return errors.New("no shim token configured")
	}
	s.setReady(false)
	s.log.Info("shim started", "agent", s.o.Agent, "pid", os.Getpid())

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)

	if s.o.ReapOrphans || os.Getpid() == 1 {
		go s.reaper(ctx)
	}

	connCtx, stopConnecting := context.WithCancel(ctx)
	defer stopConnecting()
	go s.connectLoop(connCtx)

	termAt := time.Now()
	select {
	case <-ctx.Done():
	case sig := <-sigs:
		termAt = time.Now()
		s.log.Info("shim received signal, shutting down", "signal", sig.String())
	}
	// Keep connecting while the process stops: an agent that restarts
	// meanwhile sees the shutdown through on its new connection.
	s.shutdown(termAt)
	stopConnecting()
	// Drop the agent connection; the next shim connects on its own.
	s.mu.Lock()
	for c := range s.conns {
		c.c.Close()
	}
	s.mu.Unlock()
	return nil
}

// shutdown stops a running process by itself, with or without an agent: it
// reports terminating, applies the stop configuration, waits until TermLead
// before the grace period (counted from termAt) ends, then sends SIGTERM and,
// KillGrace later, SIGKILL to the process group.
func (s *Supervisor) shutdown(termAt time.Time) {
	s.mu.Lock()
	s.stopping = true
	running := s.pid != 0
	pid := s.pid
	exited := s.exited
	ptmx := s.ptmx
	stop := s.stop
	s.mu.Unlock()
	s.setReady(false)
	s.broadcast(&protocol.Message{Type: protocol.TypeTerminating})
	if !running {
		return
	}
	s.applyStop(pid, ptmx, stop)
	wait := time.Until(termAt.Add(s.o.GracePeriod - s.o.TermLead))
	select {
	case <-exited:
		return
	case <-time.After(wait):
	}
	s.log.Warn("process did not stop in time, terminating process group", "pid", pid)
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

// applyStop runs the stop configuration: the stop command on stdin, or the
// signal to the process group (Wings' mapping, unknown signals are SIGKILL).
// Without a configuration the process group gets SIGTERM.
func (s *Supervisor) applyStop(pid int, ptmx *os.File, stop *protocol.StopConfig) {
	if stop != nil && stop.Type == protocol.StopCommand && ptmx != nil {
		s.log.Info("stopping process with the stop command", "pid", pid)
		if _, err := ptmx.Write([]byte(stop.Value + "\n")); err == nil {
			return
		}
	}
	sig := unix.SIGTERM
	if stop != nil && stop.Type == protocol.StopSignal {
		sig = StopSignal(stop.Value)
	}
	s.log.Info("stopping process with a signal", "pid", pid, "signal", unix.SignalName(sig))
	_ = unix.Kill(-pid, sig)
}

// StopSignal maps a Wings stop signal value to a signal the way Wings does:
// SIGABRT, SIGINT (or "C") and SIGTERM are kept, anything else is SIGKILL.
func StopSignal(value string) unix.Signal {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "SIGABRT":
		return unix.SIGABRT
	case "SIGINT", "C":
		return unix.SIGINT
	case "SIGTERM":
		return unix.SIGTERM
	default:
		return unix.SIGKILL
	}
}

// setReady creates or removes the readiness file.
func (s *Supervisor) setReady(ready bool) {
	if s.o.ReadyFile == "" {
		return
	}
	if ready {
		if err := os.WriteFile(s.o.ReadyFile, nil, 0o600); err != nil {
			s.log.Warn("cannot write the readiness file", "path", s.o.ReadyFile, "error", err)
		}
		return
	}
	if err := os.Remove(s.o.ReadyFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Warn("cannot remove the readiness file", "path", s.o.ReadyFile, "error", err)
	}
}

// updateReady keeps the readiness file in line with the agent's state: present
// while the agent reports running, the process runs and the shim is not
// shutting down.
func (s *Supervisor) updateReady() {
	s.mu.Lock()
	ready := s.state == "running" && s.pid != 0 && !s.stopping
	s.mu.Unlock()
	s.setReady(ready)
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

// connectLoop dials the agent, runs the mutual handshake and serves the
// connection, reconnecting until ctx ends (agent restarts and replacements,
// rejected handshakes).
func (s *Supervisor) connectLoop(ctx context.Context) {
	backoff := 100 * time.Millisecond
	for ctx.Err() == nil {
		c, err := s.dial(ctx)
		if err == nil {
			enc := protocol.NewEncoder(c)
			var dec *protocol.Decoder
			dec, err = protocol.Answer(c, enc, s.o.Token, s.o.PodUID, protocol.HandshakeTimeout)
			if err == nil {
				s.log.Info("connected to agent", "agent", s.o.Agent)
				since := time.Now()
				s.serve(c, enc, dec)
				s.log.Info("agent connection ended")
				// A connection the agent drops at once (a rejected proof) keeps
				// backing off; one that was in use reconnects quickly.
				if time.Since(since) > 5*time.Second {
					backoff = 100 * time.Millisecond
				}
			} else {
				_ = c.Close()
			}
		}
		if err != nil {
			s.log.Debug("agent not reachable", "agent", s.o.Agent, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
}

// dial connects to the agent, over TLS when AgentCA is set.
func (s *Supervisor) dial(ctx context.Context) (net.Conn, error) {
	d := net.Dialer{Timeout: protocol.HandshakeTimeout}
	c, err := d.DialContext(ctx, "tcp", s.o.Agent)
	if err != nil || s.o.AgentCA == "" {
		return c, err
	}
	host, _, err := net.SplitHostPort(s.o.Agent)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	roots, err := pki.LoadPool(s.o.AgentCA)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	tc := tls.Client(c, &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: host})
	hctx, cancel := context.WithTimeout(ctx, protocol.HandshakeTimeout)
	defer cancel()
	if err := tc.HandshakeContext(hctx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("tls: %w", err)
	}
	return tc, nil
}

func (s *Supervisor) serve(c net.Conn, enc *protocol.Encoder, dec *protocol.Decoder) {
	// Events flow only after the agent subscribes, so a connection the agent
	// has not picked up yet never blocks the output relay.
	cn := &conn{enc: enc, c: c}
	s.mu.Lock()
	s.conns[cn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, cn)
		s.mu.Unlock()
		c.Close()
	}()
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
		if m.Stop != nil {
			s.setStop(m.Stop)
		}
		if err := s.start(m.Env); err != nil {
			return &protocol.Message{Error: err.Error(), Status: s.status()}
		}
		return &protocol.Message{OK: true, Status: s.status()}
	case protocol.TypeConfigure:
		if m.Stop == nil {
			return &protocol.Message{Error: "configure without a stop configuration"}
		}
		s.setStop(m.Stop)
		return &protocol.Message{OK: true}
	case protocol.TypeState:
		s.mu.Lock()
		s.state = m.Value
		s.mu.Unlock()
		s.updateReady()
		return &protocol.Message{OK: true}
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

func (s *Supervisor) setStop(c *protocol.StopConfig) {
	cp := *c
	s.mu.Lock()
	s.stop = &cp
	s.mu.Unlock()
}

func (s *Supervisor) status() *protocol.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &protocol.Status{Version: protocol.Version, Running: s.pid != 0, PID: s.pid, LastExit: s.lastExit, Stopping: s.stopping, PodUID: s.o.PodUID}
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
	// The shim token never reaches the game process.
	cmd.Env = WithoutKey(MergeEnv(os.Environ(), env), protocol.TokenEnv)
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
// to waitLoop; orphans are reaped and dropped here. childExit therefore never
// fills while no process runs (nothing drains it then), and never holds an
// entry that a later process reusing the pid could be mistaken for.
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

// killStragglers SIGKILLs everything left in the PID namespace. A process that
// calls setsid leaves the child's process group, so the group kill above misses
// it: Wine's wineserver does exactly that, and the game it hosts then survives
// a stop, holding its ports and the WINEPREFIX lock. The next start stacks a
// second server on top and neither makes progress.
//
// This is only safe as PID 1 of a PID namespace, where the namespace holds
// nothing but the shim and what the game started. The pod does not set
// shareProcessNamespace, so the agent and the other containers are not in it.
// Outside a namespace this would kill unrelated processes, so the check is on
// the real PID and not on a configuration flag.
func (s *Supervisor) killStragglers() {
	if os.Getpid() != 1 {
		return
	}
	for _, pid := range stragglerPIDs("/proc", 1) {
		if err := unix.Kill(pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
			s.log.Warn("could not kill straggler", "pid", pid, "error", err)
			continue
		}
		s.log.Info("killed straggler that escaped the process group", "pid", pid)
	}
}

// stragglerPIDs lists the processes in procDir other than self. The pids are
// read from the directory names, so a process that exits meanwhile simply
// yields ESRCH to the caller.
func stragglerPIDs(procDir string, self int) []int {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self || pid <= 0 {
			continue
		}
		out = append(out, pid)
	}
	return out
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
	s.killStragglers()
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
	s.updateReady()
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

// WithoutKey returns env without the entries for key.
func WithoutKey(env []string, key string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k != key {
			out = append(out, kv)
		}
	}
	return out
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
