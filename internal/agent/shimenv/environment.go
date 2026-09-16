// Package shimenv implements Wings' environment.ProcessEnvironment on top of
// the shim socket in the game container. See ARCHITECTURE.md section 6.4.
package shimenv

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/events"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/server"
	"github.com/pelican/wings/system"

	"github.com/Claiyc/pelican-k8s/internal/shim/protocol"
)

// Compile-time checks: the environment satisfies Wings' interfaces and hooks.
var (
	_ environment.ProcessEnvironment  = (*Environment)(nil)
	_ server.ImageAndStopConfigurable = (*Environment)(nil)
	_ server.Attachable               = (*Environment)(nil)
)

// ErrNotAttached mirrors the Docker environment's error.
var ErrNotAttached = errors.New("environment/shim: not attached to process")

// Options configure an Environment.
type Options struct {
	// SocketPath is the shim's unix socket.
	SocketPath string
	// RunLog is the per-run console log written by the agent (Readlog source).
	RunLog string
	// ExtraEnv is appended to every start (e.g. INTERNAL_IP=<pod ip>).
	ExtraEnv []string
	// DialTimeout bounds how long Attach waits for the shim socket.
	DialTimeout time.Duration
	Logger      *slog.Logger
}

// Environment drives one game process through the shim.
type Environment struct {
	id  string
	o   Options
	log *slog.Logger

	cfg     *environment.Configuration
	emitter *events.Bus
	st      *system.AtomicString

	metaMu sync.RWMutex
	image  string
	stop   remote.ProcessStopConfiguration

	logCallbackMx sync.Mutex
	logCallback   func([]byte)

	mu        sync.Mutex
	client    *protocol.Client
	connected chan struct{} // closed while a client is connected; replaced on disconnect
	attached  bool
	lastExit  *protocol.ExitState
	startedAt time.Time
	running   bool
	exitedCh  chan struct{} // closed when an exit is observed for the current run
	// disconnectedAt is when the shim connection was last lost while a process
	// was supposed to be alive (the game container restarted underneath us).
	disconnectedAt time.Time

	runLogMu sync.Mutex
	runLog   *os.File

	pipeW *io.PipeWriter

	ctx    context.Context
	cancel context.CancelFunc
}

// New creates an environment for the server with the given id and starts the
// background connection loop to the shim.
func New(id string, cfg *environment.Configuration, o Options) *Environment {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = 30 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Environment{
		id:        id,
		o:         o,
		log:       o.Logger.With("server", id, "environment", "shim"),
		cfg:       cfg,
		emitter:   events.NewBus(),
		st:        system.NewAtomicString(environment.ProcessOfflineState),
		connected: make(chan struct{}),
		exitedCh:  make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
	}
	close(e.exitedCh) // nothing running
	e.startOutputPipe()
	go e.connectLoop()
	return e
}

// Close stops the connection loop.
func (e *Environment) Close() {
	e.cancel()
}

// Type implements ProcessEnvironment.
func (e *Environment) Type() string { return "shim" }

// Config implements ProcessEnvironment.
func (e *Environment) Config() *environment.Configuration { return e.cfg }

// Events implements ProcessEnvironment.
func (e *Environment) Events() *events.Bus { return e.emitter }

// SetImage implements server.ImageAndStopConfigurable. The image is applied by
// the operator through a pod recreate; it is only recorded here.
func (e *Environment) SetImage(i string) {
	e.metaMu.Lock()
	e.image = i
	e.metaMu.Unlock()
}

// Image returns the recorded image.
func (e *Environment) Image() string {
	e.metaMu.RLock()
	defer e.metaMu.RUnlock()
	return e.image
}

// SetStopConfiguration implements server.ImageAndStopConfigurable.
func (e *Environment) SetStopConfiguration(c remote.ProcessStopConfiguration) {
	e.metaMu.Lock()
	e.stop = c
	e.metaMu.Unlock()
}

func (e *Environment) stopConfig() remote.ProcessStopConfiguration {
	e.metaMu.RLock()
	defer e.metaMu.RUnlock()
	return e.stop
}

// State implements ProcessEnvironment.
func (e *Environment) State() string { return e.st.Load() }

// SetState implements ProcessEnvironment with the same validation as Docker.
func (e *Environment) SetState(state string) {
	if state != environment.ProcessOfflineState &&
		state != environment.ProcessStartingState &&
		state != environment.ProcessRunningState &&
		state != environment.ProcessStoppingState {
		panic(fmt.Errorf("invalid server state received: %s", state))
	}
	if e.State() != state {
		e.st.Store(state)
		e.Events().Publish(environment.StateChangeEvent, state)
	}
}

// SetLogCallback implements ProcessEnvironment.
func (e *Environment) SetLogCallback(f func([]byte)) {
	e.logCallbackMx.Lock()
	e.logCallback = f
	e.logCallbackMx.Unlock()
}

// IsAttached implements server.Attachable.
func (e *Environment) IsAttached() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.attached && e.client != nil
}

// Exists implements ProcessEnvironment; the pod lifecycle belongs to the operator.
func (e *Environment) Exists() (bool, error) { return true, nil }

// Create implements ProcessEnvironment (no-op).
func (e *Environment) Create() error { return nil }

// InSituUpdate implements ProcessEnvironment; resources are applied by the operator.
func (e *Environment) InSituUpdate() error { return nil }

// OnBeforeStart implements ProcessEnvironment.
func (e *Environment) OnBeforeStart(ctx context.Context) error {
	_, err := e.waitClient(ctx)
	return err
}

// IsRunning implements ProcessEnvironment.
func (e *Environment) IsRunning(ctx context.Context) (bool, error) {
	c := e.currentClient()
	if c == nil {
		return false, nil
	}
	st, err := c.Status(ctx)
	if err != nil {
		return false, err
	}
	return st.Running, nil
}

// Uptime implements ProcessEnvironment.
func (e *Environment) Uptime(ctx context.Context) (int64, error) {
	c := e.currentClient()
	if c == nil {
		return 0, nil
	}
	st, err := c.Status(ctx)
	if err != nil {
		return 0, err
	}
	if !st.Running || st.StartedAt == nil {
		return 0, nil
	}
	return time.Since(*st.StartedAt).Milliseconds(), nil
}

// ExitState implements ProcessEnvironment.
func (e *Environment) ExitState() (uint32, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastExit == nil {
		return 1, false, nil
	}
	return uint32(e.lastExit.Code), e.lastExit.OOMKilled, nil
}

// InjectExit records an exit observed by the operator (e.g. an OOM kill of the
// whole game container) and moves the process to offline so that Wings' crash
// handling runs. It is ignored when the process has already been restarted
// since the container came back (the reconnect path may have handled the
// exit first), so a late relay never kills a fresh start.
func (e *Environment) InjectExit(code int, oomKilled bool) {
	e.mu.Lock()
	restartedSince := !e.disconnectedAt.IsZero() && e.startedAt.After(e.disconnectedAt)
	alreadyOffline := e.State() == environment.ProcessOfflineState
	if restartedSince {
		e.mu.Unlock()
		e.log.Info("ignoring injected exit state: process restarted since the container came back", "code", code, "oom", oomKilled)
		return
	}
	if alreadyOffline {
		// The reconnect path recorded a placeholder; keep the authoritative details.
		if e.lastExit != nil {
			e.lastExit.Code = code
			e.lastExit.OOMKilled = oomKilled
		} else {
			e.lastExit = &protocol.ExitState{Code: code, OOMKilled: oomKilled, At: time.Now()}
		}
		e.mu.Unlock()
		return
	}
	e.lastExit = &protocol.ExitState{Code: code, OOMKilled: oomKilled, At: time.Now()}
	e.running = false
	e.markExitedLocked()
	e.disconnectedAt = time.Time{}
	e.mu.Unlock()
	e.log.Info("exit state injected", "code", code, "oom", oomKilled)
	e.SetState(environment.ProcessOfflineState)
}

func (e *Environment) markExitedLocked() {
	select {
	case <-e.exitedCh:
	default:
		close(e.exitedCh)
	}
}

// Attach implements ProcessEnvironment: it waits for the shim connection and
// marks the environment as attached so that commands can be sent.
func (e *Environment) Attach(ctx context.Context) error {
	if _, err := e.waitClient(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	e.attached = true
	e.mu.Unlock()
	return nil
}

// Start implements ProcessEnvironment.
func (e *Environment) Start(ctx context.Context) error {
	sawError := false
	defer func() {
		if sawError {
			e.SetState(environment.ProcessStoppingState)
			e.SetState(environment.ProcessOfflineState)
		}
	}()

	c, err := e.waitClient(ctx)
	if err != nil {
		return err
	}
	st, err := c.Status(ctx)
	if err != nil {
		return fmt.Errorf("environment/shim: status: %w", err)
	}
	if st.Running {
		e.SetState(environment.ProcessRunningState)
		return e.Attach(ctx)
	}

	e.truncateRunLog()
	e.SetState(environment.ProcessStartingState)
	sawError = true

	if err := e.Attach(ctx); err != nil {
		return fmt.Errorf("environment/shim: attach: %w", err)
	}

	e.mu.Lock()
	e.exitedCh = make(chan struct{})
	e.lastExit = nil
	e.mu.Unlock()

	env := e.buildEnv()
	actx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := c.Start(actx, env); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "already running") {
			// Someone else started it, or the reply was lost: treat it as running.
			if st, serr := c.Status(ctx); serr == nil && st.Running {
				sawError = false
				return nil
			}
		}
		return fmt.Errorf("environment/shim: start: %w", err)
	}
	e.mu.Lock()
	e.startedAt = time.Now()
	e.running = true
	e.mu.Unlock()
	sawError = false
	return nil
}

// buildEnv assembles the process environment the way Wings does for Docker,
// including the 127.0.0.1 rewrite to the configured interface.
func (e *Environment) buildEnv() []string {
	a := e.cfg.Allocations()
	evs := append([]string(nil), e.cfg.EnvironmentVariables()...)
	iface := config.Get().Docker.Network.Interface
	if a.DefaultMapping != nil && a.DefaultMapping.Port != 0 && iface != "" {
		for i, v := range evs {
			if v == "SERVER_IP=127.0.0.1" {
				evs[i] = "SERVER_IP=" + iface
			}
		}
	}
	return append(evs, e.o.ExtraEnv...)
}

// Stop implements ProcessEnvironment.
func (e *Environment) Stop(ctx context.Context) error {
	s := e.stopConfig()
	if s.Type == "" || s.Type == remote.ProcessStopSignal {
		var signal string
		switch strings.ToUpper(s.Value) {
		case "SIGABRT":
			signal = "SIGABRT"
		case "SIGINT", "C":
			signal = "SIGINT"
		case "SIGTERM":
			signal = "SIGTERM"
		default:
			signal = "SIGKILL"
		}
		return e.Terminate(ctx, signal)
	}
	if e.State() != environment.ProcessOfflineState {
		e.SetState(environment.ProcessStoppingState)
	}
	if e.IsAttached() && s.Type == remote.ProcessStopCommand {
		return e.SendCommand(s.Value)
	}
	// Not attached: the equivalent of "docker stop" is a SIGTERM to the process.
	c := e.currentClient()
	if c == nil {
		e.SetState(environment.ProcessOfflineState)
		return nil
	}
	if err := c.Signal(ctx, "SIGTERM"); err != nil {
		if strings.Contains(err.Error(), "not running") {
			e.SetState(environment.ProcessOfflineState)
			return nil
		}
		return fmt.Errorf("environment/shim: cannot stop process: %w", err)
	}
	return nil
}

// WaitForStop implements ProcessEnvironment.
func (e *Environment) WaitForStop(ctx context.Context, duration time.Duration, terminate bool) error {
	tctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-tctx.Done():
		}
	}()

	doTermination := func(step string) error {
		e.log.Warn("process stop did not complete in time, terminating", "step", step, "duration", duration)
		return e.Terminate(ctx, "SIGKILL")
	}

	if err := e.Stop(tctx); err != nil {
		if terminate && errors.Is(err, context.DeadlineExceeded) {
			return doTermination("stop")
		}
		return err
	}

	e.mu.Lock()
	exited := e.exitedCh
	e.mu.Unlock()

	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if terminate {
				return doTermination("parent-context")
			}
			return ctx.Err()
		case <-tctx.Done():
			if terminate {
				return doTermination("wait")
			}
			return errors.New("environment/shim: timed out waiting for process to stop")
		case <-exited:
			return nil
		case <-t.C:
			if running, err := e.IsRunning(tctx); err == nil && !running {
				return nil
			}
		}
	}
}

// Terminate implements ProcessEnvironment.
func (e *Environment) Terminate(ctx context.Context, signal string) error {
	c := e.currentClient()
	if c == nil {
		if e.State() != environment.ProcessOfflineState {
			e.SetState(environment.ProcessStoppingState)
			e.SetState(environment.ProcessOfflineState)
		}
		return nil
	}
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	if !st.Running {
		if e.State() != environment.ProcessOfflineState {
			e.SetState(environment.ProcessStoppingState)
			e.SetState(environment.ProcessOfflineState)
		}
		return nil
	}
	e.SetState(environment.ProcessStoppingState)
	if err := c.Signal(ctx, signal); err != nil && !strings.Contains(err.Error(), "not running") {
		return err
	}
	e.mu.Lock()
	exited := e.exitedCh
	e.mu.Unlock()
	select {
	case <-exited:
		e.SetState(environment.ProcessOfflineState)
		return nil
	case <-time.After(10 * time.Second):
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := c.Kill(ctx); err != nil && !strings.Contains(err.Error(), "not running") {
		return err
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		e.log.Error("process still alive after SIGKILL")
	}
	e.SetState(environment.ProcessOfflineState)
	return nil
}

// Destroy implements ProcessEnvironment.
func (e *Environment) Destroy() error {
	e.SetState(environment.ProcessStoppingState)
	if c := e.currentClient(); c != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = c.Kill(ctx)
	}
	e.SetState(environment.ProcessOfflineState)
	return nil
}

// SendCommand implements ProcessEnvironment.
func (e *Environment) SendCommand(cmd string) error {
	if !e.IsAttached() {
		return fmt.Errorf("environment/shim: cannot send command: %w", ErrNotAttached)
	}
	s := e.stopConfig()
	if s.Type == remote.ProcessStopCommand && cmd == s.Value {
		e.SetState(environment.ProcessStoppingState)
	}
	c := e.currentClient()
	if c == nil {
		return ErrNotAttached
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Stdin(ctx, []byte(cmd+"\n")); err != nil {
		return fmt.Errorf("environment/shim: could not write to process: %w", err)
	}
	return nil
}

// Readlog implements ProcessEnvironment: the tail of the agent-written run log.
func (e *Environment) Readlog(lines int) ([]string, error) {
	return TailLines(e.o.RunLog, lines)
}

// TailLines returns the last n lines of a file.
func TailLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []string{}, nil
		}
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	const window = 512 * 1024
	var offset int64
	if st.Size() > window {
		offset = st.Size() - window
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	first := true
	for sc.Scan() {
		if first && offset > 0 {
			first = false // drop the partial first line
			continue
		}
		first = false
		out = append(out, sc.Text())
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// --- connection management ---

func (e *Environment) currentClient() *protocol.Client {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.client
}

// waitClient blocks until a shim connection exists.
func (e *Environment) waitClient(ctx context.Context) (*protocol.Client, error) {
	deadline, cancel := context.WithTimeout(ctx, e.o.DialTimeout)
	defer cancel()
	for {
		e.mu.Lock()
		c, ch := e.client, e.connected
		e.mu.Unlock()
		if c != nil {
			return c, nil
		}
		select {
		case <-ch:
		case <-deadline.Done():
			return nil, fmt.Errorf("environment/shim: shim socket %s is not reachable: %w", e.o.SocketPath, deadline.Err())
		}
	}
}

func (e *Environment) connectLoop() {
	backoff := 250 * time.Millisecond
	for {
		if e.ctx.Err() != nil {
			return
		}
		c, err := protocol.Dial(e.ctx, e.o.SocketPath)
		if err != nil {
			select {
			case <-e.ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 5*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 250 * time.Millisecond
		e.onConnected(c)
		e.consume(c)
		e.onDisconnected(c)
	}
}

func (e *Environment) onConnected(c *protocol.Client) {
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	st, err := c.Subscribe(ctx, false)
	if err != nil {
		e.log.Warn("subscribe to shim failed", "error", err)
		c.Close()
		return
	}
	e.log.Info("connected to shim", "running", st.Running, "pid", st.PID)

	e.mu.Lock()
	e.client = c
	close(e.connected)
	e.running = st.Running
	if st.Running && st.StartedAt != nil {
		e.startedAt = *st.StartedAt
	}
	state := e.State()
	e.mu.Unlock()

	switch {
	case st.Running && state == environment.ProcessOfflineState:
		// Agent restart while the game keeps running: re-attach like Wings does after a reboot.
		e.mu.Lock()
		e.attached = true
		e.exitedCh = make(chan struct{})
		e.mu.Unlock()
		if empty, _ := e.runLogEmpty(); empty {
			_, _ = c.Subscribe(ctx, true) // replay the ring buffer into the run log
		}
		e.SetState(environment.ProcessRunningState)
	case !st.Running && (state == environment.ProcessStartingState || state == environment.ProcessRunningState):
		// The game container restarted underneath us (e.g. OOM kill). The operator
		// injects the authoritative exit state; record a placeholder so crash
		// handling runs even if that never arrives.
		e.mu.Lock()
		if st.LastExit != nil {
			e.lastExit = st.LastExit
		} else {
			e.lastExit = &protocol.ExitState{Code: 137, At: time.Now()}
		}
		e.running = false
		e.markExitedLocked()
		e.mu.Unlock()
		e.SetState(environment.ProcessOfflineState)
	}
}

func (e *Environment) onDisconnected(c *protocol.Client) {
	e.mu.Lock()
	if e.client == c {
		e.client = nil
		e.connected = make(chan struct{})
	}
	if e.State() != environment.ProcessOfflineState {
		e.disconnectedAt = time.Now()
	}
	e.mu.Unlock()
	e.log.Warn("shim connection lost", "error", c.Err())
}

func (e *Environment) consume(c *protocol.Client) {
	for ev := range c.Events {
		switch ev.Type {
		case protocol.TypeOutput:
			e.writeOutput(ev.Data)
		case protocol.TypeStarted:
			e.mu.Lock()
			e.running = true
			e.startedAt = time.Now()
			e.mu.Unlock()
		case protocol.TypeExited:
			e.mu.Lock()
			e.running = false
			e.lastExit = ev.Exit
			e.markExitedLocked()
			e.mu.Unlock()
			e.SetState(environment.ProcessOfflineState)
		case protocol.TypeStats:
			if ev.Stats != nil && e.State() != environment.ProcessOfflineState {
				e.emitter.Publish(environment.ResourceEvent, environment.Stats{
					Memory:      ev.Stats.MemoryBytes,
					MemoryLimit: ev.Stats.MemoryLimitBytes,
					CpuAbsolute: ev.Stats.CPUPercent,
					Network:     environment.NetworkStats{RxBytes: ev.Stats.NetworkRx, TxBytes: ev.Stats.NetworkTx},
					DiskIo:      environment.DiskIoStats{ReadBytes: ev.Stats.DiskReadBytes, WriteBytes: ev.Stats.DiskWrite},
					Uptime:      ev.Stats.UptimeMillis,
				})
			}
		}
	}
}

// --- output handling ---

// startOutputPipe feeds raw PTY bytes through Wings' ScanReader so that lines
// and carriage returns are handled exactly like Docker attach output.
func (e *Environment) startOutputPipe() {
	pr, pw := io.Pipe()
	e.pipeW = pw
	go func() {
		_ = system.ScanReader(pr, func(line []byte) {
			e.appendRunLog(line)
			e.logCallbackMx.Lock()
			cb := e.logCallback
			e.logCallbackMx.Unlock()
			if cb != nil {
				cb(line)
			}
		})
	}()
}

func (e *Environment) writeOutput(b []byte) {
	_, _ = e.pipeW.Write(b)
}

func (e *Environment) appendRunLog(line []byte) {
	e.runLogMu.Lock()
	defer e.runLogMu.Unlock()
	if e.runLog == nil {
		if err := os.MkdirAll(filepath.Dir(e.o.RunLog), 0o700); err != nil {
			return
		}
		f, err := os.OpenFile(e.o.RunLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return
		}
		e.runLog = f
	}
	_, _ = e.runLog.Write(append(append([]byte(nil), line...), '\n'))
}

func (e *Environment) truncateRunLog() {
	e.runLogMu.Lock()
	defer e.runLogMu.Unlock()
	if e.runLog != nil {
		e.runLog.Close()
		e.runLog = nil
	}
	_ = os.MkdirAll(filepath.Dir(e.o.RunLog), 0o700)
	_ = os.WriteFile(e.o.RunLog, nil, 0o600)
}

func (e *Environment) runLogEmpty() (bool, error) {
	st, err := os.Stat(e.o.RunLog)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	return st.Size() == 0, nil
}
