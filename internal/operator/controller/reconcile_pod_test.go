package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/internal/operator/render"
	"github.com/Claiyc/pelican-k8s/internal/operator/settings"
)

var errBoom = errors.New("boom")

// podHarness is a harness with existing pods and a scope pointing at them,
// for exercising the phases of reconcilePods one at a time.
type podHarness struct {
	*harness
	pod      *corev1.Pod
	agentPod *corev1.Pod
	s        *scope
}

// newPodHarness reconciles a GameServer to the point where its pods exist
// (agent ready or not) and builds the scope reconcilePods would see.
func newPodHarness(t *testing.T, agentReady bool, cls *v1alpha1.GameServerClass, funcs interceptor.Funcs) *podHarness {
	t.Helper()
	h := newHarnessWith(t, funcs, newGS(), cls)
	h.reconcile(2)
	h.createPod(agentReady)
	h.reconcile(2)
	ph := &podHarness{harness: h, pod: h.pod(), agentPod: h.agentPod()}
	ph.s = ph.scope(cls)
	return ph
}

// scope builds the scope from the current stored GameServer and the harness pod.
func (h *podHarness) scope(cls *v1alpha1.GameServerClass) *scope {
	h.t.Helper()
	gs := h.gs()
	st, err := settings.Parse(gs.Spec.Panel.Settings)
	if err != nil {
		h.t.Fatal(err)
	}
	return &scope{
		ctx:      context.Background(),
		gs:       gs,
		class:    cls,
		settings: st,
		in:       &render.Input{GS: gs, Class: cls, Settings: st},
		pod:      h.pod,
		agentPod: h.agentPod,
		now:      metav1.NewTime(h.now),
		requeue:  requeueSlow,
	}
}

// events returns the events recorded so far, as "Reason" strings.
func (h *podHarness) events() []string {
	h.t.Helper()
	rec := h.r.Recorder.(*record.FakeRecorder)
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func hasEvent(events []string, reason string) bool {
	for _, e := range events {
		if strings.Contains(e, " "+reason+" ") {
			return true
		}
	}
	return false
}

func condition(s *scope, t string) *metav1.Condition {
	return meta.FindStatusCondition(s.gs.Status.Conditions, t)
}

func withTermination(pod *corev1.Pod, at time.Time) {
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: render.GameContainer, RestartCount: 1,
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 137, Reason: "OOMKilled", ContainerID: "cri://abc", FinishedAt: metav1.Time{Time: at},
		}},
	}}
}

func failoverClass(after time.Duration) *v1alpha1.GameServerClass {
	c := newClass()
	c.Spec.Failover.ForceDeleteAfter = &metav1.Duration{Duration: after}
	return c
}

func (h *podHarness) createNode(ready bool) {
	h.t.Helper()
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}},
	}
	if err := h.c.Create(context.Background(), node); err != nil {
		h.t.Fatal(err)
	}
}

func TestReconcilePodsTerminating(t *testing.T) {
	tests := []struct {
		name        string
		class       *v1alpha1.GameServerClass
		node        *bool // nil: no node object
		terminating time.Duration
		wantNodeLos bool
		wantDeleted bool
	}{
		{name: "fencing disabled", class: newClass(), terminating: time.Hour},
		{name: "node missing", class: failoverClass(time.Minute), terminating: time.Hour},
		{name: "node ready", class: failoverClass(time.Minute), node: ptr(true), terminating: time.Hour},
		{name: "node lost within the window", class: failoverClass(time.Hour), node: ptr(false), terminating: time.Minute, wantNodeLos: true},
		{name: "node lost past the window", class: failoverClass(time.Minute), node: ptr(false), terminating: time.Hour, wantNodeLos: true, wantDeleted: true},
	}
	for _, which := range []string{"game", "agent"} {
		for _, tt := range tests {
			t.Run(which+"/"+tt.name, func(t *testing.T) {
				h := newPodHarness(t, true, tt.class, interceptor.Funcs{})
				if tt.node != nil {
					h.createNode(*tt.node)
				}
				pod, cond, get := h.pod, v1alpha1.ConditionGamePodReady, h.harness.pod
				if which == "agent" {
					pod, cond, get = h.agentPod, v1alpha1.ConditionAgentReady, h.harness.agentPod
				}
				deleting := metav1.NewTime(h.now.Add(-tt.terminating))
				pod.DeletionTimestamp = &deleting
				pod.Spec.NodeName = "node-1"

				if err := h.r.reconcilePods(h.s); err != nil {
					t.Fatal(err)
				}

				nodeLost := condition(h.s, v1alpha1.ConditionNodeLost)
				if tt.wantNodeLos != (nodeLost != nil && nodeLost.Status == metav1.ConditionTrue) {
					t.Fatalf("NodeLost condition %+v, want lost=%v", nodeLost, tt.wantNodeLos)
				}
				if c := condition(h.s, cond); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "Terminating" {
					t.Fatalf("%s condition %+v", cond, c)
				}
				if h.s.requeue != requeueFast {
					t.Fatalf("requeue %v", h.s.requeue)
				}
				if deleted := get() == nil; deleted != tt.wantDeleted {
					t.Fatalf("pod deleted=%v, want %v", deleted, tt.wantDeleted)
				}
				if got := hasEvent(h.events(), "ForceDelete"); got != tt.wantDeleted {
					t.Fatalf("ForceDelete event=%v, want %v", got, tt.wantDeleted)
				}
			})
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestReconcilePodsTerminatingForceDeleteErrors(t *testing.T) {
	var grace *int64
	funcs := interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		o := &client.DeleteOptions{}
		o.ApplyOptions(opts)
		grace = o.GracePeriodSeconds
		return errBoom
	}}
	h := newPodHarness(t, true, failoverClass(time.Minute), funcs)
	h.createNode(false)
	deleting := metav1.NewTime(h.now.Add(-time.Hour))
	h.pod.DeletionTimestamp = &deleting
	h.pod.Spec.NodeName = "node-1"

	if err := h.r.reconcilePods(h.s); !errors.Is(err, errBoom) {
		t.Fatalf("err %v, want the delete error", err)
	}
	if grace == nil || *grace != 0 {
		t.Fatalf("force delete must use a zero grace period, got %v", grace)
	}
}

func TestReconcilePodsTerminatingForceDeleteIgnoresNotFound(t *testing.T) {
	h := newPodHarness(t, true, failoverClass(time.Minute), interceptor.Funcs{})
	h.createNode(false)
	deleting := metav1.NewTime(h.now.Add(-time.Hour))
	h.pod.DeletionTimestamp = &deleting
	h.pod.Spec.NodeName = "node-1"
	if err := h.c.Delete(context.Background(), h.pod.DeepCopy()); err != nil {
		t.Fatal(err)
	}

	if err := h.r.reconcilePods(h.s); err != nil {
		t.Fatalf("a pod that is already gone is not an error: %v", err)
	}
}

func TestReconcileResizeWithoutGameContainer(t *testing.T) {
	h := newPodHarness(t, true, newClass(), interceptor.Funcs{})
	h.pod.Spec.Containers = nil
	h.s.gs.Status.Conditions = nil
	if h.r.reconcileResize(h.s, h.pod) {
		t.Fatal("no game container: nothing to recreate")
	}
	if c := condition(h.s, v1alpha1.ConditionResizePending); c != nil {
		t.Fatalf("no resize condition expected, got %+v", c)
	}
}

func TestReconcileResizeDeferredAndSettled(t *testing.T) {
	h := newPodHarness(t, true, newClass(), interceptor.Funcs{})

	if h.r.reconcileResize(h.s, h.pod) {
		t.Fatal("matching resources need no recreate")
	}
	if c := condition(h.s, v1alpha1.ConditionResizePending); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "Applied" {
		t.Fatalf("settled condition %+v", c)
	}

	h.pod.Status.Conditions = append(h.pod.Status.Conditions, corev1.PodCondition{Type: "PodResizePending", Status: corev1.ConditionTrue, Reason: "Deferred"})
	if h.r.reconcileResize(h.s, h.pod) {
		t.Fatal("a deferred resize does not need a recreate")
	}
	if c := condition(h.s, v1alpha1.ConditionResizePending); c == nil || c.Status != metav1.ConditionTrue || c.Reason != "Deferred" {
		t.Fatalf("deferred condition %+v", c)
	}
}

func TestReconcileResizeApplies(t *testing.T) {
	h := newPodHarness(t, true, newClass(), interceptor.Funcs{})
	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Panel.Settings = settingsJSON(4096, 200, 5120, "ghcr.io/pelican-eggs/yolks:java_21", false)
	})
	h.s = h.scope(newClass())
	h.events()

	if h.r.reconcileResize(h.s, h.pod) {
		t.Fatal("an in-place resize does not need a recreate")
	}
	if c := condition(h.s, v1alpha1.ConditionResizePending); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "Applied" {
		t.Fatalf("condition %+v", c)
	}
	if !hasEvent(h.events(), "Resized") {
		t.Fatal("Resized event expected")
	}
}

func TestReconcileResizeFailureIsReported(t *testing.T) {
	funcs := interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if sub == "resize" {
			return errBoom
		}
		return c.SubResource(sub).Update(ctx, obj, opts...)
	}}
	h := newPodHarness(t, true, newClass(), funcs)
	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Panel.Settings = settingsJSON(4096, 200, 5120, "ghcr.io/pelican-eggs/yolks:java_21", false)
	})
	h.s = h.scope(newClass())
	h.events()

	if h.r.reconcileResize(h.s, h.pod) {
		t.Fatal("a failed resize is retried, not recreated")
	}
	if c := condition(h.s, v1alpha1.ConditionResizePending); c == nil || c.Status != metav1.ConditionTrue || c.Reason != "ResizeFailed" || !strings.Contains(c.Message, "boom") {
		t.Fatalf("condition %+v", c)
	}
	if hasEvent(h.events(), "Resized") {
		t.Fatal("no Resized event after a failure")
	}
}

func TestRelayTerminationErrorLeavesStatusUntouched(t *testing.T) {
	h := newPodHarness(t, true, newClass(), interceptor.Funcs{})
	h.agent.exitErr = errBoom
	h.s.agent = h.agent
	withTermination(h.pod, h.now)

	err := h.r.relayTermination(h.s, h.pod)
	if !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "relay exit state") {
		t.Fatalf("err %v", err)
	}
	if h.s.gs.Status.Agent.RelayedExit != "" || h.s.gs.Status.Process.LastExit != nil {
		t.Fatalf("status changed despite the failed relay: %+v", h.s.gs.Status)
	}
}

func TestRelayTerminationSkipsRelayedAndMissing(t *testing.T) {
	h := newPodHarness(t, true, newClass(), interceptor.Funcs{})
	h.s.agent = h.agent

	if err := h.r.relayTermination(h.s, h.pod); err != nil {
		t.Fatal(err)
	}
	withTermination(h.pod, h.now)
	key, _, _ := lastTermination(h.pod)
	h.s.gs.Status.Agent.RelayedExit = key
	if err := h.r.relayTermination(h.s, h.pod); err != nil {
		t.Fatal(err)
	}
	if calls := h.agent.Calls(); len(calls) != 0 {
		t.Fatalf("nothing to relay, agent got %v", calls)
	}
}

func TestRecreate(t *testing.T) {
	tests := []struct {
		name          string
		state         string
		getErr        error
		deleteErr     error
		restart       bool
		agent, game   bool
		wantErr       error
		wantGameGone  bool
		wantAgentGone bool
		wantObserved  int64
	}{
		{name: "agent unreachable", game: true, getErr: errBoom, wantErr: errBoom},
		{name: "process still running", game: true, state: v1alpha1.ProcessRunning},
		{name: "game template, offline", game: true, state: v1alpha1.ProcessOffline, wantGameGone: true},
		{name: "agent template, offline", agent: true, state: v1alpha1.ProcessOffline, wantAgentGone: true},
		{name: "agent template, running", agent: true, state: v1alpha1.ProcessRunning},
		{name: "restart request, offline", restart: true, state: v1alpha1.ProcessOffline, wantGameGone: true, wantAgentGone: true, wantObserved: 3},
		{name: "restart request, running", restart: true, state: v1alpha1.ProcessRunning},
		{name: "delete fails", game: true, state: v1alpha1.ProcessOffline, deleteErr: errBoom, wantErr: errBoom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			funcs := interceptor.Funcs{}
			if tt.deleteErr != nil {
				funcs.Delete = func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					return tt.deleteErr
				}
			}
			h := newPodHarness(t, true, newClass(), funcs)
			h.agent.state, h.agent.getErr = tt.state, tt.getErr
			h.s.agent = h.agent
			h.s.gs.Spec.Power.RestartRequest = 3
			h.events()

			err := h.r.recreate(h.s, tt.restart, tt.agent, tt.game)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err %v, want %v", err, tt.wantErr)
			}
			if gone := h.s.pod == nil; gone != tt.wantGameGone {
				t.Fatalf("scope game pod cleared=%v, want %v", gone, tt.wantGameGone)
			}
			if gone := h.harness.pod() == nil; gone != tt.wantGameGone {
				t.Fatalf("game pod deleted=%v, want %v", gone, tt.wantGameGone)
			}
			if gone := h.s.agentPod == nil && h.s.agent == nil; gone != tt.wantAgentGone {
				t.Fatalf("scope agent cleared=%v, want %v", gone, tt.wantAgentGone)
			}
			if gone := h.harness.agentPod() == nil; gone != tt.wantAgentGone {
				t.Fatalf("agent pod deleted=%v, want %v", gone, tt.wantAgentGone)
			}
			if tt.wantGameGone || tt.wantAgentGone {
				if h.s.requeue != requeueFast || !hasEvent(h.events(), "Recreate") {
					t.Fatal("a recreate requeues fast and records an event")
				}
			}
			if got := h.s.gs.Status.Power.ObservedRestartRequest; got != tt.wantObserved {
				t.Fatalf("ObservedRestartRequest %d, want %d", got, tt.wantObserved)
			}
		})
	}
}

func TestRecreateAlreadyGone(t *testing.T) {
	h := newPodHarness(t, true, newClass(), interceptor.Funcs{})
	h.s.agent = h.agent
	if err := h.c.Delete(context.Background(), h.pod.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	if err := h.r.recreate(h.s, false, false, true); err != nil {
		t.Fatalf("a missing pod is not an error: %v", err)
	}
	if h.s.pod != nil {
		t.Fatal("pod must be forgotten")
	}
}

// A game pod whose game container never started, and an agent pod that never
// became ready, are replaced right away: neither holds the process.
func TestReconcilePodsRecreatesUnstartedOutdatedPods(t *testing.T) {
	h := newPodHarness(t, false, newClass(), interceptor.Funcs{})
	h.pod.Status.ContainerStatuses = nil
	h.s.gs.Status.TemplateHash = "something-else"
	h.s.gs.Status.Agent.TemplateHash = "something-else"
	h.events()

	if err := h.r.reconcilePods(h.s); err != nil {
		t.Fatal(err)
	}
	if h.s.pod != nil || h.harness.pod() != nil {
		t.Fatal("the unstarted outdated game pod must be deleted")
	}
	if h.s.agentPod != nil || h.harness.agentPod() != nil {
		t.Fatal("the unready outdated agent pod must be deleted")
	}
	if c := condition(h.s, v1alpha1.ConditionAgentReady); c == nil || c.Reason != "Starting" {
		t.Fatalf("AgentReady condition %+v", c)
	}
	if !hasEvent(h.events(), "Recreate") {
		t.Fatal("Recreate event expected")
	}
}

// A restart request waits for an agent that can tell the process is offline.
func TestReconcilePodsRestartWaitsForTheAgent(t *testing.T) {
	h := newPodHarness(t, false, newClass(), interceptor.Funcs{})
	h.s.gs.Spec.Power.RestartRequest = 5

	if err := h.r.reconcilePods(h.s); err != nil {
		t.Fatal(err)
	}
	if h.harness.pod() == nil || h.harness.agentPod() == nil {
		t.Fatal("both pods must be kept until the process is known to be offline")
	}
	if h.s.gs.Status.Power.ObservedRestartRequest != 0 {
		t.Fatal("restart request must not be acknowledged yet")
	}
}

func TestReconcilePodsKeepsStartedPodWhileAgentNotReady(t *testing.T) {
	h := newPodHarness(t, false, newClass(), interceptor.Funcs{})
	h.s.gs.Status.TemplateHash = "something-else"
	withTermination(h.pod, h.now)

	if err := h.r.reconcilePods(h.s); err != nil {
		t.Fatal(err)
	}
	if h.s.pod == nil || h.harness.pod() == nil {
		t.Fatal("a pod whose game container ran must wait for the process to go offline")
	}
	if h.s.requeue != requeueFast {
		t.Fatalf("requeue %v", h.s.requeue)
	}
}

func TestReconcilePodsAgentTokenError(t *testing.T) {
	h := newPodHarness(t, true, newClass(), interceptor.Funcs{})
	sec := &corev1.Secret{}
	if !h.get(sec, names.AgentSecret(uuid)) {
		t.Fatal("precondition: agent secret exists")
	}
	if err := h.c.Delete(context.Background(), sec); err != nil {
		t.Fatal(err)
	}

	if err := h.r.reconcilePods(h.s); err == nil {
		t.Fatal("a missing agent secret must fail the reconcile")
	}
	if h.s.agent != nil {
		t.Fatal("no agent without a token")
	}
}

func TestReconcilePodsRelayError(t *testing.T) {
	h := newPodHarness(t, true, newClass(), interceptor.Funcs{})
	h.agent.exitErr = errBoom
	withTermination(h.pod, h.now)
	h.pod.Status.ContainerStatuses[0].State.Running = &corev1.ContainerStateRunning{}

	if err := h.r.reconcilePods(h.s); !errors.Is(err, errBoom) {
		t.Fatalf("err %v, want the relay error", err)
	}
}

func TestReconcilePodsUpToDateDoesNotRecreate(t *testing.T) {
	h := newPodHarness(t, true, newClass(), interceptor.Funcs{})

	if err := h.r.reconcilePods(h.s); err != nil {
		t.Fatal(err)
	}
	if h.s.pod == nil || h.s.agent == nil {
		t.Fatal("up-to-date pods keep their agent")
	}
	if c := condition(h.s, v1alpha1.ConditionRecreatePending); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "UpToDate" {
		t.Fatalf("RecreatePending %+v", c)
	}
	if c := condition(h.s, v1alpha1.ConditionAgentReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("AgentReady %+v", c)
	}
	if c := condition(h.s, v1alpha1.ConditionGamePodReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("GamePodReady %+v", c)
	}
}

func TestGamePodReadyReasons(t *testing.T) {
	cases := map[string]struct {
		mutate func(h *podHarness)
		reason string
	}{
		"no pod, stopped": {func(h *podHarness) { h.s.pod = nil }, "NotRequested"},
		"no pod, running": {func(h *podHarness) { h.s.pod = nil; h.s.gs.Spec.Power.Desired = v1alpha1.PowerRunning }, "NoPod"},
		"unschedulable": {func(h *podHarness) {
			h.pod.Spec.NodeName = ""
			h.pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: "0/3 nodes are available"}}
		}, "Unschedulable"},
		"agent not ready":   {func(h *podHarness) { h.s.agentPod.Status.ContainerStatuses[0].Ready = false }, "AgentNotReady"},
		"not running":       {func(h *podHarness) { h.pod.Status.ContainerStatuses = nil }, "Starting"},
		"shim not attached": {func(h *podHarness) { h.agent.shimPod = "" }, "ShimNotAttached"},
		"other pod's shim":  {func(h *podHarness) { h.agent.shimPod = "old" }, "ShimNotAttached"},
		"ready":             {func(h *podHarness) {}, "Ready"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newPodHarness(t, true, newClass(), interceptor.Funcs{})
			tc.mutate(h)
			if err := h.r.reconcilePods(h.s); err != nil {
				t.Fatal(err)
			}
			if c := condition(h.s, v1alpha1.ConditionGamePodReady); c == nil || c.Reason != tc.reason {
				t.Fatalf("GamePodReady %+v, want %s", c, tc.reason)
			}
		})
	}
}

// A pod of the single-pod layout is deleted as soon as the operator sees it.
func TestLegacyPodIsDeleted(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	legacy := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: names.Pod(uuid), Namespace: ns, Labels: map[string]string{v1alpha1.LabelServerUUID: uuid, v1alpha1.LabelComponent: "game"}},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "prepare"}, {Name: render.AgentContainer}},
			Containers:     []corev1.Container{{Name: render.GameContainer}},
		},
	}
	if err := h.c.Create(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	if h.pod() != nil {
		t.Fatal("the legacy pod must be deleted")
	}
	if !hasEvent(recordedEvents(h), "LegacyPodDeleted") {
		t.Fatal("LegacyPodDeleted event expected")
	}
}

// The resize phase always runs, even when the pod is already due for a
// recreate for another reason: it owns the ResizePending condition and the
// in-place resize attempt, and the recreate may stay deferred for a while.
func TestReconcilePodsResizesEvenWhenRecreateIsPending(t *testing.T) {
	reasons := map[string]struct {
		pending func(*scope)
		reason  string
	}{
		"outdated template": {func(s *scope) { s.gs.Status.TemplateHash = "something-else" }, "ResizeNeedsRecreate"},
		"restart requested": {func(s *scope) { s.gs.Spec.Power.RestartRequest = 1 }, "RestartRequested"},
	}
	for name, tc := range reasons {
		t.Run(name+"/resize applied", func(t *testing.T) {
			h := newPodHarness(t, true, newClass(), interceptor.Funcs{})
			h.agent.state = v1alpha1.ProcessRunning
			h.updateGS(func(gs *v1alpha1.GameServer) {
				gs.Spec.Panel.Settings = settingsJSON(4096, 200, 5120, "ghcr.io/pelican-eggs/yolks:java_21", false)
			})
			h.s = h.scope(newClass())
			tc.pending(h.s)
			h.events()

			if err := h.r.reconcilePods(h.s); err != nil {
				t.Fatal(err)
			}
			if !hasEvent(h.events(), "Resized") {
				t.Fatal("the in-place resize must still be attempted")
			}
			if c := condition(h.s, v1alpha1.ConditionResizePending); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "Applied" {
				t.Fatalf("ResizePending %+v", c)
			}
			if c := condition(h.s, v1alpha1.ConditionRecreatePending); c == nil || c.Status != metav1.ConditionTrue {
				t.Fatalf("RecreatePending %+v", c)
			}
		})
		t.Run(name+"/limit removal", func(t *testing.T) {
			h := newPodHarness(t, true, newClass(), interceptor.Funcs{})
			h.agent.state = v1alpha1.ProcessRunning
			h.updateGS(func(gs *v1alpha1.GameServer) {
				gs.Spec.Panel.Settings = settingsJSON(2048, 0, 5120, "ghcr.io/pelican-eggs/yolks:java_21", false)
			})
			h.s = h.scope(newClass())
			tc.pending(h.s)

			if err := h.r.reconcilePods(h.s); err != nil {
				t.Fatal(err)
			}
			if c := condition(h.s, v1alpha1.ConditionResizePending); c == nil || c.Status != metav1.ConditionTrue || c.Reason != "RecreateRequired" {
				t.Fatalf("ResizePending %+v", c)
			}
			if c := condition(h.s, v1alpha1.ConditionRecreatePending); c == nil || c.Reason != tc.reason {
				t.Fatalf("RecreatePending %+v, want %s", c, tc.reason)
			}
		})
	}
}

func TestGameReplicas(t *testing.T) {
	cases := []struct {
		name      string
		desired   v1alpha1.PowerState
		suspended bool
		noPod     bool
		noAgent   bool
		deleting  bool
		unstarted bool
		state     string
		want      int32
	}{
		{name: "running, no pod yet", desired: v1alpha1.PowerRunning, noPod: true, want: 1},
		{name: "running, process running", desired: v1alpha1.PowerRunning, state: v1alpha1.ProcessRunning, want: 1},
		{name: "running, crashed and offline", desired: v1alpha1.PowerRunning, state: v1alpha1.ProcessOffline, want: 1},
		{name: "stopped, no pod", desired: v1alpha1.PowerStopped, noPod: true, want: 0},
		{name: "stopped, still stopping", desired: v1alpha1.PowerStopped, state: v1alpha1.ProcessStopping, want: 1},
		{name: "stopped, offline", desired: v1alpha1.PowerStopped, state: v1alpha1.ProcessOffline, want: 0},
		{name: "stopped, agent unavailable", desired: v1alpha1.PowerStopped, noAgent: true, want: 1},
		{name: "stopped, game container never started", desired: v1alpha1.PowerStopped, unstarted: true, state: v1alpha1.ProcessRunning, want: 0},
		{name: "stopped, pod terminating", desired: v1alpha1.PowerStopped, deleting: true, state: v1alpha1.ProcessRunning, want: 0},
		{name: "suspended, running", desired: v1alpha1.PowerRunning, suspended: true, state: v1alpha1.ProcessRunning, want: 1},
		{name: "suspended, offline", desired: v1alpha1.PowerRunning, suspended: true, state: v1alpha1.ProcessOffline, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPodHarness(t, true, newClass(), interceptor.Funcs{})
			h.s.gs.Spec.Power.Desired = tc.desired
			h.s.settings.Suspended = tc.suspended
			h.agent.state = tc.state
			h.s.agent = h.agent
			if tc.noAgent {
				h.s.agent = nil
			}
			if tc.noPod {
				h.s.pod = nil
			}
			if tc.deleting {
				now := metav1.NewTime(h.now)
				h.pod.DeletionTimestamp = &now
			}
			if tc.unstarted {
				h.pod.Status.ContainerStatuses = nil
			}
			got, err := h.r.gameReplicas(h.s)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("replicas %d, want %d", got, tc.want)
			}
		})
	}
	t.Run("agent error", func(t *testing.T) {
		h := newPodHarness(t, true, newClass(), interceptor.Funcs{})
		h.agent.getErr = errBoom
		h.s.agent = h.agent
		if _, err := h.r.gameReplicas(h.s); !errors.Is(err, errBoom) {
			t.Fatalf("err %v", err)
		}
	})
}
