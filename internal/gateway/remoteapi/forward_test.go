package remoteapi

import (
	"context"
	"io"
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
}

func (p *panelStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.paths = append(p.paths, r.URL.Path)
	p.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (p *panelStub) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.paths...)
}

func forwardHandler(t *testing.T) (*Handler, *panelStub) {
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
	return &Handler{
		Store:  st,
		Panel:  panel.New(srv.URL, "id", "token", "test"),
		Agents: agents.NewResolver(st, time.Second),
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, stub
}

func callerRequest(path, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	return r.WithContext(context.WithValue(r.Context(), ctxKey{}, testUUID))
}

// The Panel call is made on a context that outlives the agent's request, and
// still reaches the Panel.
func TestInstallResultForwardsToPanel(t *testing.T) {
	h, stub := forwardHandler(t)
	w := httptest.NewRecorder()
	h.installResult(w, callerRequest("/api/remote/servers/"+testUUID+"/install", `{"successful":true}`))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status %d", w.Code)
	}
	if got := stub.seen(); len(got) != 1 || got[0] != "/api/remote/servers/"+testUUID+"/install" {
		t.Fatalf("panel calls %v", got)
	}
}

func TestContainerStatusForwardsToPanel(t *testing.T) {
	h, stub := forwardHandler(t)
	w := httptest.NewRecorder()
	h.containerStatus(w, callerRequest("/api/remote/servers/"+testUUID+"/container/status", `{"data":{"previous_state":"starting","new_state":"running"}}`))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status %d", w.Code)
	}
	if got := stub.seen(); len(got) != 1 || got[0] != "/api/remote/servers/"+testUUID+"/container/status" {
		t.Fatalf("panel calls %v", got)
	}
}

// A usage sample for a server without a pod is dropped quietly.
func TestRecordUsageWithoutPod(t *testing.T) {
	h, _ := forwardHandler(t)
	h.recordUsage(context.Background(), testUUID)
}
