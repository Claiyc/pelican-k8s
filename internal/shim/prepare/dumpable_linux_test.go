//go:build linux

package prepare

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// dumpableEnv makes the test binary print its dumpable flag and exit.
const dumpableEnv = "PELICAN_TEST_PRINT_DUMPABLE"

func TestMain(m *testing.M) {
	if os.Getenv(dumpableEnv) != "" {
		v, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
		if err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		fmt.Println(v)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// The copied shim is execute-only, so every process that runs it (the shim
// itself, and kubelet's readiness exec, which carries the shim token in its
// environment) is non-dumpable from exec on: a process of the same UID cannot
// read its /proc/<pid>/environ.
func TestCopiedShimRunsNonDumpable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any file, so the kernel keeps its processes dumpable")
	}
	bin := filepath.Join(t.TempDir(), "bin", "shim")
	// copySelf copies the running executable: here, this test binary.
	if err := copySelf(bin); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(bin); err == nil {
		t.Fatal("the copied shim must not be readable")
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), dumpableEnv+"=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run the copy: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "0" {
		t.Fatalf("dumpable %q, want 0", got)
	}
	// A second copy over the first, as when init containers run again.
	if err := copySelf(bin); err != nil {
		t.Fatalf("copy over an existing shim: %v", err)
	}
}
