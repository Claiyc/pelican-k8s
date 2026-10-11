package routes

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
	"github.com/pelican/wings/config"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/router"
	"github.com/pelican/wings/server/transfer"
)

// transferResults records the transfer results the agent's Wings reports.
var transferResults struct {
	sync.Mutex
	got []bool
}

func (stubPanel) SetTransferStatus(_ context.Context, _ string, successful bool) error {
	transferResults.Lock()
	defer transferResults.Unlock()
	transferResults.got = append(transferResults.got, successful)
	return nil
}

func reportedTransfers() []bool {
	transferResults.Lock()
	defer transferResults.Unlock()
	return append([]bool(nil), transferResults.got...)
}

// transferRequest builds the push a source Wings sends: the server files as
// a tar.gz with its checksum, signed for this agent.
func transferRequest(t *testing.T, scope string, files map[string]string, checksum string) *http.Request {
	t.Helper()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if checksum == "" {
		sum := sha256.Sum256(archive.Bytes())
		checksum = hex.EncodeToString(sum[:])
	}
	var body bytes.Buffer
	mp := multipart.NewWriter(&body)
	w, _ := mp.CreateFormFile("archive", "archive.tar.gz")
	_, _ = w.Write(archive.Bytes())
	cw, _ := mp.CreateFormField("checksum_archive")
	_, _ = cw.Write([]byte(checksum))
	if err := mp.Close(); err != nil {
		t.Fatal(err)
	}
	tok, err := jwt.Sign(map[string]any{"iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(), "sub": serverID, "scope": scope}, jwt.NewHS256([]byte(apiToken)))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/transfers", &body)
	req.Header.Set("Authorization", "Bearer "+string(tok))
	req.Header.Set("Content-Type", mp.FormDataContentType())
	return req
}

// newTransferFixture serves the fixture's server through Wings' own router.
func newTransferFixture(t *testing.T) *fixture {
	f := newFixture(t, sleepArgv, remote.ProcessStopConfiguration{})
	// As in the agent: files are chowned to its own user, and the shim
	// writes the machine-id (the chart disables Wings').
	config.Update(func(c *config.Configuration) {
		c.System.User.Uid, c.System.User.Gid = os.Getuid(), os.Getgid()
		c.System.MachineID.Enable = false
	})
	f.wings = router.Configure(f.manager, stubPanel{}).ServeHTTP
	return f
}

func TestIncomingTransferExtractsIntoTheServer(t *testing.T) {
	f := newTransferFixture(t)
	before := len(reportedTransfers())
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, transferRequest(t, "transfer", map[string]string{"server.properties": "motd=moved", "world/level.dat": "level"}, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if f.manager.Len() != 1 || f.manager.All()[0] != f.srv {
		t.Fatalf("Wings must receive into the agent's server, not build another: %d servers", f.manager.Len())
	}
	got, err := os.ReadFile(filepath.Join(f.srv.Filesystem().Path(), "server.properties"))
	if err != nil || string(got) != "motd=moved" {
		t.Fatalf("server.properties = %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(f.srv.Filesystem().Path(), "world", "level.dat")); err != nil {
		t.Fatal(err)
	}
	if r := reportedTransfers(); len(r) != before+1 || !r[len(r)-1] {
		t.Fatalf("results = %v", r)
	}
	if transfer.Incoming().Get(serverID) != nil || f.srv.IsTransferring() {
		t.Fatal("the transfer must be finished")
	}
}

func TestIncomingTransferChecksumMismatchFails(t *testing.T) {
	f := newTransferFixture(t)
	before := len(reportedTransfers())
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, transferRequest(t, "transfer", map[string]string{"a": "b"}, strings.Repeat("0", 64)))
	if rec.Code == http.StatusOK {
		t.Fatal("a corrupt archive must fail the transfer")
	}
	if r := reportedTransfers(); len(r) != before+1 || r[len(r)-1] {
		t.Fatalf("results = %v", r)
	}
	// Wings drops the server on a failed transfer; the gateway removes the
	// GameServer when the failure is reported.
	if f.manager.Len() != 0 || transfer.Incoming().Get(serverID) != nil {
		t.Fatalf("servers = %d", f.manager.Len())
	}
}

func TestIncomingTransferRejectedIsUndone(t *testing.T) {
	f := newTransferFixture(t)
	before := len(reportedTransfers())
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, transferRequest(t, "websocket", map[string]string{"a": "b"}, ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if transfer.Incoming().Get(serverID) != nil || f.srv.IsTransferring() || f.manager.Len() != 1 {
		t.Fatal("a rejected push must leave the server as it was")
	}
	if r := reportedTransfers(); len(r) != before {
		t.Fatalf("a rejected push reported a result: %v", r)
	}
}

func TestIncomingTransferIsActivity(t *testing.T) {
	f := newFixture(t, sleepArgv, remote.ProcessStopConfiguration{})
	var reasons any
	f.wings = func(w http.ResponseWriter, r *http.Request) {
		if tr := transfer.Incoming().Get(serverID); tr == nil || tr.Server != f.srv || !f.srv.IsTransferring() {
			t.Error("the agent's server must be registered as the transfer target")
		}
		reasons = decode(t, f.do("GET", "/internal/v1/activity", "Bearer "+apiToken, ""))["reasons"]
		w.WriteHeader(http.StatusOK)
	}
	f.handler.ServeHTTP(httptest.NewRecorder(), transferRequest(t, "transfer", map[string]string{"a": "b"}, ""))
	if rs, _ := reasons.([]any); len(rs) != 1 || rs[0] != "transfer" {
		t.Fatalf("a transfer in flight must keep the agent from moving: %v", reasons)
	}
}

func TestIncomingTransferRegistersOnlyAnAuthorizedTarget(t *testing.T) {
	f := newFixture(t, sleepArgv, remote.ProcessStopConfiguration{})
	f.wings = func(w http.ResponseWriter, r *http.Request) {
		if transfer.Incoming().Get(serverID) != nil || f.srv.IsTransferring() {
			t.Errorf("%s: nothing may be registered before Wings accepts the token", r.Header.Get("X-Case"))
		}
		w.WriteHeader(http.StatusForbidden)
	}
	cases := map[string]*http.Request{
		"no token":    httptest.NewRequest(http.MethodPost, "/api/transfers", nil),
		"wrong scope": transferRequest(t, "websocket", map[string]string{"a": "b"}, ""),
	}
	for name, req := range cases {
		req.Header.Set("X-Case", name)
		f.handler.ServeHTTP(httptest.NewRecorder(), req)
	}
}
