package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/gateway/agents"
	"github.com/Claiyc/pelican-k8s/internal/gateway/config"
	"github.com/Claiyc/pelican-k8s/internal/gateway/panel"
	"github.com/Claiyc/pelican-k8s/internal/gateway/panelapi"
	"github.com/Claiyc/pelican-k8s/internal/gateway/remoteapi"
	"github.com/Claiyc/pelican-k8s/internal/gateway/serversync"
	"github.com/Claiyc/pelican-k8s/internal/gateway/sftprelay"
	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/test/fakepanel"
)

const (
	uuid      = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	ns        = "pelican-servers"
	sysNS     = "pelican-system"
	nodeID    = "nodeid"
	nodeToken = "node-token-secret"
)

// fakeAPIServer is the slice of the Kubernetes API the gateway touches
// directly: discovery plus get/create of Secrets.
type fakeAPIServer struct {
	*httptest.Server
	mu       sync.Mutex
	secrets  map[string]json.RawMessage
	getCode  int // when non-zero, Secret GETs fail with this status
	postCode int // when non-zero, Secret POSTs fail with this status
	posts    int
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()
	f := &fakeAPIServer{secrets: map[string]json.RawMessage{}}
	status := func(w http.ResponseWriter, code int, reason string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": reason, "code": code, "message": reason})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "APIVersions", "versions": []string{"v1"}})
	})
	mux.HandleFunc("/apis", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "APIGroupList", "apiVersion": "v1", "groups": []any{}})
	})
	mux.HandleFunc("/api/v1", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "APIResourceList", "groupVersion": "v1", "resources": []map[string]any{
			{"name": "secrets", "namespaced": true, "kind": "Secret", "verbs": []string{"get", "create", "list", "watch"}},
		}})
	})
	mux.HandleFunc("/api/v1/namespaces/{ns}/secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.getCode != 0 {
			status(w, f.getCode, "InternalError")
			return
		}
		raw, ok := f.secrets[r.PathValue("ns")+"/"+r.PathValue("name")]
		if !ok {
			status(w, http.StatusNotFound, "NotFound")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	})
	mux.HandleFunc("POST /api/v1/namespaces/{ns}/secrets", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.posts++
		if f.postCode != 0 {
			status(w, f.postCode, "Forbidden")
			return
		}
		var sec corev1.Secret
		if err := json.NewDecoder(r.Body).Decode(&sec); err != nil {
			status(w, http.StatusBadRequest, "BadRequest")
			return
		}
		sec.APIVersion, sec.Kind = "v1", "Secret"
		sec.Namespace = r.PathValue("ns")
		sec.ResourceVersion = "1"
		raw, _ := json.Marshal(sec)
		f.secrets[sec.Namespace+"/"+sec.Name] = raw
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(raw)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected API request %s %s", r.Method, r.URL.Path)
		status(w, http.StatusNotFound, "NotFound")
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// restConfig speaks JSON: the fake server does not implement protobuf.
func (f *fakeAPIServer) restConfig() *rest.Config {
	return &rest.Config{Host: f.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json", AcceptContentTypes: "application/json"}}
}

func (f *fakeAPIServer) secret(t *testing.T, key string) *corev1.Secret {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.secrets[key]
	if !ok {
		return nil
	}
	var sec corev1.Secret
	if err := json.Unmarshal(raw, &sec); err != nil {
		t.Fatal(err)
	}
	return &sec
}

func useRestConfig(t *testing.T, rc *rest.Config) {
	t.Helper()
	old := restConfig
	SetRestConfig(rc)
	t.Cleanup(func() { restConfig = old })
}

func testConfig(panelURL string) *config.Config {
	return &config.Config{
		PanelURL: panelURL, NodeTokenID: nodeID, NodeToken: nodeToken,
		ServersNamespace: ns, SystemNamespace: sysNS, DefaultClass: "default",
		SFTPHostKeySecret: "hostkey", AdvertisedVersion: "1.0.0",
		StateCacheTTL: time.Second, ResyncInterval: time.Hour, Timezone: "UTC",
		ListenPanel: "127.0.0.1:0", ListenRemote: "127.0.0.1:0", ListenSFTP: "127.0.0.1:0",
	}
}

// fakeCache stands in for the informer cache.
type fakeCache struct {
	cache.Cache
	synced bool
}

func (c *fakeCache) Start(ctx context.Context) error       { <-ctx.Done(); return nil }
func (c *fakeCache) WaitForCacheSync(context.Context) bool { return c.synced }

// newGateway wires a Gateway the way New does, on a fake client and fake Panel.
func newGateway(t *testing.T, cfg *config.Config, objs ...client.Object) *Gateway {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithStatusSubresource(&v1alpha1.GameServer{}).WithObjects(objs...).Build()
	log := slog.Default()
	st := store.New(c, cfg.ServersNamespace, cfg.DefaultClass)
	p := panel.New(cfg.PanelURL, cfg.NodeTokenID, cfg.NodeToken, cfg.UserAgent())
	res := agents.NewResolver(st, cfg.StateCacheTTL)
	sy := &serversync.Syncer{Store: st, Panel: p, Timezone: cfg.Timezone, Log: log}
	sessions := sftprelay.NewSessions(cfg.NodeToken)
	g := &Gateway{Cfg: cfg, Log: log, Store: st, Panel: p, Agents: res, Sync: sy, cache: &fakeCache{synced: true}}
	g.PanelAPI = &panelapi.Handler{Cfg: cfg, Store: st, Agents: res, Sync: sy, Log: log, Diagnostics: g.diagnostics}
	g.Remote = &remoteapi.Handler{Store: st, Panel: p, Sync: sy, Agents: res, Sftp: sessions, Log: log}
	g.Relay = &sftprelay.Relay{Listen: cfg.ListenSFTP, Panel: p, Store: st, Agents: res, Sessions: sessions, Log: log}
	return g
}

func startedPod(name, ip string) *corev1.Pod {
	started := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status:     corev1.PodStatus{PodIP: ip, ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", Started: &started}}},
	}
}

func gameServer(u string) *v1alpha1.GameServer {
	gs := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: names.ForUUID(u), Namespace: ns}}
	gs.Spec.Panel.UUID = u
	return gs
}

func TestScheme(t *testing.T) {
	s := Scheme()
	if !s.Recognizes(v1alpha1.GroupVersion.WithKind("GameServer")) {
		t.Error("GameServer not registered")
	}
	if !s.Recognizes(v1alpha1.GroupVersion.WithKind("GameServerClass")) {
		t.Error("GameServerClass not registered")
	}
	if !s.Recognizes(corev1.SchemeGroupVersion.WithKind("Pod")) || !s.Recognizes(corev1.SchemeGroupVersion.WithKind("Secret")) {
		t.Error("core kinds not registered")
	}
}

func TestSetRestConfig(t *testing.T) {
	rc := &rest.Config{Host: "https://example.invalid"}
	useRestConfig(t, rc)
	if clientRestConfig(nil) != rc {
		t.Fatal("rest config not recorded")
	}
}

func TestNew(t *testing.T) {
	api := newFakeAPIServer(t)
	for _, tc := range []struct {
		name     string
		metallb  bool
		wantPool bool
	}{{"without metallb", false, false}, {"with metallb", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig("http://panel.invalid")
			cfg.MetalLBPools = tc.metallb
			cfg.MetalLBPoolNames = []string{"pool-a"}
			cfg.MetalLBMaxAddresses = 7
			g, err := New(context.Background(), cfg, api.restConfig(), slog.Default())
			if err != nil {
				t.Fatal(err)
			}
			if g.Store == nil || g.Panel == nil || g.Agents == nil || g.Sync == nil || g.Remote == nil || g.Relay == nil || g.PanelAPI == nil || g.cache == nil {
				t.Fatalf("incomplete wiring: %+v", g)
			}
			if g.PanelAPI.WS == nil {
				t.Error("websocket proxy not wired")
			}
			if (g.PanelAPI.Pools != nil) != tc.wantPool {
				t.Errorf("Pools wired = %v, want %v", g.PanelAPI.Pools != nil, tc.wantPool)
			}
			if g.Relay.Listen != cfg.ListenSFTP || g.Store.Namespace != ns || g.Store.DefaultClass != "default" {
				t.Errorf("relay/store settings: %+v %+v", g.Relay, g.Store)
			}
		})
	}
}

func TestNewInvalidRestConfig(t *testing.T) {
	cfg := testConfig("http://panel.invalid")
	if _, err := New(context.Background(), cfg, &rest.Config{Host: "://bad host"}, slog.Default()); err == nil {
		t.Fatal("want error for an unusable rest config")
	}
}

func TestEnsureHostKey(t *testing.T) {
	ctx := context.Background()
	key := sysNS + "/hostkey"

	t.Run("generates and stores a key", func(t *testing.T) {
		api := newFakeAPIServer(t)
		useRestConfig(t, api.restConfig())
		g := newGateway(t, testConfig("http://panel.invalid"))
		signer, err := g.ensureHostKey(ctx)
		if err != nil {
			t.Fatal(err)
		}
		sec := api.secret(t, key)
		if sec == nil || len(sec.Data["id_ed25519"]) == 0 {
			t.Fatalf("secret not stored: %+v", sec)
		}
		if sec.Labels["app.kubernetes.io/part-of"] != "pelican-k8s" {
			t.Errorf("labels = %v", sec.Labels)
		}
		// A second call (another replica, or a restart) reuses the stored key.
		again, err := g.ensureHostKey(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if string(again.PublicKey().Marshal()) != string(signer.PublicKey().Marshal()) {
			t.Error("host key changed between calls")
		}
		api.mu.Lock()
		posts := api.posts
		api.mu.Unlock()
		if posts != 1 {
			t.Errorf("secret created %d times", posts)
		}
	})

	t.Run("lookup failure", func(t *testing.T) {
		api := newFakeAPIServer(t)
		api.getCode = http.StatusInternalServerError
		useRestConfig(t, api.restConfig())
		g := newGateway(t, testConfig("http://panel.invalid"))
		if _, err := g.ensureHostKey(ctx); err == nil {
			t.Fatal("want error")
		}
	})

	t.Run("create failure", func(t *testing.T) {
		api := newFakeAPIServer(t)
		api.postCode = http.StatusForbidden
		useRestConfig(t, api.restConfig())
		g := newGateway(t, testConfig("http://panel.invalid"))
		if _, err := g.ensureHostKey(ctx); err == nil {
			t.Fatal("want error")
		}
	})

	t.Run("stored key is corrupt", func(t *testing.T) {
		api := newFakeAPIServer(t)
		raw, _ := json.Marshal(corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: metav1.ObjectMeta{Name: "hostkey", Namespace: sysNS}, Data: map[string][]byte{"id_ed25519": []byte("junk")}})
		api.secrets[key] = raw
		useRestConfig(t, api.restConfig())
		g := newGateway(t, testConfig("http://panel.invalid"))
		if _, err := g.ensureHostKey(ctx); err == nil {
			t.Fatal("want parse error")
		}
	})
}

func TestDiagnostics(t *testing.T) {
	fp := fakepanel.New(nodeID, nodeToken)
	ps := httptest.NewServer(fp.Handler())
	defer ps.Close()

	ready := "2a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	gsReady := gameServer(ready)
	gsReady.Status.Phase = "Running"
	gsReady.Status.Process.State = "running"
	gsReady.Spec.Power.Desired = v1alpha1.PowerRunning
	gsDown := gameServer(uuid)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.AgentSecret(ready), Namespace: ns}, Data: map[string][]byte{"token_id": []byte("a"), "token": []byte("b")}}
	game := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: names.Pod(ready), Namespace: ns}, Spec: corev1.PodSpec{NodeName: "node-a"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	g := newGateway(t, testConfig(ps.URL), gsReady, gsDown, startedPod(names.AgentPod(ready), "10.0.0.5"), game, secret)

	out := g.diagnostics(context.Background())
	for _, want := range []string{
		"pelican-k8s gateway",
		"panel: " + ps.URL,
		"node token id: " + nodeID,
		"servers namespace: " + ns,
		"panel reachable: yes",
		"gameservers: 2",
		ready + " phase=Running process=running desired=Running agent=yes game=Running@node-a",
		uuid + " phase=",
		"agent=no game=none",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnostics missing %q:\n%s", want, out)
		}
	}

	t.Run("panel unreachable", func(t *testing.T) {
		g := newGateway(t, testConfig("http://127.0.0.1:1"))
		out := g.diagnostics(context.Background())
		if !strings.Contains(out, "panel reachable: no (") || !strings.Contains(out, "gameservers: 0") {
			t.Errorf("unexpected report:\n%s", out)
		}
	})
}

func TestResetServersState(t *testing.T) {
	t.Run("resets once nothing is in flight", func(t *testing.T) {
		fp := fakepanel.New(nodeID, nodeToken)
		ps := httptest.NewServer(fp.Handler())
		defer ps.Close()
		g := newGateway(t, testConfig(ps.URL), gameServer(uuid))
		done := make(chan struct{})
		go func() { g.resetServersState(context.Background()); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("reset never completed")
		}
		if n := len(fp.CallsMatching("/servers/reset")); n != 1 {
			t.Fatalf("reset calls = %d, want 1", n)
		}
	})

	busy := map[string]func(*v1alpha1.GameServer){
		"install pending": func(gs *v1alpha1.GameServer) {
			gs.Spec.Install.Generation = 2
			gs.Status.Install.ObservedGeneration = 1
		},
		"install running": func(gs *v1alpha1.GameServer) { gs.Status.Install.Result = v1alpha1.InstallRunning },
		"restore in flight": func(gs *v1alpha1.GameServer) {
			gs.Status.Backups.Pending = []v1alpha1.PendingBackup{{UUID: "b-1", Agent: "agent-pod-1/0"}}
		},
	}
	// The agent pod whose instance runs the restore.
	agentPod := func() *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: names.AgentPod(uuid), Namespace: ns, UID: "agent-pod-1"}}
	}
	for name, mutate := range busy {
		t.Run("waits while "+name, func(t *testing.T) {
			fp := fakepanel.New(nodeID, nodeToken)
			ps := httptest.NewServer(fp.Handler())
			defer ps.Close()
			gs := gameServer(uuid)
			mutate(gs)
			g := newGateway(t, testConfig(ps.URL), gs, agentPod())
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { g.resetServersState(ctx); close(done) }()
			time.Sleep(100 * time.Millisecond)
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("did not stop on cancel")
			}
			if n := len(fp.CallsMatching("/servers/reset")); n != 0 {
				t.Fatalf("reset sent while busy (%d calls)", n)
			}
		})
	}

	// A restore of an earlier agent instance ended with it.
	t.Run("resets after a restore of a replaced agent", func(t *testing.T) {
		fp := fakepanel.New(nodeID, nodeToken)
		ps := httptest.NewServer(fp.Handler())
		defer ps.Close()
		gs := gameServer(uuid)
		gs.Status.Backups.Pending = []v1alpha1.PendingBackup{{UUID: "b-1", Agent: "agent-pod-0/0"}}
		g := newGateway(t, testConfig(ps.URL), gs, agentPod())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		g.resetServersState(ctx)
		if n := len(fp.CallsMatching("/servers/reset")); n != 1 {
			t.Fatalf("reset calls = %d, want 1", n)
		}
	})

	t.Run("panel failure keeps retrying until canceled", func(t *testing.T) {
		g := newGateway(t, testConfig("http://127.0.0.1:1"))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { g.resetServersState(ctx); close(done) }()
		time.Sleep(100 * time.Millisecond)
		select {
		case <-done:
			t.Fatal("must not give up after a failed reset")
		default:
		}
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("did not stop on cancel")
		}
	})
}

func TestRun(t *testing.T) {
	t.Run("serves and shuts down on cancel", func(t *testing.T) {
		api := newFakeAPIServer(t)
		useRestConfig(t, api.restConfig())
		fp := fakepanel.New(nodeID, nodeToken)
		ps := httptest.NewServer(fp.Handler())
		defer ps.Close()
		g := newGateway(t, testConfig(ps.URL))
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() { errc <- g.Run(ctx) }()

		// The host key is created and handed to the relay before serving.
		deadline := time.Now().Add(5 * time.Second)
		for api.secret(t, sysNS+"/hostkey") == nil || len(fp.CallsMatching("/servers/reset")) == 0 {
			if time.Now().After(deadline) {
				t.Fatal("gateway did not finish starting")
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		select {
		case err := <-errc:
			if err != nil {
				t.Fatalf("Run = %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("Run did not return")
		}
		if g.Relay.HostKey == nil {
			t.Error("host key not set on the relay")
		}
	})

	t.Run("cache sync timeout", func(t *testing.T) {
		g := newGateway(t, testConfig("http://panel.invalid"))
		g.cache = &fakeCache{synced: false}
		err := g.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "cache sync") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("host key failure", func(t *testing.T) {
		api := newFakeAPIServer(t)
		api.getCode = http.StatusInternalServerError
		useRestConfig(t, api.restConfig())
		g := newGateway(t, testConfig("http://panel.invalid"))
		err := g.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "sftp host key") {
			t.Fatalf("err = %v", err)
		}
	})

	for _, which := range []string{"panel", "remote"} {
		t.Run(which+" listener failure is returned", func(t *testing.T) {
			api := newFakeAPIServer(t)
			useRestConfig(t, api.restConfig())
			taken, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer taken.Close()
			fp := fakepanel.New(nodeID, nodeToken)
			ps := httptest.NewServer(fp.Handler())
			defer ps.Close()
			cfg := testConfig(ps.URL)
			if which == "panel" {
				cfg.ListenPanel = taken.Addr().String()
			} else {
				cfg.ListenRemote = taken.Addr().String()
			}
			g := newGateway(t, cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			err = g.Run(ctx)
			want := which + " api"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want mention of %q", err, want)
			}
		})
	}

	t.Run("sftp relay failure is returned", func(t *testing.T) {
		api := newFakeAPIServer(t)
		useRestConfig(t, api.restConfig())
		taken, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer taken.Close()
		fp := fakepanel.New(nodeID, nodeToken)
		ps := httptest.NewServer(fp.Handler())
		defer ps.Close()
		cfg := testConfig(ps.URL)
		cfg.ListenSFTP = taken.Addr().String()
		g := newGateway(t, cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err = g.Run(ctx)
		if err == nil || !strings.Contains(err.Error(), "sftp relay") {
			t.Fatalf("err = %v", err)
		}
	})
}
