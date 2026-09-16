package installer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/server"
	"github.com/pelican/wings/system"

	"github.com/Claiyc/pelican-k8s/internal/agent/gatewayclient"
)

type fakeGateway struct {
	mu       sync.Mutex
	prepared int
	result   string
	strict   bool
}

func (f *fakeGateway) InstallPrepared(ctx context.Context, uuid string) (*gatewayclient.PreparedResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepared++
	return &gatewayclient.PreparedResponse{Generation: 3, StrictExitCode: f.strict}, nil
}

func (f *fakeGateway) GetInstallState(ctx context.Context, uuid string, gen int64) (*gatewayclient.InstallState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &gatewayclient.InstallState{Generation: gen, Result: f.result}, nil
}

// nopClient satisfies remote.Client for server construction; no method is called.
type nopClient struct{ remote.Client }

func newServer(t *testing.T, root string) *server.Server {
	t.Helper()
	c, _ := config.NewAtPath("/dev/null")
	c.AuthenticationToken = "t"
	c.System.RootDirectory = root
	c.System.LogDirectory = filepath.Join(root, "logs")
	c.System.Data = filepath.Join(root, "volumes")
	config.Set(c)
	s, err := server.New(nopClient{})
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := json.Marshal(map[string]any{"uuid": "1a2b3c4d-0000-4000-8000-000000000000", "environment": map[string]any{"FOO": "bar"}, "allocations": map[string]any{"default": map[string]any{"ip": "0.0.0.0", "port": 25565}}})
	if err := s.SyncWithConfiguration(remote.ServerConfigurationResponse{Settings: settings, ProcessConfiguration: &remote.ProcessConfiguration{}}); err != nil {
		t.Fatal(err)
	}
	return s
}

func runInstall(t *testing.T, gw *fakeGateway, exitCode string, failAfter time.Duration) (error, []string, string) {
	t.Helper()
	root := t.TempDir()
	s := newServer(t, root)
	inst := &Installer{Gateway: gw, Root: root, LogDir: filepath.Join(root, "logs"), PollInterval: 50 * time.Millisecond}

	sinkCh := make(chan []byte, 64)
	s.Sink(system.InstallSink).On(sinkCh)
	var mu sync.Mutex
	var lines []string
	go func() {
		for l := range sinkCh {
			mu.Lock()
			lines = append(lines, string(l))
			mu.Unlock()
		}
	}()

	go func() {
		dir := filepath.Join(root, "install", "3")
		for {
			if _, err := os.Stat(dir); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		f, _ := os.Create(filepath.Join(dir, "output.log"))
		f.WriteString("downloading server.jar\r\n")
		time.Sleep(100 * time.Millisecond)
		f.WriteString("done\n")
		f.Close()
		if failAfter > 0 {
			time.Sleep(failAfter)
			gw.mu.Lock()
			gw.result = "Failed"
			gw.mu.Unlock()
			return
		}
		os.WriteFile(filepath.Join(dir, "exit-code.tmp"), []byte(exitCode+"\n"), 0o644)
		os.Rename(filepath.Join(dir, "exit-code.tmp"), filepath.Join(dir, "exit-code"))
	}()

	err := inst.Run(s, &remote.InstallationScript{ContainerImage: "ghcr.io/pelican-eggs/installers:debian", Entrypoint: "bash", Script: "#!/bin/bash\n"})
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	logb, _ := os.ReadFile(filepath.Join(root, "logs", "install", s.ID()+".log"))
	return err, append([]string(nil), lines...), string(logb)
}

func TestInstallSucceeds(t *testing.T) {
	gw := &fakeGateway{}
	err, lines, logfile := runInstall(t, gw, "0", 0)
	if err != nil {
		t.Fatal(err)
	}
	if gw.prepared != 1 {
		t.Fatalf("prepared reported %d times", gw.prepared)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "downloading server.jar") || !strings.Contains(joined, "done") || strings.Contains(joined, "\r") {
		t.Fatalf("sink lines %q", lines)
	}
	if !strings.Contains(logfile, "| Script Output\n| ------------------------------\ndownloading server.jar") || !strings.Contains(logfile, "FOO=bar") || !strings.Contains(logfile, "Container Image:      ghcr.io/pelican-eggs/installers:debian") {
		t.Fatalf("install log:\n%s", logfile)
	}
}

func TestInstallLenientExitCode(t *testing.T) {
	if err, _, _ := runInstall(t, &fakeGateway{}, "1", 0); err != nil {
		t.Fatalf("non-zero exit must not fail without strictExitCode: %v", err)
	}
}

func TestInstallStrictExitCode(t *testing.T) {
	err, _, _ := runInstall(t, &fakeGateway{strict: true}, "1", 0)
	if err == nil || !strings.Contains(err.Error(), "exit code 1") {
		t.Fatalf("expected strict failure, got %v", err)
	}
}

func TestInstallFailedByOperator(t *testing.T) {
	err, _, logfile := runInstall(t, &fakeGateway{}, "0", 150*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "job failed") {
		t.Fatalf("expected operator failure, got %v", err)
	}
	if !strings.Contains(logfile, "downloading server.jar") {
		t.Fatalf("log should contain partial output:\n%s", logfile)
	}
}
