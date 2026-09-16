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
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
}

func (f *fakeAgent) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}
func (f *fakeAgent) Healthy(context.Context) bool { return true }
func (f *fakeAgent) GetServer(ctx context.Context, uuid string) (*agentclient.State, error) {
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
	return nil
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
		"allocations": map[string]any{"default": map[string]any{"ip": "192.168.1.122", "port": 30565}, "mappings": map[string][]int{"192.168.1.122": {30565}}},
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
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.GameServer{}).WithObjects(objs...).Build()
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

// createPod simulates the StatefulSet controller creating the pod from the current template.
func (h *harness) createPod(agentReady bool) *corev1.Pod {
	h.t.Helper()
	sts := &appsv1.StatefulSet{}
	if !h.get(sts, names.StatefulSet(uuid)) {
		h.t.Fatal("statefulset missing")
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: names.Pod(uuid), Namespace: ns, Labels: sts.Spec.Template.Labels, Annotations: sts.Spec.Template.Annotations, UID: types.UID("pod-" + h.now.Format("150405"))},
		Spec:       sts.Spec.Template.Spec,
	}
	if err := h.c.Create(context.Background(), pod); err != nil {
		h.t.Fatal(err)
	}
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.5"}
	started := agentReady
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "agent", Started: &started, Ready: agentReady}}
	if err := h.c.Status().Update(context.Background(), pod); err != nil {
		h.t.Fatal(err)
	}
	return pod
}

func (h *harness) pod() *corev1.Pod {
	p := &corev1.Pod{}
	if !h.get(p, names.Pod(uuid)) {
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
	if !h.get(&np, names.NetworkPolicy(uuid)) {
		t.Fatal("network policy")
	}
	var sts appsv1.StatefulSet
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
	p := h.pod()
	started := true
	p.Status.InitContainerStatuses[0].Started, p.Status.InitContainerStatuses[0].Ready = &started, true
	if err := h.c.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	h.reconcile(2)
	if calls := h.agent.Calls(); strings.Join(calls, ",") != "power:start" {
		t.Fatalf("calls %v", calls)
	}
	st := h.gs().Status
	if st.Agent.PodUID != string(p.UID) || st.Power.ObservedGeneration != 1 || st.Power.LastAction == nil || st.Power.LastAction.Action != "start" {
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
	h.updateGS(func(gs *v1alpha1.GameServer) { gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1} })
	h.reconcile(1)
	// restart while running
	h.agent.state = "running"
	h.updateGS(func(gs *v1alpha1.GameServer) { gs.Spec.Power.Generation = 2 })
	h.reconcile(1)
	// stop
	h.updateGS(func(gs *v1alpha1.GameServer) { gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerStopped, Generation: 3} })
	h.reconcile(1)
	// kill
	h.updateGS(func(gs *v1alpha1.GameServer) { gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerStopped, Generation: 4, Kill: true} })
	h.reconcile(1)
	// stop when already offline: no call
	h.agent.state = "offline"
	h.updateGS(func(gs *v1alpha1.GameServer) { gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerStopped, Generation: 5} })
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

func TestExitStateRelayedOnce(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	p := h.createPod(true)
	h.reconcile(2)
	p = h.pod()
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
	h.updateGS(func(gs *v1alpha1.GameServer) { gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1} })
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
	// The new pod starts because desired is Running.
	h.now = h.now.Add(time.Minute)
	h.createPod(true)
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
	_, err := h2.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: names.ForUUID(uuid)}})
	h2.reconcile(0)
	_, err = h2.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: names.ForUUID(uuid)}})
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
	// Pod still exists (no STS controller in the fake): the PVC waits.
	var pvc corev1.PersistentVolumeClaim
	if !h.get(&pvc, names.PVC(uuid)) {
		t.Fatal("pvc deleted too early")
	}
	if err := h.c.Delete(context.Background(), h.pod()); err != nil {
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
	h.c.Delete(context.Background(), h.pod())
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
