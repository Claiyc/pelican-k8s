package installer

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/Claiyc/pelican-k8s/internal/agent/shimenv"
	"github.com/Claiyc/pelican-k8s/internal/shim/protocol"
)

type fakeGateway struct {
	mu       sync.Mutex
	prepared int
	result   string
	strict   bool
	// endWith is the result runInstall's failAfter sets; empty means "Failed".
	endWith string
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

func runInstall(t *testing.T, gw *fakeGateway, exitCode string, failAfter time.Duration) ([]string, string, error) {
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
			if gw.endWith != "" {
				gw.result = gw.endWith
			}
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
	return append([]string(nil), lines...), string(logb), err
}

func TestInstallSucceeds(t *testing.T) {
	gw := &fakeGateway{}
	lines, logfile, err := runInstall(t, gw, "0", 0)
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
	if _, _, err := runInstall(t, &fakeGateway{}, "1", 0); err != nil {
		t.Fatalf("non-zero exit must not fail without strictExitCode: %v", err)
	}
}

func TestInstallStrictExitCode(t *testing.T) {
	_, _, err := runInstall(t, &fakeGateway{strict: true}, "1", 0)
	if err == nil || !strings.Contains(err.Error(), "exit code 1") {
		t.Fatalf("expected strict failure, got %v", err)
	}
}

func TestInstallFailedByOperator(t *testing.T) {
	_, logfile, err := runInstall(t, &fakeGateway{}, "0", 150*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "job failed") {
		t.Fatalf("expected operator failure, got %v", err)
	}
	if !strings.Contains(logfile, "downloading server.jar") {
		t.Fatalf("log should contain partial output:\n%s", logfile)
	}
}

// recordingClient records the install results that pass the filter and
// serves the install script Wings fetches.
type recordingClient struct {
	remote.Client
	mu      sync.Mutex
	reports []remote.InstallStatusRequest
}

func (c *recordingClient) GetInstallationScript(context.Context, string) (remote.InstallationScript, error) {
	return remote.InstallationScript{Entrypoint: "bash", Script: "#!/bin/bash\n"}, nil
}

func (c *recordingClient) Reports() []remote.InstallStatusRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]remote.InstallStatusRequest(nil), c.reports...)
}

func (c *recordingClient) SetInstallationStatus(_ context.Context, _ string, data remote.InstallStatusRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reports = append(c.reports, data)
	return nil
}

func TestInstallSupersededEndsTheWait(t *testing.T) {
	gw := &fakeGateway{endWith: "Superseded"}
	lines, _, err := runInstall(t, gw, "0", 150*time.Millisecond)
	if !errors.Is(err, ErrInstallSuperseded) {
		t.Fatalf("expected ErrInstallSuperseded, got %v", err)
	}
	if errors.Is(err, ErrInstallFailed) {
		t.Fatal("a superseded install is not a failed one")
	}
	if !strings.Contains(strings.Join(lines, "\n"), "downloading server.jar") {
		t.Fatalf("sink lines %q", lines)
	}
}

func TestSupersededRunReportsNothing(t *testing.T) {
	const id = "1a2b3c4d-0000-4000-8000-000000000000"
	inst := &Installer{}
	rec := &recordingClient{}
	c := inst.Client(rec)

	// The run Wings reports after was superseded: its result is dropped once.
	inst.superseded.Store(id, struct{}{})
	if err := c.SetInstallationStatus(context.Background(), id, remote.InstallStatusRequest{Successful: false}); err != nil {
		t.Fatal(err)
	}
	if len(rec.reports) != 0 {
		t.Fatalf("superseded result reached the gateway: %+v", rec.reports)
	}
	// The newer generation's result goes through.
	if err := c.SetInstallationStatus(context.Background(), id, remote.InstallStatusRequest{Successful: true, Reinstall: true}); err != nil {
		t.Fatal(err)
	}
	if len(rec.reports) != 1 || !rec.reports[0].Successful || !rec.reports[0].Reinstall {
		t.Fatalf("reports %+v", rec.reports)
	}
}

// TestSupersededResultNeverReachesTheGateway runs Wings' own install path with
// the agent's wiring: the superseded run releases the install lock and its
// result is dropped, and the next run's result goes through.
func TestSupersededResultNeverReachesTheGateway(t *testing.T) {
	root := t.TempDir()
	newServer(t, root) // config for the manager's server
	gw := &fakeGateway{}
	inst := &Installer{Gateway: gw, Root: root, LogDir: filepath.Join(root, "logs"), PollInterval: 50 * time.Millisecond}
	rec := &recordingClient{}
	ln, err := protocol.Listen("127.0.0.1:0", []byte("shim-token"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	reg := shimenv.NewRegistry(ln, nil, nil)
	m := server.NewEmptyManager(inst.Client(rec), server.WithEnvironmentFactory(reg.Factory), server.WithInstaller(inst))
	settings, _ := json.Marshal(map[string]any{"uuid": "1a2b3c4d-0000-4000-8000-000000000000", "allocations": map[string]any{"default": map[string]any{"ip": "0.0.0.0", "port": 25565}}})
	s, err := m.InitServer(remote.ServerConfigurationResponse{Settings: settings, ProcessConfiguration: &remote.ProcessConfiguration{}})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "install", "3")
	waitDir := func() {
		for {
			if _, err := os.Stat(dir); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// A newer generation supersedes the run while it waits for its Job.
	go func() {
		waitDir()
		gw.mu.Lock()
		gw.result = "Superseded"
		gw.mu.Unlock()
	}()
	if err := s.Install(); !errors.Is(err, ErrInstallSuperseded) {
		t.Fatalf("expected ErrInstallSuperseded, got %v", err)
	}
	if s.IsInstalling() {
		t.Fatal("the superseded run still holds the install lock")
	}
	if got := rec.Reports(); len(got) != 0 {
		t.Fatalf("superseded result reached the gateway: %+v", got)
	}

	// The newer generation's run reports as usual.
	gw.mu.Lock()
	gw.result = ""
	gw.mu.Unlock()
	go func() {
		waitDir()
		os.WriteFile(filepath.Join(dir, "exit-code"), []byte("0\n"), 0o644)
	}()
	if err := s.Install(); err != nil {
		t.Fatal(err)
	}
	if got := rec.Reports(); len(got) != 1 || !got[0].Successful {
		t.Fatalf("reports %+v", got)
	}
}
