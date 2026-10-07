package agents

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
)

const (
	testUUID = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	testNS   = "pelican-servers"
)

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func agentPod(ip string, started bool) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: names.AgentPod(testUUID), Namespace: testNS},
		Status: corev1.PodStatus{
			PodIP:             ip,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", Started: &started}},
		},
	}
}

func agentSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: names.AgentSecret(testUUID), Namespace: testNS},
		Data:       map[string][]byte{"token_id": []byte("tid"), "token": []byte("tok")},
	}
}

// redirect makes the resolver dial srv no matter which pod IP the target has,
// since the agent port is fixed.
func redirect(r *Resolver, srv *httptest.Server) {
	addr := srv.Listener.Addr().String()
	r.Transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
}

func TestTargetAddresses(t *testing.T) {
	v4 := Target{PodIP: "10.1.2.3"}
	if got := v4.HTTPBase(); got != "http://10.1.2.3:8080" {
		t.Errorf("HTTPBase = %q", got)
	}
	if got := v4.SFTPAddr(); got != "10.1.2.3:2022" {
		t.Errorf("SFTPAddr = %q", got)
	}
	v6 := Target{PodIP: "fd00::5"}
	if got := v6.HTTPBase(); got != "http://[fd00::5]:8080" {
		t.Errorf("HTTPBase v6 = %q", got)
	}
	if got := v6.SFTPAddr(); got != "[fd00::5]:2022" {
		t.Errorf("SFTPAddr v6 = %q", got)
	}
}

func TestResolve(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		objs    []client.Object
		wantErr error
		anyErr  bool
	}{
		{name: "no pod", objs: []client.Object{agentSecret()}, wantErr: ErrUnavailable},
		{name: "agent not started", objs: []client.Object{agentPod("10.0.0.1", false), agentSecret()}, wantErr: ErrUnavailable},
		{name: "no pod ip", objs: []client.Object{agentPod("", true), agentSecret()}, wantErr: ErrUnavailable},
		{name: "missing token secret", objs: []client.Object{agentPod("10.0.0.1", true)}, anyErr: true},
		{name: "ready", objs: []client.Object{agentPod("10.0.0.1", true), agentSecret()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewResolver(store.New(newClient(t, tc.objs...), testNS, "default"), time.Second)
			got, err := r.Resolve(ctx, testUUID)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case tc.anyErr:
				if err == nil {
					t.Fatal("want error")
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				want := Target{UUID: testUUID, PodIP: "10.0.0.1", TokenID: "tid", Token: "tok"}
				if *got != want {
					t.Fatalf("target = %+v, want %+v", *got, want)
				}
			}
		})
	}
}

// Wait holds a call while the agent pod is not ready and gives up after
// HTTPWait.
func TestWait(t *testing.T) {
	ctx := context.Background()
	t.Run("agent becomes ready", func(t *testing.T) {
		c := newClient(t, agentPod("10.0.0.1", false), agentSecret())
		r := NewResolver(store.New(c, testNS, "default"), time.Second)
		r.HTTPWait, r.Poll = 5*time.Second, 10*time.Millisecond
		errc := make(chan error, 1)
		go func() {
			time.Sleep(50 * time.Millisecond)
			if err := c.Delete(ctx, agentPod("10.0.0.1", false)); err != nil {
				errc <- err
				return
			}
			errc <- c.Create(ctx, agentPod("10.0.0.1", true))
		}()
		got, err := r.Wait(ctx, testUUID)
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
		if err != nil || got.PodIP != "10.0.0.1" {
			t.Fatalf("Wait = %+v, %v; want the agent once it is ready", got, err)
		}
	})
	t.Run("gives up", func(t *testing.T) {
		r := NewResolver(store.New(newClient(t, agentSecret()), testNS, "default"), time.Second)
		r.HTTPWait, r.Poll = 50*time.Millisecond, 10*time.Millisecond
		start := time.Now()
		if _, err := r.Wait(ctx, testUUID); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
		if time.Since(start) < 50*time.Millisecond {
			t.Fatal("Wait returned before HTTPWait")
		}
	})
	t.Run("other errors are returned at once", func(t *testing.T) {
		r := NewResolver(store.New(newClient(t, agentPod("10.0.0.1", true)), testNS, "default"), time.Second)
		r.HTTPWait = time.Hour
		if _, err := r.Wait(ctx, testUUID); err == nil || errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v, want the missing secret", err)
		}
	})
}

func TestProxy(t *testing.T) {
	var gotAuth, gotXFF, gotHost, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotXFF, gotHost, gotPath = r.Header.Get("Authorization"), r.Header.Get("X-Forwarded-For"), r.Host, r.URL.RequestURI()
		w.Header().Set("User-Agent", "Pelican Wings/vdevelop (id:agentid)")
		w.Header().Set("X-Custom", "kept")
		_, _ = io.WriteString(w, "agent says hi")
	}))
	defer srv.Close()
	r := NewResolver(nil, time.Second)
	redirect(r, srv)
	target := &Target{UUID: testUUID, PodIP: "10.0.0.9", Token: "agent-secret"}

	t.Run("forwards with agent token", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/servers/x/files?a=b", nil)
		req.Header.Set("Authorization", "Bearer panel-token")
		req.Header.Set("X-Forwarded-For", "203.0.113.7")
		rec := httptest.NewRecorder()
		r.Proxy(rec, req, target, nil)
		if rec.Code != 200 || rec.Body.String() != "agent says hi" {
			t.Fatalf("response %d %q", rec.Code, rec.Body.String())
		}
		if gotAuth != "Bearer agent-secret" {
			t.Errorf("Authorization = %q", gotAuth)
		}
		if gotXFF != "" {
			t.Errorf("X-Forwarded-For leaked: %q", gotXFF)
		}
		if gotHost != "10.0.0.9:8080" {
			t.Errorf("Host = %q", gotHost)
		}
		if gotPath != "/api/servers/x/files?a=b" {
			t.Errorf("path = %q", gotPath)
		}
		if rec.Header().Get("User-Agent") != "" {
			t.Errorf("agent User-Agent must be stripped, got %q", rec.Header().Get("User-Agent"))
		}
		if rec.Header().Get("X-Custom") != "kept" {
			t.Error("other headers must pass through")
		}
	})

	t.Run("rewrite hook", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/panel/path", nil)
		rec := httptest.NewRecorder()
		r.Proxy(rec, req, target, func(out *http.Request) { out.URL.Path = "/agent/path" })
		if gotPath != "/agent/path" {
			t.Errorf("path = %q", gotPath)
		}
	})

	t.Run("agent unreachable", func(t *testing.T) {
		dead := NewResolver(nil, time.Second)
		dead.Transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New(`boom "quoted"`)
		}
		rec := httptest.NewRecorder()
		dead.Proxy(rec, httptest.NewRequest("GET", "/x", nil), target, nil)
		if rec.Code != http.StatusBadGateway || rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("response %d %v", rec.Code, rec.Header())
		}
		var body struct{ Error string }
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body is not valid JSON (%q): %v", rec.Body.String(), err)
		}
		if !strings.Contains(body.Error, `boom "quoted"`) {
			t.Errorf("error = %q", body.Error)
		}
	})
}

func TestDo(t *testing.T) {
	var gotMethod, gotAuth, gotAccept, gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotMethod, gotAuth, gotAccept, gotCT, gotBody = r.Method, r.Header.Get("Authorization"), r.Header.Get("Accept"), r.Header.Get("Content-Type"), string(b)
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "short and stout")
	}))
	defer srv.Close()
	r := NewResolver(nil, time.Second)
	redirect(r, srv)
	target := &Target{PodIP: "10.0.0.9", Token: "tok"}

	status, body, err := r.Do(context.Background(), target, http.MethodPost, "/api/x", strings.NewReader(`{"a":1}`), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusTeapot || string(body) != "short and stout" {
		t.Errorf("got %d %q", status, body)
	}
	if gotMethod != "POST" || gotAuth != "Bearer tok" || gotAccept != "application/json" || gotCT != "application/json" || gotBody != `{"a":1}` {
		t.Errorf("request: %s %q %q %q %q", gotMethod, gotAuth, gotAccept, gotCT, gotBody)
	}

	if _, _, err := r.Do(context.Background(), target, http.MethodGet, "/x", nil, ""); err != nil || gotCT != "" {
		t.Errorf("no content type expected, got %q (err %v)", gotCT, err)
	}

	t.Run("invalid method", func(t *testing.T) {
		if _, _, err := r.Do(context.Background(), target, "BAD METHOD", "/x", nil, ""); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := r.Do(ctx, target, http.MethodGet, "/x", nil, ""); err == nil {
			t.Fatal("want error")
		}
	})
}

func TestState(t *testing.T) {
	var hits atomic.Int32
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/api/servers/"+testUUID {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"state":"running"}`)
	}))
	defer srv.Close()
	r := NewResolver(nil, time.Hour)
	redirect(r, srv)
	target := &Target{UUID: testUUID, PodIP: "10.0.0.9", Token: "tok"}
	ctx := context.Background()

	body, err := r.State(ctx, target, false)
	if err != nil || string(body) != `{"state":"running"}` {
		t.Fatalf("State = %q, %v", body, err)
	}
	if _, err := r.State(ctx, target, false); err != nil || hits.Load() != 1 {
		t.Fatalf("second call must hit the cache; hits=%d err=%v", hits.Load(), err)
	}
	if _, err := r.State(ctx, target, true); err != nil || hits.Load() != 2 {
		t.Fatalf("force must bypass the cache; hits=%d err=%v", hits.Load(), err)
	}
	r.Invalidate(testUUID)
	if _, err := r.State(ctx, target, false); err != nil || hits.Load() != 3 {
		t.Fatalf("Invalidate must drop the cache; hits=%d err=%v", hits.Load(), err)
	}

	t.Run("expired entry refetches", func(t *testing.T) {
		short := NewResolver(nil, time.Nanosecond)
		redirect(short, srv)
		before := hits.Load()
		_, _ = short.State(ctx, target, false)
		time.Sleep(time.Millisecond)
		_, _ = short.State(ctx, target, false)
		if hits.Load()-before != 2 {
			t.Fatalf("hits = %d, want 2", hits.Load()-before)
		}
	})

	t.Run("non-200 is an error and not cached", func(t *testing.T) {
		status = http.StatusInternalServerError
		fresh := NewResolver(nil, time.Hour)
		redirect(fresh, srv)
		if _, err := fresh.State(ctx, target, false); err == nil || !strings.Contains(err.Error(), "500") {
			t.Fatalf("err = %v", err)
		}
		status = http.StatusOK
		if _, err := fresh.State(ctx, target, false); err != nil {
			t.Fatalf("failure must not be cached: %v", err)
		}
	})

	t.Run("transport error", func(t *testing.T) {
		dead := NewResolver(nil, time.Hour)
		dead.Transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("down")
		}
		if _, err := dead.State(ctx, target, false); err == nil {
			t.Fatal("want error")
		}
	})
}
