package store

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/internal/operator/render"
)

const (
	ns   = "servers"
	uid1 = "11111111-1111-1111-1111-111111111111"
	uid2 = "22222222-2222-2222-2222-222222222222"
)

func newStore(t *testing.T, objs ...client.Object) *Store {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.GameServer{}).Build()
	return New(c, ns, "default")
}

func gameServer(uuid, class string) *v1alpha1.GameServer {
	return &v1alpha1.GameServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: names.ForUUID(uuid)},
		Spec:       v1alpha1.GameServerSpec{ClassName: class},
	}
}

func agentSecret(uuid, id, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: names.AgentSecret(uuid),
			Labels: map[string]string{v1alpha1.LabelComponent: "agent", v1alpha1.LabelServerUUID: uuid},
		},
		Data: map[string][]byte{"token_id": []byte(id), "token": []byte(token)},
	}
}

func TestGetExistsList(t *testing.T) {
	s := newStore(t, gameServer(uid1, ""), gameServer(uid2, ""))
	ctx := context.Background()
	gs, err := s.Get(ctx, uid1)
	if err != nil || gs.Name != names.ForUUID(uid1) {
		t.Fatalf("Get = %v, %v", gs, err)
	}
	if _, err := s.Get(ctx, "missing"); err == nil {
		t.Fatal("Get of a missing server succeeded")
	}
	if !s.Exists(ctx, uid1) || s.Exists(ctx, "missing") {
		t.Fatal("Exists is wrong")
	}
	list, err := s.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %d, %v", len(list), err)
	}
}

func TestClassAndClassSpec(t *testing.T) {
	def := &v1alpha1.GameServerClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	def.Spec.Exposure.ExternalIPs = []string{"192.0.2.1"}
	big := &v1alpha1.GameServerClass{ObjectMeta: metav1.ObjectMeta{Name: "big"}}
	big.Spec.Exposure.ExternalIPs = []string{"192.0.2.2"}
	s := newStore(t, def, big)
	ctx := context.Background()

	cls, err := s.Class(ctx, nil)
	if err != nil || cls.Name != "default" {
		t.Fatalf("nil server: %v, %v", cls, err)
	}
	cls, err = s.Class(ctx, gameServer(uid1, ""))
	if err != nil || cls.Name != "default" {
		t.Fatalf("empty className: %v, %v", cls, err)
	}
	cls, err = s.Class(ctx, gameServer(uid1, "big"))
	if err != nil || cls.Name != "big" {
		t.Fatalf("explicit class: %v, %v", cls, err)
	}
	if _, err := s.Class(ctx, gameServer(uid1, "nope")); err == nil {
		t.Fatal("unknown class succeeded")
	}
	if got := s.ClassSpec(ctx, gameServer(uid1, "big")).Exposure.ExternalIPs; len(got) != 1 || got[0] != "192.0.2.2" {
		t.Fatalf("ClassSpec = %v", got)
	}
	if got := s.ClassSpec(ctx, gameServer(uid1, "nope")); len(got.Exposure.ExternalIPs) != 0 {
		t.Fatalf("unknown class must give an empty spec, got %+v", got)
	}
}

func TestPod(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: names.Pod(uid1)}}
	s := newStore(t, pod)
	ctx := context.Background()
	got, err := s.Pod(ctx, uid1)
	if err != nil || got == nil {
		t.Fatalf("Pod = %v, %v", got, err)
	}
	got, err = s.Pod(ctx, uid2)
	if err != nil || got != nil {
		t.Fatalf("missing pod must be nil without error, got %v, %v", got, err)
	}
	if got, err := s.AgentPod(ctx, uid1); err != nil || got != nil {
		t.Fatalf("the game pod is not the agent pod: %v, %v", got, err)
	}
}

func TestSecretsAndConfigMaps(t *testing.T) {
	env := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: names.EnvSecret(uid1)},
		Data:       map[string][]byte{"A": []byte("1"), "B": []byte("two")},
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: names.InstallConfigMap(uid1, 3)},
		Data:       map[string]string{"install.sh": "echo hi"},
	}
	s := newStore(t, env, cm, agentSecret(uid1, "tid", "tok"))
	ctx := context.Background()

	id, tok, err := s.AgentToken(ctx, uid1)
	if err != nil || id != "tid" || tok != "tok" {
		t.Fatalf("AgentToken = %q %q %v", id, tok, err)
	}
	if _, _, err := s.AgentToken(ctx, uid2); err == nil {
		t.Fatal("AgentToken of a missing secret succeeded")
	}

	vars, err := s.EnvSecret(ctx, uid1)
	if err != nil || len(vars) != 2 || vars["B"] != "two" {
		t.Fatalf("EnvSecret = %v, %v", vars, err)
	}
	if _, err := s.EnvSecret(ctx, uid2); err == nil {
		t.Fatal("EnvSecret of a missing secret succeeded")
	}

	script, err := s.InstallScript(ctx, uid1, 3)
	if err != nil || script != "echo hi" {
		t.Fatalf("InstallScript = %q, %v", script, err)
	}
	if _, err := s.InstallScript(ctx, uid1, 4); err == nil {
		t.Fatal("InstallScript of another generation succeeded")
	}
}

func TestServerByAgentToken(t *testing.T) {
	s := newStore(t, gameServer(uid1, ""), agentSecret(uid1, "tid", "tok"))
	ctx := context.Background()

	for _, bad := range []string{"", "nodot", ".tok", "tid.", "tid.wrong", "other.tok"} {
		if _, ok := s.ServerByAgentToken(ctx, bad); ok {
			t.Errorf("%q must not authenticate", bad)
		}
	}
	// The first lookup fills the cache from the secret list, the second uses it.
	for i := 0; i < 2; i++ {
		uuid, ok := s.ServerByAgentToken(ctx, "tid.tok")
		if !ok || uuid != uid1 {
			t.Fatalf("lookup %d = %q, %v", i, uuid, ok)
		}
	}
	// A rotated token misses the cache and is picked up by the refresh.
	// (The old token keeps working until the server is deleted because a
	// cache hit is not re-validated against the secret.)
	sec := agentSecret(uid1, "tid", "rotated")
	existing := &corev1.Secret{}
	if err := s.Client.Get(ctx, client.ObjectKeyFromObject(sec), existing); err != nil {
		t.Fatal(err)
	}
	existing.Data["token"] = []byte("rotated")
	if err := s.Client.Update(ctx, existing); err != nil {
		t.Fatal(err)
	}
	if uuid, ok := s.ServerByAgentToken(ctx, "tid.rotated"); !ok || uuid != uid1 {
		t.Fatalf("rotated token = %q, %v", uuid, ok)
	}
}

func TestServerByAgentTokenSkipsMalformedSecrets(t *testing.T) {
	noLabel := agentSecret(uid2, "x", "y")
	delete(noLabel.Labels, v1alpha1.LabelServerUUID)
	wrongName := agentSecret(uid1, "w", "z")
	wrongName.Name = "something-else"
	s := newStore(t, noLabel, wrongName)
	for _, b := range []string{"x.y", "w.z"} {
		if _, ok := s.ServerByAgentToken(context.Background(), b); ok {
			t.Errorf("%q from a malformed secret must not authenticate", b)
		}
	}
}

func TestPatchSpecAndStatus(t *testing.T) {
	s := newStore(t, gameServer(uid1, ""))
	ctx := context.Background()

	if err := s.PatchSpec(ctx, uid1, map[string]any{"power": map[string]any{"desired": "Running", "generation": 2}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PatchStatus(ctx, uid1, map[string]any{"process": map[string]any{"state": "running"}}); err != nil {
		t.Fatal(err)
	}
	gs, _ := s.Get(ctx, uid1)
	if gs.Spec.Power.Desired != v1alpha1.PowerRunning || gs.Spec.Power.Generation != 2 {
		t.Fatalf("spec = %+v", gs.Spec.Power)
	}
	if gs.Status.Process.State != "running" {
		t.Fatalf("status = %+v", gs.Status.Process)
	}
	if err := s.PatchSpec(ctx, uid2, map[string]any{}); err == nil {
		t.Fatal("PatchSpec of a missing server succeeded")
	}
	if err := s.PatchStatus(ctx, uid2, map[string]any{}); err == nil {
		t.Fatal("PatchStatus of a missing server succeeded")
	}
	if err := s.PatchSpec(ctx, uid1, map[string]any{"bad": func() {}}); err == nil {
		t.Fatal("unmarshalable spec succeeded")
	}
	if err := s.PatchStatus(ctx, uid1, map[string]any{"bad": func() {}}); err == nil {
		t.Fatal("unmarshalable status succeeded")
	}
}

func TestPatchStatusAt(t *testing.T) {
	s := newStore(t, gameServer(uid1, ""))
	ctx := context.Background()
	gs, _ := s.Get(ctx, uid1)
	if err := s.PatchStatusAt(ctx, uid1, gs.ResourceVersion, map[string]any{"process": map[string]any{"state": "starting"}}); err != nil {
		t.Fatal(err)
	}
	err := s.PatchStatusAt(ctx, uid1, gs.ResourceVersion, map[string]any{"process": map[string]any{"state": "offline"}})
	if !apierrors.IsConflict(err) {
		t.Fatalf("patch at a stale version: %v, want a conflict", err)
	}
	if gs, _ = s.Get(ctx, uid1); gs.Status.Process.State != "starting" {
		t.Fatalf("status = %+v", gs.Status.Process)
	}
	if err := s.PatchStatusAt(ctx, uid1, "1", map[string]any{"bad": func() {}}); err == nil {
		t.Fatal("unmarshalable status succeeded")
	}
}

func TestUpdateBackups(t *testing.T) {
	gs := gameServer(uid1, "")
	gs.Status.Backups.Pending = []v1alpha1.PendingBackup{{UUID: "ended", Agent: "pod-0/0"}, {UUID: "live", Agent: "pod-1/2"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: names.AgentPod(uid1), UID: "pod-1"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: render.AgentContainer, RestartCount: 2}}}}
	s := newStore(t, gs, pod)
	ctx := context.Background()
	var seen []string
	var instance string
	err := s.UpdateBackups(ctx, uid1, func(live []v1alpha1.PendingBackup, agent string) []v1alpha1.PendingBackup {
		seen, instance = nil, agent
		for _, p := range live {
			seen = append(seen, p.UUID)
		}
		return append(live, v1alpha1.PendingBackup{UUID: "new", Agent: agent})
	})
	if err != nil {
		t.Fatal(err)
	}
	if instance != "pod-1/2" || len(seen) != 1 || seen[0] != "live" {
		t.Fatalf("agent %q, live %v", instance, seen)
	}
	got, _ := s.Get(ctx, uid1)
	if p := got.Status.Backups.Pending; len(p) != 2 || p[0].UUID != "live" || p[1].UUID != "new" || p[1].Agent != "pod-1/2" {
		t.Fatalf("pending = %+v", p)
	}
	// An unchanged list is not written; an empty one clears the field.
	rv := got.ResourceVersion
	if err := s.UpdateBackups(ctx, uid1, func(live []v1alpha1.PendingBackup, _ string) []v1alpha1.PendingBackup { return live }); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Get(ctx, uid1); got.ResourceVersion != rv {
		t.Fatalf("unchanged list written: version %s, was %s", got.ResourceVersion, rv)
	}
	if err := s.UpdateBackups(ctx, uid1, func([]v1alpha1.PendingBackup, string) []v1alpha1.PendingBackup { return nil }); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Get(ctx, uid1); len(got.Status.Backups.Pending) != 0 {
		t.Fatalf("pending = %+v", got.Status.Backups.Pending)
	}
	// An unknown server is an error.
	if err := s.UpdateBackups(ctx, uid2, func(live []v1alpha1.PendingBackup, _ string) []v1alpha1.PendingBackup { return live }); !apierrors.IsNotFound(err) {
		t.Fatalf("unknown server: %v", err)
	}
}

func TestSetCondition(t *testing.T) {
	s := newStore(t, gameServer(uid1, ""))
	ctx := context.Background()
	get := func() metav1.Condition {
		gs, _ := s.Get(ctx, uid1)
		if len(gs.Status.Conditions) != 1 {
			t.Fatalf("conditions = %+v", gs.Status.Conditions)
		}
		return gs.Status.Conditions[0]
	}
	cond := metav1.Condition{Type: v1alpha1.ConditionAgentReady, Status: metav1.ConditionFalse, Reason: "Waiting"}
	if err := s.SetCondition(ctx, uid1, cond); err != nil {
		t.Fatal(err)
	}
	first := get()
	if first.Reason != "Waiting" || first.LastTransitionTime.IsZero() {
		t.Fatalf("created = %+v", first)
	}

	// Same status: the transition time is preserved, the reason updated.
	cond.Reason = "StillWaiting"
	if err := s.SetCondition(ctx, uid1, cond); err != nil {
		t.Fatal(err)
	}
	same := get()
	if same.Reason != "StillWaiting" || !same.LastTransitionTime.Equal(&first.LastTransitionTime) {
		t.Fatalf("same status = %+v (first %+v)", same, first)
	}

	cond.Status, cond.Reason = metav1.ConditionTrue, "Ready"
	if err := s.SetCondition(ctx, uid1, cond); err != nil {
		t.Fatal(err)
	}
	if got := get(); got.Status != metav1.ConditionTrue || got.Reason != "Ready" {
		t.Fatalf("flipped = %+v", got)
	}

	if err := s.SetCondition(ctx, uid2, cond); err == nil {
		t.Fatal("SetCondition of a missing server succeeded")
	}
}

func TestPodAgentReady(t *testing.T) {
	podWith := func(ip string, started *bool, name string) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{
			PodIP:             ip,
			ContainerStatuses: []corev1.ContainerStatus{{Name: name, Started: started}},
		}}
	}
	now := metav1.Now()
	deleting := podWith("10.0.0.1", ptr.To(true), render.AgentContainer)
	deleting.DeletionTimestamp = &now

	tests := []struct {
		name  string
		pod   *corev1.Pod
		ready bool
		ip    string
	}{
		{"nil", nil, false, ""},
		{"no ip", podWith("", ptr.To(true), render.AgentContainer), false, ""},
		{"deleting", deleting, false, ""},
		{"started", podWith("10.0.0.1", ptr.To(true), render.AgentContainer), true, "10.0.0.1"},
		{"not started", podWith("10.0.0.1", ptr.To(false), render.AgentContainer), false, "10.0.0.1"},
		{"started unknown", podWith("10.0.0.1", nil, render.AgentContainer), false, "10.0.0.1"},
		{"other container", podWith("10.0.0.1", ptr.To(true), "sidecar"), false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, ip := PodAgentReady(tt.pod)
			if ready != tt.ready || ip != tt.ip {
				t.Fatalf("got %v %q, want %v %q", ready, ip, tt.ready, tt.ip)
			}
		})
	}
}

func TestDescribe(t *testing.T) {
	if got := Describe(gameServer(uid1, "")); got != ns+"/"+names.ForUUID(uid1) {
		t.Fatalf("Describe = %q", got)
	}
}
