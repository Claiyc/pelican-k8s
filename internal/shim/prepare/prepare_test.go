package prepare

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLayout(t *testing.T) {
	dir := t.TempDir()
	l := Layout{Bin: filepath.Join(dir, "shared", "bin", "shim"), Shared: filepath.Join(dir, "shared"), Data: filepath.Join(dir, "data"), UUID: "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"}
	if err := l.Run(); err != nil {
		t.Fatal(err)
	}
	if err := l.Run(); err != nil { // idempotent
		t.Fatal(err)
	}
	for _, p := range []string{"data/volumes/1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d", "data/install", "data/logs/install", "shared/run", "shared/etc"} {
		if st, err := os.Stat(filepath.Join(dir, p)); err != nil || !st.IsDir() {
			t.Fatalf("missing dir %s: %v", p, err)
		}
	}
	mid, err := os.ReadFile(filepath.Join(dir, "data", "machine-id"))
	if err != nil || strings.TrimSpace(string(mid)) != "1a2b3c4d5e6f4a7b8c9d0e1f2a3b4c5d" {
		t.Fatalf("machine-id %q %v", mid, err)
	}
	st, err := os.Stat(l.Bin)
	if err != nil || st.Mode()&0o111 == 0 || st.Size() == 0 {
		t.Fatalf("shim binary not copied: %v", err)
	}
	if err := (Layout{Data: dir}).Run(); err == nil {
		t.Fatal("expected error without uuid")
	}
}

func TestProbe(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "bin"), 0o755)
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	os.WriteFile(filepath.Join(root, "etc", "passwd"), []byte("root:x:0:0:root:/root:/bin/bash\ncontainer:x:999:999::/home/container:/bin/bash\nsomeone:x:1000:1000::/home/someone:/bin/sh\n"), 0o644)
	os.WriteFile(filepath.Join(root, "etc", "group"), []byte("root:x:0:\ncontainer:x:999:\n"), 0o644)
	out := t.TempDir()
	p := Probe{Root: root, ArgvOut: filepath.Join(out, "argv"), PasswdOut: filepath.Join(out, "passwd"), GroupOut: filepath.Join(out, "group"), Name: "container", Home: "/home/container", UID: 1000, GID: 1000}
	if err := p.Run(); err == nil {
		t.Fatal("expected ErrNoEntrypoint")
	}
	os.WriteFile(filepath.Join(root, "entrypoint.sh"), []byte("#!/bin/bash\n"), 0o755)
	if err := p.Run(); err != nil {
		t.Fatal(err)
	}
	var argv []string
	b, _ := os.ReadFile(p.ArgvOut)
	json.Unmarshal(b, &argv)
	if strings.Join(argv, " ") != "/bin/sh /entrypoint.sh" {
		t.Fatalf("argv %v", argv)
	}
	os.WriteFile(filepath.Join(root, "bin", "bash"), []byte(""), 0o755)
	if err := p.Run(); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p.ArgvOut)
	json.Unmarshal(b, &argv)
	if strings.Join(argv, " ") != "/bin/bash /entrypoint.sh" {
		t.Fatalf("argv %v", argv)
	}
	pw, _ := os.ReadFile(p.PasswdOut)
	want := "root:x:0:0:root:/root:/bin/bash\ncontainer:x:1000:1000::/home/container:/bin/sh\n"
	if string(pw) != want {
		t.Fatalf("passwd:\n%s", pw)
	}
	gr, _ := os.ReadFile(p.GroupOut)
	if string(gr) != "root:x:0:\ncontainer:x:1000:\n" {
		t.Fatalf("group:\n%s", gr)
	}
	// Forced argv skips probing.
	forced := Probe{Root: t.TempDir(), ArgvOut: filepath.Join(out, "argv2"), Argv: []string{"/usr/bin/tini", "--", "/x"}}
	if err := forced.Run(); err != nil {
		t.Fatal(err)
	}
}

func TestInstallRun(t *testing.T) {
	dir := t.TempDir()
	server := filepath.Join(dir, "server")
	os.MkdirAll(server, 0o755)
	script := filepath.Join(dir, "install.sh")
	os.WriteFile(script, []byte("#!/bin/sh\necho installing $FOO\necho created > /mnt/x/file.txt\nexit 7\n"), 0o755)
	os.MkdirAll(filepath.Join(dir, "mnt"), 0o755)
	t.Setenv("FOO", "bar")
	r := InstallRun{Argv: []string{"/bin/sh", "-c", "echo installing $FOO; echo created > " + server + "/file.txt; exit 7"}, LogFile: filepath.Join(dir, "out", "output.log"), ExitFile: filepath.Join(dir, "out", "exit-code"), ChownPath: server, UID: os.Getuid(), GID: os.Getgid(), Stdout: os.Stderr}
	code, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 {
		t.Fatalf("code %d", code)
	}
	log, _ := os.ReadFile(r.LogFile)
	if !strings.Contains(string(log), "installing bar") {
		t.Fatalf("log %q", log)
	}
	ec, _ := os.ReadFile(r.ExitFile)
	if strings.TrimSpace(string(ec)) != "7" {
		t.Fatalf("exit file %q", ec)
	}
	if _, err := os.Stat(filepath.Join(server, "file.txt")); err != nil {
		t.Fatal(err)
	}
}
