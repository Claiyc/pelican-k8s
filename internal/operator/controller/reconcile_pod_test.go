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

// podHarness is a harness with an existing pod and a scope pointing at it, for
// exercising the phases of reconcilePod one at a time.
type podHarness struct {
	*harness
	pod *corev1.Pod
	s   *scope
}

// newPodHarness reconciles a GameServer to the point where its pod exists
// (agent ready or not) and builds the scope reconcilePod would see.
func newPodHarness(t *testing.T, agentReady bool, cls *v1alpha1.GameServerClass, funcs interceptor.Funcs) *podHarness {
	t.Helper()
	h := newHarnessWith(t, funcs, newGS(), cls)
	h.reconcile(2)
	h.createPod(agentReady)
	h.reconcile(2)
	ph := &podHarness{harness: h, pod: h.pod()}
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

func TestReconcilePodTerminating(t *testing.T) {
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
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newPodHarness(t, true, tt.class, interceptor.Funcs{})
			if tt.node != nil {
				h.createNode(*tt.node)
			}
			deleting := metav1.NewTime(h.now.Add(-tt.terminating))
			h.pod.DeletionTimestamp = &deleting
			h.pod.Spec.NodeName = "node-1"

			if err := h.r.reconcilePod(h.s); err != nil {
				t.Fatal(err)
			}

			nodeLost := condition(h.s, v1alpha1.ConditionNodeLost)
			if tt.wantNodeLos != (nodeLost != nil && nodeLost.Status == metav1.ConditionTrue) {
				t.Fatalf("NodeLost condition %+v, want lost=%v", nodeLost, tt.wantNodeLos)
			}
			if c := condition(h.s, v1alpha1.ConditionAgentReady); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "Terminating" {
				t.Fatalf("AgentReady condition %+v", c)
			}
			if h.s.requeue != requeueFast {
				t.Fatalf("requeue %v", h.s.requeue)
			}
			if deleted := h.harness.pod() == nil; deleted != tt.wantDeleted {
				t.Fatalf("pod deleted=%v, want %v", deleted, tt.wantDeleted)
			}
			if got := hasEvent(h.events(), "ForceDelete"); got != tt.wantDeleted {
				t.Fatalf("ForceDelete event=%v, want %v", got, tt.wantDeleted)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestReconcilePodTerminatingForceDeleteErrors(t *testing.T) {
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

	if err := h.r.reconcilePod(h.s); !errors.Is(err, errBoom) {
		t.Fatalf("err %v, want the delete error", err)
	}
	if grace == nil || *grace != 0 {
		t.Fatalf("force delete must use a zero grace period, got %v", grace)
	}
}

func TestReconcilePodTerminatingForceDeleteIgnoresNotFound(t *testing.T) {
	h := newPodHarness(t, true, failoverClass(time.Minute), interceptor.Funcs{})
	h.createNode(false)
	deleting := metav1.NewTime(h.now.Add(-time.Hour))
	h.pod.DeletionTimestamp = &deleting
	h.pod.Spec.NodeName = "node-1"
	if err := h.c.Delete(context.Background(), h.pod.DeepCopy()); err != nil {
		t.Fatal(err)
	}

	if err := h.r.reconcilePod(h.s); err != nil {
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

func TestRecreateWhenOffline(t *testing.T) {
	tests := []struct {
		name        string
		state       string
		getErr      error
		deleteErr   error
		restart     bool
		wantErr     error
		wantDeleted bool
	}{
		{name: "agent unreachable", getErr: errBoom, wantErr: errBoom},
		{name: "process still running", state: v1alpha1.ProcessRunning},
		{name: "offline", state: v1alpha1.ProcessOffline, wantDeleted: true},
		{name: "offline with restart request", state: v1alpha1.ProcessOffline, restart: true, wantDeleted: true},
		{name: "delete fails", state: v1alpha1.ProcessOffline, deleteErr: errBoom, wantErr: errBoom},
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

			err := h.r.recreateWhenOffline(h.s, h.pod, tt.restart)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err %v, want %v", err, tt.wantErr)
			}
			if gone := h.s.pod == nil; gone != tt.wantDeleted {
				t.Fatalf("scope pod cleared=%v, want %v", gone, tt.wantDeleted)
			}
			if tt.wantDeleted {
				if h.s.agent != nil || h.s.requeue != requeueFast {
					t.Fatalf("agent %v requeue %v: a recreate drops the agent and requeues fast", h.s.agent, h.s.requeue)
				}
				if h.harness.pod() != nil {
					t.Fatal("pod must be deleted")
				}
				if !hasEvent(h.events(), "Recreate") {
					t.Fatal("Recreate event expected")
				}
				wantObserved := int64(0)
				if tt.restart {
					wantObserved = 3
				}
				if got := h.s.gs.Status.Power.ObservedRestartRequest; got != wantObserved {
					t.Fatalf("ObservedRestartRequest %d, want %d", got, wantObserved)
				}
			} else if h.s.agent == nil || h.harness.pod() == nil {
				t.Fatal("pod and agent must be kept")
			}
		})
	}
}

func TestDeletePodForRecreate(t *testing.T) {
	t.Run("already gone", func(t *testing.T) {
		h := newPodHarness(t, true, newClass(), interceptor.Funcs{})
		if err := h.c.Delete(context.Background(), h.pod.DeepCopy()); err != nil {
			t.Fatal(err)
		}
		if err := h.r.deletePodForRecreate(h.s, h.pod, false); err != nil {
			t.Fatalf("a missing pod is not an error: %v", err)
		}
		if h.s.pod != nil {
			t.Fatal("pod must be forgotten")
		}
	})
	t.Run("error keeps the pod and the request", func(t *testing.T) {
		funcs := interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error { return errBoom }}
		h := newPodHarness(t, true, newClass(), funcs)
		h.s.gs.Spec.Power.RestartRequest = 2
		if err := h.r.deletePodForRecreate(h.s, h.pod, true); !errors.Is(err, errBoom) {
			t.Fatalf("err %v", err)
		}
		if h.s.pod == nil || h.s.gs.Status.Power.ObservedRestartRequest != 0 {
			t.Fatal("a failed delete must not acknowledge the restart or forget the pod")
		}
	})
}

// An agent that is not ready yet, with a game container that never started,
// lets an outdated pod be replaced right away.
func TestReconcilePodRecreatesUnstartedOutdatedPod(t *testing.T) {
	h := newPodHarness(t, false, newClass(), interceptor.Funcs{})
	h.s.gs.Status.TemplateHash = "something-else"
	h.s.gs.Spec.Power.RestartRequest = 5
	h.events()

	if err := h.r.reconcilePod(h.s); err != nil {
		t.Fatal(err)
	}
	if h.s.pod != nil || h.harness.pod() != nil {
		t.Fatal("the unstarted outdated pod must be deleted")
	}
	if h.s.gs.Status.Power.ObservedRestartRequest != 5 {
		t.Fatal("restart request must be acknowledged")
	}
	if c := condition(h.s, v1alpha1.ConditionAgentReady); c == nil || c.Reason != "Starting" {
		t.Fatalf("AgentReady condition %+v", c)
	}
	if !hasEvent(h.events(), "Recreate") {
		t.Fatal("Recreate event expected")
	}
}

func TestReconcilePodKeepsStartedPodWhileAgentNotReady(t *testing.T) {
	h := newPodHarness(t, false, newClass(), interceptor.Funcs{})
	h.s.gs.Status.TemplateHash = "something-else"
	withTermination(h.pod, h.now)

	if err := h.r.reconcilePod(h.s); err != nil {
		t.Fatal(err)
	}
	if h.s.pod == nil || h.harness.pod() == nil {
		t.Fatal("a pod whose game container ran must wait for the process to go offline")
	}
	if h.s.requeue != requeueFast {
		t.Fatalf("requeue %v", h.s.requeue)
	}
}

func TestReconcilePodAgentTokenError(t *testing.T) {
	h := newPodHarness(t, true, newClass(), interceptor.Funcs{})
	sec := &corev1.Secret{}
	if !h.get(sec, names.AgentSecret(uuid)) {
		t.Fatal("precondition: agent secret exists")
	}
	if err := h.c.Delete(context.Background(), sec); err != nil {
		t.Fatal(err)
	}

	if err := h.r.reconcilePod(h.s); err == nil {
		t.Fatal("a missing agent secret must fail the reconcile")
	}
	if h.s.agent != nil {
		t.Fatal("no agent without a token")
	}
}

func TestReconcilePodRelayError(t *testing.T) {
	h := newPodHarness(t, true, newClass(), interceptor.Funcs{})
	h.agent.exitErr = errBoom
	withTermination(h.pod, h.now)

	if err := h.r.reconcilePod(h.s); !errors.Is(err, errBoom) {
		t.Fatalf("err %v, want the relay error", err)
	}
}

func TestReconcilePodUpToDateDoesNotRecreate(t *testing.T) {
	h := newPodHarness(t, true, newClass(), interceptor.Funcs{})

	if err := h.r.reconcilePod(h.s); err != nil {
		t.Fatal(err)
	}
	if h.s.pod == nil || h.s.agent == nil {
		t.Fatal("an up-to-date pod keeps its agent")
	}
	if c := condition(h.s, v1alpha1.ConditionRecreatePending); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "UpToDate" {
		t.Fatalf("RecreatePending %+v", c)
	}
	if c := condition(h.s, v1alpha1.ConditionAgentReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("AgentReady %+v", c)
	}
}
