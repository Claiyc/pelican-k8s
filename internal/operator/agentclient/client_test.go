package agentclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type seen struct {
	method, path, auth, ctype string
	body                      map[string]any
}

func server(t *testing.T, status int, resp string) (*Client, *seen) {
	t.Helper()
	s := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.method, s.path = r.Method, r.URL.Path
		s.auth, s.ctype = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		if r.ContentLength > 0 {
			_ = json.NewDecoder(r.Body).Decode(&s.body)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL+"/", "tok", nil), s
}

func TestHealthy(t *testing.T) {
	c, s := server(t, 200, "")
	if !c.Healthy(context.Background()) || s.path != "/internal/v1/healthz" {
		t.Fatalf("healthy=false path=%s", s.path)
	}
	c, _ = server(t, 503, "")
	if c.Healthy(context.Background()) {
		t.Fatal("503 must be unhealthy")
	}
	c = New("http://127.0.0.1:1", "t", nil)
	if c.Healthy(context.Background()) {
		t.Fatal("unreachable must be unhealthy")
	}
	c = New("http://bad host", "t", nil)
	if c.Healthy(context.Background()) {
		t.Fatal("bad URL must be unhealthy")
	}
}

func TestGetServer(t *testing.T) {
	c, s := server(t, 200, `{"state":"running","is_suspended":true,"utilization":{"memory_bytes":5,"uptime":7}}`)
	st, err := c.GetServer(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "running" || !st.IsSuspended || st.Utilization.MemoryBytes != 5 || st.Utilization.Uptime != 7 {
		t.Fatalf("state: %+v", st)
	}
	if s.method != "GET" || s.path != "/api/servers/u1" || s.auth != "Bearer tok" {
		t.Fatalf("request: %+v", s)
	}

	c, _ = server(t, 200, `not json`)
	if _, err := c.GetServer(context.Background(), "u1"); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestShimAndActivity(t *testing.T) {
	c, s := server(t, 200, `{"attached":true,"podUID":"p1","running":true}`)
	sh, err := c.Shim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sh.Attached || sh.PodUID != "p1" || !sh.Running || sh.Terminating {
		t.Fatalf("shim: %+v", sh)
	}
	if s.method != "GET" || s.path != "/internal/v1/shim" || s.auth != "Bearer tok" {
		t.Fatalf("request: %+v", s)
	}

	c, s = server(t, 200, `{"busy":true,"reasons":["files","pull"]}`)
	act, err := c.Activity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !act.Busy || len(act.Reasons) != 2 || act.Reasons[1] != "pull" {
		t.Fatalf("activity: %+v", act)
	}
	if s.path != "/internal/v1/activity" {
		t.Fatalf("request: %+v", s)
	}
}

func TestCommands(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		call   func(*Client) error
		method string
		path   string
		body   map[string]any
	}{
		{"power", func(c *Client) error { return c.Power(ctx, "u", "kill") }, "POST", "/api/servers/u/power", map[string]any{"action": "kill"}},
		{"sync", func(c *Client) error { return c.Sync(ctx, "u") }, "POST", "/api/servers/u/sync", nil},
		{"install", func(c *Client) error { return c.Install(ctx, "u", false) }, "POST", "/api/servers/u/install", nil},
		{"reinstall", func(c *Client) error { return c.Install(ctx, "u", true) }, "POST", "/api/servers/u/reinstall", nil},
		{"delete", func(c *Client) error { return c.Delete(ctx, "u") }, "DELETE", "/api/servers/u", nil},
		{"exit", func(c *Client) error { return c.ExitState(ctx, 137, true) }, "POST", "/internal/v1/exit-state", map[string]any{"code": float64(137), "oomKilled": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, s := server(t, 204, "")
			if err := tc.call(c); err != nil {
				t.Fatal(err)
			}
			if s.method != tc.method || s.path != tc.path || s.auth != "Bearer tok" {
				t.Fatalf("request: %+v", s)
			}
			if tc.body == nil {
				if s.body != nil || s.ctype != "" {
					t.Fatalf("unexpected body/content-type: %+v", s)
				}
				return
			}
			if s.ctype != "application/json" {
				t.Fatalf("content-type %q", s.ctype)
			}
			for k, v := range tc.body {
				if s.body[k] != v {
					t.Fatalf("body[%s]=%v want %v", k, s.body[k], v)
				}
			}
		})
	}
}

func TestErrors(t *testing.T) {
	ctx := context.Background()
	c, _ := server(t, 409, "busy")
	if err := c.Power(ctx, "u", "start"); !errors.Is(err, ErrConflict) {
		t.Fatalf("409: %v", err)
	}
	c, _ = server(t, 500, "  boom \n")
	err := c.Sync(ctx, "u")
	if err == nil || !strings.Contains(err.Error(), "HTTP 500: boom") {
		t.Fatalf("500: %v", err)
	}
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrUnavailable) {
		t.Fatal("500 must not map to a sentinel")
	}
	c = New("http://127.0.0.1:1", "t", nil)
	if err := c.Sync(ctx, "u"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreachable: %v", err)
	}
	c = New("http://bad host", "t", nil)
	if err := c.Sync(ctx, "u"); err == nil {
		t.Fatal("bad URL must error")
	}
	if err := c.do(ctx, "POST", "/x", make(chan int), nil); err == nil {
		t.Fatal("unmarshalable body must error")
	}
}
