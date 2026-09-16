//go:build e2e

// Package e2e exercises a deployed pelican-k8s gateway the way the Panel and a
// browser do. It needs a live server and these variables:
//
//	PELICAN_E2E_GATEWAY   https://wings.example.com
//	PELICAN_E2E_TOKEN     node daemon token
//	PELICAN_E2E_PANEL_URL https://panel.example.com   (websocket Origin)
//	PELICAN_E2E_SERVER    server uuid
//	PELICAN_E2E_INSECURE  "true" to skip TLS verification
//
//	go test -tags e2e ./test/e2e -v -timeout 20m
package e2e

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
	"github.com/gorilla/websocket"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type env struct {
	gateway, token, panel, server string
	insecure                      bool
	http                          *http.Client
}

func load(t *testing.T) *env {
	t.Helper()
	e := &env{gateway: strings.TrimSuffix(os.Getenv("PELICAN_E2E_GATEWAY"), "/"), token: os.Getenv("PELICAN_E2E_TOKEN"), panel: os.Getenv("PELICAN_E2E_PANEL_URL"), server: os.Getenv("PELICAN_E2E_SERVER"), insecure: os.Getenv("PELICAN_E2E_INSECURE") == "true"}
	if e.gateway == "" || e.token == "" || e.server == "" {
		t.Skip("PELICAN_E2E_* not set")
	}
	e.http = &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: e.insecure}}}
	return e
}

func (e *env) call(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.gateway+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := e.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if ua := res.Header.Get("User-Agent"); !strings.HasPrefix(ua, "Pelican Wings/v") {
		t.Fatalf("%s %s: missing Wings User-Agent (%q)", method, path, ua)
	}
	return res.StatusCode, string(b)
}

func (e *env) state(t *testing.T) string {
	_, body := e.call(t, "GET", "/api/servers/"+e.server, "")
	var d struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal([]byte(body), &d)
	return d.State
}

func (e *env) waitState(t *testing.T, want string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if e.state(t) == want {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("server did not reach %s (now %s)", want, e.state(t))
}

func (e *env) jwt(t *testing.T, scope string, extra map[string]any) string {
	t.Helper()
	now := time.Now()
	claims := map[string]any{
		"iss": e.panel, "aud": []string{e.gateway}, "jti": fmt.Sprintf("e2e-%d", now.UnixNano()), "iat": now.Unix(), "nbf": now.Add(-5 * time.Minute).Unix(), "exp": now.Add(10 * time.Minute).Unix(),
		"server_uuid": e.server, "user_uuid": "0f5e4d3c-2b1a-4c9d-8e7f-6a5b4c3d2e1f", "scope": scope, "unique_id": fmt.Sprintf("u%d", now.UnixNano()),
		"permissions": []string{"websocket.connect", "control.console", "control.start", "control.stop", "control.restart", "admin.websocket.errors"},
	}
	for k, v := range extra {
		claims[k] = v
	}
	tok, err := jwt.Sign(claims, jwt.NewHS256([]byte(e.token)))
	if err != nil {
		t.Fatal(err)
	}
	return string(tok)
}

func TestNodeEndpoints(t *testing.T) {
	e := load(t)
	code, body := e.call(t, "GET", "/api/system", "")
	if code != 200 || !strings.Contains(body, `"version"`) {
		t.Fatalf("system: %d %s", code, body)
	}
	if code, _ := e.call(t, "GET", "/api/system/ips", ""); code != 200 {
		t.Fatal("ips")
	}
	if code, _ := e.call(t, "GET", "/api/servers", ""); code != 200 {
		t.Fatal("servers")
	}
	if code, _ := e.call(t, "POST", "/api/update", "{}"); code != 200 {
		t.Fatal("update")
	}
	req, _ := http.NewRequest("GET", e.gateway+"/api/system", nil)
	res, _ := e.http.Do(req)
	if res.StatusCode != 401 {
		t.Fatalf("unauthenticated request returned %d", res.StatusCode)
	}
}

func TestFilesAndConsole(t *testing.T) {
	e := load(t)
	if code, body := e.call(t, "GET", "/api/servers/"+e.server+"/files/list-directory?directory=%2F", ""); code != 200 || !strings.Contains(body, `"name"`) {
		t.Fatalf("list: %d %s", code, body)
	}
	if code, _ := e.call(t, "POST", "/api/servers/"+e.server+"/files/write?file=%2Fe2e.txt", "hello from e2e"); code != 204 {
		t.Fatal("write")
	}
	if _, body := e.call(t, "GET", "/api/servers/"+e.server+"/files/contents?file=%2Fe2e.txt", ""); body != "hello from e2e" {
		t.Fatalf("contents %q", body)
	}
	// Signed download through the gateway (token re-signed for the agent).
	tok := e.jwt(t, "file-download", map[string]any{"file_path": "/e2e.txt"})
	res, err := e.http.Get(e.gateway + "/download/file?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(b) != "hello from e2e" {
		t.Fatalf("download: %d %q", res.StatusCode, b)
	}
	// One-time token: the second use must fail.
	res, _ = e.http.Get(e.gateway + "/download/file?token=" + tok)
	res.Body.Close()
	if res.StatusCode == 200 {
		t.Fatal("download token was reusable")
	}
	if code, _ := e.call(t, "POST", "/api/servers/"+e.server+"/files/delete", `{"root":"/","files":["e2e.txt"]}`); code != 204 {
		t.Fatal("delete")
	}
}

func TestPowerAndWebsocket(t *testing.T) {
	e := load(t)
	if st := e.state(t); st != "offline" {
		e.call(t, "POST", "/api/servers/"+e.server+"/power", `{"action":"stop"}`)
		e.waitState(t, "offline", 3*time.Minute)
	}
	if code, _ := e.call(t, "POST", "/api/servers/"+e.server+"/power", `{"action":"start"}`); code != 202 {
		t.Fatal("start")
	}
	e.waitState(t, "running", 5*time.Minute)

	dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: e.insecure}, HandshakeTimeout: 15 * time.Second}
	wsURL := strings.Replace(strings.Replace(e.gateway, "https://", "wss://", 1), "http://", "ws://", 1) + "/api/servers/" + e.server + "/ws"
	c, _, err := dialer.Dial(wsURL, http.Header{"Origin": []string{e.panel}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	send := func(ev string, args ...string) {
		if err := c.WriteJSON(map[string]any{"event": ev, "args": args}); err != nil {
			t.Fatal(err)
		}
	}
	// A token signed with the wrong key is rejected at the gateway.
	bad, _ := jwt.Sign(map[string]any{"server_uuid": e.server, "exp": time.Now().Add(time.Hour).Unix()}, jwt.NewHS256([]byte("wrong")))
	send("auth", string(bad))
	seen := map[string]int{}
	var console []string
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	read := func() (string, []string) {
		var m struct {
			Event string   `json:"event"`
			Args  []string `json:"args"`
		}
		if err := c.ReadJSON(&m); err != nil {
			t.Fatalf("read: %v (seen %v)", err, seen)
		}
		seen[m.Event]++
		return m.Event, m.Args
	}
	if ev, _ := read(); ev != "jwt error" {
		t.Fatalf("expected jwt error for a bad token, got %s", ev)
	}
	send("auth", e.jwt(t, "websocket", nil))
	for seen["auth success"] == 0 || seen["status"] == 0 {
		read()
	}
	send("send logs")
	send("send stats")
	for len(console) < 3 || seen["stats"] == 0 {
		ev, args := read()
		if ev == "console output" {
			console = append(console, args...)
		}
	}
	send("send command", "list")
	deadline := time.Now().Add(20 * time.Second)
	got := false
	for time.Now().Before(deadline) && !got {
		ev, args := read()
		if ev == "console output" && strings.Contains(strings.Join(args, " "), "players online") {
			got = true
		}
	}
	if !got {
		t.Fatal("no response to the list command on the console")
	}
	// Stop through the websocket: the gateway records the intent, the operator acts.
	send("set state", "stop")
	for seen["status"] < 3 {
		ev, args := read()
		if ev == "status" && len(args) > 0 && args[0] == "offline" {
			break
		}
	}
	e.waitState(t, "offline", 3*time.Minute)
}

// TestSFTP logs in through the gateway relay with Panel credentials and lists
// the server directory. Needs PELICAN_E2E_SFTP (host:port), PELICAN_E2E_SFTP_USER
// (panel username, without the .uuid suffix) and PELICAN_E2E_SFTP_PASSWORD.
func TestSFTP(t *testing.T) {
	e := load(t)
	addr, user, pass := os.Getenv("PELICAN_E2E_SFTP"), os.Getenv("PELICAN_E2E_SFTP_USER"), os.Getenv("PELICAN_E2E_SFTP_PASSWORD")
	if addr == "" || user == "" || pass == "" {
		t.Skip("PELICAN_E2E_SFTP* not set")
	}
	cfg := &ssh.ClientConfig{User: user + "." + e.server[:8], Auth: []ssh.AuthMethod{ssh.Password(pass)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 15 * time.Second}
	conn, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatalf("ssh dial: %v", err)
	}
	defer conn.Close()
	c, err := sftp.NewClient(conn)
	if err != nil {
		t.Fatalf("sftp subsystem: %v", err)
	}
	defer c.Close()
	if err := c.MkdirAll("/e2e-sftp"); err != nil {
		t.Fatal(err)
	}
	f, err := c.Create("/e2e-sftp/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("via sftp relay")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	entries, err := c.ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, en := range entries {
		names = append(names, en.Name())
	}
	if !strings.Contains(strings.Join(names, ","), "e2e-sftp") {
		t.Fatalf("directory listing %v", names)
	}
	// The file is visible through the HTTP file API as well.
	if _, body := e.call(t, "GET", "/api/servers/"+e.server+"/files/contents?file=%2Fe2e-sftp%2Fhello.txt", ""); body != "via sftp relay" {
		t.Fatalf("contents %q", body)
	}
	if err := c.RemoveAll("/e2e-sftp"); err != nil {
		t.Fatal(err)
	}
	// Wrong password is rejected.
	bad := &ssh.ClientConfig{User: user + "." + e.server[:8], Auth: []ssh.AuthMethod{ssh.Password("wrong-" + pass)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 15 * time.Second}
	if c2, err := ssh.Dial("tcp", addr, bad); err == nil {
		c2.Close()
		t.Fatal("wrong password accepted")
	}
}
