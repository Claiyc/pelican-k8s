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
	// failStates makes that many GetInstallState calls fail.
	failStates int
	states     int
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
	f.states++
	if f.failStates > 0 {
		f.failStates--
		return nil, errors.New("gateway unavailable")
	}
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
		writeExitCode(dir, exitCode)
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
	if !inst.ReportPending(id) {
		t.Fatal("a superseded run that has not reported must be pending")
	}
	if err := c.SetInstallationStatus(context.Background(), id, remote.InstallStatusRequest{Successful: false}); err != nil {
		t.Fatal(err)
	}
	if len(rec.reports) != 0 {
		t.Fatalf("superseded result reached the gateway: %+v", rec.reports)
	}
	if inst.ReportPending(id) {
		t.Fatal("still pending after the report was dropped")
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
		writeExitCode(dir, "0")
	}()
	if err := s.Install(); err != nil {
		t.Fatal(err)
	}
	if got := rec.Reports(); len(got) != 1 || !got[0].Successful {
		t.Fatalf("reports %+v", got)
	}
}

// TestKilledJobOfASupersededGenerationIsNoResult covers a superseded install
// whose Job was already running: deleting it kills the script, and the shim
// still writes an exit code. That code must not count as the install's result.
func TestKilledJobOfASupersededGenerationIsNoResult(t *testing.T) {
	gw := &fakeGateway{result: "Superseded"}
	root := t.TempDir()
	s := newServer(t, root)
	// Only the exit-code check may notice the supersession: no poll in time.
	inst := &Installer{Gateway: gw, Root: root, LogDir: filepath.Join(root, "logs"), PollInterval: time.Hour}
	go func() {
		dir := filepath.Join(root, "install", "3")
		for {
			if _, err := os.Stat(dir); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		writeExitCode(dir, "143")
	}()
	done := make(chan error, 1)
	go func() { done <- inst.Run(s, &remote.InstallationScript{Entrypoint: "bash", Script: "#!/bin/bash\n"}) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrInstallSuperseded) {
			t.Fatalf("expected ErrInstallSuperseded, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	if _, ok := inst.superseded.Load(s.ID()); !ok {
		t.Fatal("the killed run's result must be marked for dropping")
	}
}

// writeExitCode writes exit-code the way the shim does, atomically: the
// installer must never read it half written.
func writeExitCode(dir, code string) {
	tmp := filepath.Join(dir, "exit-code.tmp")
	os.WriteFile(tmp, []byte(code+"\n"), 0o644)
	os.Rename(tmp, filepath.Join(dir, "exit-code"))
}

// runWithExitCode runs an install whose Job left exitCode, with no poll in
// time: only the check after the exit code can change the outcome.
func runWithExitCode(t *testing.T, gw *fakeGateway, exitCode string) (*Installer, *server.Server, error) {
	t.Helper()
	root := t.TempDir()
	s := newServer(t, root)
	inst := &Installer{Gateway: gw, Root: root, LogDir: filepath.Join(root, "logs"), PollInterval: time.Hour}
	go func() {
		dir := filepath.Join(root, "install", "3")
		for {
			if _, err := os.Stat(dir); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		writeExitCode(dir, exitCode)
	}()
	done := make(chan error, 1)
	go func() { done <- inst.Run(s, &remote.InstallationScript{Entrypoint: "bash", Script: "#!/bin/bash\n"}) }()
	select {
	case err := <-done:
		return inst, s, err
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	return nil, nil, nil
}

// TestKilledJobOfAFailedJobIsAFailure covers a Job of the current generation
// that timed out: the shim wrote the killed script's code, the operator
// recorded the Job as failed.
func TestKilledJobOfAFailedJobIsAFailure(t *testing.T) {
	_, _, err := runWithExitCode(t, &fakeGateway{result: "Failed"}, "143")
	if !errors.Is(err, ErrInstallFailed) || errors.Is(err, ErrInstallSuperseded) {
		t.Fatalf("expected ErrInstallFailed, got %v", err)
	}
}

// TestExitCodeWaitsForTheGatewayCheck covers a gateway that cannot answer when
// the exit code appears: the code is not taken until it can, so a killed run
// of a superseded generation is still recognised.
func TestExitCodeWaitsForTheGatewayCheck(t *testing.T) {
	gw := &fakeGateway{result: "Superseded", failStates: 2}
	_, _, err := runWithExitCode(t, gw, "143")
	if !errors.Is(err, ErrInstallSuperseded) {
		t.Fatalf("expected ErrInstallSuperseded after the gateway recovered, got %v", err)
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if gw.states < 3 {
		t.Fatalf("the exit code was taken without a successful check (%d calls)", gw.states)
	}
}

// TestExitCodeOfTheCurrentGenerationIsTheResult checks that the gateway's
// answer for the current generation leaves the exit code as the result.
func TestExitCodeOfTheCurrentGenerationIsTheResult(t *testing.T) {
	if _, _, err := runWithExitCode(t, &fakeGateway{result: "Running", strict: true}, "2"); err == nil || !strings.Contains(err.Error(), "exit code 2") {
		t.Fatalf("expected the strict exit code failure, got %v", err)
	}
}
