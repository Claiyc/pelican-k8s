// Package cgroup samples resource usage of the container's own cgroup (v2)
// and network namespace, mirroring what Wings derives from Docker stats.
package cgroup

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Claiyc/pelican-k8s/internal/shim/protocol"
)

// Reader samples the cgroup at Root (normally /sys/fs/cgroup) and the network
// counters at NetDev (normally /proc/net/dev). It keeps the previous CPU
// sample to compute a usage percentage.
type Reader struct {
	Root    string
	NetDev  string
	MemInfo string

	prevCPUUsec uint64
	prevAt      time.Time
}

// New returns a reader for the container's own cgroup.
func New() *Reader {
	return &Reader{Root: "/sys/fs/cgroup", NetDev: "/proc/net/dev", MemInfo: "/proc/meminfo"}
}

// Available reports whether a cgroup v2 hierarchy is readable at Root.
func (r *Reader) Available() bool {
	_, err := os.Stat(filepath.Join(r.Root, "cgroup.controllers"))
	return err == nil
}

// Sample reads one usage sample. Missing files yield zero values, not errors,
// so a shim on an unusual node still reports what it can.
func (r *Reader) Sample(startedAt time.Time) *protocol.Stats {
	now := time.Now()
	st := &protocol.Stats{}
	if !startedAt.IsZero() {
		st.UptimeMillis = now.Sub(startedAt).Milliseconds()
	}

	cur := readUint(filepath.Join(r.Root, "memory.current"))
	inactive := readKeyed(filepath.Join(r.Root, "memory.stat"))["inactive_file"]
	if inactive < cur {
		st.MemoryBytes = cur - inactive
	} else {
		st.MemoryBytes = cur
	}
	st.MemoryLimitBytes = readUint(filepath.Join(r.Root, "memory.max"))
	if st.MemoryLimitBytes == 0 {
		st.MemoryLimitBytes = r.totalMemory()
	}

	usage := readKeyed(filepath.Join(r.Root, "cpu.stat"))["usage_usec"]
	if !r.prevAt.IsZero() && usage >= r.prevCPUUsec {
		wall := now.Sub(r.prevAt).Microseconds()
		if wall > 0 {
			pct := float64(usage-r.prevCPUUsec) / float64(wall) * 100
			st.CPUPercent = float64(int64(pct*1000)) / 1000
		}
	}
	r.prevCPUUsec = usage
	r.prevAt = now

	st.DiskReadBytes, st.DiskWrite = r.io()
	st.NetworkRx, st.NetworkTx = r.net()
	return st
}

// OOMKills returns the oom_kill counter of memory.events.
func (r *Reader) OOMKills() uint64 {
	return readKeyed(filepath.Join(r.Root, "memory.events"))["oom_kill"]
}

func (r *Reader) io() (read, write uint64) {
	f, err := os.Open(filepath.Join(r.Root, "io.stat"))
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		for _, kv := range strings.Fields(s.Text())[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			n, _ := strconv.ParseUint(v, 10, 64)
			switch k {
			case "rbytes":
				read += n
			case "wbytes":
				write += n
			}
		}
	}
	return read, write
}

func (r *Reader) net() (rx, tx uint64) {
	f, err := os.Open(r.NetDev)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "lo" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		a, _ := strconv.ParseUint(fields[0], 10, 64)
		b, _ := strconv.ParseUint(fields[8], 10, 64)
		rx += a
		tx += b
	}
	return rx, tx
}

func (r *Reader) totalMemory() uint64 {
	kb := readKeyed(r.MemInfo)["MemTotal:"]
	return kb * 1024
}

func readUint(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v := strings.TrimSpace(string(b))
	if v == "max" {
		return 0
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// readKeyed parses "key value" lines (memory.stat, cpu.stat, memory.events, meminfo).
func readKeyed(path string) map[string]uint64 {
	out := map[string]uint64{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 2 {
			continue
		}
		n, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		out[fields[0]] = n
	}
	return out
}

// String renders a sample for logs.
func String(s *protocol.Stats) string {
	return fmt.Sprintf("mem=%d/%d cpu=%.1f%% rx=%d tx=%d rd=%d wr=%d", s.MemoryBytes, s.MemoryLimitBytes, s.CPUPercent, s.NetworkRx, s.NetworkTx, s.DiskReadBytes, s.DiskWrite)
}
