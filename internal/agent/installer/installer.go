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
	// PollInterval is how often the gateway is asked whether the Job failed.
	PollInterval time.Duration
	Logger       *slog.Logger
}

// ErrInstallFailed marks a failed installation.
var ErrInstallFailed = errors.New("installer: installation failed")

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

	exitCode, failedByOperator, err := i.waitForCompletion(ctx, s.ID(), gen, exitFile)
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
	case failedByOperator:
		s.Events().Publish(server.DaemonMessageEvent, "Installation job failed.")
		return fmt.Errorf("%w: job failed or timed out", ErrInstallFailed)
	case prep.StrictExitCode && exitCode != 0:
		s.Events().Publish(server.DaemonMessageEvent, fmt.Sprintf("Installation script exited with code %d.", exitCode))
		return fmt.Errorf("%w: script exit code %d", ErrInstallFailed, exitCode)
	}
	s.Events().Publish(server.DaemonMessageEvent, "Installation process completed.")
	return nil
}

// waitForCompletion returns when exit-code appears or the gateway reports the
// Job as failed. The returned bool is true when the operator failed the install.
func (i *Installer) waitForCompletion(ctx context.Context, uuid string, gen int64, exitFile string) (int, bool, error) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	poll := time.NewTicker(i.PollInterval)
	defer poll.Stop()
	for {
		if b, err := os.ReadFile(exitFile); err == nil {
			code, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				return 0, false, fmt.Errorf("installer: parse exit code %q: %w", b, err)
			}
			return code, false, nil
		}
		select {
		case <-ctx.Done():
			return 0, false, ctx.Err()
		case <-t.C:
		case <-poll.C:
			st, err := i.Gateway.GetInstallState(ctx, uuid, gen)
			if err != nil {
				i.logger().Warn("install state poll failed", "error", err)
				continue
			}
			if st.Result == "Failed" {
				return 0, true, nil
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
			f, err = os.Open(path)
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
func (i *Installer) writeInstallLog(s *server.Server, script *remote.InstallationScript, outputLog string) error {
	logDir := i.LogDir
	if logDir == "" {
		logDir = config.Get().System.LogDirectory
	}
	p := filepath.Join(logDir, "install", s.ID()+".log")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	kernel := "unknown"
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		kernel = strings.TrimSpace(string(b))
	}
	if err := logHeader.Execute(f, map[string]any{"UUID": s.ID(), "Image": script.ContainerImage, "Entrypoint": script.Entrypoint, "Kernel": kernel, "Env": s.GetEnvironmentVariables()}); err != nil {
		return err
	}
	in, err := os.Open(outputLog)
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
