package prepare

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// file creates a regular file at dir/name; paths below it cannot be created.
func file(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLayoutKeepsExistingMachineID(t *testing.T) {
	dir := t.TempDir()
	l := Layout{Data: dir, UUID: "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"}
	if err := os.WriteFile(filepath.Join(dir, "machine-id"), []byte("keepme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.Run(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "machine-id")); string(b) != "keepme\n" {
		t.Fatalf("machine-id was rewritten: %q", b)
	}
	// Without Shared and Bin nothing outside Data is touched.
	if _, err := os.Stat(filepath.Join(dir, "run")); err == nil {
		t.Fatal("shared directories created without Shared")
	}
}

func TestLayoutErrors(t *testing.T) {
	uuid := "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	dir := t.TempDir()
	blocker := file(t, dir, "blocker")

	for name, l := range map[string]Layout{
		"shim binary":      {Bin: filepath.Join(blocker, "bin", "shim"), Data: dir, UUID: uuid},
		"shared directory": {Shared: filepath.Join(blocker, "shared"), Data: dir, UUID: uuid},
		"data directory":   {Data: filepath.Join(blocker, "data"), UUID: uuid},
	} {
		if err := l.Run(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}

	if os.Getuid() != 0 {
		ro := t.TempDir()
		if err := os.Chmod(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(ro, 0o700)
		if err := (Layout{Data: filepath.Join(ro, "d"), UUID: uuid}).Run(); err == nil {
			t.Error("a read-only data root must fail")
		}
		// Directories exist but machine-id cannot be written.
		rw := t.TempDir()
		if err := (Layout{Data: rw, UUID: uuid}).Run(); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(rw, "machine-id")); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(rw, 0o500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(rw, 0o700)
		err := (Layout{Data: rw, UUID: uuid}).Run()
		if err == nil || !strings.Contains(err.Error(), "machine-id") {
			t.Errorf("unwritable machine-id: %v", err)
		}
	}
}

func TestProbeWithoutImagePasswd(t *testing.T) {
	root, out := t.TempDir(), t.TempDir()
	p := Probe{Root: root, Argv: []string{"/x"}, ArgvOut: filepath.Join(out, "argv"), PasswdOut: filepath.Join(out, "etc", "passwd"), GroupOut: filepath.Join(out, "etc", "group"), Name: "container", Home: "/home/container", UID: 1000, GID: 1001}
	if err := p.Run(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p.PasswdOut); string(b) != "container:x:1000:1001::/home/container:/bin/sh\n" {
		t.Fatalf("passwd %q", b)
	}
	if b, _ := os.ReadFile(p.GroupOut); string(b) != "container:x:1001:\n" {
		t.Fatalf("group %q", b)
	}
}

func TestProbeFiltersConflictingAndMalformedEntries(t *testing.T) {
	root, out := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	passwd := "root:x:0:0::/root:/bin/sh\n\ngarbage\nnotnumeric:x:abc:0::/:/bin/sh\nbyname:x:5:5::/:/bin/sh\ncontainer:x:77:77::/:/bin/sh\nbyid:x:1000:1000::/:/bin/sh\n"
	if err := os.WriteFile(filepath.Join(root, "etc", "passwd"), []byte(passwd), 0o644); err != nil {
		t.Fatal(err)
	}
	p := Probe{Root: root, Argv: []string{"/x"}, ArgvOut: filepath.Join(out, "argv"), PasswdOut: filepath.Join(out, "passwd"), Name: "container", Home: "/h", UID: 1000, GID: 1000}
	if err := p.Run(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p.PasswdOut)
	want := "root:x:0:0::/root:/bin/sh\nnotnumeric:x:abc:0::/:/bin/sh\nbyname:x:5:5::/:/bin/sh\ncontainer:x:1000:1000::/h:/bin/sh\n"
	if string(b) != want {
		t.Fatalf("passwd:\n%s\nwant:\n%s", b, want)
	}
}

func TestProbeErrors(t *testing.T) {
	dir := t.TempDir()
	blocker := file(t, dir, "blocker")
	base := Probe{Root: t.TempDir(), Argv: []string{"/x"}, Name: "c", Home: "/h", UID: 1, GID: 1}

	p := base
	p.ArgvOut = filepath.Join(blocker, "argv")
	if err := p.Run(); err == nil {
		t.Error("argv output below a file")
	}
	p = base
	p.ArgvOut = filepath.Join(dir, "argv")
	p.PasswdOut = filepath.Join(blocker, "passwd")
	if err := p.Run(); err == nil {
		t.Error("passwd output below a file")
	}
	p = base
	p.ArgvOut = filepath.Join(dir, "argv")
	p.GroupOut = filepath.Join(blocker, "group")
	if err := p.Run(); err == nil {
		t.Error("group output below a file")
	}
	p = base
	p.Argv = nil
	p.ArgvOut = filepath.Join(dir, "argv")
	if err := p.Run(); !errors.Is(err, ErrNoEntrypoint) {
		t.Errorf("err = %v", err)
	}
}

func TestInstallRunErrors(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	blocker := file(t, dir, "blocker")
	ok := func() InstallRun {
		return InstallRun{Argv: []string{"/bin/sh", "-c", "exit 0"}, LogFile: filepath.Join(dir, "out", "log"), ExitFile: filepath.Join(dir, "out", "exit"), Stdout: os.Stderr}
	}

	r := ok()
	r.Argv = nil
	if _, err := r.Run(ctx); err == nil || !strings.Contains(err.Error(), "no command") {
		t.Errorf("no command: %v", err)
	}

	r = ok()
	r.LogFile = filepath.Join(blocker, "log")
	if _, err := r.Run(ctx); err == nil {
		t.Error("log below a file")
	}

	r = ok()
	r.Argv = []string{filepath.Join(dir, "does-not-exist")}
	if _, err := r.Run(ctx); err == nil || !strings.Contains(err.Error(), "spawn") {
		t.Errorf("missing binary: %v", err)
	}

	// The script ran, but the exit code cannot be recorded: the code is still returned.
	r = ok()
	r.Argv = []string{"/bin/sh", "-c", "exit 3"}
	r.ExitFile = filepath.Join(blocker, "exit")
	code, err := r.Run(ctx)
	if err == nil || code != 3 {
		t.Errorf("unwritable exit file: code %d err %v", code, err)
	}
}

func TestInstallRunReportsSignalDeathAndClearsOldExitCode(t *testing.T) {
	dir := t.TempDir()
	exit := filepath.Join(dir, "exit")
	if err := os.WriteFile(exit, []byte("99\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := InstallRun{Argv: []string{"/bin/sh", "-c", "kill -KILL $$"}, LogFile: filepath.Join(dir, "log"), ExitFile: exit, Stdout: os.Stderr}
	code, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if code != 128+int(syscall.SIGKILL) {
		t.Fatalf("code %d", code)
	}
	if b, _ := os.ReadFile(exit); strings.TrimSpace(string(b)) != "137" {
		t.Fatalf("exit file %q", b)
	}
}

func TestInstallRunSuccessWritesZeroAndChownsMissingPathQuietly(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	r := InstallRun{
		Argv: []string{"/bin/sh", "-c", "echo hello"}, LogFile: filepath.Join(dir, "log"), ExitFile: filepath.Join(dir, "exit"),
		ChownPath: filepath.Join(dir, "missing"), UID: os.Getuid(), GID: os.Getgid(), Stdout: &out,
	}
	code, err := r.Run(context.Background())
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if !strings.Contains(out.String(), "hello") {
		t.Fatalf("stdout %q", out.String())
	}
	if strings.Contains(out.String(), "warning") {
		t.Fatalf("chown of a missing path is best effort and silent: %q", out.String())
	}
	if b, _ := os.ReadFile(r.ExitFile); string(b) != "0\n" {
		t.Fatalf("exit file %q", b)
	}
}

// The Job's termination signal reaches the script's process group.
func TestInstallRunForwardsSIGTERM(t *testing.T) {
	dir := t.TempDir()
	r := InstallRun{Argv: []string{"/bin/sh", "-c", "echo ready; sleep 60"}, LogFile: filepath.Join(dir, "log"), ExitFile: filepath.Join(dir, "exit"), Stdout: os.Stderr}
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := r.Run(context.Background())
		done <- result{code, err}
	}()
	// "ready" is only written once the signal handler is installed.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, _ := os.ReadFile(r.LogFile); strings.Contains(string(b), "ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("script never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-done:
		if res.err != nil || res.code != 128+int(syscall.SIGTERM) {
			t.Fatalf("code %d err %v", res.code, res.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the script was not terminated")
	}
}

func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a", "b", "file")
	if err := writeAtomic(p, []byte("1")); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(p, []byte("2")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "2" {
		t.Fatalf("content %q", b)
	}
	if _, err := os.Stat(p + ".tmp"); err == nil {
		t.Fatal("temporary file left behind")
	}
	blocker := file(t, dir, "blocker")
	if err := writeAtomic(filepath.Join(blocker, "x"), nil); err == nil {
		t.Fatal("parent is a file")
	}
	// The destination is a directory: the rename fails.
	if err := os.MkdirAll(filepath.Join(dir, "dest"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(dir, "dest"), []byte("x")); err == nil {
		t.Fatal("renaming over a directory must fail")
	}
}
