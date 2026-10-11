package prepare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// InstallRun executes an egg install script the way Wings' installer container
// does: in a PTY, with the Job's environment, streaming output to a log file
// that the agent tails. When the script ends the exit code is written to
// ExitFile and the server directory is chowned to the game UID. A script that
// was ended because the Job's pod is terminated (Job deleted, timed out or
// evicted) leaves no exit code: Run returns ErrTerminated, the pod fails, and
// the agent takes the result from the gateway instead.
type InstallRun struct {
	Argv      []string
	LogFile   string
	ExitFile  string
	ChownPath string
	UID, GID  int
	Dir       string
	Stdout    io.Writer
}

// ErrTerminated is returned when the pod's termination ended the script.
var ErrTerminated = errors.New("install-run: terminated before the script finished")

// Run executes the script and returns its exit code. An error is returned when
// the script could not be run at all, its exit code could not be recorded, or
// the pod's termination ended it (ErrTerminated).
func (r InstallRun) Run(ctx context.Context) (int, error) {
	if len(r.Argv) == 0 {
		return 0, errors.New("install-run: no command")
	}
	if r.Stdout == nil {
		r.Stdout = os.Stdout
	}
	if err := os.MkdirAll(filepath.Dir(r.LogFile), 0o755); err != nil {
		return 0, err
	}
	logf, err := os.OpenFile(r.LogFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	_ = os.Remove(r.ExitFile)

	cmd := exec.Command(r.Argv[0], r.Argv[1:]...)
	cmd.Dir = r.Dir
	cmd.Env = os.Environ()
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return 0, fmt.Errorf("install-run: spawn %v: %w", r.Argv, err)
	}
	defer ptmx.Close()

	// Forward termination (Job deletion, activeDeadlineSeconds) to the script.
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)
	var terminated atomic.Bool
	go func() {
		select {
		case <-sigs:
		case <-ctx.Done():
			return
		}
		terminated.Store(true)
		_ = unix.Kill(-cmd.Process.Pid, unix.SIGTERM)
		time.Sleep(10 * time.Second)
		_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
	}()

	copyDone := make(chan struct{})
	go func() {
		defer close(copyDone)
		w := io.MultiWriter(logf, r.Stdout)
		_, _ = io.Copy(w, ptmx) // ends with EIO when the slave closes
	}()

	code := 0
	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			ws := ee.Sys().(syscall.WaitStatus)
			if ws.Signaled() {
				code = 128 + int(ws.Signal())
			} else {
				code = ws.ExitStatus()
			}
		} else {
			return 0, err
		}
	}
	<-copyDone

	if r.ChownPath != "" {
		if err := chownRecursive(r.ChownPath, r.UID, r.GID); err != nil {
			fmt.Fprintf(r.Stdout, "install-run: warning: chown %s: %v\n", r.ChownPath, err)
		}
	}
	// The killed script's code is no install result: the operator records
	// the Job as failed, or the generation was superseded.
	if terminated.Load() {
		return code, ErrTerminated
	}
	if err := writeAtomic(r.ExitFile, []byte(fmt.Sprintf("%d\n", code))); err != nil {
		return code, err
	}
	return code, nil
}

// chownRecursive hands root and everything below it to uid:gid. The walk is
// scoped to root, so an install script that swaps a directory for a symlink
// cannot send the chown outside it.
func chownRecursive(dir string, uid, gid int) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best effort: skip unreadable entries
		}
		_ = root.Lchown(path, uid, gid)
		return nil
	})
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
