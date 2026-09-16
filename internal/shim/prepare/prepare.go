// Package prepare implements the shim's init-container duties: copying the
// shim binary into the shared emptyDir, laying out the PVC, resolving the egg
// image entrypoint and generating passwd entries, and running install scripts.
package prepare

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Layout creates the Wings root layout on the PVC and the shared /pelican
// directories. It is idempotent.
type Layout struct {
	// Bin is where the shim binary is copied to (e.g. /pelican/bin/shim). Empty skips the copy.
	Bin string
	// Shared is the emptyDir shared with the game container (e.g. /pelican).
	Shared string
	// Data is the PVC root (Wings root_directory).
	Data string
	// UUID is the server UUID.
	UUID string
}

// Run performs the preparation.
func (l Layout) Run() error {
	if l.UUID == "" {
		return errors.New("prepare: uuid is required")
	}
	if l.Bin != "" {
		if err := copySelf(l.Bin); err != nil {
			return err
		}
	}
	if l.Shared != "" {
		for _, d := range []string{"run", "etc"} {
			if err := os.MkdirAll(filepath.Join(l.Shared, d), 0o770); err != nil {
				return err
			}
		}
	}
	dirs := []string{
		filepath.Join("volumes", l.UUID),
		"install",
		"logs",
		filepath.Join("logs", "install"),
		"backups",
		"tmp",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(l.Data, d), 0o700); err != nil {
			return fmt.Errorf("prepare: mkdir %s: %w", d, err)
		}
	}
	mid := filepath.Join(l.Data, "machine-id")
	if _, err := os.Stat(mid); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(mid, []byte(strings.ReplaceAll(l.UUID, "-", "")+"\n"), 0o644); err != nil {
			return fmt.Errorf("prepare: write machine-id: %w", err)
		}
	}
	return nil
}

func copySelf(dst string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	in, err := os.Open(self)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// Probe resolves the egg image entrypoint by convention and generates passwd
// and group files with an entry for the pinned UID. It runs inside the egg image.
type Probe struct {
	// Root is the filesystem root to inspect ("/" in production).
	Root string
	// ArgvOut receives the resolved argv as a JSON array.
	ArgvOut string
	// PasswdOut and GroupOut receive generated files; empty skips them.
	PasswdOut string
	GroupOut  string
	// Name, Home, UID and GID describe the container user.
	Name string
	Home string
	UID  int
	GID  int
	// Argv forces the argv instead of probing.
	Argv []string
}

// ErrNoEntrypoint is returned when the image does not follow the yolk convention.
var ErrNoEntrypoint = errors.New("probe: cannot determine image entrypoint: no /entrypoint.sh found; set an entrypointOverride in the GameServerClass")

// Run performs the probe.
func (p Probe) Run() error {
	argv := p.Argv
	if len(argv) == 0 {
		var err error
		argv, err = p.resolveArgv()
		if err != nil {
			return err
		}
	}
	b, err := json.Marshal(argv)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.ArgvOut), 0o770); err != nil {
		return err
	}
	if err := os.WriteFile(p.ArgvOut, b, 0o644); err != nil {
		return err
	}
	if p.PasswdOut != "" {
		if err := p.writePasswd(); err != nil {
			return err
		}
	}
	if p.GroupOut != "" {
		if err := p.writeGroup(); err != nil {
			return err
		}
	}
	return nil
}

func (p Probe) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(p.Root, rel))
	return err == nil
}

func (p Probe) resolveArgv() ([]string, error) {
	if !p.exists("entrypoint.sh") {
		return nil, ErrNoEntrypoint
	}
	shell := "/bin/sh"
	if p.exists("bin/bash") {
		shell = "/bin/bash"
	}
	return []string{shell, "/entrypoint.sh"}, nil
}

func (p Probe) writePasswd() error {
	lines := filterEntries(readLines(filepath.Join(p.Root, "etc/passwd")), p.Name, p.UID)
	lines = append(lines, fmt.Sprintf("%s:x:%d:%d::%s:/bin/sh", p.Name, p.UID, p.GID, p.Home))
	return writeLines(p.PasswdOut, lines)
}

func (p Probe) writeGroup() error {
	lines := filterEntries(readLines(filepath.Join(p.Root, "etc/group")), p.Name, p.GID)
	lines = append(lines, fmt.Sprintf("%s:x:%d:", p.Name, p.GID))
	return writeLines(p.GroupOut, lines)
}

// filterEntries drops passwd/group lines whose name or numeric id (third field) matches.
func filterEntries(lines []string, name string, id int) []string {
	out := lines[:0]
	for _, l := range lines {
		f := strings.Split(l, ":")
		if len(f) < 3 {
			continue
		}
		if f[0] == name {
			continue
		}
		if n, err := strconv.Atoi(f[2]); err == nil && n == id {
			continue
		}
		out = append(out, l)
	}
	return out
}

func readLines(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		if t := strings.TrimSpace(s.Text()); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func writeLines(path string, lines []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o770); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}
