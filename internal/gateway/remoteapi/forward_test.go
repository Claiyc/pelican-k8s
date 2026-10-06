package remoteapi

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/gateway/agents"
	"github.com/Claiyc/pelican-k8s/internal/gateway/panel"
	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
)

const testUUID = "11111111-2222-3333-4444-555555555555"

// panelStub records the paths the gateway forwards to the Panel.
type panelStub struct {
	mu    sync.Mutex
	paths []string
	// onRequest runs when a forwarded call arrives, before the stub answers.
	onRequest func()
}

func (p *panelStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.paths = append(p.paths, r.URL.Path)
	hook := p.onRequest
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (p *panelStub) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.paths...)
}

// logBuffer is a slog sink the test can read while handlers log.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func forwardHandler(t *testing.T) (*Handler, *panelStub, *logBuffer) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	gs := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: names.ForUUID(testUUID), Namespace: "pelican-servers"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gs).WithStatusSubresource(gs).Build()
	st := store.New(c, "pelican-servers", "default")
	stub := &panelStub{}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	logs := &logBuffer{}
	return &Handler{
		Store:  st,
		Panel:  panel.New(srv.URL, "id", "token", "test"),
		Agents: agents.NewResolver(st, time.Second),
		Log:    slog.New(slog.NewTextHandler(logs, nil)),
	}, stub, logs
}

// callerRequest returns a request from the agent that owns testUUID, and the
// function that cancels it the way an agent client timeout would.
func callerRequest(path, body string) (*http.Request, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, testUUID))
	return httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx), cancel
}

// The agent's request is cancelled as soon as the Panel call arrives. The call
// is made on a context that outlives the request, so it still completes.
func testForwardOutlivesRequest(t *testing.T, call func(*Handler, http.ResponseWriter, *http.Request), path, body string) {
	t.Helper()
	h, stub, logs := forwardHandler(t)
	r, cancel := callerRequest(path, body)
	defer cancel()
	stub.onRequest = cancel
	w := httptest.NewRecorder()
	call(h, w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status %d", w.Code)
	}
	if got := stub.seen(); len(got) != 1 || got[0] != path {
		t.Fatalf("panel calls %v", got)
	}
	if r.Context().Err() == nil {
		t.Fatal("the request was not cancelled while forwarding")
	}
	if strings.Contains(logs.String(), "forwarding") {
		t.Fatalf("the forward failed: %s", logs.String())
	}
}

func TestInstallResultForwardOutlivesRequest(t *testing.T) {
	path := "/api/remote/servers/" + testUUID + "/install"
	testForwardOutlivesRequest(t, (*Handler).installResult, path, `{"successful":true}`)
}

func TestContainerStatusForwardOutlivesRequest(t *testing.T) {
	path := "/api/remote/servers/" + testUUID + "/container/status"
	testForwardOutlivesRequest(t, (*Handler).containerStatus, path, `{"data":{"previous_state":"starting","new_state":"running"}}`)
}

// A usage sample for a server without a pod is dropped quietly.
func TestRecordUsageWithoutPod(t *testing.T) {
	h, _, _ := forwardHandler(t)
	h.recordUsage(context.Background(), testUUID)
}
