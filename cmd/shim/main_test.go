package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Claiyc/pelican-k8s/internal/shim/protocol"
)

// TestShimRunHelper is re-executed by TestGameCannotReadShimToken as the
// shim itself ("shim run").
func TestShimRunHelper(t *testing.T) {
	if os.Getenv("GO_WANT_SHIM_RUN") != "1" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("SHIM_ARGS")), &args); err != nil {
		os.Exit(2)
	}
	if err := runCmd(args, slog.New(slog.NewTextHandler(os.Stderr, nil))); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// The game process runs with the shim's UID and no capabilities, as in the
// pod. It must find the token neither in its own environment nor in the
// shim's (/proc/<shim>/environ), which the non-dumpable shim closes to it.
func TestGameCannotReadShimToken(t *testing.T) {
	const token = "shim-token-under-test"
	dir := t.TempDir()
	ln, err := protocol.Listen("127.0.0.1:0", []byte(token), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	script := `echo "own:${PELICAN_SHIM_TOKEN:-none}"; ` +
		`if cat /proc/$PPID/environ >/dev/null 2>&1; then ` +
		`if tr '\0' '\n' </proc/$PPID/environ | grep -q '^PELICAN_SHIM_TOKEN='; then echo "parent:token"; else echo "parent:readable"; fi; ` +
		`else echo "parent:denied"; fi; echo done`
	args := []string{"--agent", ln.Addr(), "--ready-file", "", "--dir", dir, "--tmp", "", "--no-stats", "--", "/bin/sh", "-c", script}
	encoded, _ := json.Marshal(args)
	bin := os.Args[0]
	cmd := exec.Command(bin, "-test.run=^TestShimRunHelper$")
	if os.Getuid() == 0 {
		// Root has CAP_SYS_PTRACE, which ignores dumpability: run the shim
		// like the pod does, as an unprivileged UID.
		const uid = 65534
		bin = filepath.Join(dir, "shim.test")
		copyFile(t, os.Args[0], bin)
		cmd = exec.Command(bin, "-test.run=^TestShimRunHelper$")
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: uid}}
		for _, p := range []string{filepath.Dir(dir), dir} {
			if err := os.Chmod(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chown(dir, uid, uid); err != nil {
			t.Fatal(err)
		}
	}
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "GO_WANT_SHIM_RUN=1", "SHIM_ARGS=" + string(encoded), protocol.TokenEnv + "=" + token}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := ln.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Subscribe(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Start(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for !strings.Contains(out.String(), "done") {
		select {
		case ev, ok := <-c.Events:
			if !ok {
				t.Fatalf("connection closed; output %q", out.String())
			}
			if ev.Type == protocol.TypeOutput {
				out.Write(ev.Data)
			}
		case <-ctx.Done():
			t.Fatalf("timeout; output %q", out.String())
		}
	}
	got := out.String()
	if strings.Contains(got, token) || strings.Contains(got, "parent:token") {
		t.Fatalf("the game process read the shim token: %q", got)
	}
	if !strings.Contains(got, "own:none") || !strings.Contains(got, "parent:denied") {
		t.Fatalf("output %q", got)
	}
}

func TestReadyCmd(t *testing.T) {
	file := filepath.Join(t.TempDir(), "ready")
	if err := readyCmd([]string{"--file", file}); err == nil {
		t.Fatal("ready without the file")
	}
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := readyCmd([]string{"--file", file}); err != nil {
		t.Fatalf("not ready with the file: %v", err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	src, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
}
