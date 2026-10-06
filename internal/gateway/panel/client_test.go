package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pelican/wings/remote"

	"github.com/Claiyc/pelican-k8s/test/fakepanel"
)

const (
	tokenID = "tid"
	token   = "secret"
	uuid    = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
)

func newFake(t *testing.T) (*Client, *fakepanel.Panel) {
	t.Helper()
	fp := fakepanel.New(tokenID, token)
	fp.Add(&fakepanel.Server{
		UUID:                 uuid,
		Settings:             fakepanel.PaperSettings(uuid, 25565, 1024),
		ProcessConfiguration: fakepanel.PaperProcessConfiguration(),
		Install:              fakepanel.InstallScript{ContainerImage: "img", Entrypoint: "ash", Script: "echo hi"},
	})
	srv := httptest.NewServer(fp.Handler())
	t.Cleanup(srv.Close)
	return New(srv.URL+"/", tokenID, token, "test-agent"), fp
}

func TestNewTrimsTrailingSlash(t *testing.T) {
	c := New("https://panel.example/", "a", "b", "ua")
	if c.base != "https://panel.example/api/remote" {
		t.Fatalf("base = %q", c.base)
	}
}

func TestDoSendsCredentialsAndQuery(t *testing.T) {
	var (
		mu  sync.Mutex
		got *http.Request
		buf strings.Builder
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		got = r
		b := make([]byte, 64)
		n, _ := r.Body.Read(b)
		buf.Write(b[:n])
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("short and stout"))
	}))
	defer srv.Close()
	c := New(srv.URL, tokenID, token, "ua/1")
	status, body, err := c.Do(context.Background(), http.MethodPut, "/x", []byte(`{"a":1}`), map[string]string{"k": "v"})
	if err != nil || status != http.StatusTeapot || string(body) != "short and stout" {
		t.Fatalf("Do = %d %q %v", status, body, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got.Method != http.MethodPut || got.URL.Path != "/api/remote/x" || got.URL.Query().Get("k") != "v" {
		t.Fatalf("request = %s %s", got.Method, got.URL)
	}
	if got.Header.Get("Authorization") != "Bearer tid.secret" || got.Header.Get("User-Agent") != "ua/1" ||
		got.Header.Get("Accept") != "application/json" || got.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", got.Header)
	}
	if buf.String() != `{"a":1}` {
		t.Fatalf("body = %q", buf.String())
	}
}

func TestDoErrors(t *testing.T) {
	c := New("http://127.0.0.1:1", tokenID, token, "ua")
	if _, _, err := c.Do(context.Background(), http.MethodGet, "/x", nil, nil); err == nil {
		t.Fatal("connection refused must be an error")
	}
	if _, _, err := c.Do(context.Background(), "BAD METHOD", "/x", nil, nil); err == nil {
		t.Fatal("invalid method must be an error")
	}
	if err := c.getJSON(context.Background(), "/x", nil, &struct{}{}); err == nil {
		t.Fatal("getJSON must pass transport errors on")
	}
	if err := c.post(context.Background(), "/x", nil); err == nil {
		t.Fatal("post must pass transport errors on")
	}
	if err := c.post(context.Background(), "/x", func() {}); err == nil {
		t.Fatal("unmarshalable body must be an error")
	}
	if _, err := c.ValidateSftpCredentials(context.Background(), remote.SftpAuthRequest{}); err == nil {
		t.Fatal("ValidateSftpCredentials must pass transport errors on")
	}
}

func TestGetServerConfiguration(t *testing.T) {
	c, _ := newFake(t)
	cfg, err := c.GetServerConfiguration(context.Background(), uuid)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(cfg.Settings, &settings); err != nil || settings["uuid"] != uuid {
		t.Fatalf("settings = %s (%v)", cfg.Settings, err)
	}
	if !strings.Contains(string(cfg.ProcessConfiguration), "server.properties") {
		t.Fatalf("process configuration = %s", cfg.ProcessConfiguration)
	}
}

func TestNotFoundIsTyped(t *testing.T) {
	c, _ := newFake(t)
	_, err := c.GetServerConfiguration(context.Background(), "missing")
	if !IsNotFound(err) {
		t.Fatalf("err = %v, want a 404 *Error", err)
	}
	var pe *Error
	if !strings.Contains(err.Error(), "HTTP 404") || !asError(err, &pe) || pe.Status != 404 || pe.Body == "" {
		t.Fatalf("error = %v", err)
	}
	if IsNotFound(nil) {
		t.Fatal("nil is not a 404")
	}
	if IsNotFound(&Error{Status: 500}) {
		t.Fatal("500 is not a 404")
	}
}

func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

func TestAuthFailureIsAnError(t *testing.T) {
	c, _ := newFake(t)
	c.token = "wrong"
	_, err := c.GetInstallationScript(context.Background(), uuid)
	pe, ok := err.(*Error)
	if !ok || pe.Status != http.StatusForbidden {
		t.Fatalf("err = %v", err)
	}
}

func TestGetServerConfigurationBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	c := New(srv.URL, tokenID, token, "ua")
	if _, err := c.GetServerConfiguration(context.Background(), uuid); err == nil {
		t.Fatal("invalid JSON must be an error")
	}
}

func TestGetInstallationScript(t *testing.T) {
	c, _ := newFake(t)
	s, err := c.GetInstallationScript(context.Background(), uuid)
	if err != nil || s.ContainerImage != "img" || s.Entrypoint != "ash" || s.Script != "echo hi" {
		t.Fatalf("script = %+v, %v", s, err)
	}
	if _, err := c.GetInstallationScript(context.Background(), "missing"); !IsNotFound(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestListServersPaginates(t *testing.T) {
	var (
		mu    sync.Mutex
		pages []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		mu.Lock()
		pages = append(pages, r.URL.Query().Get("page")+"/"+r.URL.Query().Get("per_page"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"uuid": "srv-" + strconv.Itoa(page)}},
			"meta": map[string]any{"current_page": page, "last_page": 3},
		})
	}))
	defer srv.Close()
	c := New(srv.URL, tokenID, token, "ua")
	list, err := c.ListServers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].Uuid != "srv-1" || list[2].Uuid != "srv-3" {
		t.Fatalf("list = %+v", list)
	}
	if strings.Join(pages, ",") != "1/50,2/50,3/50" {
		t.Fatalf("requested pages %v", pages)
	}
}

func TestListServersSinglePageAndErrors(t *testing.T) {
	c, _ := newFake(t)
	list, err := c.ListServers(context.Background())
	if err != nil || len(list) != 1 || list[0].Uuid != uuid {
		t.Fatalf("list = %+v, %v", list, err)
	}
	c.token = "wrong"
	if _, err := c.ListServers(context.Background()); err == nil {
		t.Fatal("rejected credentials must fail the listing")
	}
}

func TestPosts(t *testing.T) {
	c, fp := newFake(t)
	ctx := context.Background()
	if err := c.ResetServersState(ctx); err != nil {
		t.Fatal(err)
	}
	if got := fp.CallsMatching("POST /api/remote/servers/reset"); len(got) != 1 {
		t.Fatalf("calls = %v", fp.Calls)
	}
	if err := c.PushServerStateChange(ctx, uuid, "starting", "running"); err != nil {
		t.Fatal(err)
	}
	if !fp.WaitForState(uuid, "running", time.Second) {
		t.Fatalf("state = %q", fp.Get(uuid).State)
	}
	if err := c.SetInstallationStatus(ctx, uuid, true, false); err != nil {
		t.Fatal(err)
	}
	if got := fp.InstallResults[uuid]; len(got) != 1 || got[0]["successful"] != true {
		t.Fatalf("install results = %v", got)
	}
	// Posts to an unknown server surface the Panel's status.
	if err := c.PushServerStateChange(ctx, "missing", "a", "b"); !IsNotFound(err) {
		t.Fatalf("err = %v", err)
	}
	if err := c.SetInstallationStatus(ctx, "missing", true, false); !IsNotFound(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateSftpCredentials(t *testing.T) {
	c, fp := newFake(t)
	ctx := context.Background()
	if _, err := c.ValidateSftpCredentials(ctx, remote.SftpAuthRequest{User: "u", Pass: "p"}); err == nil {
		t.Fatal("a Panel that rejects everything must fail the login")
	} else if pe, ok := err.(*Error); !ok || pe.Status != http.StatusForbidden {
		t.Fatalf("err = %v", err)
	}
	fp.SftpAuth = func(req map[string]any) (map[string]any, bool) {
		if req["username"] != "u" {
			return nil, false
		}
		return map[string]any{"server": uuid, "user": "user-1", "permissions": []string{"file.read"}}, true
	}
	res, err := c.ValidateSftpCredentials(ctx, remote.SftpAuthRequest{User: "u", Pass: "p"})
	if err != nil || res.Server != uuid || res.User != "user-1" || len(res.Permissions) != 1 {
		t.Fatalf("res = %+v, %v", res, err)
	}
}

func TestValidateSftpCredentialsBadResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("garbage"))
	}))
	defer srv.Close()
	c := New(srv.URL, tokenID, token, "ua")
	if _, err := c.ValidateSftpCredentials(context.Background(), remote.SftpAuthRequest{}); err == nil {
		t.Fatal("an unparsable response must be an error")
	}
}
