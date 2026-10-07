package panelapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/gateway/agents"
	"github.com/Claiyc/pelican-k8s/internal/gateway/config"
	"github.com/Claiyc/pelican-k8s/internal/gateway/jwtx"
	"github.com/Claiyc/pelican-k8s/internal/gateway/panel"
	"github.com/Claiyc/pelican-k8s/internal/gateway/serversync"
	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/test/fakepanel"
)

const (
	ns        = "pelican-servers"
	nodeID    = "nodeid"
	nodeToken = "node-token-secret"
	agentID   = "agentid"
	agentTok  = "agent-token-secret"
	uuid      = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	otherUUID = "9a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
)

// agentCall is one request the fake agent received.
type agentCall struct {
	method, uri, auth, body string
}

type fixture struct {
	t     *testing.T
	c     client.Client
	st    *store.Store
	fp    *fakepanel.Panel
	h     *Handler
	srv   *httptest.Server
	agent *httptest.Server

	mu        sync.Mutex
	agentCall []agentCall
	// stateStatus is the status the fake agent answers for the server state.
	stateStatus int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	cls := &v1alpha1.GameServerClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceMemory:           resource.MustParse("8Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.GameServer{}).WithObjects(cls, node).Build()
	st := store.New(c, ns, "default")

	fp := fakepanel.New(nodeID, nodeToken)
	fp.Add(&fakepanel.Server{
		UUID: uuid, Settings: fakepanel.PaperSettings(uuid, 30565, 1024), ProcessConfiguration: fakepanel.PaperProcessConfiguration(),
		Install: fakepanel.InstallScript{ContainerImage: "img", Entrypoint: "ash", Script: "echo"},
	})
	ps := httptest.NewServer(fp.Handler())
	t.Cleanup(ps.Close)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{PanelURL: ps.URL, NodeTokenID: nodeID, NodeToken: nodeToken, ServersNamespace: ns, AdvertisedVersion: "1.2.3", StateCacheTTL: time.Millisecond}
	pc := panel.New(ps.URL, nodeID, nodeToken, cfg.UserAgent())

	f := &fixture{t: t, c: c, st: st, fp: fp, stateStatus: 200}
	f.agent = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.agentCall = append(f.agentCall, agentCall{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), string(b)})
		status := f.stateStatus
		f.mu.Unlock()
		w.Header().Set("User-Agent", "Pelican Wings/agent (id:secret)")
		if r.URL.Path == "/api/servers/"+uuid {
			if status != 200 {
				w.WriteHeader(status)
				return
			}
			_, _ = w.Write([]byte(`{"state":"running","from":"agent"}`))
			return
		}
		_, _ = w.Write([]byte("agent says " + r.URL.Path))
	}))
	t.Cleanup(f.agent.Close)

	res := agents.NewResolver(st, time.Millisecond)
	res.HTTPWait, res.Poll = 50*time.Millisecond, 10*time.Millisecond
	agentAddr := strings.TrimPrefix(f.agent.URL, "http://")
	res.Transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, agentAddr)
	}
	f.h = &Handler{
		Cfg: cfg, Store: st, Agents: res, Log: log,
		Sync: &serversync.Syncer{Store: st, Panel: pc, Timezone: "UTC", Log: log},
	}
	f.srv = httptest.NewServer(f.h.Routes())
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) calls() []agentCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agentCall(nil), f.agentCall...)
}

func (f *fixture) do(method, path, body string, token string) (int, string, http.Header) {
	f.t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), res.Header
}

// node calls with the node token.
func (f *fixture) node(method, path, body string) (int, string) {
	f.t.Helper()
	code, b, _ := f.do(method, path, body, nodeToken)
	return code, b
}

func (f *fixture) create() {
	f.t.Helper()
	if err := f.h.Sync.Create(context.Background(), uuid, false); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) addPod() {
	f.t.Helper()
	ctx := context.Background()
	started := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: names.AgentPod(uuid), Namespace: ns}}
	if err := f.c.Create(ctx, pod); err != nil {
		f.t.Fatal(err)
	}
	pod.Status = corev1.PodStatus{PodIP: "10.0.0.1", ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", Started: &started}}}
	if err := f.c.Status().Update(ctx, pod); err != nil {
		f.t.Fatal(err)
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.AgentSecret(uuid), Namespace: ns,
		Labels: map[string]string{v1alpha1.LabelServerUUID: uuid, v1alpha1.LabelComponent: "agent"}},
		Data: map[string][]byte{"token_id": []byte(agentID), "token": []byte(agentTok)}}
	if err := f.c.Create(ctx, sec); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) gs() *v1alpha1.GameServer {
	f.t.Helper()
	gs, err := f.st.Get(context.Background(), uuid)
	if err != nil {
		f.t.Fatal(err)
	}
	return gs
}

func decodeMap(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("body %q: %v", s, err)
	}
	return m
}

func TestHealthzNeedsNoAuthAndSetsUserAgent(t *testing.T) {
	f := newFixture(t)
	code, body, hdr := f.do("GET", "/healthz", "", "")
	if code != 200 || decodeMap(t, body)["ok"] != true {
		t.Fatalf("%d %s", code, body)
	}
	if got := hdr.Get("User-Agent"); got != "Pelican Wings/v1.2.3 (id:nodeid)" {
		t.Fatalf("User-Agent = %q", got)
	}
}

func TestNodeTokenAuth(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name, header string
		code         int
	}{
		{"missing", "", 401},
		{"wrong scheme", "Basic abc", 401},
		{"no token", "Bearer", 401},
		{"wrong token", "Bearer nope", 403},
		{"right token", "Bearer " + nodeToken, 200},
	}
	for _, tt := range tests {
		req, _ := http.NewRequest("GET", f.srv.URL+"/api/system", nil)
		if tt.header != "" {
			req.Header.Set("Authorization", tt.header)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != tt.code {
			t.Errorf("%s: %d, want %d", tt.name, res.StatusCode, tt.code)
		}
		if tt.code == 401 && res.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: missing WWW-Authenticate", tt.name)
		}
	}
}

func TestUnknownRouteAndPanicRecovery(t *testing.T) {
	f := newFixture(t)
	if code, body, _ := f.do("GET", "/nope", "", ""); code != 404 || !strings.Contains(body, "does not exist") {
		t.Fatalf("%d %s", code, body)
	}
	f.h.Diagnostics = func(context.Context) string { panic("boom") }
	if code, body := f.node("GET", "/api/diagnostics", ""); code != 500 || !strings.Contains(body, "internal error") {
		t.Fatalf("%d %s", code, body)
	}
}

func TestStaticEndpoints(t *testing.T) {
	f := newFixture(t)
	if code, body := f.node("POST", "/api/update", ""); code != 200 || decodeMap(t, body)["applied"] != false {
		t.Fatalf("update: %d %s", code, body)
	}
	if code, body := f.node("GET", "/api/system/docker/disk", ""); code != 200 || decodeMap(t, body) == nil {
		t.Fatalf("disk: %d %s", code, body)
	}
	if code, body := f.node("DELETE", "/api/system/docker/image/prune", ""); code != 200 || decodeMap(t, body)["SpaceReclaimed"] != float64(0) {
		t.Fatalf("prune: %d %s", code, body)
	}
}

func TestUnsupportedTransfers(t *testing.T) {
	f := newFixture(t)
	f.create()
	// The Panel-signed transfer endpoint has no node token.
	if code, _, _ := f.do("POST", "/api/transfers", "", ""); code != 501 {
		t.Fatalf("POST /api/transfers = %d", code)
	}
	for _, c := range [][2]string{
		{"DELETE", "/api/transfers/" + uuid},
		{"POST", "/api/servers/" + uuid + "/transfer"},
		{"DELETE", "/api/servers/" + uuid + "/transfer"},
	} {
		if code, body := f.node(c[0], c[1], ""); code != 501 || !strings.Contains(body, "not supported") {
			t.Errorf("%s %s: %d %s", c[0], c[1], code, body)
		}
	}
	// A transfer of a server that does not exist is a 404 first.
	if code, _ := f.node("POST", "/api/servers/"+otherUUID+"/transfer", ""); code != 404 {
		t.Fatalf("code = %d", code)
	}
}

func TestSystemInfo(t *testing.T) {
	f := newFixture(t)
	f.create()
	if err := f.st.PatchStatus(context.Background(), uuid, map[string]any{"process": map[string]any{"state": "running"}}); err != nil {
		t.Fatal(err)
	}
	code, body := f.node("GET", "/api/system", "")
	m := decodeMap(t, body)
	if code != 200 || m["version"] != "1.2.3" || m["os"] != "linux" || m["architecture"] == "" || m["cpu_count"].(float64) < 1 {
		t.Fatalf("v1: %d %s", code, body)
	}

	code, body = f.node("GET", "/api/system?v=2", "")
	m = decodeMap(t, body)
	if code != 200 || m["version"] != "1.2.3" {
		t.Fatalf("v2: %d %s", code, body)
	}
	docker := m["docker"].(map[string]any)
	containers := docker["containers"].(map[string]any)
	if containers["total"] != float64(1) || containers["running"] != float64(1) || containers["stopped"] != float64(0) {
		t.Fatalf("containers = %v", containers)
	}
	if docker["storage"].(map[string]any)["driver"] != "csi" {
		t.Fatalf("docker = %v", docker)
	}
}

func TestDiagnostics(t *testing.T) {
	f := newFixture(t)
	code, body, hdr := f.do("GET", "/api/diagnostics", "", nodeToken)
	if code != 200 || !strings.HasPrefix(body, "pelican-k8s gateway ") || !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain") {
		t.Fatalf("default: %d %q %v", code, body, hdr)
	}
	f.h.Diagnostics = func(context.Context) string { return "custom report" }
	if code, body := f.node("GET", "/api/diagnostics", ""); code != 200 || body != "custom report" {
		t.Fatalf("custom: %d %q", code, body)
	}
}

func TestUtilization(t *testing.T) {
	f := newFixture(t)
	f.create()
	now := metav1.Now()
	if err := f.st.PatchStatus(context.Background(), uuid, map[string]any{"usage": map[string]any{"memoryBytes": 1000, "diskBytes": 2000, "updatedAt": now}}); err != nil {
		t.Fatal(err)
	}
	code, body := f.node("GET", "/api/system/utilization", "")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	m := decodeMap(t, body)
	var u struct {
		MemoryTotal, MemoryUsed, DiskTotal, DiskUsed float64
	}
	u.MemoryTotal, _ = m["memory_total"].(float64)
	u.MemoryUsed, _ = m["memory_used"].(float64)
	u.DiskTotal, _ = m["disk_total"].(float64)
	u.DiskUsed, _ = m["disk_used"].(float64)
	if u.MemoryTotal != 8<<30 || u.DiskTotal != 100<<30 || u.MemoryUsed != 1000 || u.DiskUsed != 2000 {
		t.Fatalf("utilization = %s", body)
	}
}

func TestListAndGetServerFallbackState(t *testing.T) {
	f := newFixture(t)
	f.create()

	// Without a pod the state is "missing" and the configuration is served.
	code, body := f.node("GET", "/api/servers/"+uuid, "")
	m := decodeMap(t, body)
	if code != 200 || m["state"] != "missing" || m["is_suspended"] != false || m["configuration"].(map[string]any)["uuid"] != uuid {
		t.Fatalf("get: %d %s", code, body)
	}
	if m["utilization"].(map[string]any)["state"] != "missing" {
		t.Fatalf("utilization = %v", m["utilization"])
	}

	code, body = f.node("GET", "/api/servers", "")
	var list []map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil || code != 200 || len(list) != 1 || list[0]["state"] != "missing" {
		t.Fatalf("list: %d %s (%v)", code, body, err)
	}

	// With a pod, the recorded process state is used when the agent is unreachable.
	f.addPod()
	f.mu.Lock()
	f.stateStatus = 500
	f.mu.Unlock()
	if err := f.st.PatchStatus(context.Background(), uuid, map[string]any{"process": map[string]any{"state": "starting"}}); err != nil {
		t.Fatal(err)
	}
	_, body = f.node("GET", "/api/servers/"+uuid, "")
	if decodeMap(t, body)["state"] != "starting" {
		t.Fatalf("body = %s", body)
	}
	// No recorded state means offline.
	if err := f.st.PatchStatus(context.Background(), uuid, map[string]any{"process": map[string]any{"state": ""}}); err != nil {
		t.Fatal(err)
	}
	_, body = f.node("GET", "/api/servers/"+uuid, "")
	if decodeMap(t, body)["state"] != "offline" {
		t.Fatalf("body = %s", body)
	}

	if code, _ := f.node("GET", "/api/servers/"+otherUUID, ""); code != 404 {
		t.Fatalf("unknown server code = %d", code)
	}
}

func TestFallbackStateReportsSuspension(t *testing.T) {
	f := newFixture(t)
	f.fp.Get(uuid).Settings["suspended"] = true
	f.create()
	_, body := f.node("GET", "/api/servers/"+uuid, "")
	if decodeMap(t, body)["is_suspended"] != true {
		t.Fatalf("body = %s", body)
	}
}

func TestGetServerUsesAgentState(t *testing.T) {
	f := newFixture(t)
	f.create()
	f.addPod()
	code, body := f.node("GET", "/api/servers/"+uuid, "")
	if code != 200 || decodeMap(t, body)["from"] != "agent" {
		t.Fatalf("%d %s", code, body)
	}
	calls := f.calls()
	if len(calls) == 0 || calls[0].auth != "Bearer "+agentTok {
		t.Fatalf("agent calls = %+v", calls)
	}
	_, body = f.node("GET", "/api/servers", "")
	if !strings.Contains(body, `"from":"agent"`) {
		t.Fatalf("list = %s", body)
	}
}

func TestCreateServer(t *testing.T) {
	f := newFixture(t)
	for name, body := range map[string]string{
		"not json":  `nope`,
		"short id":  `{"uuid":"abc"}`,
		"no id":     `{}`,
		"long id":   `{"uuid":"` + uuid + `0"}`,
		"wrong typ": `{"uuid":5}`,
	} {
		if code, b := f.node("POST", "/api/servers", body); code != 422 || !strings.Contains(b, "could not be validated") {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	if code, body := f.node("POST", "/api/servers", `{"uuid":"`+uuid+`","start_on_completion":true}`); code != 202 {
		t.Fatalf("%d %s", code, body)
	}
	if gs := f.gs(); !gs.Spec.Install.StartOnInstall || gs.Spec.Install.Generation != 1 {
		t.Fatalf("install = %+v", gs.Spec.Install)
	}
	// The Panel does not know this server.
	if code, body := f.node("POST", "/api/servers", `{"uuid":"`+otherUUID+`"}`); code != 500 || !strings.Contains(body, "failed to create server") {
		t.Fatalf("%d %s", code, body)
	}
}

func TestDeleteServer(t *testing.T) {
	f := newFixture(t)
	f.create()
	if code, _ := f.node("DELETE", "/api/servers/"+uuid, ""); code != 204 {
		t.Fatalf("code = %d", code)
	}
	if f.st.Exists(context.Background(), uuid) {
		t.Fatal("server still exists")
	}
	if code, _ := f.node("DELETE", "/api/servers/"+uuid, ""); code != 404 {
		t.Fatalf("second delete = %d", code)
	}
}

func TestSyncServer(t *testing.T) {
	f := newFixture(t)
	f.create()
	f.fp.Get(uuid).Settings["meta"].(map[string]any)["name"] = "Renamed"
	if code, body := f.node("POST", "/api/servers/"+uuid+"/sync", ""); code != 204 {
		t.Fatalf("%d %s", code, body)
	}
	if f.gs().Annotations[v1alpha1.AnnotationPanelName] != "Renamed" {
		t.Fatal("sync did not apply the Panel configuration")
	}
	// The Panel forgot the server.
	delete(f.fp.Servers, uuid)
	if code, _ := f.node("POST", "/api/servers/"+uuid+"/sync", ""); code != 500 {
		t.Fatalf("code = %d", code)
	}
}

func TestInstallAndReinstall(t *testing.T) {
	f := newFixture(t)
	f.create()
	if code, body := f.node("POST", "/api/servers/"+uuid+"/install", ""); code != 202 {
		t.Fatalf("%d %s", code, body)
	}
	if in := f.gs().Spec.Install; in.Generation != 2 || in.Reinstall {
		t.Fatalf("install = %+v", in)
	}
	if code, body := f.node("POST", "/api/servers/"+uuid+"/reinstall", ""); code != 202 {
		t.Fatalf("%d %s", code, body)
	}
	if in := f.gs().Spec.Install; in.Generation != 3 || !in.Reinstall {
		t.Fatalf("install = %+v", in)
	}

	// A reinstall right after a power action is refused, a plain install is not.
	err := f.st.PatchStatus(context.Background(), uuid, map[string]any{"power": map[string]any{"lastAction": map[string]any{"action": "start", "at": metav1.Now()}}})
	if err != nil {
		t.Fatal(err)
	}
	if code, body := f.node("POST", "/api/servers/"+uuid+"/reinstall", ""); code != 409 || !strings.Contains(body, "power action") {
		t.Fatalf("%d %s", code, body)
	}
	if f.gs().Spec.Install.Generation != 3 {
		t.Fatal("a refused reinstall must not bump the generation")
	}
	if code, _ := f.node("POST", "/api/servers/"+uuid+"/install", ""); code != 202 {
		t.Fatalf("install after power action = %d", code)
	}
	// An old power action no longer blocks.
	old := metav1.NewTime(time.Now().Add(-time.Minute))
	_ = f.st.PatchStatus(context.Background(), uuid, map[string]any{"power": map[string]any{"lastAction": map[string]any{"action": "start", "at": old}}})
	if code, _ := f.node("POST", "/api/servers/"+uuid+"/reinstall", ""); code != 202 {
		t.Fatalf("reinstall after an old action = %d", code)
	}

	delete(f.fp.Servers, uuid)
	if code, _ := f.node("POST", "/api/servers/"+uuid+"/install", ""); code != 500 {
		t.Fatalf("install without panel server = %d", code)
	}
}

func TestPower(t *testing.T) {
	f := newFixture(t)
	f.create()
	if code, b := f.node("POST", "/api/servers/"+uuid+"/power", `{"action":"start"}`); code != 202 {
		t.Fatalf("%d %s", code, b)
	}
	if p := f.gs().Spec.Power; p.Desired != v1alpha1.PowerRunning || p.Generation != 1 {
		t.Fatalf("power = %+v", p)
	}
	if code, _ := f.node("POST", "/api/servers/"+uuid+"/power", `{"action":"kill"}`); code != 202 {
		t.Fatalf("kill = %d", code)
	}
	if p := f.gs().Spec.Power; p.Desired != v1alpha1.PowerStopped || !p.Kill {
		t.Fatalf("power = %+v", p)
	}
	if code, b := f.node("POST", "/api/servers/"+uuid+"/power", `{"action":"explode"}`); code != 422 || !strings.Contains(b, "not valid") {
		t.Fatalf("%d %s", code, b)
	}
	if code, b := f.node("POST", "/api/servers/"+uuid+"/power", `nope`); code != 400 || !strings.Contains(b, "invalid body") {
		t.Fatalf("%d %s", code, b)
	}
	delete(f.fp.Servers, uuid)
	if code, _ := f.node("POST", "/api/servers/"+uuid+"/power", `{"action":"restart"}`); code != 500 {
		t.Fatalf("restart with a failing sync = %d", code)
	}
	// Stop does not depend on the Panel.
	if code, _ := f.node("POST", "/api/servers/"+uuid+"/power", `{"action":"stop"}`); code != 202 {
		t.Fatalf("stop = %d", code)
	}
}

func TestPowerRefusesStartOfSuspendedServer(t *testing.T) {
	f := newFixture(t)
	f.fp.Get(uuid).Settings["suspended"] = true
	f.create()
	for _, action := range []string{"start", "restart"} {
		if code, b := f.node("POST", "/api/servers/"+uuid+"/power", `{"action":"`+action+`"}`); code != 400 || !strings.Contains(b, "suspended") {
			t.Errorf("%s: %d %s", action, code, b)
		}
	}
	if f.gs().Spec.Power.Generation != 0 {
		t.Fatal("a refused start must not change the spec")
	}
	// Stopping a suspended server is fine.
	if code, _ := f.node("POST", "/api/servers/"+uuid+"/power", `{"action":"stop"}`); code != 202 {
		t.Fatalf("stop = %d", code)
	}
}

func TestDeauthorize(t *testing.T) {
	f := newFixture(t)
	f.create()
	f.addPod()
	if code, _ := f.node("POST", "/api/deauthorize-user", `{"user":"u1","servers":["`+uuid+`","`+otherUUID+`"]}`); code != 204 {
		t.Fatalf("code = %d", code)
	}
	calls := f.calls()
	if len(calls) != 1 || calls[0].uri != "/api/deauthorize-user" || calls[0].auth != "Bearer "+agentTok {
		t.Fatalf("agent calls = %+v", calls)
	}
	var sent struct {
		User    string   `json:"user"`
		Servers []string `json:"servers"`
	}
	_ = json.Unmarshal([]byte(calls[0].body), &sent)
	if sent.User != "u1" || len(sent.Servers) != 1 || sent.Servers[0] != uuid {
		t.Fatalf("payload = %s", calls[0].body)
	}

	// No server list means all servers; garbage bodies are tolerated.
	if code, _ := f.node("POST", "/api/deauthorize-user", `{"user":"u2"}`); code != 204 {
		t.Fatalf("code = %d", code)
	}
	if code, _ := f.node("POST", "/api/deauthorize-user", `garbage`); code != 204 {
		t.Fatalf("code = %d", code)
	}
	if got := len(f.calls()); got != 3 {
		t.Fatalf("agent calls = %d, want 3", got)
	}
	// An unreachable agent is only logged.
	f.agent.Close()
	if code, _ := f.node("POST", "/api/deauthorize-user", `{"user":"u3"}`); code != 204 {
		t.Fatalf("code = %d", code)
	}
}

func TestProxy(t *testing.T) {
	f := newFixture(t)
	f.create()
	// No pod yet.
	if code, body := f.node("GET", "/api/servers/"+uuid+"/files/list-directory", ""); code != 503 || !strings.Contains(body, "unavailable") {
		t.Fatalf("%d %s", code, body)
	}
	f.addPod()
	code, body, hdr := f.do("GET", "/api/servers/"+uuid+"/files/list-directory?directory=%2F", "", nodeToken)
	if code != 200 || body != "agent says /api/servers/"+uuid+"/files/list-directory" {
		t.Fatalf("%d %s", code, body)
	}
	if hdr.Get("User-Agent") != "Pelican Wings/v1.2.3 (id:nodeid)" {
		t.Fatalf("the agent's User-Agent leaked: %q", hdr.Get("User-Agent"))
	}
	last := f.calls()[len(f.calls())-1]
	if last.uri != "/api/servers/"+uuid+"/files/list-directory?directory=%2F" || last.auth != "Bearer "+agentTok {
		t.Fatalf("agent call = %+v", last)
	}
	// Proxying needs the node token and an existing server.
	if code, _, _ := f.do("GET", "/api/servers/"+uuid+"/files/list-directory", "", ""); code != 401 {
		t.Fatalf("unauthenticated proxy = %d", code)
	}
	if code, _ := f.node("GET", "/api/servers/"+otherUUID+"/files/list-directory", ""); code != 404 {
		t.Fatalf("unknown server = %d", code)
	}
	// A dead agent is a bad gateway.
	f.agent.Close()
	if code, body := f.node("GET", "/api/servers/"+uuid+"/files/list-directory", ""); code != 502 || !strings.Contains(body, "agent request failed") {
		t.Fatalf("%d %s", code, body)
	}
}

func TestAgentUnavailableMapping(t *testing.T) {
	f := newFixture(t)
	f.create()
	// A pod that is ready but has no agent secret maps to 404 (not found).
	f.addPod()
	if err := f.c.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.AgentSecret(uuid), Namespace: ns}}); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.node("GET", "/api/servers/"+uuid+"/files/list-directory", ""); code != 404 {
		t.Fatalf("missing secret = %d", code)
	}
	w := httptest.NewRecorder()
	f.h.agentUnavailable(w, errors.New("dial failed"))
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "dial failed") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestBackupProxyRecordsPendingBackups(t *testing.T) {
	f := newFixture(t)
	f.create()
	f.addPod()

	// Create: the backup uuid is in the body, which the agent still receives.
	if code, _ := f.node("POST", "/api/servers/"+uuid+"/backup", `{"uuid":"b-1","ignore":""}`); code != 200 {
		t.Fatalf("create code = %d", code)
	}
	last := f.calls()[len(f.calls())-1]
	if last.uri != "/api/servers/"+uuid+"/backup" || last.body != `{"uuid":"b-1","ignore":""}` {
		t.Fatalf("agent call = %+v", last)
	}
	pending := f.gs().Status.Backups.Pending
	if len(pending) != 1 || pending[0].UUID != "b-1" {
		t.Fatalf("pending = %+v", pending)
	}
	// The same backup again is not recorded twice.
	f.node("POST", "/api/servers/"+uuid+"/backup", `{"uuid":"b-1"}`)
	if got := len(f.gs().Status.Backups.Pending); got != 1 {
		t.Fatalf("pending = %d", got)
	}
	// Restore: the backup uuid is in the path.
	if code, _ := f.node("POST", "/api/servers/"+uuid+"/backup/b-2/restore", `{}`); code != 200 {
		t.Fatalf("restore code = %d", code)
	}
	if got := len(f.gs().Status.Backups.Pending); got != 2 {
		t.Fatalf("pending = %+v", f.gs().Status.Backups.Pending)
	}
	// A body without a uuid records nothing.
	f.node("POST", "/api/servers/"+uuid+"/backup", `nope`)
	if got := len(f.gs().Status.Backups.Pending); got != 2 {
		t.Fatalf("pending = %d", got)
	}
}

func signToken(t *testing.T, key string, claims map[string]any) string {
	t.Helper()
	tok, err := jwt.Sign(claims, jwt.NewHS256([]byte(key)))
	if err != nil {
		t.Fatal(err)
	}
	return string(tok)
}

func fileClaims(scope string, exp time.Time) map[string]any {
	return map[string]any{
		"iat": time.Now().Unix(), "exp": exp.Unix(), "jti": "j", "server_uuid": uuid,
		"user_uuid": "u", "scope": scope, "unique_id": "one", "file_path": "/x.txt",
	}
}

func TestSignedProxyResignsForTheAgent(t *testing.T) {
	f := newFixture(t)
	f.create()
	f.addPod()
	tok := signToken(t, nodeToken, fileClaims("file-download", time.Now().Add(time.Minute)))

	code, body, _ := f.do("GET", "/download/file?token="+tok, "", "")
	if code != 200 || body != "agent says /download/file" {
		t.Fatalf("%d %s", code, body)
	}
	last := f.calls()[len(f.calls())-1]
	_, agentToken, ok := strings.Cut(last.uri, "token=")
	if !ok {
		t.Fatalf("agent uri = %q", last.uri)
	}
	claims, _, err := jwtx.Verify([]byte(agentToken), []byte(agentTok))
	if err != nil || claims.ServerUUID != uuid || claims.Scope != "file-download" {
		t.Fatalf("the agent token must verify with the agent key: %+v %v", claims, err)
	}
	if agentToken == tok {
		t.Fatal("the Panel's token must not be forwarded")
	}

	// Each scope accepts only its own route.
	up := signToken(t, nodeToken, fileClaims("file-upload", time.Now().Add(time.Minute)))
	if code, _, _ := f.do("POST", "/upload/file?token="+up, "x", ""); code != 200 {
		t.Fatalf("upload = %d", code)
	}
	if code, _, _ := f.do("GET", "/download/backup?token="+up, "", ""); code != 404 {
		t.Fatalf("a file-upload token must not download a backup: %d", code)
	}
}

func TestSignedProxyRejections(t *testing.T) {
	f := newFixture(t)
	f.create()
	exp := time.Now().Add(time.Minute)

	noServer := fileClaims("file-download", exp)
	noServer["server_uuid"] = ""
	tests := []struct {
		name  string
		token string
		code  int
	}{
		{"garbage", "garbage", 403},
		{"other key", signToken(t, "other", fileClaims("file-download", exp)), 403},
		{"expired", signToken(t, nodeToken, fileClaims("file-download", time.Now().Add(-time.Minute))), 403},
		{"wrong scope", signToken(t, nodeToken, fileClaims("websocket", exp)), 404},
		{"no server", signToken(t, nodeToken, noServer), 404},
		{"no pod", signToken(t, nodeToken, fileClaims("file-download", exp)), 503},
	}
	for _, tt := range tests {
		code, body, _ := f.do("GET", "/download/file?token="+tt.token, "", "")
		if code != tt.code {
			t.Errorf("%s: %d, want %d", tt.name, code, tt.code)
		}
		// The library's errors already start with "jwt: "; it must not be doubled.
		if strings.Contains(body, "jwt: jwt:") {
			t.Errorf("%s: body %q has a doubled jwt prefix", tt.name, body)
		}
		if tt.name == "expired" && !strings.Contains(body, "jwt: token expired") {
			t.Errorf("%s: body %q, want the expiry error", tt.name, body)
		}
	}
	if code, _, _ := f.do("GET", "/download/file", "", ""); code != 403 {
		t.Errorf("no token: %d", code)
	}
}

func TestWebsocket(t *testing.T) {
	f := newFixture(t)
	if code, body, _ := f.do("GET", "/api/servers/"+uuid+"/ws", "", ""); code != 500 || !strings.Contains(body, "not configured") {
		t.Fatalf("%d %s", code, body)
	}
	f.h.WS = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "ws for "+r.PathValue("server"))
	})
	srv := httptest.NewServer(f.h.Routes())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/api/servers/" + uuid + "/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if string(b) != "ws for "+uuid {
		t.Fatalf("body = %q", b)
	}
}
