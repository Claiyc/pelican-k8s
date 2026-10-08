package serversync

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/gateway/panel"
	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/test/fakepanel"
)

const (
	ns      = "pelican-servers"
	nodeID  = "nodeid"
	nodeTok = "node-token"
	uuid    = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	uuid2   = "2a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
)

type fixture struct {
	t     *testing.T
	c     client.Client
	st    *store.Store
	fp    *fakepanel.Panel
	sync  *Syncer
	panel *panel.Client
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	cls := &v1alpha1.GameServerClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec:       v1alpha1.GameServerClassSpec{Resources: v1alpha1.ResourcesSpec{UnlimitedMemoryMiB: 2048}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.GameServer{}).WithObjects(cls).Build()
	fp := fakepanel.New(nodeID, nodeTok)
	srv := httptest.NewServer(fp.Handler())
	t.Cleanup(srv.Close)
	st := store.New(c, ns, "default")
	pc := panel.New(srv.URL, nodeID, nodeTok, "test")
	return &fixture{t: t, c: c, st: st, fp: fp, panel: pc, sync: &Syncer{Store: st, Panel: pc, Timezone: "Europe/Berlin", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
}

func (f *fixture) addPanelServer(id string, memory int) *fakepanel.Server {
	f.t.Helper()
	s := &fakepanel.Server{
		UUID:                 id,
		Settings:             fakepanel.PaperSettings(id, 30565, memory),
		ProcessConfiguration: fakepanel.PaperProcessConfiguration(),
		Install:              fakepanel.InstallScript{ContainerImage: "installer:alpine", Entrypoint: "ash", Script: "#!/bin/ash\r\necho hi\r\n"},
	}
	f.fp.Add(s)
	return s
}

func (f *fixture) gs(id string) *v1alpha1.GameServer {
	f.t.Helper()
	gs, err := f.st.Get(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return gs
}

func TestRevision(t *testing.T) {
	a := Revision([]byte("a"), []byte("b"))
	if !strings.HasPrefix(a, "sha256:") || len(a) != len("sha256:")+32 {
		t.Fatalf("revision = %q", a)
	}
	if a != Revision([]byte("a"), []byte("b")) {
		t.Fatal("revision is not stable")
	}
	// The separator keeps ("ab","") and ("a","b") apart.
	if Revision([]byte("ab"), nil) == a {
		t.Fatal("boundary between settings and process configuration is lost")
	}
}

func TestSplitSettings(t *testing.T) {
	raw, env, err := splitSettings(json.RawMessage(`{"uuid":"x","environment":{"A":"1"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"uuid":"x"}` || env["A"] != "1" {
		t.Fatalf("raw = %s env = %v", raw, env)
	}
	if _, env, err := splitSettings(json.RawMessage(`{"uuid":"x"}`)); err != nil || env != nil {
		t.Fatalf("no environment: %v %v", env, err)
	}
	if _, _, err := splitSettings(json.RawMessage(`[1]`)); err == nil {
		t.Fatal("non-object settings must fail")
	}
}

func TestParseInvocation(t *testing.T) {
	got := ParseInvocation("java -Xmx{{SERVER_MEMORY}}M -jar {{JAR}} --port {{SERVER_PORT}} --ip {{SERVER_IP}} {{UNKNOWN}}",
		map[string]string{"JAR": "s.jar"}, 512, 25565, "1.2.3.4")
	want := "java -Xmx512M -jar s.jar --port 25565 --ip 1.2.3.4 ${UNKNOWN}"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestStringify(t *testing.T) {
	tests := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"s", "s"},
		{true, "true"},
		{float64(3), "3"},
		{1.5, "1.5"},
		{[]any{"a", float64(1)}, `["a",1]`},
		{map[string]any{"k": "v"}, `{"k":"v"}`},
	}
	for _, tt := range tests {
		if got := stringify(tt.in); got != tt.want {
			t.Errorf("stringify(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCompactJSON(t *testing.T) {
	if got := string(compactJSON(json.RawMessage("{ \"a\" :  1 }"))); got != `{"a":1}` {
		t.Fatalf("got %q", got)
	}
	if got := string(compactJSON(nil)); got != "null" {
		t.Fatalf("empty = %q", got)
	}
	if got := string(compactJSON(json.RawMessage("{broken"))); got != "{broken" {
		t.Fatalf("invalid input must pass through, got %q", got)
	}
}

func TestCreate(t *testing.T) {
	f := newFixture(t)
	f.addPanelServer(uuid, 0)
	ctx := context.Background()
	if err := f.sync.Create(ctx, uuid, true); err != nil {
		t.Fatal(err)
	}
	gs := f.gs(uuid)
	if gs.Spec.Panel.UUID != uuid || gs.Spec.Panel.UUIDShort != names.Short(uuid) || gs.Spec.ClassName != "default" {
		t.Fatalf("spec = %+v", gs.Spec)
	}
	if gs.Spec.Power.Desired != v1alpha1.PowerStopped {
		t.Fatalf("a new server must be stopped, got %q", gs.Spec.Power.Desired)
	}
	in := gs.Spec.Install
	if in.Generation != 1 || in.Image != "installer:alpine" || in.Entrypoint != "ash" || !in.StartOnInstall || in.ScriptConfigMap != names.InstallConfigMap(uuid, 1) {
		t.Fatalf("install = %+v", in)
	}
	if gs.Labels[v1alpha1.LabelServerUUID] != uuid || gs.Labels[v1alpha1.LabelEggUUID] == "" || gs.Annotations[v1alpha1.AnnotationPanelName] != "Spike" {
		t.Fatalf("metadata = %v %v", gs.Labels, gs.Annotations)
	}
	if strings.Contains(string(gs.Spec.Panel.Settings.Raw), "environment") {
		t.Fatal("the environment must live in the Secret, not in spec.panel.settings")
	}

	env, err := f.st.EnvSecret(ctx, uuid)
	if err != nil {
		t.Fatal(err)
	}
	if env["SERVER_JARFILE"] != "server.jar" || env["SERVER_PORT"] != "30565" || env["SERVER_IP"] != "0.0.0.0" || env["TZ"] != "Europe/Berlin" {
		t.Fatalf("env = %v", env)
	}
	if !strings.Contains(env["STARTUP"], "-jar server.jar") || strings.Contains(env["STARTUP"], "{{") {
		t.Fatalf("STARTUP = %q", env["STARTUP"])
	}
	script, err := f.st.InstallScript(ctx, uuid, 1)
	if err != nil || script != "#!/bin/ash\necho hi\n" {
		t.Fatalf("script = %q, %v", script, err)
	}
	// The env secret is owned by the server so it is garbage collected.
	sec := &corev1.Secret{}
	if err := f.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: names.EnvSecret(uuid)}, sec); err != nil || len(sec.OwnerReferences) != 1 {
		t.Fatalf("secret owner refs = %v (%v)", sec.OwnerReferences, err)
	}
}

func TestCreateKeepsTZFromEnvironment(t *testing.T) {
	f := newFixture(t)
	s := f.addPanelServer(uuid, 1024)
	s.Settings["environment"].(map[string]any)["TZ"] = "Asia/Tokyo"
	if err := f.sync.Create(context.Background(), uuid, false); err != nil {
		t.Fatal(err)
	}
	env, _ := f.st.EnvSecret(context.Background(), uuid)
	if env["TZ"] != "Asia/Tokyo" {
		t.Fatalf("TZ = %q", env["TZ"])
	}
}

func TestCreateErrors(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.sync.Create(ctx, uuid, false); err == nil || !strings.Contains(err.Error(), "fetch configuration") {
		t.Fatalf("unknown server: %v", err)
	}
	// Settings that belong to another server are refused.
	s := f.addPanelServer(uuid, 1024)
	s.Settings["uuid"] = uuid2
	if err := f.sync.Create(ctx, uuid, false); err == nil || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("uuid mismatch: %v", err)
	}
	if f.st.Exists(ctx, uuid) {
		t.Fatal("nothing may be created on a mismatch")
	}
}

func TestCreateExistingSyncsAndRequestsInstall(t *testing.T) {
	f := newFixture(t)
	s := f.addPanelServer(uuid, 1024)
	ctx := context.Background()
	if err := f.sync.Create(ctx, uuid, false); err != nil {
		t.Fatal(err)
	}
	s.Settings["build"].(map[string]any)["memory_limit"] = 4096
	if err := f.sync.Create(ctx, uuid, false); err != nil {
		t.Fatal(err)
	}
	gs := f.gs(uuid)
	if gs.Spec.Install.Generation != 2 || !strings.Contains(string(gs.Spec.Panel.Settings.Raw), "4096") {
		t.Fatalf("install gen %d, settings %s", gs.Spec.Install.Generation, gs.Spec.Panel.Settings.Raw)
	}
}

func TestApplyOnlyPatchesOnChange(t *testing.T) {
	f := newFixture(t)
	s := f.addPanelServer(uuid, 1024)
	ctx := context.Background()
	if err := f.sync.Create(ctx, uuid, false); err != nil {
		t.Fatal(err)
	}
	before := f.gs(uuid)
	if err := f.sync.Sync(ctx, uuid); err != nil {
		t.Fatal(err)
	}
	if after := f.gs(uuid); after.ResourceVersion != before.ResourceVersion {
		t.Fatal("an unchanged configuration must not touch the CR")
	}

	s.Settings["meta"].(map[string]any)["name"] = "Renamed"
	s.Settings["environment"].(map[string]any)["SERVER_JARFILE"] = "other.jar"
	if err := f.sync.Sync(ctx, uuid); err != nil {
		t.Fatal(err)
	}
	after := f.gs(uuid)
	if after.Spec.Panel.PanelRevision == before.Spec.Panel.PanelRevision || after.Annotations[v1alpha1.AnnotationPanelName] != "Renamed" {
		t.Fatalf("not updated: %+v %v", after.Spec.Panel.PanelRevision, after.Annotations)
	}
	env, _ := f.st.EnvSecret(ctx, uuid)
	if env["SERVER_JARFILE"] != "other.jar" {
		t.Fatalf("env = %v", env)
	}

	// An environment-only change is picked up even though the revision is unchanged.
	s.Settings["environment"].(map[string]any)["SERVER_JARFILE"] = "third.jar"
	if err := f.sync.Sync(ctx, uuid); err != nil {
		t.Fatal(err)
	}
	if env, _ := f.st.EnvSecret(ctx, uuid); env["SERVER_JARFILE"] != "third.jar" {
		t.Fatalf("env-only change lost: %v", env)
	}
}

func TestApplyErrors(t *testing.T) {
	f := newFixture(t)
	f.addPanelServer(uuid, 1024)
	ctx := context.Background()
	if err := f.sync.Sync(ctx, uuid); !apierrors.IsNotFound(err) {
		t.Fatalf("sync of an unknown CR: %v", err)
	}
	if err := f.sync.Sync(ctx, "missing"); err == nil || !strings.Contains(err.Error(), "fetch configuration") {
		t.Fatalf("sync of an unknown panel server: %v", err)
	}
	_ = f.sync.Create(ctx, uuid, false)
	if err := f.sync.Apply(ctx, uuid, &panel.ServerConfiguration{Settings: json.RawMessage(`[]`)}); err == nil {
		t.Fatal("malformed settings must fail")
	}
	if err := f.sync.Apply(ctx, uuid, &panel.ServerConfiguration{Settings: json.RawMessage(`{"environment":{}}`)}); err == nil {
		t.Fatal("settings without a uuid must fail")
	}
}

func TestAdopt(t *testing.T) {
	f := newFixture(t)
	s := f.addPanelServer(uuid, 1024)
	ctx := context.Background()
	cfg, _ := f.panel.GetServerConfiguration(ctx, uuid)
	if err := f.sync.Adopt(ctx, uuid, cfg); err != nil {
		t.Fatal(err)
	}
	gs := f.gs(uuid)
	if gs.Spec.Install.Generation != 0 {
		t.Fatalf("an adopted server must not be installed: %+v", gs.Spec.Install)
	}
	if _, err := f.st.EnvSecret(ctx, uuid); err != nil {
		t.Fatal(err)
	}
	// Adopting twice is harmless.
	if err := f.sync.Adopt(ctx, uuid, cfg); err != nil {
		t.Fatalf("second adopt: %v", err)
	}
	s.Settings["uuid"] = "other"
	bad, _ := f.panel.GetServerConfiguration(ctx, uuid)
	if err := f.sync.Adopt(ctx, uuid, bad); err == nil {
		t.Fatal("mismatching uuid must fail")
	}
}

func TestRequestInstall(t *testing.T) {
	f := newFixture(t)
	s := f.addPanelServer(uuid, 1024)
	ctx := context.Background()
	if err := f.sync.Create(ctx, uuid, true); err != nil {
		t.Fatal(err)
	}
	s.Install = fakepanel.InstallScript{ContainerImage: "installer:v2", Entrypoint: "bash", Script: "echo two"}
	if err := f.sync.RequestInstall(ctx, uuid, true); err != nil {
		t.Fatal(err)
	}
	in := f.gs(uuid).Spec.Install
	if in.Generation != 2 || !in.Reinstall || in.Image != "installer:v2" || in.Entrypoint != "bash" || in.StartOnInstall || in.ScriptConfigMap != names.InstallConfigMap(uuid, 2) {
		t.Fatalf("install = %+v", in)
	}
	if script, err := f.st.InstallScript(ctx, uuid, 2); err != nil || script != "echo two" {
		t.Fatalf("script = %q, %v", script, err)
	}

	if err := f.sync.RequestInstall(ctx, "missing", false); err == nil {
		t.Fatal("unknown server must fail")
	}
}

// Two power actions on different gateway replicas get two generations: the
// one that loses the write reads the other's and takes the next.
func TestPowerTakesItsOwnGeneration(t *testing.T) {
	f := newFixture(t)
	f.addPanelServer(uuid, 1024)
	ctx := context.Background()
	if err := f.sync.Create(ctx, uuid, false); err != nil {
		t.Fatal(err)
	}
	raced := false
	f.st.Client = interceptor.NewClient(f.c.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if _, ok := obj.(*v1alpha1.GameServer); ok && !raced {
			raced = true
			other := []byte(`{"spec":{"power":{"desired":"Running","generation":1}}}`)
			if err := c.Patch(ctx, obj.DeepCopyObject().(client.Object), client.RawPatch(types.MergePatchType, other)); err != nil {
				t.Fatal(err)
			}
		}
		return c.Patch(ctx, obj, patch, opts...)
	}})
	if err := f.sync.Power(ctx, uuid, "stop"); err != nil {
		t.Fatal(err)
	}
	if p := f.gs(uuid).Spec.Power; !raced || p.Generation != 2 || p.Desired != v1alpha1.PowerStopped {
		t.Fatalf("power = %+v (raced %v), want the stop at generation 2", p, raced)
	}
}

func TestPower(t *testing.T) {
	f := newFixture(t)
	s := f.addPanelServer(uuid, 1024)
	ctx := context.Background()
	if err := f.sync.Create(ctx, uuid, false); err != nil {
		t.Fatal(err)
	}
	type want struct {
		desired v1alpha1.PowerState
		gen     int64
		kill    bool
	}
	steps := []struct {
		action string
		want   want
	}{
		{"start", want{v1alpha1.PowerRunning, 1, false}},
		{"stop", want{v1alpha1.PowerStopped, 2, false}},
		{"restart", want{v1alpha1.PowerRunning, 3, false}},
		{"kill", want{v1alpha1.PowerStopped, 4, true}},
	}
	for _, st := range steps {
		if err := f.sync.Power(ctx, uuid, st.action); err != nil {
			t.Fatalf("%s: %v", st.action, err)
		}
		p := f.gs(uuid).Spec.Power
		if p.Desired != st.want.desired || p.Generation != st.want.gen || p.Kill != st.want.kill {
			t.Fatalf("%s: power = %+v, want %+v", st.action, p, st.want)
		}
	}
	if err := f.sync.Power(ctx, uuid, "explode"); err == nil || !strings.Contains(err.Error(), "invalid power action") {
		t.Fatalf("invalid action: %v", err)
	}

	// Start refreshes the configuration from the Panel first.
	s.Settings["environment"].(map[string]any)["SERVER_JARFILE"] = "fresh.jar"
	if err := f.sync.Power(ctx, uuid, "start"); err != nil {
		t.Fatal(err)
	}
	if env, _ := f.st.EnvSecret(ctx, uuid); env["SERVER_JARFILE"] != "fresh.jar" {
		t.Fatalf("start must sync first, env = %v", env)
	}
	// Stop does not need the Panel at all.
	f.fp.Servers = map[string]*fakepanel.Server{}
	if err := f.sync.Power(ctx, uuid, "stop"); err != nil {
		t.Fatalf("stop without the Panel: %v", err)
	}
	if err := f.sync.Power(ctx, uuid, "start"); err == nil {
		t.Fatal("start must fail when the sync fails")
	}
	if err := f.sync.Power(ctx, "missing", "stop"); err == nil {
		t.Fatal("unknown server must fail")
	}
}

func TestDelete(t *testing.T) {
	f := newFixture(t)
	f.addPanelServer(uuid, 1024)
	ctx := context.Background()
	_ = f.sync.Create(ctx, uuid, false)
	if err := f.sync.Delete(ctx, uuid); err != nil {
		t.Fatal(err)
	}
	if f.st.Exists(ctx, uuid) {
		t.Fatal("server still exists")
	}
	if err := f.sync.Delete(ctx, uuid); err != nil {
		t.Fatalf("deleting a missing server must succeed: %v", err)
	}
}

func decode(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAgentConfiguration(t *testing.T) {
	f := newFixture(t)
	f.addPanelServer(uuid, 0)
	ctx := context.Background()
	_ = f.sync.Create(ctx, uuid, false)

	settings, proc, err := f.sync.AgentConfiguration(ctx, f.gs(uuid))
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, settings)
	def := m["allocations"].(map[string]any)["default"].(map[string]any)
	if def["ip"] != BindIP || def["port"].(float64) != 30565 {
		t.Fatalf("default allocation = %v", def)
	}
	if mem := m["build"].(map[string]any)["memory_limit"].(float64); mem != 2048 {
		t.Fatalf("unlimited memory must use the class default, got %v", mem)
	}
	env := m["environment"].(map[string]any)
	if env["SERVER_JARFILE"] != "server.jar" {
		t.Fatalf("environment = %v", env)
	}
	if !strings.Contains(string(proc), "server.properties") {
		t.Fatalf("process configuration = %s", proc)
	}
}

func TestAgentConfigurationKeepsLimitsAndMissingAllocation(t *testing.T) {
	f := newFixture(t)
	s := f.addPanelServer(uuid, 1024)
	s.Settings["allocations"] = map[string]any{"default": map[string]any{"ip": "127.0.0.1", "port": 0}}
	ctx := context.Background()
	_ = f.sync.Create(ctx, uuid, false)

	settings, _, err := f.sync.AgentConfiguration(ctx, f.gs(uuid))
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, settings)
	if ip := m["allocations"].(map[string]any)["default"].(map[string]any)["ip"]; ip != "127.0.0.1" {
		t.Fatalf("a server without an allocation keeps its ip, got %v", ip)
	}
	if mem := m["build"].(map[string]any)["memory_limit"].(float64); mem != 1024 {
		t.Fatalf("memory = %v", mem)
	}
}

func TestAgentConfigurationMissingEnvSecretAndBadSettings(t *testing.T) {
	f := newFixture(t)
	f.addPanelServer(uuid, 1024)
	ctx := context.Background()
	_ = f.sync.Create(ctx, uuid, false)
	gs := f.gs(uuid)

	if err := f.c.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: names.EnvSecret(uuid)}}); err != nil {
		t.Fatal(err)
	}
	settings, _, err := f.sync.AgentConfiguration(ctx, gs)
	if err != nil {
		t.Fatalf("a missing env secret is tolerated: %v", err)
	}
	if env := decode(t, settings)["environment"].(map[string]any); len(env) != 0 {
		t.Fatalf("environment = %v", env)
	}

	gs.Spec.Panel.Settings.Raw = []byte("{bad")
	if _, _, err := f.sync.AgentConfiguration(ctx, gs); err == nil {
		t.Fatal("unparsable settings must fail")
	}
}

func TestResync(t *testing.T) {
	f := newFixture(t)
	s1 := f.addPanelServer(uuid, 1024)
	ctx := context.Background()
	_ = f.sync.Create(ctx, uuid, false)
	// A server only the cluster knows about.
	f.addPanelServer(uuid2, 512)
	if err := f.sync.Create(ctx, uuid2, false); err != nil {
		t.Fatal(err)
	}
	delete(f.fp.Servers, uuid2)
	// A server only the Panel knows about.
	const uuid3 = "3a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	f.addPanelServer(uuid3, 256)
	// A changed configuration.
	s1.Settings["build"].(map[string]any)["memory_limit"] = 8192

	if err := f.sync.Resync(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(f.gs(uuid).Spec.Panel.Settings.Raw), "8192") {
		t.Fatal("changed configuration not applied")
	}
	if gs := f.gs(uuid3); gs.Spec.Install.Generation != 0 {
		t.Fatalf("adopted server must not be reinstalled: %+v", gs.Spec.Install)
	}
	cond := findCondition(f.gs(uuid2), v1alpha1.ConditionOrphaned)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("orphan condition = %+v", cond)
	}
	if !f.st.Exists(ctx, uuid2) {
		t.Fatal("orphans must never be deleted")
	}
	if findCondition(f.gs(uuid), v1alpha1.ConditionOrphaned) != nil {
		t.Fatal("a listed server must not be flagged")
	}

	// The server returns to the Panel: the flag clears.
	f.addPanelServer(uuid2, 512)
	if err := f.sync.Resync(ctx); err != nil {
		t.Fatal(err)
	}
	cond = findCondition(f.gs(uuid2), v1alpha1.ConditionOrphaned)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("orphan condition after return = %+v", cond)
	}
}

func TestResyncToleratesPerServerFailures(t *testing.T) {
	f := newFixture(t)
	s1 := f.addPanelServer(uuid, 1024)
	ctx := context.Background()
	_ = f.sync.Create(ctx, uuid, false)
	const uuid3 = "3a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	s3 := f.addPanelServer(uuid3, 256)

	// Unparsable settings for a known server and for a server to adopt.
	s1.Settings["environment"] = "not a map"
	s1.Settings["uuid"] = []int{1}
	s3.Settings["uuid"] = "mismatch"
	if err := f.sync.Resync(ctx); err != nil {
		t.Fatalf("per-server failures must not fail the resync: %v", err)
	}
	if f.st.Exists(ctx, uuid3) {
		t.Fatal("a server with bad settings must not be adopted")
	}
}

func TestResyncPanelDown(t *testing.T) {
	f := newFixture(t)
	f.sync.Panel = panel.New("http://127.0.0.1:1", nodeID, nodeTok, "test")
	if err := f.sync.Resync(context.Background()); err == nil {
		t.Fatal("an unreachable Panel must fail the resync")
	}
}

func TestRunResyncStopsWithContext(t *testing.T) {
	f := newFixture(t)
	f.addPanelServer(uuid, 1024)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		f.sync.RunResync(ctx, 10*time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !f.st.Exists(context.Background(), uuid) {
		if time.Now().After(deadline) {
			t.Fatal("the initial resync did not adopt the server")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunResync did not stop")
	}
}

func TestRunResyncLogsFailures(t *testing.T) {
	f := newFixture(t)
	f.sync.Panel = panel.New("http://127.0.0.1:1", nodeID, nodeTok, "test")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		f.sync.RunResync(ctx, time.Hour)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunResync did not stop")
	}
}
