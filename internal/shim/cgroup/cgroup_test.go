package cgroup

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSample(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "cgroup.controllers", "cpu memory io")
	write(t, dir, "memory.current", "1048576\n")
	write(t, dir, "memory.max", "max\n")
	write(t, dir, "memory.stat", "anon 500000\ninactive_file 48576\n")
	write(t, dir, "cpu.stat", "usage_usec 1000000\nuser_usec 900000\n")
	write(t, dir, "io.stat", "8:0 rbytes=100 wbytes=200 rios=1 wios=2 dbytes=0 dios=0\n259:0 rbytes=5 wbytes=6\n")
	write(t, dir, "memory.events", "low 0\noom 1\noom_kill 2\n")
	write(t, dir, "netdev", `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 9999 10 0 0 0 0 0 0 9999 10 0 0 0 0 0 0
  eth0: 12345 100 0 0 0 0 0 0 67890 200 0 0 0 0 0 0
`)
	write(t, dir, "meminfo", "MemTotal:       16000000 kB\nMemFree: 1 kB\n")

	r := &Reader{Root: dir, NetDev: filepath.Join(dir, "netdev"), MemInfo: filepath.Join(dir, "meminfo")}
	if !r.Available() {
		t.Fatal("expected cgroup to be available")
	}
	started := time.Now().Add(-2 * time.Second)
	s := r.Sample(started)
	if s.MemoryBytes != 1048576-48576 {
		t.Fatalf("memory %d", s.MemoryBytes)
	}
	if s.MemoryLimitBytes != 16000000*1024 {
		t.Fatalf("limit %d", s.MemoryLimitBytes)
	}
	if s.CPUPercent != 0 {
		t.Fatalf("first sample cpu should be 0, got %v", s.CPUPercent)
	}
	if s.DiskReadBytes != 105 || s.DiskWrite != 206 {
		t.Fatalf("io %d %d", s.DiskReadBytes, s.DiskWrite)
	}
	if s.NetworkRx != 12345 || s.NetworkTx != 67890 {
		t.Fatalf("net %d %d", s.NetworkRx, s.NetworkTx)
	}
	if s.UptimeMillis < 1900 {
		t.Fatalf("uptime %d", s.UptimeMillis)
	}
	if r.OOMKills() != 2 {
		t.Fatalf("oom %d", r.OOMKills())
	}

	// Second sample: 500ms of CPU over the elapsed wall time.
	r.prevAt = time.Now().Add(-time.Second)
	write(t, dir, "cpu.stat", "usage_usec 1500000\n")
	s = r.Sample(started)
	if s.CPUPercent < 45 || s.CPUPercent > 55 {
		t.Fatalf("cpu percent %v", s.CPUPercent)
	}
	write(t, dir, "memory.max", "2147483648\n")
	if got := r.Sample(started).MemoryLimitBytes; got != 2147483648 {
		t.Fatalf("limit %d", got)
	}
}

func TestMissingFiles(t *testing.T) {
	r := &Reader{Root: t.TempDir(), NetDev: "/nonexistent", MemInfo: "/nonexistent"}
	if r.Available() {
		t.Fatal("should not be available")
	}
	s := r.Sample(time.Time{})
	if s.MemoryBytes != 0 || s.UptimeMillis != 0 || s.NetworkRx != 0 {
		t.Fatalf("expected zeros, got %+v", s)
	}
}
