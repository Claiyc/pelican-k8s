package prepare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// InstallRun executes an egg install script the way Wings' installer container
// does: in a PTY, with the Job's environment, streaming output to a log file
// that the agent tails. When the script ends the exit code is written to
// ExitFile and the server directory is chowned to the game UID.
type InstallRun struct {
	Argv      []string
	LogFile   string
	ExitFile  string
	ChownPath string
	UID, GID  int
	Dir       string
	Stdout    io.Writer
}

// Run executes the script and returns its exit code. An error is returned only
// when the script could not be run at all.
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
	go func() {
		select {
		case <-sigs:
		case <-ctx.Done():
			return
		}
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
	if err := writeAtomic(r.ExitFile, []byte(fmt.Sprintf("%d\n", code))); err != nil {
		return code, err
	}
	return code, nil
}

func chownRecursive(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // best effort: skip unreadable entries
		}
		_ = os.Lchown(path, uid, gid)
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
