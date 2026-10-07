package remoteapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pelican/wings/remote"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/gateway/agents"
	"github.com/Claiyc/pelican-k8s/internal/gateway/panel"
	"github.com/Claiyc/pelican-k8s/internal/gateway/serversync"
	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/test/fakepanel"
)

const (
	ns       = "pelican-servers"
	nodeID   = "nodeid"
	nodeTok  = "node-token"
	uuid     = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	otherID  = "9a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	agentID  = "agentid"
	agentTok = "agent-token"
	bearer   = agentID + "." + agentTok
)

type fakeSessions map[string]*remote.SftpAuthResponse

func (f fakeSessions) Lookup(user, pass string) (*remote.SftpAuthResponse, bool) {
	r, ok := f[user+":"+pass]
	return r, ok
}

type fixture struct {
	t        *testing.T
	c        client.Client
	st       *store.Store
	fp       *fakepanel.Panel
	h        *Handler
	srv      *httptest.Server
	agent    *httptest.Server
	agentHit chan string
	// agentState is the process state the fake agent reports.
	agentState atomic.Value
	// onPoll, when set, runs once during the next agent request, after the
	// reported state was read.
	onPoll atomic.Pointer[func()]
}

type options struct {
	panelDown bool
	noSftp    bool
}

func newFixture(t *testing.T, o options) *fixture {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	cls := &v1alpha1.GameServerClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec:       v1alpha1.GameServerClassSpec{Install: v1alpha1.InstallJobSpec{StrictExitCode: true}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.GameServer{}).WithObjects(cls).Build()
	st := store.New(c, ns, "default")

	fp := fakepanel.New(nodeID, nodeTok)
	fp.Add(&fakepanel.Server{
		UUID: uuid, Settings: fakepanel.PaperSettings(uuid, 30565, 1024), ProcessConfiguration: fakepanel.PaperProcessConfiguration(),
		Install: fakepanel.InstallScript{ContainerImage: "img", Entrypoint: "ash", Script: "echo"},
	})
	ps := httptest.NewServer(fp.Handler())
	t.Cleanup(ps.Close)
	panelURL := ps.URL
	if o.panelDown {
		panelURL = "http://127.0.0.1:1"
	}
	pc := panel.New(panelURL, nodeID, nodeTok, "test")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	f := &fixture{t: t, c: c, st: st, fp: fp, agentHit: make(chan string, 16)}
	f.agentState.Store("running")
	f.agent = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.agentHit <- r.Method + " " + r.URL.Path
		state := f.agentState.Load()
		if hook := f.onPoll.Swap(nil); hook != nil {
			(*hook)()
		}
		_, _ = fmt.Fprintf(w, `{"state":%q,"utilization":{"memory_bytes":4096,"cpu_absolute":12.34,"disk_bytes":99}}`, state)
	}))
	t.Cleanup(f.agent.Close)

	res := agents.NewResolver(st, time.Millisecond)
	// Every pod IP resolves to the fake agent, whatever port is dialled.
	agentAddr := strings.TrimPrefix(f.agent.URL, "http://")
	res.Transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, agentAddr)
	}
	f.h = &Handler{
		Store: st, Panel: pc, Agents: res, Log: log,
		RepollEvery: 10 * time.Millisecond, RepollFor: 300 * time.Millisecond,
		Sync: &serversync.Syncer{Store: st, Panel: pc, Timezone: "UTC", Log: log},
		Sftp: fakeSessions{"user:pw": {Server: uuid, User: "u-1"}, "user:foreign": {Server: otherID, User: "u-2"}},
	}
	if o.noSftp {
		f.h.Sftp = nil
	}
	f.srv = httptest.NewServer(f.h.Routes())
	t.Cleanup(f.srv.Close)
	return f
}

// withServer creates the GameServer through the syncer, plus the agent
// secret and a ready pod.
func (f *fixture) withServer(startOnInstall bool) {
	f.t.Helper()
	ctx := context.Background()
	if err := f.h.Sync.Create(ctx, uuid, startOnInstall); err != nil {
		f.t.Fatal(err)
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.AgentSecret(uuid), Namespace: ns,
		Labels: map[string]string{v1alpha1.LabelServerUUID: uuid, v1alpha1.LabelComponent: "agent"}},
		Data: map[string][]byte{"token_id": []byte(agentID), "token": []byte(agentTok)}}
	if err := f.c.Create(ctx, sec); err != nil {
		f.t.Fatal(err)
	}
	f.addPod("10.0.0.1")
}

func (f *fixture) addPod(ip string) {
	f.t.Helper()
	ctx := context.Background()
	started := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: names.AgentPod(uuid), Namespace: ns}}
	if err := f.c.Create(ctx, pod); err != nil {
		f.t.Fatal(err)
	}
	pod.Status = corev1.PodStatus{PodIP: ip, ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", Started: &started}}}
	if err := f.c.Status().Update(ctx, pod); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) do(method, path, body, token string) (int, string) {
	f.t.Helper()
	req, _ := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func (f *fixture) gs() *v1alpha1.GameServer {
	f.t.Helper()
	gs, err := f.st.Get(context.Background(), uuid)
	if err != nil {
		f.t.Fatal(err)
	}
	return gs
}

func (f *fixture) setPending(uuids ...string) {
	f.t.Helper()
	var pending []map[string]any
	for _, u := range uuids {
		pending = append(pending, map[string]any{"uuid": u, "startedAt": metav1.Now()})
	}
	if err := f.st.PatchStatus(context.Background(), uuid, map[string]any{"backups": map[string]any{"pending": pending}}); err != nil {
		f.t.Fatal(err)
	}
}

func TestAuthRejectsUnknownAgents(t *testing.T) {
	f := newFixture(t, options{})
	f.withServer(false)
	for _, tok := range []string{"", "nodot", "bad.token", agentID + ".wrong"} {
		code, body := f.do("GET", "/api/remote/servers", "", tok)
		if code != 403 || !strings.Contains(body, "AccessDeniedHttpException") {
			t.Errorf("token %q: %d %s", tok, code, body)
		}
	}
}

func TestUnmatchedRoute(t *testing.T) {
	f := newFixture(t, options{})
	f.withServer(false)
	var got string
	f.h.OnUnmatched = func(method, path string) { got = method + " " + path }
	code, body := f.do("GET", "/api/remote/nothing", "", bearer)
	if code != 404 || !strings.Contains(body, "NotFoundHttpException") || got != "GET /api/remote/nothing" {
		t.Fatalf("%d %s (callback %q)", code, body, got)
	}
	// The callback is optional.
	f.h.OnUnmatched = nil
	if code, _ := f.do("GET", "/api/remote/nothing", "", bearer); code != 404 {
		t.Fatalf("code = %d", code)
	}
	// Transfers are not supported.
	if code, _ := f.do("POST", "/api/remote/servers/"+uuid+"/transfer/success", "", bearer); code != 404 {
		t.Fatalf("transfer code = %d", code)
	}
}

func TestResetServers(t *testing.T) {
	f := newFixture(t, options{})
	f.withServer(false)
	if code, _ := f.do("POST", "/api/remote/servers/reset", "", bearer); code != 204 {
		t.Fatalf("code = %d", code)
	}
	if len(f.fp.CallsMatching("servers/reset")) != 0 {
		t.Fatal("the reset must not reach the Panel")
	}
}

func TestListAndGetServer(t *testing.T) {
	f := newFixture(t, options{})
	f.withServer(false)

	code, body := f.do("GET", "/api/remote/servers", "", bearer)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var list struct {
		Data []struct {
			UUID     string         `json:"uuid"`
			Settings map[string]any `json:"settings"`
		} `json:"data"`
		Meta remote.Pagination `json:"meta"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 || list.Data[0].UUID != uuid || list.Meta.Total != 1 || list.Meta.LastPage != 1 {
		t.Fatalf("list = %s", body)
	}
	if list.Data[0].Settings["environment"].(map[string]any)["SERVER_JARFILE"] != "server.jar" {
		t.Fatalf("environment missing: %s", body)
	}

	code, body = f.do("GET", "/api/remote/servers/"+uuid, "", bearer)
	var one map[string]json.RawMessage
	_ = json.Unmarshal([]byte(body), &one)
	if code != 200 || one["settings"] == nil || one["process_configuration"] == nil || one["uuid"] != nil {
		t.Fatalf("get = %d %s", code, body)
	}
	// Another server's UUID looks like it does not exist.
	if code, _ := f.do("GET", "/api/remote/servers/"+otherID, "", bearer); code != 404 {
		t.Fatalf("foreign server code = %d", code)
	}
}

func TestListAndGetServerFailWithoutGameServer(t *testing.T) {
	f := newFixture(t, options{})
	f.withServer(false)
	if err := f.c.Delete(context.Background(), f.gs()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/remote/servers", "/api/remote/servers/" + uuid} {
		if code, body := f.do("GET", path, "", bearer); code != 500 || !strings.Contains(body, "ServerError") {
			t.Errorf("%s: %d %s", path, code, body)
		}
	}
}

func TestGetInstall(t *testing.T) {
	f := newFixture(t, options{})
	f.withServer(false)
	code, body := f.do("GET", "/api/remote/servers/"+uuid+"/install", "", bearer)
	if code != 200 || !strings.Contains(body, `"container_image":"img"`) || !strings.Contains(body, `"script":"echo"`) {
		t.Fatalf("%d %s", code, body)
	}
	// A generation without a script is a 404, not an empty script.
	if err := f.st.PatchSpec(context.Background(), uuid, map[string]any{"install": map[string]any{"generation": 7}}); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.do("GET", "/api/remote/servers/"+uuid+"/install", "", bearer); code != 404 {
		t.Fatalf("missing script code = %d", code)
	}
	if err := f.c.Delete(context.Background(), f.gs()); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.do("GET", "/api/remote/servers/"+uuid+"/install", "", bearer); code != 404 {
		t.Fatalf("missing server code = %d", code)
	}
}

func TestInstallPreparedAndState(t *testing.T) {
	f := newFixture(t, options{})
	f.withServer(false)
	code, body := f.do("POST", "/api/remote/servers/"+uuid+"/install/prepared", "", bearer)
	if code != 200 || !strings.Contains(body, `"generation":1`) || !strings.Contains(body, `"strict_exit_code":true`) {
		t.Fatalf("%d %s", code, body)
	}
	if st := f.gs().Status.Install; st.PreparedGeneration != 1 || st.Result != v1alpha1.InstallRunning {
		t.Fatalf("status = %+v", st)
	}

	tests := []struct {
		name, query, result string
		finished            bool
	}{
		{"running", "?generation=1", "Running", false},
		{"no generation", "", "Running", false},
		{"superseded", "?generation=5", "Superseded", true},
	}
	for _, tt := range tests {
		code, body := f.do("GET", "/api/remote/servers/"+uuid+"/install/state"+tt.query, "", bearer)
		var out struct {
			Generation int64  `json:"generation"`
			Result     string `json:"result"`
			Finished   bool   `json:"job_finished"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		if code != 200 || out.Result != tt.result || out.Finished != tt.finished || out.Generation != 1 {
			t.Errorf("%s: %d %s", tt.name, code, body)
		}
	}

	_ = f.st.PatchStatus(context.Background(), uuid, map[string]any{"install": map[string]any{"result": "Succeeded"}})
	_, body = f.do("GET", "/api/remote/servers/"+uuid+"/install/state?generation=1", "", bearer)
	if !strings.Contains(body, `"job_finished":true`) {
		t.Fatalf("a finished job must say so: %s", body)
	}

	// No result yet means not finished.
	_ = f.st.PatchStatus(context.Background(), uuid, map[string]any{"install": map[string]any{"result": ""}})
	_, body = f.do("GET", "/api/remote/servers/"+uuid+"/install/state", "", bearer)
	if !strings.Contains(body, `"job_finished":false`) {
		t.Fatalf("no result: %s", body)
	}

	if err := f.c.Delete(context.Background(), f.gs()); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.do("POST", "/api/remote/servers/"+uuid+"/install/prepared", "", bearer); code != 404 {
		t.Fatalf("prepared without server = %d", code)
	}
	if code, _ := f.do("GET", "/api/remote/servers/"+uuid+"/install/state", "", bearer); code != 404 {
		t.Fatalf("state without server = %d", code)
	}
}

func TestInstallResult(t *testing.T) {
	f := newFixture(t, options{})
	f.withServer(true)
	if code, _ := f.do("POST", "/api/remote/servers/"+uuid+"/install", `{"successful":true,"reinstall":false}`, bearer); code != 204 {
		t.Fatalf("code = %d", code)
	}
	gs := f.gs()
	if gs.Status.Install.Result != v1alpha1.InstallSucceeded || gs.Status.Install.ReportedGeneration != 1 || gs.Status.Install.FinishedAt == nil {
		t.Fatalf("status = %+v", gs.Status.Install)
	}
	if gs.Spec.Power.Desired != v1alpha1.PowerRunning || gs.Spec.Power.Generation != 1 {
		t.Fatalf("start_on_completion must start the server: %+v", gs.Spec.Power)
	}
	if got := f.fp.InstallResults[uuid]; len(got) != 1 || got[0]["successful"] != true {
		t.Fatalf("panel results = %v", got)
	}
}

func TestInstallResultFailureDoesNotStart(t *testing.T) {
	f := newFixture(t, options{})
	f.withServer(true)
	if code, _ := f.do("POST", "/api/remote/servers/"+uuid+"/install", `{"successful":false,"reinstall":true}`, bearer); code != 204 {
		t.Fatalf("code = %d", code)
	}
	gs := f.gs()
	if gs.Status.Install.Result != v1alpha1.InstallFailed {
		t.Fatalf("status = %+v", gs.Status.Install)
	}
	if gs.Spec.Power.Desired == v1alpha1.PowerRunning {
		t.Fatal("a failed install must not start the server")
	}
	if got := f.fp.InstallResults[uuid]; len(got) != 1 || got[0]["successful"] != false || got[0]["reinstall"] != true {
		t.Fatalf("panel results = %v", got)
	}
}

func TestInstallResultToleratesPanelErrors(t *testing.T) {
	f := newFixture(t, options{panelDown: true})
	f.withServer0()
	// Garbage body counts as a failed install.
	if code, _ := f.do("POST", "/api/remote/servers/"+uuid+"/install", `garbage`, bearer); code != 204 {
		t.Fatalf("code = %d", code)
	}
	if f.gs().Status.Install.Result != v1alpha1.InstallFailed {
		t.Fatalf("status = %+v", f.gs().Status.Install)
	}
	// With a successful install the start fails (the Panel is down) but is only logged.
	_ = f.st.PatchSpec(context.Background(), uuid, map[string]any{"install": map[string]any{"startOnInstall": true}})
	if code, _ := f.do("POST", "/api/remote/servers/"+uuid+"/install", `{"successful":true}`, bearer); code != 204 {
		t.Fatalf("code = %d", code)
	}
	if f.gs().Status.Install.Result != v1alpha1.InstallSucceeded {
		t.Fatalf("status = %+v", f.gs().Status.Install)
	}

	if err := f.c.Delete(context.Background(), f.gs()); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.do("POST", "/api/remote/servers/"+uuid+"/install", `{}`, bearer); code != 404 {
		t.Fatalf("missing server code = %d", code)
	}
}

// withServer0 sets up a server without going through the Panel (which is down).
func (f *fixture) withServer0() {
	f.t.Helper()
	gs := &v1alpha1.GameServer{
		ObjectMeta: metav1.ObjectMeta{Name: names.ForUUID(uuid), Namespace: ns},
		Spec:       v1alpha1.GameServerSpec{Panel: v1alpha1.PanelSpec{UUID: uuid}, Install: v1alpha1.InstallSpec{Generation: 1}},
	}
	if err := f.c.Create(context.Background(), gs); err != nil {
		f.t.Fatal(err)
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.AgentSecret(uuid), Namespace: ns,
		Labels: map[string]string{v1alpha1.LabelServerUUID: uuid, v1alpha1.LabelComponent: "agent"}},
		Data: map[string][]byte{"token_id": []byte(agentID), "token": []byte(agentTok)}}
	if err := f.c.Create(context.Background(), sec); err != nil {
		f.t.Fatal(err)
	}
}

func TestContainerStatusRecordsAndForwards(t *testing.T) {
	f := newFixture(t, options{})
	f.withServer(false)
	if err := f.st.PatchSpec(context.Background(), uuid, map[string]any{"power": map[string]any{"desired": "Running"}}); err != nil {
		t.Fatal(err)
	}
	f.agentState.Store("starting")
	code, _ := f.do("POST", "/api/remote/servers/"+uuid+"/container/status", `{"data":{"previous_state":"offline","new_state":"starting"}}`, bearer)
	if code != 204 {
		t.Fatalf("code = %d", code)
	}
	gs := f.gs()
	if gs.Status.Process.State != "starting" || gs.Status.Process.Since == nil {
		t.Fatalf("process = %+v", gs.Status.Process)
	}
	if !f.fp.WaitForState(uuid, "starting", time.Second) {
		t.Fatal("state not forwarded to the Panel")
	}
	if gs.Spec.Power.Desired != v1alpha1.PowerRunning {
		t.Fatal("a normal transition must not change the desired power state")
	}
	// The state is read back from the agent, and the usage sample comes from it.
	for range 2 {
		select {
		case hit := <-f.agentHit:
			if hit != "GET /api/servers/"+uuid {
				t.Fatalf("agent hit %q", hit)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("state and usage were not read from the agent")
		}
	}
	waitFor(t, func() bool {
		u := f.gs().Status.Usage
		return u != nil && u.MemoryBytes == 4096 && u.CPUPercent == "12.3" && u.DiskBytes == 99
	})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The agent posts each state change from its own goroutine, so "starting"
// can arrive after "running". The status keeps the agent's current state.
func TestContainerStatusOutOfOrder(t *testing.T) {
	f := newFixture(t, options{})
	f.withServer(false)
	post := func(prev, next string) {
		t.Helper()
		body := fmt.Sprintf(`{"data":{"previous_state":%q,"new_state":%q}}`, prev, next)
		if code, _ := f.do("POST", "/api/remote/servers/"+uuid+"/container/status", body, bearer); code != 204 {
			t.Fatalf("code = %d", code)
		}
	}
	f.agentState.Store("running")
	post("starting", "running")
	post("offline", "starting")
	if got := f.gs().Status.Process.State; got != v1alpha1.ProcessRunning {
		t.Fatalf("process state %q after a late starting post, want running", got)
	}
	if !f.fp.WaitForState(uuid, "starting", time.Second) {
		t.Fatal("the posted change was not forwarded to the Panel")
	}

	// Without a ready agent the posted state is recorded, and the agent is
	// polled again until it answers: a late post must not stay recorded.
	ctx := context.Background()
	pod, _ := f.st.AgentPod(ctx, uuid)
	ip := pod.Status.PodIP
	pod.Status.PodIP = ""
	_ = f.c.Status().Update(ctx, pod)
	post("offline", "starting")
	if got := f.gs().Status.Process.State; got != v1alpha1.ProcessStarting {
		t.Fatalf("process state %q without an agent, want the posted starting", got)
	}
	pod, _ = f.st.AgentPod(ctx, uuid)
	pod.Status.PodIP = ip
	_ = f.c.Status().Update(ctx, pod)
	deadline := time.Now().Add(time.Second)
	for f.gs().Status.Process.State != v1alpha1.ProcessRunning {
		if time.Now().After(deadline) {
			t.Fatalf("process state %q once the agent answers, want running", f.gs().Status.Process.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// With several gateway replicas, a post's poll can return before another
// replica's newer poll and be written after it. The older result must not
// stay recorded.
func TestContainerStatusAcrossReplicas(t *testing.T) {
	f := newFixture(t, options{})
	f.withServer(false)
	other := &Handler{Store: f.h.Store, Panel: f.h.Panel, Agents: f.h.Agents, Log: f.h.Log, Sync: f.h.Sync}
	f.agentState.Store("starting")
	hook := func() {
		// The agent reaches running while this replica's poll is in flight,
		// and the other replica records it first.
		f.agentState.Store("running")
		other.recordState(context.Background(), uuid, "running")
	}
	f.onPoll.Store(&hook)
	f.h.recordState(context.Background(), uuid, "starting")
	if got := f.gs().Status.Process.State; got != v1alpha1.ProcessRunning {
		t.Fatalf("process state %q, want running", got)
	}
}

func TestContainerStatusIntentionalStop(t *testing.T) {
	body := `{"data":{"previous_state":"stopping","new_state":"offline"}}`
	stop := func(f *fixture) v1alpha1.PowerState {
		f.t.Helper()
		if code, _ := f.do("POST", "/api/remote/servers/"+uuid+"/container/status", body, bearer); code != 204 {
			f.t.Fatalf("code = %d", code)
		}
		return f.gs().Spec.Power.Desired
	}
	ctx := context.Background()

	f := newFixture(t, options{})
	f.withServer(false)
	_ = f.st.PatchSpec(ctx, uuid, map[string]any{"power": map[string]any{"desired": "Running"}})
	if stop(f) != v1alpha1.PowerStopped {
		t.Fatal("stopping -> offline must record an intentional stop")
	}

	// A stop the operator issued for a pending power generation (a restart
	// into a new game pod) is not the user's.
	_ = f.st.PatchSpec(ctx, uuid, map[string]any{"power": map[string]any{"desired": "Running", "generation": 2}})
	if stop(f) != v1alpha1.PowerRunning {
		t.Fatal("a stop for a pending power generation must not change the desired state")
	}

	// The stop half of a restart the operator issued is not the user's; a
	// stop long after the restart is.
	for _, c := range []struct {
		ago  time.Duration
		want v1alpha1.PowerState
	}{{time.Minute, v1alpha1.PowerRunning}, {restartWindow + time.Minute, v1alpha1.PowerStopped}} {
		f := newFixture(t, options{})
		f.withServer(false)
		_ = f.st.PatchSpec(ctx, uuid, map[string]any{"power": map[string]any{"desired": "Running"}})
		_ = f.st.PatchStatus(ctx, uuid, map[string]any{"power": map[string]any{"lastAction": map[string]any{"action": "restart", "at": metav1.NewTime(time.Now().Add(-c.ago))}}})
		if got := stop(f); got != c.want {
			t.Fatalf("restart %s ago: desired %s, want %s", c.ago, got, c.want)
		}
	}

	// While either pod is terminating (eviction, drain) the stop is not intentional.
	for _, name := range []string{names.AgentPod(uuid), names.Pod(uuid)} {
		f := newFixture(t, options{})
		f.withServer(false)
		_ = f.st.PatchSpec(ctx, uuid, map[string]any{"power": map[string]any{"desired": "Running"}})
		pod := &corev1.Pod{}
		if err := f.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, pod); apierrors.IsNotFound(err) {
			pod = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
			if err := f.c.Create(ctx, pod); err != nil {
				t.Fatal(err)
			}
		}
		pod.Finalizers = []string{"test/keep"}
		if err := f.c.Update(ctx, pod); err != nil {
			t.Fatal(err)
		}
		if err := f.c.Delete(ctx, pod); err != nil {
			t.Fatal(err)
		}
		if stop(f) != v1alpha1.PowerRunning {
			t.Fatalf("a stop while %s terminates must not change the desired state", name)
		}
	}
}

func TestContainerStatusBadBodyAndNoPod(t *testing.T) {
	f := newFixture(t, options{})
	f.withServer(false)
	if code, body := f.do("POST", "/api/remote/servers/"+uuid+"/container/status", `nope`, bearer); code != 400 || !strings.Contains(body, "BadRequest") {
		t.Fatalf("%d %s", code, body)
	}
	// Without a pod the stop is still intentional.
	ctx := context.Background()
	pod, _ := f.st.AgentPod(ctx, uuid)
	_ = f.c.Delete(ctx, pod)
	_ = f.st.PatchSpec(ctx, uuid, map[string]any{"power": map[string]any{"desired": "Running"}})
	f.do("POST", "/api/remote/servers/"+uuid+"/container/status", `{"data":{"previous_state":"stopping","new_state":"offline"}}`, bearer)
	if f.gs().Spec.Power.Desired != v1alpha1.PowerStopped {
		t.Fatal("stop without a pod must record desired=Stopped")
	}
}

func TestRecordUsageIgnoresAgentProblems(t *testing.T) {
	f := newFixture(t, options{})
	f.withServer(false)
	// No usable agent: unready pod.
	ctx := context.Background()
	pod, _ := f.st.AgentPod(ctx, uuid)
	pod.Status.PodIP = ""
	_ = f.c.Status().Update(ctx, pod)
	f.h.recordUsage(ctx, uuid)
	if f.gs().Status.Usage != nil {
		t.Fatal("no usage without an agent")
	}

	// Agent answers garbage.
	pod.Status.PodIP = "10.0.0.1"
	_ = f.c.Status().Update(ctx, pod)
	f.agent.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("garbage")) })
	f.h.recordUsage(ctx, uuid)
	if f.gs().Status.Usage != nil {
		t.Fatal("no usage from an unparsable state")
	}

	// Agent errors.
	f.agent.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	f.h.recordUsage(ctx, uuid)
	if f.gs().Status.Usage != nil {
		t.Fatal("no usage from a failing agent")
	}
}

func TestActivity(t *testing.T) {
	f := newFixture(t, options{})
	f.withServer(false)
	body := `{"data":[{"server":"` + uuid + `","event":"server:power.start"},{"server":"` + otherID + `","event":"x"},{"event":"no-server"},"junk"]}`
	if code, _ := f.do("POST", "/api/remote/activity", body, bearer); code != 204 {
		t.Fatalf("code = %d", code)
	}
	if len(f.fp.Activity) != 1 || f.fp.Activity[0]["server"] != uuid {
		t.Fatalf("forwarded = %v", f.fp.Activity)
	}

	// Rows of other servers only: nothing is forwarded at all.
	before := len(f.fp.CallsMatching("/activity"))
	if code, _ := f.do("POST", "/api/remote/activity", `{"data":[{"server":"`+otherID+`"}]}`, bearer); code != 204 {
		t.Fatalf("code = %d", code)
	}
	if len(f.fp.CallsMatching("/activity")) != before {
		t.Fatal("foreign rows must not reach the Panel")
	}

	if code, body := f.do("POST", "/api/remote/activity", `nope`, bearer); code != 400 || !strings.Contains(body, "BadRequest") {
		t.Fatalf("%d %s", code, body)
	}
}

func TestActivityPanelResponses(t *testing.T) {
	var status int
	var reply string
	panelSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	defer panelSrv.Close()
	f := newFixture(t, options{})
	f.withServer(false)
	f.h.Panel = panel.New(panelSrv.URL, nodeID, nodeTok, "test")
	row := `{"data":[{"server":"` + uuid + `"}]}`

	status, reply = 422, `{"errors":[]}`
	if code, body := f.do("POST", "/api/remote/activity", row, bearer); code != 204 || body != "" {
		t.Fatalf("rejected rows must be dropped: %d %q", code, body)
	}
	status, reply = 500, "boom"
	if code, body := f.do("POST", "/api/remote/activity", row, bearer); code != 500 || body != "boom" {
		t.Fatalf("other statuses are relayed: %d %q", code, body)
	}

	f.h.Panel = panel.New("http://127.0.0.1:1", nodeID, nodeTok, "test")
	if code, body := f.do("POST", "/api/remote/activity", row, bearer); code != 502 || !strings.Contains(body, "BadGateway") {
		t.Fatalf("%d %s", code, body)
	}
}

func TestBackupRequiresPendingRecord(t *testing.T) {
	f := newFixture(t, options{})
	f.withServer(false)
	if code, body := f.do("POST", "/api/remote/backups/b-1", `{"successful":true}`, bearer); code != 404 || !strings.Contains(body, "unknown backup") {
		t.Fatalf("%d %s", code, body)
	}
	// The record of another backup does not help.
	f.setPending("b-2")
	if code, _ := f.do("GET", "/api/remote/backups/b-1", "", bearer); code != 404 {
		t.Fatalf("code = %d", code)
	}
}

func TestBackupForwardsAndClearsPending(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []string
	)
	panelSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI()+" "+string(b))
		mu.Unlock()
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"parts":["u1"]}`))
			return
		}
		w.WriteHeader(204)
	}))
	defer panelSrv.Close()
	f := newFixture(t, options{})
	f.withServer(false)
	f.h.Panel = panel.New(panelSrv.URL, nodeID, nodeTok, "test")
	f.setPending("b-1", "b-2")

	// A GET (upload parts) is forwarded with its query and keeps the record.
	code, body := f.do("GET", "/api/remote/backups/b-1?size=10", "", bearer)
	if code != 200 || !strings.Contains(body, "parts") {
		t.Fatalf("%d %s", code, body)
	}
	if len(f.gs().Status.Backups.Pending) != 2 {
		t.Fatal("a GET must not clear the record")
	}
	// Completion and restore posts are forwarded and clear only their own record.
	if code, _ := f.do("POST", "/api/remote/backups/b-1", `{"successful":true}`, bearer); code != 204 {
		t.Fatalf("code = %d", code)
	}
	pending := f.gs().Status.Backups.Pending
	if len(pending) != 1 || pending[0].UUID != "b-2" {
		t.Fatalf("pending = %+v", pending)
	}
	if code, _ := f.do("POST", "/api/remote/backups/b-2/restore", `{}`, bearer); code != 204 {
		t.Fatalf("code = %d", code)
	}
	if len(f.gs().Status.Backups.Pending) != 0 {
		t.Fatal("pending must be empty")
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{
		`GET /api/remote/backups/b-1?size=10 `,
		`POST /api/remote/backups/b-1 {"successful":true}`,
		`POST /api/remote/backups/b-2/restore {}`,
	}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("panel saw %q", seen)
	}
}

func TestBackupErrors(t *testing.T) {
	f := newFixture(t, options{})
	f.withServer(false)
	f.setPending("b-1")
	f.h.Panel = panel.New("http://127.0.0.1:1", nodeID, nodeTok, "test")
	if code, body := f.do("POST", "/api/remote/backups/b-1", `{}`, bearer); code != 502 || !strings.Contains(body, "BadGateway") {
		t.Fatalf("%d %s", code, body)
	}
	if len(f.gs().Status.Backups.Pending) != 1 {
		t.Fatal("a failed forward must keep the record")
	}
	if err := f.c.Delete(context.Background(), f.gs()); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.do("POST", "/api/remote/backups/b-1", `{}`, bearer); code != 404 {
		t.Fatalf("missing server code = %d", code)
	}
}

func TestSftpAuth(t *testing.T) {
	f := newFixture(t, options{})
	f.withServer(false)
	code, body := f.do("POST", "/api/remote/sftp/auth", `{"username":"user","password":"pw"}`, bearer)
	if code != 200 || !strings.Contains(body, `"server":"`+uuid+`"`) {
		t.Fatalf("%d %s", code, body)
	}
	for name, body := range map[string]string{
		"wrong password": `{"username":"user","password":"nope"}`,
		"other server":   `{"username":"user","password":"foreign"}`,
	} {
		if code, _ := f.do("POST", "/api/remote/sftp/auth", body, bearer); code != 403 {
			t.Errorf("%s: code = %d", name, code)
		}
	}
	if code, b := f.do("POST", "/api/remote/sftp/auth", `nope`, bearer); code != 400 || !strings.Contains(b, "BadRequest") {
		t.Fatalf("%d %s", code, b)
	}
	f.h.Sftp = nil
	if code, b := f.do("POST", "/api/remote/sftp/auth", `{"username":"user","password":"pw"}`, bearer); code != 403 || !strings.Contains(b, "not configured") {
		t.Fatalf("%d %s", code, b)
	}
}
