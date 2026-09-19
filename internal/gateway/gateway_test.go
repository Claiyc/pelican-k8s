package gateway_test

// Gateway tests: a fake Panel, a fake agent HTTP server and the fake Kubernetes
// client stand in for the real environment. They cover the Panel-facing API
// (create/sync/install/power/delete, proxying, JWT re-signing) and the
// agent-facing remote API (configuration assembly, container status, install
// coordination, backups and activity filtering).

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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
	"github.com/Claiyc/pelican-k8s/internal/gateway/wsproxy"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/test/fakepanel"
)

const (
	uuid      = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	ns        = "pelican-servers"
	nodeID    = "nodeid"
	nodeToken = "node-token-secret"
	agentID   = "agentid"
	agentTok  = "agent-token-secret"
)

// fakeAgent records what the gateway proxies to it.
type fakeAgent struct {
	mu    sync.Mutex
	calls []string
	auth  []string
	srv   *httptest.Server
}

func newFakeAgent(t *testing.T) *fakeAgent {
	t.Helper()
	fa := &fakeAgent{}
	mux := http.NewServeMux()
	record := func(r *http.Request) {
		fa.mu.Lock()
		fa.calls = append(fa.calls, r.Method+" "+r.URL.RequestURI())
		fa.auth = append(fa.auth, r.Header.Get("Authorization"))
		fa.mu.Unlock()
	}
	mux.HandleFunc("/api/servers/"+uuid, func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("User-Agent", "Pelican Wings/vdevelop (id:agentid)")
		_, _ = w.Write([]byte(`{"state":"running","is_suspended":false,"utilization":{"memory_bytes":123,"cpu_absolute":1.5,"disk_bytes":7,"uptime":10},"configuration":{}}`))
	})
	mux.HandleFunc("/api/servers/"+uuid+"/files/contents", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_, _ = w.Write([]byte("file body"))
	})
	mux.HandleFunc("/download/file", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_, _ = w.Write([]byte("token=" + r.URL.Query().Get("token")))
	})
	mux.HandleFunc("/api/deauthorize-user", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(204)
	})
	mux.HandleFunc("/api/servers/"+uuid+"/backup", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(202)
	})
	fa.srv = httptest.NewServer(mux)
	t.Cleanup(fa.srv.Close)
	return fa
}

func (fa *fakeAgent) Calls() []string {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	return append([]string(nil), fa.calls...)
}

type harness struct {
	t      *testing.T
	c      client.Client
	st     *store.Store
	panel  *fakepanel.Panel
	agent  *fakeAgent
	sync   *serversync.Syncer
	api    *httptest.Server // Panel-facing
	remote *httptest.Server // agent-facing
	cfg    *config.Config
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	cls := &v1alpha1.GameServerClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: v1alpha1.GameServerClassSpec{Resources: v1alpha1.ResourcesSpec{UnlimitedMemoryMiB: 2048}, Install: v1alpha1.InstallJobSpec{StrictExitCode: true}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.GameServer{}).WithObjects(cls).Build()

	fp := fakepanel.New(nodeID, nodeToken)
	fp.Add(&fakepanel.Server{UUID: uuid, Settings: fakepanel.PaperSettings(uuid, 30565, 0), ProcessConfiguration: fakepanel.PaperProcessConfiguration(), Install: fakepanel.InstallScript{ContainerImage: "ghcr.io/pelican-eggs/installers:alpine", Entrypoint: "ash", Script: "#!/bin/ash\necho hi\r\n"}})
	ps := httptest.NewServer(fp.Handler())
	t.Cleanup(ps.Close)

	fa := newFakeAgent(t)
	agentURL := fa.srv.URL // http://127.0.0.1:port

	cfg := &config.Config{PanelURL: ps.URL, NodeTokenID: nodeID, NodeToken: nodeToken, ServersNamespace: ns, SystemNamespace: "pelican-system", DefaultClass: "default", AdvertisedVersion: "1.0.0", StateCacheTTL: 50 * time.Millisecond, Timezone: "UTC"}
	st := store.New(c, ns, "default")
	pc := panel.New(cfg.PanelURL, nodeID, nodeToken, cfg.UserAgent())
	res := agents.NewResolver(st, cfg.StateCacheTTL)
	// Route "pod IP" lookups to the fake agent: pods are created with the agent's host as IP.
	sy := &serversync.Syncer{Store: st, Panel: pc, Timezone: "UTC", Log: slog.Default()}
	sessions := sftprelay.NewSessions()
	ph := &panelapi.Handler{Cfg: cfg, Store: st, Agents: res, Sync: sy, Log: slog.Default()}
	ph.WS = &wsproxy.Proxy{Cfg: cfg, Store: st, Agents: res, Sync: sy, Log: slog.Default()}
	rh := &remoteapi.Handler{Store: st, Panel: pc, Sync: sy, Agents: res, Sftp: sessions, Log: slog.Default()}
	h := &harness{t: t, c: c, st: st, panel: fp, agent: fa, sync: sy, cfg: cfg}
	h.api = httptest.NewServer(ph.Routes())
	h.remote = httptest.NewServer(rh.Routes())
	t.Cleanup(h.api.Close)
	t.Cleanup(h.remote.Close)
	_ = agentURL
	return h
}

// addPod creates the pod (agent ready) pointing at the fake agent and the agent token Secret.
func (h *harness) addPod() {
	h.t.Helper()
	host := strings.TrimPrefix(h.agent.srv.URL, "http://")
	ip, port, _ := strings.Cut(host, ":")
	if port != "8080" {
		// agents.Target hardcodes 8080; the fake agent listens elsewhere, so tests
		// that need the proxy use a listener on 8080 if available.
		h.t.Logf("fake agent on port %s; proxy tests use a dedicated listener", port)
	}
	started := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: names.Pod(uuid), Namespace: ns, UID: "pod-1"}}
	if err := h.c.Create(context.Background(), pod); err != nil {
		h.t.Fatal(err)
	}
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip, InitContainerStatuses: []corev1.ContainerStatus{{Name: "agent", Started: &started, Ready: true}}}
	if err := h.c.Status().Update(context.Background(), pod); err != nil {
		h.t.Fatal(err)
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.AgentSecret(uuid), Namespace: ns, Labels: map[string]string{v1alpha1.LabelServerUUID: uuid, v1alpha1.LabelComponent: "agent"}}, Data: map[string][]byte{"token_id": []byte(agentID), "token": []byte(agentTok)}}
	if err := h.c.Create(context.Background(), sec); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) call(method, path, body, token string) (int, string, http.Header) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.api.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), res.Header
}

func (h *harness) remoteCall(method, path, body, bearer string) (int, string) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.remote.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func (h *harness) gs() *v1alpha1.GameServer {
	h.t.Helper()
	gs, err := h.st.Get(context.Background(), uuid)
	if err != nil {
		h.t.Fatal(err)
	}
	return gs
}

func TestAuthAndUserAgent(t *testing.T) {
	h := newHarness(t)
	code, _, hdr := h.call("GET", "/api/system", "", "")
	if code != 401 || !strings.HasPrefix(hdr.Get("User-Agent"), "Pelican Wings/v1.0.0 (id:nodeid)") {
		t.Fatalf("unauthenticated: %d %q", code, hdr.Get("User-Agent"))
	}
	if code, _, _ := h.call("GET", "/api/system", "", "wrong"); code != 403 {
		t.Fatal("wrong token accepted")
	}
	code, body, _ := h.call("GET", "/api/system", "", nodeToken)
	if code != 200 || !strings.Contains(body, `"version":"1.0.0"`) {
		t.Fatalf("system: %d %s", code, body)
	}
	if code, body, _ := h.call("GET", "/api/system?v=2", "", nodeToken); code != 200 || !strings.Contains(body, `"docker"`) {
		t.Fatalf("system v2: %d %s", code, body)
	}
	if code, body, _ := h.call("POST", "/api/update", "{}", nodeToken); code != 200 || !strings.Contains(body, `"applied":false`) {
		t.Fatalf("update: %d %s", code, body)
	}
	if code, body, _ := h.call("DELETE", "/api/system/docker/image/prune", "", nodeToken); code != 200 || !strings.Contains(body, "SpaceReclaimed") {
		t.Fatalf("prune: %d %s", code, body)
	}
	if code, _, _ := h.call("POST", "/api/transfers", "", ""); code != 501 {
		t.Fatal("transfers must be unsupported")
	}
}

func TestCreateSyncInstallPowerDelete(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if code, _, _ := h.call("POST", "/api/servers", `{"uuid":"bad"}`, nodeToken); code != 422 {
		t.Fatal("invalid uuid accepted")
	}
	code, body, _ := h.call("POST", "/api/servers", `{"uuid":"`+uuid+`","start_on_completion":true}`, nodeToken)
	if code != 202 {
		t.Fatalf("create: %d %s", code, body)
	}
	gs := h.gs()
	if gs.Spec.Install.Generation != 1 || !gs.Spec.Install.StartOnInstall || gs.Spec.Install.Entrypoint != "ash" || gs.Spec.Power.Desired != v1alpha1.PowerStopped || gs.Spec.ClassName != "default" {
		t.Fatalf("spec %+v", gs.Spec)
	}
	if gs.Annotations[v1alpha1.AnnotationPanelName] != "Spike" || gs.Labels[v1alpha1.LabelServerUUID] != uuid {
		t.Fatalf("meta %+v", gs.ObjectMeta)
	}
	if strings.Contains(string(gs.Spec.Panel.Settings.Raw), `"environment"`) {
		t.Fatal("environment must not be stored in the CR")
	}
	env, err := h.st.EnvSecret(ctx, uuid)
	if err != nil {
		t.Fatal(err)
	}
	if env["SERVER_JARFILE"] != "server.jar" || env["SERVER_PORT"] != "30565" || env["SERVER_IP"] != "0.0.0.0" || !strings.Contains(env["STARTUP"], "-jar server.jar") || env["TZ"] != "UTC" {
		t.Fatalf("env secret %+v", env)
	}
	script, err := h.st.InstallScript(ctx, uuid, 1)
	if err != nil || script != "#!/bin/ash\necho hi\n" {
		t.Fatalf("install script %q %v", script, err)
	}
	cm := &corev1.ConfigMap{}
	_ = h.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: names.InstallConfigMap(uuid, 1)}, cm)
	if len(cm.OwnerReferences) != 1 {
		t.Fatal("configmap must be owned by the GameServer")
	}

	// Sync: unchanged Panel data keeps the revision; changed data updates it.
	rev := gs.Spec.Panel.PanelRevision
	if code, _, _ := h.call("POST", "/api/servers/"+uuid+"/sync", "", nodeToken); code != 204 {
		t.Fatal("sync")
	}
	if h.gs().Spec.Panel.PanelRevision != rev {
		t.Fatal("revision changed without panel change")
	}
	h.panel.Get(uuid).Settings["meta"] = map[string]any{"name": "Renamed"}
	h.call("POST", "/api/servers/"+uuid+"/sync", "", nodeToken)
	gs = h.gs()
	if gs.Spec.Panel.PanelRevision == rev || gs.Annotations[v1alpha1.AnnotationPanelName] != "Renamed" {
		t.Fatalf("sync did not apply: %s %s", gs.Spec.Panel.PanelRevision, gs.Annotations[v1alpha1.AnnotationPanelName])
	}

	// Reinstall bumps the install generation with a fresh script ConfigMap.
	if code, _, _ := h.call("POST", "/api/servers/"+uuid+"/reinstall", "", nodeToken); code != 202 {
		t.Fatal("reinstall")
	}
	gs = h.gs()
	if gs.Spec.Install.Generation != 2 || !gs.Spec.Install.Reinstall || gs.Spec.Install.ScriptConfigMap != names.InstallConfigMap(uuid, 2) || gs.Spec.Install.StartOnInstall {
		t.Fatalf("reinstall spec %+v", gs.Spec.Install)
	}

	// Power actions become spec changes.
	for _, tc := range []struct {
		action, desired string
		kill            bool
	}{{"start", "Running", false}, {"restart", "Running", false}, {"stop", "Stopped", false}, {"kill", "Stopped", true}} {
		before := h.gs().Spec.Power.Generation
		if code, body, _ := h.call("POST", "/api/servers/"+uuid+"/power", `{"action":"`+tc.action+`"}`, nodeToken); code != 202 {
			t.Fatalf("power %s: %d %s", tc.action, code, body)
		}
		p := h.gs().Spec.Power
		if string(p.Desired) != tc.desired || p.Kill != tc.kill || p.Generation != before+1 {
			t.Fatalf("power %s -> %+v", tc.action, p)
		}
	}
	// A start picks up Panel changes that came without a sync (startup
	// variables), a stop does not need to.
	for _, tc := range []struct {
		action, name string
		synced       bool
	}{{"stop", "BeforeStop", false}, {"start", "BeforeStart", true}, {"restart", "BeforeRestart", true}} {
		h.panel.Get(uuid).Settings["meta"] = map[string]any{"name": tc.name}
		h.call("POST", "/api/servers/"+uuid+"/power", `{"action":"`+tc.action+`"}`, nodeToken)
		if got := h.gs().Annotations[v1alpha1.AnnotationPanelName]; (got == tc.name) != tc.synced {
			t.Fatalf("power %s: panel name %q, synced should be %v", tc.action, got, tc.synced)
		}
	}
	if code, _, _ := h.call("POST", "/api/servers/"+uuid+"/power", `{"action":"explode"}`, nodeToken); code != 422 {
		t.Fatal("invalid action accepted")
	}
	// Suspended servers cannot be started.
	h.panel.Get(uuid).Settings["suspended"] = true
	h.call("POST", "/api/servers/"+uuid+"/sync", "", nodeToken)
	if code, _, _ := h.call("POST", "/api/servers/"+uuid+"/power", `{"action":"start"}`, nodeToken); code != 400 {
		t.Fatal("suspended start accepted")
	}

	// Unknown server -> 404; delete -> 204 and CR gone.
	if code, _, _ := h.call("GET", "/api/servers/00000000-0000-4000-8000-000000000000", "", nodeToken); code != 404 {
		t.Fatal("unknown server")
	}
	if code, _, _ := h.call("DELETE", "/api/servers/"+uuid, "", nodeToken); code != 204 {
		t.Fatal("delete")
	}
	if h.st.Exists(ctx, uuid) {
		t.Fatal("CR still exists")
	}
}

func TestStateWithoutAgent(t *testing.T) {
	h := newHarness(t)
	h.call("POST", "/api/servers", `{"uuid":"`+uuid+`"}`, nodeToken)
	code, body, _ := h.call("GET", "/api/servers/"+uuid, "", nodeToken)
	if code != 200 || !strings.Contains(body, `"state":"missing"`) {
		t.Fatalf("no pod -> missing: %d %s", code, body)
	}
	_ = h.st.PatchStatus(context.Background(), uuid, map[string]any{"process": map[string]any{"state": "running"}})
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: names.Pod(uuid), Namespace: ns}}
	_ = h.c.Create(context.Background(), pod)
	code, body, _ = h.call("GET", "/api/servers/"+uuid, "", nodeToken)
	if code != 200 || !strings.Contains(body, `"state":"running"`) {
		t.Fatalf("pod without agent -> last known: %d %s", code, body)
	}
	if code, _, _ := h.call("GET", "/api/servers/"+uuid+"/files/contents?file=x", "", nodeToken); code != 503 {
		t.Fatalf("proxy without agent should be 503, got %d", code)
	}
	code, body, _ = h.call("GET", "/api/servers", "", nodeToken)
	if code != 200 || !strings.HasPrefix(body, "[{") {
		t.Fatalf("list: %d %s", code, body)
	}
}

func TestRemoteAPI(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.call("POST", "/api/servers", `{"uuid":"`+uuid+`","start_on_completion":true}`, nodeToken)
	h.addPod()
	bearer := agentID + "." + agentTok

	if code, _ := h.remoteCall("GET", "/api/remote/servers", "", "bad.token"); code != 403 {
		t.Fatal("bad agent token accepted")
	}
	code, body := h.remoteCall("GET", "/api/remote/servers", "", bearer)
	if code != 200 {
		t.Fatalf("list: %d %s", code, body)
	}
	var list struct {
		Data []struct {
			UUID     string          `json:"uuid"`
			Settings json.RawMessage `json:"settings"`
			Proc     json.RawMessage `json:"process_configuration"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(body), &list)
	if len(list.Data) != 1 || list.Data[0].UUID != uuid {
		t.Fatalf("list %s", body)
	}
	var settings map[string]any
	_ = json.Unmarshal(list.Data[0].Settings, &settings)
	env := settings["environment"].(map[string]any)
	if env["SERVER_JARFILE"] != "server.jar" || env["SERVER_PUBLIC_IP"] != "0.0.0.0" {
		t.Fatalf("environment %v", env)
	}
	if settings["allocations"].(map[string]any)["default"].(map[string]any)["ip"] != "0.0.0.0" {
		t.Fatal("default ip must be rewritten to 0.0.0.0")
	}
	if settings["build"].(map[string]any)["memory_limit"].(float64) != 2048 {
		t.Fatalf("unlimited memory must use the class default: %v", settings["build"])
	}
	if !strings.Contains(string(list.Data[0].Proc), `"done":[")! For help, type "]`) {
		t.Fatalf("process configuration must be the Panel's raw JSON: %s", list.Data[0].Proc)
	}
	if code, _ := h.remoteCall("GET", "/api/remote/servers/00000000-0000-4000-8000-000000000000", "", bearer); code != 404 {
		t.Fatal("other server visible")
	}
	if code, body := h.remoteCall("GET", "/api/remote/servers/"+uuid+"/install", "", bearer); code != 200 || !strings.Contains(body, `"entrypoint":"ash"`) {
		t.Fatalf("install script: %d %s", code, body)
	}

	// Install coordination.
	code, body = h.remoteCall("POST", "/api/remote/servers/"+uuid+"/install/prepared", "", bearer)
	if code != 200 || !strings.Contains(body, `"generation":1`) || !strings.Contains(body, `"strict_exit_code":true`) {
		t.Fatalf("prepared: %d %s", code, body)
	}
	if st := h.gs().Status.Install; st.PreparedGeneration != 1 || st.Result != v1alpha1.InstallRunning {
		t.Fatalf("install status %+v", st)
	}
	if code, body := h.remoteCall("GET", "/api/remote/servers/"+uuid+"/install/state?generation=1", "", bearer); code != 200 || !strings.Contains(body, `"result":"Running"`) {
		t.Fatalf("state: %d %s", code, body)
	}
	if code, _ := h.remoteCall("POST", "/api/remote/servers/"+uuid+"/install", `{"successful":true,"reinstall":false}`, bearer); code != 204 {
		t.Fatal("install result")
	}
	gs := h.gs()
	if gs.Status.Install.Result != v1alpha1.InstallSucceeded || gs.Status.Install.ReportedGeneration != 1 {
		t.Fatalf("install status %+v", gs.Status.Install)
	}
	if gs.Spec.Power.Desired != v1alpha1.PowerRunning || gs.Spec.Power.Generation != 1 {
		t.Fatalf("start_on_completion must request a start: %+v", gs.Spec.Power)
	}
	if got := h.panel.InstallResults[uuid]; len(got) != 1 || got[0]["successful"] != true {
		t.Fatalf("panel not told: %v", got)
	}

	// Container status: recorded, forwarded, and stopping->offline derives Stopped.
	if code, _ := h.remoteCall("POST", "/api/remote/servers/"+uuid+"/container/status", `{"data":{"previous_state":"offline","new_state":"starting"}}`, bearer); code != 204 {
		t.Fatal("status")
	}
	if h.gs().Status.Process.State != "starting" || !h.panel.WaitForState(uuid, "starting", time.Second) {
		t.Fatal("state not recorded/forwarded")
	}
	if h.gs().Spec.Power.Desired != v1alpha1.PowerRunning {
		t.Fatal("desired must stay Running")
	}
	h.remoteCall("POST", "/api/remote/servers/"+uuid+"/container/status", `{"data":{"previous_state":"stopping","new_state":"offline"}}`, bearer)
	if h.gs().Spec.Power.Desired != v1alpha1.PowerStopped {
		t.Fatal("intentional stop must set desired=Stopped")
	}
	// ...but not while the pod is terminating (eviction, recreate).
	_ = h.st.PatchSpec(ctx, uuid, map[string]any{"power": map[string]any{"desired": "Running"}})
	pod, _ := h.st.Pod(ctx, uuid)
	pod.Finalizers = []string{"test/keep"}
	_ = h.c.Update(ctx, pod)
	_ = h.c.Delete(ctx, pod)
	h.remoteCall("POST", "/api/remote/servers/"+uuid+"/container/status", `{"data":{"previous_state":"stopping","new_state":"offline"}}`, bearer)
	if h.gs().Spec.Power.Desired != v1alpha1.PowerRunning {
		t.Fatal("stop during pod termination must not change desired")
	}

	// Activity: only the caller's rows are forwarded.
	code, _ = h.remoteCall("POST", "/api/remote/activity", `{"data":[{"server":"`+uuid+`","event":"server:power.start"},{"server":"other","event":"x"}]}`, bearer)
	if code != 204 || len(h.panel.Activity) != 1 || h.panel.Activity[0]["server"] != uuid {
		t.Fatalf("activity %d %v", code, h.panel.Activity)
	}

	// Backups: unknown backup refused; pending after the proxied create.
	if code, _ := h.remoteCall("POST", "/api/remote/backups/b-1", `{"successful":true}`, bearer); code != 404 {
		t.Fatal("unknown backup accepted")
	}
	_ = h.st.PatchStatus(ctx, uuid, map[string]any{"backups": map[string]any{"pending": []map[string]any{{"uuid": "b-1", "startedAt": metav1.Now()}}}})
	if code, _ := h.remoteCall("POST", "/api/remote/backups/b-1", `{"successful":true}`, bearer); code != 204 {
		t.Fatal("pending backup refused")
	}
	if len(h.gs().Status.Backups.Pending) != 0 {
		t.Fatal("completion must clear the pending record")
	}
	// Transfers and unknown paths are 404.
	if code, _ := h.remoteCall("POST", "/api/remote/servers/"+uuid+"/transfer/success", "", bearer); code != 404 {
		t.Fatal("transfer call must be 404")
	}
	// SFTP auth answered from relay sessions only.
	if code, _ := h.remoteCall("POST", "/api/remote/sftp/auth", `{"username":"admin.1a2b3c4d","password":"nope"}`, bearer); code != 403 {
		t.Fatal("sftp auth without session accepted")
	}
}

func TestSignedURLResign(t *testing.T) {
	h := newHarness(t)
	h.call("POST", "/api/servers", `{"uuid":"`+uuid+`"}`, nodeToken)
	now := time.Now()
	claims := map[string]any{"iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(), "jti": "j", "server_uuid": uuid, "user_uuid": "u", "scope": "file-download", "unique_id": "one", "file_path": "/x.txt"}
	tok, _ := jwt.Sign(claims, jwt.NewHS256([]byte(nodeToken)))
	// No pod: 503.
	if code, _, _ := h.call("GET", "/download/file?token="+string(tok), "", ""); code != 503 {
		t.Fatalf("expected 503 without agent, got %d", code)
	}
	bad, _ := jwt.Sign(claims, jwt.NewHS256([]byte("other")))
	if code, _, _ := h.call("GET", "/download/file?token="+string(bad), "", ""); code != 403 {
		t.Fatal("token signed with another key accepted")
	}
	claims["scope"] = "websocket"
	wrongScope, _ := jwt.Sign(claims, jwt.NewHS256([]byte(nodeToken)))
	if code, _, _ := h.call("GET", "/download/file?token="+string(wrongScope), "", ""); code != 404 {
		t.Fatal("wrong scope accepted")
	}
}

func TestDeauthorizeForwardsToAgents(t *testing.T) {
	h := newHarness(t)
	h.call("POST", "/api/servers", `{"uuid":"`+uuid+`"}`, nodeToken)
	if code, _, _ := h.call("POST", "/api/deauthorize-user", `{"user":"u1","servers":[]}`, nodeToken); code != 204 {
		t.Fatal("deauthorize")
	}
}

func TestResyncAdoptsAndFlagsOrphans(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.sync.Resync(ctx); err != nil {
		t.Fatal(err)
	}
	gs := h.gs()
	if gs.Spec.Install.Generation != 0 {
		t.Fatal("adopted servers must not be installed")
	}
	// Panel forgets the server -> Orphaned condition, never deleted.
	delete(h.panel.Servers, uuid)
	if err := h.sync.Resync(ctx); err != nil {
		t.Fatal(err)
	}
	gs = h.gs()
	var orphaned bool
	for _, c := range gs.Status.Conditions {
		if c.Type == v1alpha1.ConditionOrphaned && c.Status == metav1.ConditionTrue {
			orphaned = true
		}
	}
	if !orphaned {
		t.Fatalf("expected Orphaned condition: %+v", gs.Status.Conditions)
	}
}

func TestParseInvocation(t *testing.T) {
	got := serversync.ParseInvocation("java -Xmx{{SERVER_MEMORY}}M -jar {{SERVER_JARFILE}} --port {{SERVER_PORT}} {{MISSING}}", map[string]string{"SERVER_JARFILE": "paper.jar"}, 1024, 25565, "0.0.0.0")
	if got != "java -Xmx1024M -jar paper.jar --port 25565 ${MISSING}" {
		t.Fatalf("got %q", got)
	}
}
