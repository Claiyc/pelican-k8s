package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/agentclient"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/internal/operator/render"
)

const (
	uuid = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	ns   = "pelican-servers"
)

type fakeAgent struct {
	mu    sync.Mutex
	calls []string
	state string
	err   error
	// getErr and exitErr fail GetServer and ExitState.
	getErr  error
	exitErr error
	// shimPod is the game pod whose shim is attached ("" for none);
	// shimRunning reports a shim that runs the process.
	shimPod     string
	shimRunning bool
	shimErr     error
	// busy lists the agent's in-flight work.
	busy []string
}

func (f *fakeAgent) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}
func (f *fakeAgent) Healthy(context.Context) bool { return true }
func (f *fakeAgent) GetServer(ctx context.Context, uuid string) (*agentclient.State, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	st := &agentclient.State{}
	st.State = f.state
	if st.State == "" {
		st.State = "offline"
	}
	return st, nil
}
func (f *fakeAgent) Power(ctx context.Context, uuid, action string) error {
	f.record("power:" + action)
	return f.err
}
func (f *fakeAgent) Sync(ctx context.Context, uuid string) error { f.record("sync"); return nil }
func (f *fakeAgent) Install(ctx context.Context, uuid string, reinstall bool) error {
	f.record(fmt.Sprintf("install:%v", reinstall))
	return f.err
}
func (f *fakeAgent) Delete(ctx context.Context, uuid string) error { f.record("delete"); return nil }
func (f *fakeAgent) ExitState(ctx context.Context, code int32, oom bool) error {
	f.record(fmt.Sprintf("exit:%d:%v", code, oom))
	return f.exitErr
}
func (f *fakeAgent) Shim(context.Context) (*agentclient.Shim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.shimErr != nil {
		return nil, f.shimErr
	}
	return &agentclient.Shim{Attached: f.shimPod != "", PodUID: f.shimPod, Running: f.shimRunning}, nil
}
func (f *fakeAgent) Activity(context.Context) (*agentclient.Activity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &agentclient.Activity{Busy: len(f.busy) > 0, Reasons: f.busy}, nil
}
func (f *fakeAgent) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

type fakeResolver struct{}

func (fakeResolver) Digest(ctx context.Context, image string) (string, error) {
	return "sha256:" + strings.Repeat("a", 64), nil
}
func (fakeResolver) Entrypoint(ctx context.Context, image string) ([]string, error) {
	return []string{"/bin/bash", "/entrypoint.sh"}, nil
}

func settingsJSON(memory, cpu, disk int64, image string, suspended bool) apiextensionsv1.JSON {
	m := map[string]any{
		"id": 1, "uuid": uuid, "meta": map[string]any{"name": "Test"}, "suspended": suspended,
		"build":       map[string]any{"memory_limit": memory, "cpu_limit": cpu, "disk_space": disk, "oom_killer": true},
		"container":   map[string]any{"image": image},
		"allocations": map[string]any{"default": map[string]any{"ip": "192.0.2.10", "port": 30565}, "mappings": map[string][]int{"192.0.2.10": {30565}}},
		"egg":         map[string]any{"id": "egg-1"},
	}
	b, _ := json.Marshal(m)
	return apiextensionsv1.JSON{Raw: b}
}

func newGS() *v1alpha1.GameServer {
	return &v1alpha1.GameServer{
		ObjectMeta: metav1.ObjectMeta{Name: names.ForUUID(uuid), Namespace: ns, Generation: 1},
		Spec: v1alpha1.GameServerSpec{
			Panel: v1alpha1.PanelSpec{UUID: uuid, UUIDShort: uuid[:8], Settings: settingsJSON(2048, 200, 5120, "ghcr.io/pelican-eggs/yolks:java_21", false),
				EnvironmentSecretRef: corev1.LocalObjectReference{Name: names.EnvSecret(uuid)}, PanelRevision: "rev1"},
			Power:     v1alpha1.PowerSpec{Desired: v1alpha1.PowerStopped},
			ClassName: "default",
		},
	}
}

func newClass() *v1alpha1.GameServerClass {
	return &v1alpha1.GameServerClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: v1alpha1.GameServerClassSpec{
			Storage:   v1alpha1.StorageSpec{StorageClassName: "main", DefaultSizeGiB: 20, OverheadPercent: 10},
			Exposure:  v1alpha1.ExposureSpec{Mode: v1alpha1.ExposureNodePort},
			Resources: v1alpha1.ResourcesSpec{MemoryOverheadPercent: 5, CPURequestPercentOfLimit: 25, UnlimitedMemoryMiB: 4096, MinCPU: resource.MustParse("100m")},
			Security:  v1alpha1.SecuritySpec{RunAsUser: 1000},
			Images:    v1alpha1.ImagesSpec{Shim: "shim:test", Agent: "agent:test"},
			Install:   v1alpha1.InstallJobSpec{PrepareTimeoutSeconds: 600},
		},
	}
}

type harness struct {
	t     *testing.T
	c     client.Client
	r     *GameServerReconciler
	agent *fakeAgent
	now   time.Time
}

func newHarness(t *testing.T, objs ...client.Object) *harness {
	t.Helper()
	return newHarnessWith(t, interceptor.Funcs{}, objs...)
}

func newHarnessWith(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) *harness {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.GameServer{}).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
	agent := &fakeAgent{}
	h := &harness{t: t, c: c, agent: agent, now: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)}
	h.r = &GameServerReconciler{
		Client:          c,
		Recorder:        record.NewFakeRecorder(100),
		SystemNamespace: "pelican-system",
		Resolver:        fakeResolver{},
		NewAgent:        func(base, token string) AgentAPI { return agent },
		Now:             func() time.Time { return h.now },
		DefaultClass:    "default",
	}
	return h
}

func (h *harness) reconcile(n int) {
	h.t.Helper()
	for i := 0; i < n; i++ {
		if _, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: names.ForUUID(uuid)}}); err != nil {
			h.t.Fatalf("reconcile %d: %v", i, err)
		}
	}
}

func (h *harness) gs() *v1alpha1.GameServer {
	h.t.Helper()
	gs := &v1alpha1.GameServer{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: names.ForUUID(uuid)}, gs); err != nil {
		h.t.Fatal(err)
	}
	return gs
}

func (h *harness) get(obj client.Object, name string) bool {
	h.t.Helper()
	err := h.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, obj)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return true
}

func (h *harness) updateGS(mutate func(*v1alpha1.GameServer)) {
	h.t.Helper()
	gs := h.gs()
	mutate(gs)
	if err := h.c.Update(context.Background(), gs); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) patchStatus(mutate func(*v1alpha1.GameServer)) {
	h.t.Helper()
	gs := h.gs()
	mutate(gs)
	if err := h.c.Status().Update(context.Background(), gs); err != nil {
		h.t.Fatal(err)
	}
}

// createPod simulates the StatefulSet controllers creating the agent pod and
// the game pod from the current templates, the game container running and its
// shim attached to the agent. It returns the game pod.
func (h *harness) createPod(agentReady bool) *corev1.Pod {
	h.t.Helper()
	h.createAgentPod(agentReady)
	return h.createGamePod()
}

// createAgentPod creates the agent pod from the agent StatefulSet template.
func (h *harness) createAgentPod(ready bool) *corev1.Pod {
	h.t.Helper()
	pod := h.podFrom(names.AgentStatefulSet(uuid), names.AgentPod(uuid), "agent-")
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.5"}
	started := ready
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: render.AgentContainer, Started: &started, Ready: ready}}
	if err := h.c.Status().Update(context.Background(), pod); err != nil {
		h.t.Fatal(err)
	}
	return pod
}

// createGamePod creates the game pod from the game StatefulSet template with
// the game container running and its shim attached.
func (h *harness) createGamePod() *corev1.Pod {
	h.t.Helper()
	pod := h.podFrom(names.StatefulSet(uuid), names.Pod(uuid), "pod-")
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.6"}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: render.GameContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	if err := h.c.Status().Update(context.Background(), pod); err != nil {
		h.t.Fatal(err)
	}
	h.agent.mu.Lock()
	h.agent.shimPod = string(pod.UID)
	h.agent.mu.Unlock()
	return pod
}

func (h *harness) podFrom(stsName, name, uidPrefix string) *corev1.Pod {
	h.t.Helper()
	sts := &appsv1.StatefulSet{}
	if !h.get(sts, stsName) {
		h.t.Fatalf("statefulset %s missing", stsName)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: sts.Spec.Template.Labels, Annotations: sts.Spec.Template.Annotations, UID: types.UID(uidPrefix + h.now.Format("150405.000"))},
		Spec:       sts.Spec.Template.Spec,
	}
	if err := h.c.Create(context.Background(), pod); err != nil {
		h.t.Fatal(err)
	}
	return pod
}

// pod returns the game pod.
func (h *harness) pod() *corev1.Pod {
	p := &corev1.Pod{}
	if !h.get(p, names.Pod(uuid)) {
		return nil
	}
	return p
}

// deletePods deletes both pods, as kubelet would after their grace period.
func (h *harness) deletePods() error {
	for _, p := range []*corev1.Pod{h.pod(), h.agentPod()} {
		if p == nil {
			continue
		}
		if err := h.c.Delete(context.Background(), p); err != nil {
			return err
		}
	}
	return nil
}

func (h *harness) agentPod() *corev1.Pod {
	p := &corev1.Pod{}
	if !h.get(p, names.AgentPod(uuid)) {
		return nil
	}
	return p
}

func TestCreatesOwnedResources(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(3)
	gs := h.gs()
	if len(gs.Finalizers) != 1 {
		t.Fatal("finalizer missing")
	}
	var sec corev1.Secret
	if !h.get(&sec, names.AgentSecret(uuid)) || len(sec.Data["token"]) == 0 && len(sec.StringData["token"]) == 0 {
		t.Fatal("agent secret missing")
	}
	var shim corev1.Secret
	if !h.get(&shim, names.ShimSecret(uuid)) || len(shim.Data[render.ShimTokenKey]) == 0 && len(shim.StringData[render.ShimTokenKey]) == 0 || len(shim.OwnerReferences) != 1 {
		t.Fatal("shim secret missing")
	}
	if string(shim.Data[render.ShimTokenKey])+shim.StringData[render.ShimTokenKey] == string(sec.Data["token"])+sec.StringData["token"] {
		t.Fatal("the shim token must differ from the agent token")
	}
	var pvc corev1.PersistentVolumeClaim
	if !h.get(&pvc, names.PVC(uuid)) || pvc.Spec.Resources.Requests.Storage().Value() != 5632*1024*1024 || *pvc.Spec.StorageClassName != "main" {
		t.Fatalf("pvc %+v", pvc.Spec)
	}
	if len(pvc.OwnerReferences) != 1 {
		t.Fatal("pvc must be owned with Delete policy")
	}
	var svc corev1.Service
	if !h.get(&svc, names.ExposureService(uuid)) || svc.Spec.Type != corev1.ServiceTypeNodePort || svc.Spec.Ports[0].NodePort != 30565 {
		t.Fatalf("exposure %+v", svc.Spec)
	}
	if !h.get(&svc, names.AgentService(uuid)) || svc.Spec.ClusterIP != "None" {
		t.Fatal("agent service")
	}
	var np networkingv1.NetworkPolicy
	if !h.get(&np, names.NetworkPolicy(uuid)) || !h.get(&np, names.AgentNetworkPolicy(uuid)) {
		t.Fatal("network policies")
	}
	var sts appsv1.StatefulSet
	if !h.get(&sts, names.AgentStatefulSet(uuid)) || *sts.Spec.Replicas != 1 || len(sts.OwnerReferences) != 1 {
		t.Fatal("agent statefulset")
	}
	if gs.Status.Agent.TemplateHash == "" || gs.Status.Agent.TemplateHash != sts.Spec.Template.Annotations[render.AnnotationTemplateHash] {
		t.Fatalf("agent template hash %q", gs.Status.Agent.TemplateHash)
	}
	if !h.get(&sts, names.StatefulSet(uuid)) {
		t.Fatal("statefulset")
	}
	if img := sts.Spec.Template.Spec.Containers[0].Image; !strings.HasPrefix(img, "ghcr.io/pelican-eggs/yolks:java_21@sha256:") {
		t.Fatalf("image not pinned: %s", img)
	}
	if gs.Status.Phase != v1alpha1.PhasePending || gs.Status.TemplateHash == "" || len(gs.Status.Endpoints) != 1 || gs.Status.Endpoints[0].Port != 30565 {
		t.Fatalf("status %+v", gs.Status)
	}
	if !meta.IsStatusConditionFalse(gs.Status.Conditions, v1alpha1.ConditionAgentReady) {
		t.Fatal("agent should not be ready")
	}
}

func TestFreshPodStartsWhenDesiredRunning(t *testing.T) {
	gs := newGS()
	gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	h := newHarness(t, gs, newClass())
	h.reconcile(2)
	h.createPod(false)
	h.reconcile(1)
	if calls := h.agent.Calls(); len(calls) != 0 {
		t.Fatalf("agent not ready yet, calls %v", calls)
	}
	a := h.agentPod()
	started := true
	a.Status.ContainerStatuses[0].Started, a.Status.ContainerStatuses[0].Ready = &started, true
	if err := h.c.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	h.reconcile(2)
	if calls := h.agent.Calls(); strings.Join(calls, ",") != "power:start" {
		t.Fatalf("calls %v", calls)
	}
	p := h.pod()
	st := h.gs().Status
	if st.Agent.PodUID != string(a.UID) || st.Game.PodUID != string(p.UID) || st.Power.ObservedGeneration != 1 || st.Power.LastAction == nil || st.Power.LastAction.Action != "start" || st.Power.LastAction.PodUID != string(p.UID) {
		t.Fatalf("status %+v", st)
	}
	if !meta.IsStatusConditionTrue(st.Conditions, v1alpha1.ConditionAgentReady) {
		t.Fatal("agent ready condition")
	}
	// Idempotent: no second start.
	h.reconcile(2)
	if calls := h.agent.Calls(); len(calls) != 1 {
		t.Fatalf("duplicate actions %v", calls)
	}
}

func TestPowerGenerations(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	if len(h.agent.Calls()) != 0 {
		t.Fatalf("stopped server must not be started: %v", h.agent.Calls())
	}
	// start
	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	})
	h.reconcile(1)
	// restart while running
	h.agent.state = "running"
	h.updateGS(func(gs *v1alpha1.GameServer) { gs.Spec.Power.Generation = 2 })
	h.reconcile(1)
	// stop
	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerStopped, Generation: 3}
	})
	h.reconcile(1)
	// kill
	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerStopped, Generation: 4, Kill: true}
	})
	h.reconcile(1)
	// stop when already offline: no call
	h.agent.state = "offline"
	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerStopped, Generation: 5}
	})
	h.reconcile(1)
	want := "power:start,power:restart,power:stop,power:kill"
	if got := strings.Join(h.agent.Calls(), ","); got != want {
		t.Fatalf("calls %s, want %s", got, want)
	}
	if h.gs().Status.Power.ObservedGeneration != 5 {
		t.Fatalf("observed generation %d", h.gs().Status.Power.ObservedGeneration)
	}
}

func TestSyncOnRevisionChange(t *testing.T) {
	env := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.EnvSecret(uuid), Namespace: ns}, StringData: map[string]string{"A": "1"}}
	h := newHarness(t, newGS(), newClass(), env)
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	h.updateGS(func(gs *v1alpha1.GameServer) { gs.Spec.Panel.PanelRevision = "rev2" })
	h.reconcile(1)
	if got := strings.Join(h.agent.Calls(), ","); got != "sync" {
		t.Fatalf("calls %s", got)
	}
	// Env secret change triggers another sync.
	sec := &corev1.Secret{}
	h.get(sec, names.EnvSecret(uuid))
	sec.StringData = map[string]string{"A": "2"}
	if err := h.c.Update(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	if got := strings.Join(h.agent.Calls(), ","); got != "sync,sync" {
		t.Fatalf("calls %s", got)
	}
	h.reconcile(1)
	if len(h.agent.Calls()) != 2 {
		t.Fatal("sync must not repeat")
	}
}

func TestInstallFlow(t *testing.T) {
	gs := newGS()
	gs.Spec.Install = v1alpha1.InstallSpec{Generation: 1, ScriptConfigMap: names.InstallConfigMap(uuid, 1), Image: "ghcr.io/pelican-eggs/installers:alpine", Entrypoint: "ash", StartOnInstall: true}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: names.InstallConfigMap(uuid, 1), Namespace: ns}, Data: map[string]string{"install.sh": "#!/bin/ash\necho hi"}}
	h := newHarness(t, gs, newClass(), cm)
	h.reconcile(2)
	if h.gs().Status.Phase != v1alpha1.PhaseInstalling {
		t.Fatalf("phase %s", h.gs().Status.Phase)
	}
	h.createPod(true)
	h.reconcile(2)
	if got := strings.Join(h.agent.Calls(), ","); got != "install:false" {
		t.Fatalf("calls %s", got)
	}
	st := h.gs().Status.Install
	if st.RequestedGeneration != 1 || st.Result != v1alpha1.InstallRunning || st.RequestedAt == nil {
		t.Fatalf("install status %+v", st)
	}
	var job batchv1.Job
	if h.get(&job, names.InstallJob(uuid, 1)) {
		t.Fatal("job must not exist before the agent prepared")
	}
	h.reconcile(2)
	if len(h.agent.Calls()) != 1 {
		t.Fatalf("install re-requested: %v", h.agent.Calls())
	}
	// The gateway records the agent as prepared.
	h.patchStatus(func(gs *v1alpha1.GameServer) { gs.Status.Install.PreparedGeneration = 1 })
	h.reconcile(1)
	if !h.get(&job, names.InstallJob(uuid, 1)) {
		t.Fatal("job not created after prepared")
	}
	if job.Spec.Template.Spec.Containers[0].Image != "ghcr.io/pelican-eggs/installers:alpine" || len(job.OwnerReferences) != 1 {
		t.Fatalf("job %+v", job.Spec.Template.Spec.Containers[0])
	}
	if !meta.IsStatusConditionTrue(h.gs().Status.Conditions, v1alpha1.ConditionInstallPrepared) {
		t.Fatal("InstallPrepared condition")
	}
	// Job completes and the gateway records the agent's success report.
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := h.c.Status().Update(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	h.patchStatus(func(gs *v1alpha1.GameServer) {
		gs.Status.Install.Result = v1alpha1.InstallSucceeded
		gs.Status.Install.FinishedAt = &metav1.Time{Time: h.now}
	})
	h.reconcile(2)
	st = h.gs().Status.Install
	if st.ObservedGeneration != 1 {
		t.Fatalf("install not finished: %+v", st)
	}
	if h.get(&job, names.InstallJob(uuid, 1)) && job.DeletionTimestamp.IsZero() {
		t.Fatal("job should be deleted")
	}
	if !meta.IsStatusConditionTrue(h.gs().Status.Conditions, v1alpha1.ConditionInstalled) || h.gs().Status.Phase == v1alpha1.PhaseInstalling {
		t.Fatalf("installed condition/phase %+v", h.gs().Status)
	}
}

func TestInstallJobFailureAndTimeout(t *testing.T) {
	gs := newGS()
	gs.Spec.Install = v1alpha1.InstallSpec{Generation: 1, ScriptConfigMap: names.InstallConfigMap(uuid, 1)}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: names.InstallConfigMap(uuid, 1), Namespace: ns}, Data: map[string]string{"install.sh": "x"}}
	h := newHarness(t, gs, newClass(), cm)
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	h.patchStatus(func(gs *v1alpha1.GameServer) { gs.Status.Install.PreparedGeneration = 1 })
	h.reconcile(1)
	var job batchv1.Job
	h.get(&job, names.InstallJob(uuid, 1))
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "deadline"}}
	if err := h.c.Status().Update(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	h.reconcile(2)
	st := h.gs().Status.Install
	if st.Result != v1alpha1.InstallFailed || st.ObservedGeneration != 1 {
		t.Fatalf("expected failed+observed, got %+v", st)
	}
	if !h.get(&job, names.InstallJob(uuid, 1)) {
		t.Fatal("failed jobs are kept for inspection")
	}

	// Generation 2 never gets prepared: the prepare timeout fails it.
	h.updateGS(func(gs *v1alpha1.GameServer) { gs.Spec.Install.Generation = 2 })
	h.reconcile(2)
	if h.gs().Status.Install.RequestedGeneration != 2 {
		t.Fatal("generation 2 not requested")
	}
	h.now = h.now.Add(11 * time.Minute)
	h.reconcile(2)
	st = h.gs().Status.Install
	if st.Result != v1alpha1.InstallFailed || st.ObservedGeneration != 2 {
		t.Fatalf("expected timeout failure, got %+v", st)
	}
}

func TestInstallRestartsOnFreshPod(t *testing.T) {
	gs := newGS()
	gs.Spec.Install = v1alpha1.InstallSpec{Generation: 1, ScriptConfigMap: names.InstallConfigMap(uuid, 1)}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: names.InstallConfigMap(uuid, 1), Namespace: ns}, Data: map[string]string{"install.sh": "x"}}
	h := newHarness(t, gs, newClass(), cm)
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	h.patchStatus(func(gs *v1alpha1.GameServer) { gs.Status.Install.PreparedGeneration = 1 })
	h.reconcile(1)
	var job batchv1.Job
	if !h.get(&job, names.InstallJob(uuid, 1)) {
		t.Fatal("job expected")
	}
	// The agent pod is replaced mid-install.
	if err := h.c.Delete(context.Background(), h.agentPod()); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Minute)
	h.createAgentPod(true)
	h.reconcile(2)
	if got := strings.Join(h.agent.Calls(), ","); got != "install:false,install:false" {
		t.Fatalf("install must be requested again on the new agent pod: %v", got)
	}
	st := h.gs().Status.Install
	if st.RequestedGeneration != 1 || st.PreparedGeneration != 0 || st.Result != v1alpha1.InstallRunning {
		t.Fatalf("install status after restart %+v", st)
	}
	if h.get(&job, names.InstallJob(uuid, 1)) && job.DeletionTimestamp.IsZero() {
		t.Fatal("stale job should be deleted")
	}
}

func TestExitStateRelayedOnce(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	p := h.pod()
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "game", RestartCount: 1, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled", ContainerID: "cri://abc", FinishedAt: metav1.Time{Time: h.now}}}}}
	if err := h.c.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	h.reconcile(3)
	if got := strings.Join(h.agent.Calls(), ","); got != "exit:137:true" {
		t.Fatalf("calls %s", got)
	}
	st := h.gs().Status
	if st.Process.LastExit == nil || !st.Process.LastExit.OOMKilled || st.Agent.RelayedExit == "" {
		t.Fatalf("status %+v", st)
	}
}

func TestImageChangeRecreatesWhenOffline(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	h.agent.state = "running"
	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Panel.Settings = settingsJSON(2048, 200, 5120, "ghcr.io/pelican-eggs/yolks:java_25", false)
	})
	h.reconcile(2)
	var sts appsv1.StatefulSet
	h.get(&sts, names.StatefulSet(uuid))
	if !strings.HasPrefix(sts.Spec.Template.Spec.Containers[0].Image, "ghcr.io/pelican-eggs/yolks:java_25@") {
		t.Fatalf("template not updated: %s", sts.Spec.Template.Spec.Containers[0].Image)
	}
	if !meta.IsStatusConditionTrue(h.gs().Status.Conditions, v1alpha1.ConditionRecreatePending) {
		t.Fatal("RecreatePending expected")
	}
	if h.pod() == nil {
		t.Fatal("pod must not be deleted while running")
	}
	// A start request while recreate is pending stops the process first.
	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	})
	h.reconcile(1)
	if got := strings.Join(h.agent.Calls(), ","); got != "power:stop" {
		t.Fatalf("calls %s", got)
	}
	if h.gs().Status.Power.ObservedGeneration != 0 {
		t.Fatal("generation must stay pending until the fresh pod starts")
	}
	h.agent.state = "offline"
	h.reconcile(1)
	if h.pod() != nil {
		t.Fatal("pod should be deleted once offline")
	}
	if h.agentPod() == nil {
		t.Fatal("the agent pod is not part of a game template change")
	}
	// The new pod starts because desired is Running.
	h.now = h.now.Add(time.Minute)
	h.createGamePod()
	h.reconcile(2)
	if got := strings.Join(h.agent.Calls(), ","); got != "power:stop,power:start" {
		t.Fatalf("calls %s", got)
	}
	if h.gs().Status.Power.ObservedGeneration != 1 {
		t.Fatal("generation observed after fresh-pod start")
	}
}

func TestRestartRequestAndResize(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	h.updateGS(func(gs *v1alpha1.GameServer) { gs.Spec.Power.RestartRequest = 1 })
	h.reconcile(1)
	if h.pod() != nil {
		t.Fatal("offline server with restart request: pod deleted immediately")
	}
	if h.gs().Status.Power.ObservedRestartRequest != 1 {
		t.Fatal("restart request not observed")
	}
	h.createPod(true)
	h.reconcile(2)
	// Memory change: template hash unchanged, resize attempted.
	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Panel.Settings = settingsJSON(4096, 200, 5120, "ghcr.io/pelican-eggs/yolks:java_21", false)
	})
	h.reconcile(2)
	if meta.IsStatusConditionTrue(h.gs().Status.Conditions, v1alpha1.ConditionRecreatePending) {
		t.Fatal("resource change must not require a recreate")
	}
	if c := meta.FindStatusCondition(h.gs().Status.Conditions, v1alpha1.ConditionResizePending); c == nil {
		t.Fatal("resize condition missing")
	}
}

func TestPVCExpandAndShrink(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Panel.Settings = settingsJSON(2048, 200, 10240, "ghcr.io/pelican-eggs/yolks:java_21", false)
	})
	h.reconcile(1)
	var pvc corev1.PersistentVolumeClaim
	h.get(&pvc, names.PVC(uuid))
	if pvc.Spec.Resources.Requests.Storage().Value() != 11264*1024*1024 {
		t.Fatalf("pvc not expanded: %v", pvc.Spec.Resources.Requests.Storage())
	}
	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Panel.Settings = settingsJSON(2048, 200, 1024, "ghcr.io/pelican-eggs/yolks:java_21", false)
	})
	h.reconcile(1)
	h.get(&pvc, names.PVC(uuid))
	if pvc.Spec.Resources.Requests.Storage().Value() != 11264*1024*1024 {
		t.Fatal("pvc must not shrink")
	}
	if !meta.IsStatusConditionTrue(h.gs().Status.Conditions, v1alpha1.ConditionDiskShrink) {
		t.Fatal("shrink refused condition")
	}
}

func TestSuspendedAndMissingClass(t *testing.T) {
	gs := newGS()
	gs.Spec.Panel.Settings = settingsJSON(2048, 200, 5120, "ghcr.io/pelican-eggs/yolks:java_21", true)
	gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	h := newHarness(t, gs, newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	if len(h.agent.Calls()) != 0 {
		t.Fatalf("suspended server must not start: %v", h.agent.Calls())
	}
	if h.gs().Status.Phase != v1alpha1.PhaseSuspended {
		t.Fatalf("phase %s", h.gs().Status.Phase)
	}

	h2 := newHarness(t, newGS())
	_, _ = h2.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: names.ForUUID(uuid)}}) // adds the finalizer
	_, err := h2.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: names.ForUUID(uuid)}})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected class error, got %v", err)
	}
	if h2.gs().Status.Phase != v1alpha1.PhaseError {
		t.Fatalf("phase %s", h2.gs().Status.Phase)
	}
}

func TestFinalizeDeletePolicy(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	if err := h.c.Delete(context.Background(), h.gs()); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	if got := strings.Join(h.agent.Calls(), ","); got != "delete" {
		t.Fatalf("calls %s", got)
	}
	var sts appsv1.StatefulSet
	if h.get(&sts, names.StatefulSet(uuid)) {
		t.Fatal("statefulset should be deleted")
	}
	if h.get(&sts, names.AgentStatefulSet(uuid)) {
		t.Fatal("agent statefulset should be deleted")
	}
	// Pods still exist (no STS controller in the fake): the PVC waits, also
	// for the agent pod alone.
	var pvc corev1.PersistentVolumeClaim
	if !h.get(&pvc, names.PVC(uuid)) {
		t.Fatal("pvc deleted too early")
	}
	if err := h.c.Delete(context.Background(), h.pod()); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	if !h.get(&pvc, names.PVC(uuid)) {
		t.Fatal("pvc deleted while the agent pod still mounts it")
	}
	if err := h.c.Delete(context.Background(), h.agentPod()); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	if h.get(&pvc, names.PVC(uuid)) {
		t.Fatal("pvc should be deleted")
	}
	gs := &v1alpha1.GameServer{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: names.ForUUID(uuid)}, gs); !apierrors.IsNotFound(err) {
		t.Fatalf("gameserver should be gone: %v", err)
	}
}

func TestFinalizeRetainPolicy(t *testing.T) {
	cls := newClass()
	cls.Spec.Storage.DeletionPolicy = v1alpha1.DeletionRetain
	h := newHarness(t, newGS(), cls)
	h.reconcile(2)
	var pvc corev1.PersistentVolumeClaim
	h.get(&pvc, names.PVC(uuid))
	if len(pvc.OwnerReferences) != 0 {
		t.Fatal("retained pvc must not be owned")
	}
	h.createPod(true)
	h.reconcile(2)
	if err := h.c.Delete(context.Background(), h.gs()); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	if got := strings.Join(h.agent.Calls(), ","); got != "power:kill" {
		t.Fatalf("calls %s", got)
	}
	_ = h.deletePods()
	h.reconcile(1)
	if !h.get(&pvc, names.PVC(uuid)) || pvc.Labels[v1alpha1.LabelOrphanedAt] == "" {
		t.Fatalf("pvc should be retained and labelled: %+v", pvc.Labels)
	}
}

func TestPhaseFromProcessState(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	for state, phase := range map[string]v1alpha1.Phase{"starting": v1alpha1.PhaseStarting, "running": v1alpha1.PhaseRunning, "stopping": v1alpha1.PhaseStopping, "offline": v1alpha1.PhaseStopped} {
		h.patchStatus(func(gs *v1alpha1.GameServer) { gs.Status.Process.State = state })
		h.reconcile(1)
		if got := h.gs().Status.Phase; got != phase {
			t.Fatalf("state %s: phase %s want %s", state, got, phase)
		}
	}
}

var _ = render.AgentPort

// rejectNodePort mimics the API server refusing a NodePort outside
// --service-node-port-range. The fake client does not validate the range.
func rejectNodePort(port int32) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			svc, ok := obj.(*corev1.Service)
			if !ok || svc.Spec.Type != corev1.ServiceTypeNodePort {
				return c.Create(ctx, obj, opts...)
			}
			for _, p := range svc.Spec.Ports {
				if p.NodePort == port {
					return apierrors.NewInvalid(
						schema.GroupKind{Kind: "Service"}, svc.Name,
						field.ErrorList{field.Invalid(field.NewPath("spec", "ports").Index(0).Child("nodePort"), port,
							"provided port is not in the valid range. The range of valid ports is 30000-32767")},
					)
				}
			}
			return c.Create(ctx, obj, opts...)
		},
	}
}

func outOfRangeGS() *v1alpha1.GameServer {
	gs := newGS()
	gs.Spec.Panel.Settings = settingsJSON(2048, 200, 5120, "ghcr.io/pelican-eggs/yolks:java_21", false)
	raw := strings.ReplaceAll(string(gs.Spec.Panel.Settings.Raw), "30565", "7777")
	gs.Spec.Panel.Settings = apiextensionsv1.JSON{Raw: []byte(raw)}
	return gs
}

// A NodePort the API server refuses is an operator problem, not a transient
// one: it must surface as a named condition instead of an endless retry.
func TestNodePortOutOfRangeIsTerminal(t *testing.T) {
	h := newHarnessWith(t, rejectNodePort(7777), outOfRangeGS(), newClass())
	h.reconcile(3)
	gs := h.gs()

	c := meta.FindStatusCondition(gs.Status.Conditions, v1alpha1.ConditionExposureReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "PortOutOfRange" {
		t.Fatalf("exposure condition %+v", c)
	}
	if !strings.Contains(c.Message, "7777") {
		t.Fatalf("message must name the offending port: %q", c.Message)
	}
	// The raw API error must not replace the explanation.
	if strings.Contains(c.Message, "is invalid") {
		t.Fatalf("message was overwritten by the API error: %q", c.Message)
	}
	if ac := meta.FindStatusCondition(gs.Status.Conditions, v1alpha1.ConditionAgentReady); ac != nil && ac.Reason == "Error" {
		t.Fatalf("exposure problem must not be reported as an agent error: %+v", ac)
	}
	if gs.Status.Phase != v1alpha1.PhaseError {
		t.Fatalf("phase = %q, want Error", gs.Status.Phase)
	}
	if len(gs.Status.Endpoints) != 0 {
		t.Fatalf("unreachable server must publish no endpoints: %+v", gs.Status.Endpoints)
	}
	// No Service means no reachable server, so nothing should be scheduled.
	var sts appsv1.StatefulSet
	if h.get(&sts, names.StatefulSet(uuid)) {
		t.Fatal("statefulset must not be created while the server is unreachable")
	}
}

// Reconcile must stay quiet: returning the error would retry on a backoff that
// cannot fix a Panel allocation.
func TestNodePortOutOfRangeDoesNotError(t *testing.T) {
	h := newHarnessWith(t, rejectNodePort(7777), outOfRangeGS(), newClass())
	for i := 0; i < 3; i++ {
		res, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: names.ForUUID(uuid)}})
		if err != nil {
			t.Fatalf("reconcile %d returned %v, want nil", i, err)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("reconcile %d must keep requeueing so a widened range recovers", i)
		}
	}
}

// Once the allocation moves into range the condition must clear.
func TestNodePortOutOfRangeRecovers(t *testing.T) {
	h := newHarnessWith(t, rejectNodePort(7777), outOfRangeGS(), newClass())
	h.reconcile(2)
	if c := meta.FindStatusCondition(h.gs().Status.Conditions, v1alpha1.ConditionExposureReady); c == nil || c.Reason != "PortOutOfRange" {
		t.Fatalf("precondition: %+v", c)
	}

	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Panel.Settings = settingsJSON(2048, 200, 5120, "ghcr.io/pelican-eggs/yolks:java_21", false)
		gs.Generation++
	})
	h.reconcile(3)

	gs := h.gs()
	c := meta.FindStatusCondition(gs.Status.Conditions, v1alpha1.ConditionExposureReady)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != "Ready" {
		t.Fatalf("exposure did not recover: %+v", c)
	}
	var svc corev1.Service
	if !h.get(&svc, names.ExposureService(uuid)) || svc.Spec.Ports[0].NodePort != 30565 {
		t.Fatalf("exposure service %+v", svc.Spec)
	}
	if len(gs.Status.Endpoints) != 1 || gs.Status.Endpoints[0].Port != 30565 {
		t.Fatalf("endpoints %+v", gs.Status.Endpoints)
	}
}

// The API server stores an untyped Service as ClusterIP. applyService must
// read that default as no type change, not delete and recreate the Service on
// every reconcile.
func TestApplyServiceUntypedMatchesClusterIP(t *testing.T) {
	stored := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "untyped"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: corev1.ClusterIPNone},
	}
	h := newHarness(t, newGS(), newClass(), stored)
	s := &scope{ctx: context.Background(), gs: h.gs(), requeue: requeueSlow}
	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "untyped"},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Ports:     []corev1.ServicePort{{Name: "agent", Port: 8080, Protocol: corev1.ProtocolTCP}},
		},
	}
	if err := h.r.applyService(s, desired); err != nil {
		t.Fatal(err)
	}
	var svc corev1.Service
	if !h.get(&svc, "untyped") {
		t.Fatal("untyped service was deleted")
	}
	if s.requeue != requeueSlow || len(svc.Spec.Ports) != 1 {
		t.Fatalf("service not patched in place: requeue %v, spec %+v", s.requeue, svc.Spec)
	}
}

func TestLimitsRemoved(t *testing.T) {
	q := resource.MustParse
	both := corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: q("4"), corev1.ResourceMemory: q("2Gi")}}
	memOnly := corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: q("2Gi")}}
	none := corev1.ResourceRequirements{}

	if got := limitsRemoved(memOnly, both); strings.Join(got, ",") != "cpu" {
		t.Fatalf("dropping the cpu limit: %v", got)
	}
	if got := limitsRemoved(none, both); strings.Join(got, ",") != "cpu,memory" {
		t.Fatalf("dropping both: %v", got)
	}
	// Keeping a limit at a different value is a resize, not a removal.
	changed := corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: q("8"), corev1.ResourceMemory: q("2Gi")}}
	if got := limitsRemoved(changed, both); got != nil {
		t.Fatalf("changed limits are resizable: %v", got)
	}
	// Adding one is not a removal either.
	if got := limitsRemoved(both, memOnly); got != nil {
		t.Fatalf("added limits: %v", got)
	}
	if got := limitsRemoved(both, both); got != nil {
		t.Fatalf("unchanged: %v", got)
	}
}

// Setting a Panel server to unlimited CPU drops the container limit, which the
// resize subresource rejects unconditionally. The pod must be recreated instead
// of retrying a request that can never succeed.
func TestUnlimitedCPURecreatesThePod(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	if h.pod() == nil {
		t.Fatal("precondition: pod exists")
	}

	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Panel.Settings = settingsJSON(2048, 0, 5120, "ghcr.io/pelican-eggs/yolks:java_21", false)
	})
	h.reconcile(1)

	gs := h.gs()
	if c := meta.FindStatusCondition(gs.Status.Conditions, v1alpha1.ConditionResizePending); c == nil || c.Reason != "RecreateRequired" {
		t.Fatalf("resize condition %+v", c)
	}
	if c := meta.FindStatusCondition(gs.Status.Conditions, v1alpha1.ConditionRecreatePending); c == nil || c.Status != metav1.ConditionTrue || c.Reason != "ResizeNeedsRecreate" {
		t.Fatalf("recreate condition %+v", c)
	}
	if h.pod() != nil {
		t.Fatal("offline server: the pod must be recreated so the change can land")
	}

	// The fresh pod carries the new resources and settles.
	h.createGamePod()
	h.reconcile(2)
	pod := h.pod()
	if pod == nil {
		t.Fatal("pod not recreated")
	}
	i := render.GameContainerIndex(&pod.Spec)
	if _, ok := pod.Spec.Containers[i].Resources.Limits[corev1.ResourceCPU]; ok {
		t.Fatalf("new pod still has a cpu limit: %+v", pod.Spec.Containers[i].Resources)
	}
	gs = h.gs()
	if c := meta.FindStatusCondition(gs.Status.Conditions, v1alpha1.ConditionResizePending); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("resize should have settled: %+v", c)
	}
	if meta.IsStatusConditionTrue(gs.Status.Conditions, v1alpha1.ConditionRecreatePending) {
		t.Fatal("recreate should have settled")
	}
}

// A running server is not killed to apply a resource change; the recreate waits
// for the process to go offline like any other template change.
func TestUnlimitedCPUWaitsForOffline(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	h.agent.state = v1alpha1.ProcessRunning

	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Panel.Settings = settingsJSON(2048, 0, 5120, "ghcr.io/pelican-eggs/yolks:java_21", false)
	})
	h.reconcile(2)

	if h.pod() == nil {
		t.Fatal("a running server must not be killed for a resource change")
	}
	if !meta.IsStatusConditionTrue(h.gs().Status.Conditions, v1alpha1.ConditionRecreatePending) {
		t.Fatal("recreate should stay pending while the process runs")
	}
}

// lbClass puts the default class in LoadBalancer mode.
func lbClass() *v1alpha1.GameServerClass {
	c := newClass()
	c.Spec.Exposure = v1alpha1.ExposureSpec{Mode: v1alpha1.ExposureLoadBalancer}
	return c
}

// A LoadBalancer that never gets an address is usually a cluster with no load
// balancer implementation, which "waiting" does not help anyone diagnose.
func TestLoadBalancerPendingMessageBecomesActionable(t *testing.T) {
	h := newHarness(t, newGS(), lbClass())
	h.reconcile(2)

	c := meta.FindStatusCondition(h.gs().Status.Conditions, v1alpha1.ConditionExposureReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "Pending" {
		t.Fatalf("exposure condition %+v", c)
	}
	// Early on, "waiting" is the honest answer: a cloud LB takes time.
	if c.Message != "waiting for the LoadBalancer address" {
		t.Fatalf("first message should be the plain one: %q", c.Message)
	}

	h.now = h.now.Add(lbPendingGrace + time.Minute)
	h.reconcile(1)

	c = meta.FindStatusCondition(h.gs().Status.Conditions, v1alpha1.ConditionExposureReady)
	if c.Reason != "Pending" || c.Status != metav1.ConditionFalse {
		t.Fatalf("condition changed unexpectedly: %+v", c)
	}
	for _, want := range []string{"no load balancer implementation", "MetalLB", "NodePort", "30000-32767", "HostPort"} {
		if !strings.Contains(c.Message, want) {
			t.Fatalf("message must mention %q, got %q", want, c.Message)
		}
	}
}

// An address that does arrive settles the condition and publishes endpoints.
func TestLoadBalancerReadyWhenAddressArrives(t *testing.T) {
	h := newHarness(t, newGS(), lbClass())
	h.reconcile(2)

	var svc corev1.Service
	if !h.get(&svc, names.ExposureService(uuid)) {
		t.Fatal("exposure service missing")
	}
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "203.0.113.7"}}
	if err := h.c.Status().Update(context.Background(), &svc); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)

	gs := h.gs()
	if c := meta.FindStatusCondition(gs.Status.Conditions, v1alpha1.ConditionExposureReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("exposure not ready: %+v", c)
	}
	if len(gs.Status.Endpoints) != 1 || gs.Status.Endpoints[0].IP != "203.0.113.7" {
		t.Fatalf("endpoints should come from the LoadBalancer ingress: %+v", gs.Status.Endpoints)
	}
}

// apiServerServices mimics the API server storing an untyped Service as
// ClusterIP. The fake client does not default the type. Service deletes are
// counted so a test can tell a patch from a recreate.
func apiServerServices(deletes *int) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if svc, ok := obj.(*corev1.Service); ok && svc.Spec.Type == "" {
				svc.Spec.Type = corev1.ServiceTypeClusterIP
			}
			return c.Create(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*corev1.Service); ok {
				*deletes++
			}
			return c.Delete(ctx, obj, opts...)
		},
	}
}

// The API server stores the agent Service as ClusterIP. Reconciling an
// unchanged server must patch its Services in place, not read that default as
// a type change and recreate them.
func TestServicesSurviveAPIServerDefaulting(t *testing.T) {
	deletes := 0
	h := newHarnessWith(t, apiServerServices(&deletes), newGS(), lbClass())
	h.reconcile(5)

	if deletes != 0 {
		t.Fatalf("%d Service deletes across reconciles of an unchanged server, want 0", deletes)
	}
	var svc corev1.Service
	if !h.get(&svc, names.AgentService(uuid)) || svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("agent service %+v", svc.Spec)
	}
}

// A new agent pod next to a game pod whose shim already runs the process
// attaches to it: the fresh-pod rule records the pod and starts nothing.
func TestFreshAgentPodAttachesToRunningProcess(t *testing.T) {
	gs := newGS()
	gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	h := newHarness(t, gs, newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	if got := strings.Join(h.agent.Calls(), ","); got != "power:start" {
		t.Fatalf("calls %s", got)
	}
	// The agent pod is replaced while the game runs.
	if err := h.c.Delete(context.Background(), h.agentPod()); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	if c := meta.FindStatusCondition(h.gs().Status.Conditions, v1alpha1.ConditionAgentReady); c == nil || c.Reason != "NoPod" {
		t.Fatalf("AgentReady %+v", c)
	}
	if h.pod() == nil {
		t.Fatal("the game pod must survive the agent pod")
	}
	h.now = h.now.Add(time.Minute)
	h.agent.shimRunning = true
	a := h.createAgentPod(true)
	h.reconcile(2)
	if got := strings.Join(h.agent.Calls(), ","); got != "power:start" {
		t.Fatalf("a running process must not be started again: %s", got)
	}
	if st := h.gs().Status; st.Agent.PodUID != string(a.UID) {
		t.Fatalf("agent pod not recorded: %+v", st.Agent)
	}
}

// A node reboot keeps both pods and restarts their containers: the fresh
// agent container starts the process again, an agent restart next to a running
// process does not.
func TestRebootedNodeStartsTheServer(t *testing.T) {
	gs := newGS()
	gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	h := newHarness(t, gs, newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	restart := func() {
		t.Helper()
		a := h.agentPod()
		a.Status.ContainerStatuses[0].RestartCount++
		if err := h.c.Status().Update(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	h.agent.shimRunning = true
	restart()
	h.reconcile(2)
	if got := strings.Join(h.agent.Calls(), ","); got != "power:start" {
		t.Fatalf("an agent restart must not start a running process again: %s", got)
	}
	h.agent.shimRunning = false
	restart()
	h.reconcile(2)
	if got := strings.Join(h.agent.Calls(), ","); got != "power:start,power:start" {
		t.Fatalf("calls after the reboot %s", got)
	}
	if st := h.gs().Status.Agent; st.Restarts != 2 {
		t.Fatalf("agent restarts not recorded: %+v", st)
	}
	h.reconcile(2)
	if got := len(h.agent.Calls()); got != 2 {
		t.Fatalf("duplicate start: %v", h.agent.Calls())
	}
}

// A stop the Panel requests while the agent pod is replaced is applied by the
// new agent pod once it attaches to the running process.
func TestFreshAgentPodAppliesPendingStop(t *testing.T) {
	gs := newGS()
	gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	h := newHarness(t, gs, newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	if err := h.c.Delete(context.Background(), h.agentPod()); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerStopped, Generation: 2}
	})
	h.reconcile(1)
	h.now = h.now.Add(time.Minute)
	h.agent.shimRunning = true
	h.agent.state = "running"
	h.createAgentPod(true)
	h.reconcile(2)
	if got := strings.Join(h.agent.Calls(), ","); got != "power:start,power:stop" {
		t.Fatalf("calls %s", got)
	}
	if st := h.gs().Status; st.Power.ObservedGeneration != 2 {
		t.Fatalf("observed generation %d", st.Power.ObservedGeneration)
	}
}

// A fresh game pod is started only once its own shim is attached.
func TestFreshGamePodWaitsForItsShim(t *testing.T) {
	gs := newGS()
	gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	h := newHarness(t, gs, newClass())
	h.reconcile(2)
	h.createPod(true)
	h.agent.shimPod = "a-previous-pod"
	h.reconcile(2)
	if calls := h.agent.Calls(); len(calls) != 0 {
		t.Fatalf("no start before the new pod's shim is attached: %v", calls)
	}
	st := h.gs().Status
	if st.Game.PodUID != "" || st.Power.ObservedGeneration != 0 {
		t.Fatalf("the game pod must not be recorded yet: %+v", st)
	}
	if c := meta.FindStatusCondition(st.Conditions, v1alpha1.ConditionGamePodReady); c == nil || c.Reason != "ShimNotAttached" {
		t.Fatalf("GamePodReady %+v", c)
	}
	h.agent.shimPod = string(h.pod().UID)
	h.reconcile(1)
	if got := strings.Join(h.agent.Calls(), ","); got != "power:start" {
		t.Fatalf("calls %s", got)
	}
}

// An agent template change replaces only the agent pod, once the process is offline.
func TestAgentTemplateChangeReplacesTheAgentPod(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	h.agent.state = v1alpha1.ProcessRunning
	cls := &v1alpha1.GameServerClass{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: "default"}, cls); err != nil {
		t.Fatal(err)
	}
	cls.Spec.Images.Agent = "agent:v2"
	if err := h.c.Update(context.Background(), cls); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	if c := meta.FindStatusCondition(h.gs().Status.Conditions, v1alpha1.ConditionRecreatePending); c == nil || c.Reason != "AgentTemplateChanged" {
		t.Fatalf("RecreatePending %+v", c)
	}
	if h.agentPod() == nil {
		t.Fatal("the agent pod must wait for the process to go offline")
	}
	h.agent.state = v1alpha1.ProcessOffline
	// Work counts for a scheduled agent pod.
	h.bind(h.agentPod(), "node-a")
	h.bind(h.pod(), "node-a")
	h.agent.busy = []string{"files"}
	h.reconcile(1)
	if h.agentPod() == nil {
		t.Fatal("the agent pod must wait for its in-flight work to end")
	}
	h.agent.busy = nil
	h.reconcile(1)
	if h.agentPod() != nil {
		t.Fatal("the agent pod must be replaced once offline and idle")
	}
	if h.pod() == nil {
		t.Fatal("the game pod is not part of an agent template change")
	}
}

// A restart request waits for the agent's in-flight work before it replaces
// the agent pod; the game pod goes once the process is offline.
func TestRestartRequestWaitsForAgentWork(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	h.bind(h.agentPod(), "node-a")
	h.bind(h.pod(), "node-a")
	h.agent.state = v1alpha1.ProcessOffline
	h.agent.busy = []string{"restore"}
	gs := h.gs()
	gs.Spec.Power.RestartRequest = 1
	if err := h.c.Update(context.Background(), gs); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	if h.agentPod() == nil {
		t.Fatal("a busy agent pod must not be replaced")
	}
	if h.gs().Status.Power.ObservedRestartRequest != 0 {
		t.Fatal("the restart request is not done while the agent pod stays")
	}
	h.agent.busy = nil
	h.reconcile(1)
	if h.agentPod() != nil {
		t.Fatal("the agent pod must be replaced once idle")
	}
}
