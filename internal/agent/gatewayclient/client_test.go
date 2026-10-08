package gatewayclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInstallPrepared(t *testing.T) {
	var method, path, auth, accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, auth, accept = r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Accept")
		if r.Header.Get("Content-Type") != "" {
			t.Errorf("no body, no content type, got %q", r.Header.Get("Content-Type"))
		}
		_, _ = w.Write([]byte(`{"generation":4,"strict_exit_code":true}`))
	}))
	defer srv.Close()

	c := New(srv.URL+"/", "id", "secret", nil)
	got, err := c.InstallPrepared(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != 4 || !got.StrictExitCode {
		t.Fatalf("response: %+v", got)
	}
	if method != "POST" || path != "/api/remote/servers/u1/install/prepared" {
		t.Fatalf("request: %s %s", method, path)
	}
	if auth != "Bearer id.secret" || accept != "application/json" {
		t.Fatalf("headers: %q %q", auth, accept)
	}
}

func TestGetInstallState(t *testing.T) {
	var uri string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri = r.URL.RequestURI()
		_, _ = w.Write([]byte(`{"generation":7,"result":"succeeded","job_finished":true}`))
	}))
	defer srv.Close()

	got, err := New(srv.URL, "id", "s", nil).GetInstallState(context.Background(), "u1", 7)
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != 7 || got.Result != "succeeded" || !got.JobFinished {
		t.Fatalf("state: %+v", got)
	}
	if uri != "/api/remote/servers/u1/install/state?generation=7" {
		t.Fatalf("uri %s", uri)
	}
}

func TestErrors(t *testing.T) {
	ctx := context.Background()
	status := http.StatusInternalServerError
	body := " nope \n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := New(srv.URL, "id", "s", nil)

	_, err := c.InstallPrepared(ctx, "u")
	if err == nil || !strings.Contains(err.Error(), "HTTP 500: nope") {
		t.Fatalf("non-2xx: %v", err)
	}
	if _, err := c.GetInstallState(ctx, "u", 1); err == nil {
		t.Fatal("non-2xx must error")
	}

	status, body = http.StatusOK, "not json"
	if _, err := c.InstallPrepared(ctx, "u"); err == nil {
		t.Fatal("bad JSON must error")
	}

	srv.Close()
	if _, err := c.InstallPrepared(ctx, "u"); err == nil {
		t.Fatal("unreachable gateway must error")
	}
	if _, err := New("http://bad host", "i", "s", nil).InstallPrepared(ctx, "u"); err == nil {
		t.Fatal("bad URL must error")
	}
	if err := c.do(ctx, "POST", "/x", make(chan int), nil); err == nil {
		t.Fatal("unmarshalable body must error")
	}
}

func TestDoSendsBodyAndIgnoresResponseWhenOutNil(t *testing.T) {
	var ctype, got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctype = r.Header.Get("Content-Type")
		b := make([]byte, 64)
		n, _ := r.Body.Read(b)
		got = string(b[:n])
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := New(srv.URL, "i", "s", nil).do(context.Background(), "POST", "/x", map[string]int{"a": 1}, nil); err != nil {
		t.Fatal(err)
	}
	if ctype != "application/json" || strings.TrimSpace(got) != `{"a":1}` {
		t.Fatalf("ctype=%q body=%q", ctype, got)
	}
}
