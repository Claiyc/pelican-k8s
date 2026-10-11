// Package installer implements Wings' server.Installer hook with a Kubernetes
// Job run by the operator. See ARCHITECTURE.md section 8.2.
package installer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/server"
	"github.com/pelican/wings/system"

	"github.com/Claiyc/pelican-k8s/internal/agent/gatewayclient"
)

// Gateway is the subset of the gateway client used by the installer.
type Gateway interface {
	InstallPrepared(ctx context.Context, uuid string) (*gatewayclient.PreparedResponse, error)
	GetInstallState(ctx context.Context, uuid string, generation int64) (*gatewayclient.InstallState, error)
}

// Installer waits for the install Job's output on the PVC.
type Installer struct {
	Gateway Gateway
	// Root is the Wings root directory (PVC mount); install/<gen>/ lives below it.
	Root string
	// LogDir is Wings' log directory; install/<uuid>.log is written there.
	LogDir string
	// PollInterval is how often the gateway is asked whether the Job failed or
	// the generation was superseded.
	PollInterval time.Duration
	Logger       *slog.Logger

	// superseded holds the servers whose last run ended because a newer
	// generation replaced it; Client drops that run's result.
	superseded sync.Map
}

// ErrInstallFailed marks a failed installation.
var ErrInstallFailed = errors.New("installer: installation failed")

// ErrInstallSuperseded marks an installation a newer generation replaced. The
// operator has deleted its Job; the newer generation reports the result.
var ErrInstallSuperseded = errors.New("installer: installation superseded by a newer one")

// Client wraps the client Wings reports through so that the result of a
// superseded run never reaches the gateway: it would be recorded against the
// newer generation and shown in the Panel as a failed install. Wings reports
// once after every Run (Server.install calls SyncInstallState right after the
// installer), so the mark Run leaves is always consumed by that report. A new
// Run must not clear it: Wings releases the install lock before it reports, so
// the next install can start before the old report has been dropped.
func (i *Installer) Client(c remote.Client) remote.Client {
	return reportFilter{Client: c, i: i}
}

type reportFilter struct {
	remote.Client
	i *Installer
}

// ReportPending reports whether a superseded run of the server has not
// reported yet. Until it has, a new install must not start: its own report
// could otherwise come first and be dropped in place of the old one.
func (i *Installer) ReportPending(uuid string) bool {
	_, ok := i.superseded.Load(uuid)
	return ok
}

func (f reportFilter) SetInstallationStatus(ctx context.Context, uuid string, data remote.InstallStatusRequest) error {
	if _, ok := f.i.superseded.LoadAndDelete(uuid); ok {
		return nil
	}
	return f.Client.SetInstallationStatus(ctx, uuid, data)
}

// Run implements server.Installer. It is called with the install lock held.
func (i *Installer) Run(s *server.Server, script *remote.InstallationScript) error {
	log := i.logger().With("server", s.ID())
	ctx := s.Context()
	if i.PollInterval <= 0 {
		i.PollInterval = 5 * time.Second
	}

	s.PublishConsoleOutputFromDaemon("Preparing installation, waiting for the install job to be scheduled...")
	prep, err := i.Gateway.InstallPrepared(ctx, s.ID())
	if err != nil {
		return fmt.Errorf("installer: report prepared: %w", err)
	}
	gen := prep.Generation
	dir := filepath.Join(i.Root, "install", strconv.FormatInt(gen, 10))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	outputLog := filepath.Join(dir, "output.log")
	exitFile := filepath.Join(dir, "exit-code")
	log.Info("waiting for install job", "generation", gen, "output", outputLog)

	tailCtx, stopTail := context.WithCancel(ctx)
	defer stopTail()
	go i.tail(tailCtx, outputLog, s.Sink(system.InstallSink))

	exitCode, result, err := i.waitForCompletion(ctx, s.ID(), gen, exitFile)
	if err != nil {
		return err
	}
	// Let the tail catch up with the last lines before writing the permanent log.
	time.Sleep(500 * time.Millisecond)
	stopTail()

	if err := i.writeInstallLog(s, script, outputLog); err != nil {
		log.Warn("failed to write installation log", "error", err)
	}

	switch {
	case result == "Superseded":
		log.Info("install generation superseded", "generation", gen)
		s.Events().Publish(server.DaemonMessageEvent, "Installation replaced by a newer one.")
		i.superseded.Store(s.ID(), struct{}{})
		return ErrInstallSuperseded
	case result != "":
		s.Events().Publish(server.DaemonMessageEvent, "Installation job failed.")
		return fmt.Errorf("%w: job failed or timed out", ErrInstallFailed)
	case prep.StrictExitCode && exitCode != 0:
		s.Events().Publish(server.DaemonMessageEvent, fmt.Sprintf("Installation script exited with code %d.", exitCode))
		return fmt.Errorf("%w: script exit code %d", ErrInstallFailed, exitCode)
	}
	s.Events().Publish(server.DaemonMessageEvent, "Installation process completed.")
	return nil
}

// waitForCompletion returns when exit-code appears, or when the gateway reports
// the Job as failed or the generation as superseded: then the returned result
// is that report, otherwise it is empty. An exit code of a superseded
// generation or a failed Job counts as that report.
func (i *Installer) waitForCompletion(ctx context.Context, uuid string, gen int64, exitFile string) (int, string, error) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	poll := time.NewTicker(i.PollInterval)
	defer poll.Stop()
	checkFailed := false
	for {
		if b, err := os.ReadFile(exitFile); err == nil { //nolint:gosec // <agent root>/install/<generation>/exit-code, no Panel or user text in the path
			code, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				return 0, "", fmt.Errorf("installer: parse exit code %q: %w", b, err)
			}
			// The shim writes an exit code also when its Job is deleted or
			// times out: the script was killed. The code is the result only
			// once the gateway confirms that this generation is current and
			// its Job did not fail; until the gateway answers, the file is
			// read again on the next tick.
			st, err := i.Gateway.GetInstallState(ctx, uuid, gen)
			switch {
			case err != nil:
				if !checkFailed {
					i.logger().Warn("install state check failed, retrying", "error", err)
				}
				checkFailed = true
			case st.Result == "Failed" || st.Result == "Superseded":
				return code, st.Result, nil
			default:
				return code, "", nil
			}
		}
		select {
		case <-ctx.Done():
			return 0, "", ctx.Err()
		case <-t.C:
		case <-poll.C:
			st, err := i.Gateway.GetInstallState(ctx, uuid, gen)
			if err != nil {
				i.logger().Warn("install state poll failed", "error", err)
				continue
			}
			if st.Result == "Failed" || st.Result == "Superseded" {
				return 0, st.Result, nil
			}
		}
	}
}

// tail follows the output log and pushes every line to the install sink.
func (i *Installer) tail(ctx context.Context, path string, sink *system.SinkPool) {
	var f *os.File
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	var r *bufio.Reader
	partial := []byte{}
	for {
		if f == nil {
			var err error
			f, err = os.Open(path) //nolint:gosec // output.log in <agent root>/install/<generation>, no Panel or user text in the path
			if err != nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(200 * time.Millisecond):
					continue
				}
			}
			r = bufio.NewReader(f)
		}
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			partial = append(partial, line...)
		}
		if err == nil {
			out := strings.TrimRight(string(partial), "\r\n")
			out = strings.ReplaceAll(out, "\r", "")
			sink.Push([]byte(out))
			partial = partial[:0]
			continue
		}
		if !errors.Is(err, io.EOF) {
			return
		}
		select {
		case <-ctx.Done():
			if len(partial) > 0 {
				sink.Push([]byte(strings.TrimRight(string(partial), "\r\n")))
			}
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

var logHeader = template.Must(template.New("header").Parse(`Pelican Server Installation Log

|
| Details
| ------------------------------
  Server UUID:          {{.UUID}}
  Container Image:      {{.Image}}
  Container Entrypoint: {{.Entrypoint}}
  Kernel Version:       {{.Kernel}}

|
| Environment Variables
| ------------------------------
{{ range .Env }}  {{ . }}
{{ end }}

|
| Script Output
| ------------------------------
`))

// writeInstallLog writes <log_dir>/install/<uuid>.log in Wings' format so that
// GET /api/servers/:s/install-logs keeps working.
func (i *Installer) writeInstallLog(s *server.Server, script *remote.InstallationScript, outputLog string) (err error) {
	logDir := i.LogDir
	if logDir == "" {
		logDir = config.Get().System.LogDirectory
	}
	p := filepath.Join(logDir, "install", s.ID()+".log")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // <log_directory>/install/<server uuid>.log from the agent's own config
	if err != nil {
		return err
	}
	// The log is written through on Close (NFS-backed volumes report write
	// errors there), so a failed Close is a failed write.
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	kernel := "unknown"
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		kernel = strings.TrimSpace(string(b))
	}
	if err := logHeader.Execute(f, map[string]any{"UUID": s.ID(), "Image": script.ContainerImage, "Entrypoint": script.Entrypoint, "Kernel": kernel, "Env": s.GetEnvironmentVariables()}); err != nil {
		return err
	}
	in, err := os.Open(outputLog) //nolint:gosec // <agent root>/install/<generation>/output.log, no Panel or user text in the path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer in.Close()
	_, err = io.Copy(f, in)
	return err
}

func (i *Installer) logger() *slog.Logger {
	if i.Logger == nil {
		return slog.Default()
	}
	return i.Logger
}
