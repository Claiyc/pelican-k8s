package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
)

// A kill ends the process and takes the game pod with it at once, so nothing
// can start the process again in a pod that is on its way out.
func TestKillDeletesTheGamePod(t *testing.T) {
	gs := newGS()
	gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	h := newHarness(t, gs, newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	h.agent.state = v1alpha1.ProcessStopping

	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerStopped, Generation: 2, Kill: true}
	})
	h.reconcile(1)
	if got := strings.Join(h.agent.Calls(), ","); got != "power:start,power:kill" {
		t.Fatalf("calls %s", got)
	}
	if h.pod() != nil {
		t.Fatal("the game pod stays after a kill")
	}
	if got := h.replicas(names.StatefulSet(uuid)); got != 0 {
		t.Fatalf("the game pod of a killed server is recreated: replicas %d", got)
	}
	if got := h.gs().Status.Power.ObservedGeneration; got != 2 {
		t.Fatalf("observed generation %d", got)
	}
	if h.agentPod() == nil {
		t.Fatal("the agent pod went with the kill")
	}
}

// A kill of a process that is already offline still removes a game pod that
// lingers, and does not call the agent.
func TestKillOfAnOfflineProcessDeletesTheGamePod(t *testing.T) {
	gs := newGS()
	gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	h := newHarness(t, gs, newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	h.agent.state = v1alpha1.ProcessOffline

	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerStopped, Generation: 2, Kill: true}
	})
	h.reconcile(1)
	if got := strings.Join(h.agent.Calls(), ","); got != "power:start" {
		t.Fatalf("calls %s", got)
	}
	if h.pod() != nil {
		t.Fatal("the game pod stays after a kill")
	}
}

// A kill does not wait for an agent that cannot be asked: the pod goes at
// once, and the generation stays open for the agent that comes back.
func TestKillWithoutAReadyAgentDeletesTheGamePod(t *testing.T) {
	gs := newGS()
	gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	h := newHarness(t, gs, newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	agent := h.agentPod()
	agent.Status.ContainerStatuses[0].Ready = false
	if err := h.c.Status().Update(context.Background(), agent); err != nil {
		t.Fatal(err)
	}

	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerStopped, Generation: 2, Kill: true}
	})
	h.reconcile(1)
	if h.pod() != nil {
		t.Fatal("the game pod waits for the agent")
	}
	if got := strings.Join(h.agent.Calls(), ","); got != "power:start" {
		t.Fatalf("calls %s", got)
	}
	if got := h.replicas(names.StatefulSet(uuid)); got != 0 {
		t.Fatalf("replicas %d", got)
	}
}

// Nor for the shim of a game pod that has not attached yet.
func TestKillOfAGamePodWithoutAShimDeletesIt(t *testing.T) {
	gs := newGS()
	gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	h := newHarness(t, gs, newClass())
	h.reconcile(2)
	h.createAgentPod(true)
	h.reconcile(1)
	h.createGamePod()
	h.agent.mu.Lock()
	h.agent.shimPod = ""
	h.agent.mu.Unlock()

	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerStopped, Generation: 2, Kill: true}
	})
	h.reconcile(1)
	if h.pod() != nil {
		t.Fatal("the game pod waits for its shim")
	}
}

// Nor does an agent that answers with an error keep the pod: the pod goes,
// and the reconcile fails so the agent is told again.
func TestKillWithAFailingAgentDeletesTheGamePod(t *testing.T) {
	for name, fail := range map[string]func(*fakeAgent, error){
		"state": func(a *fakeAgent, err error) { a.getErr = err },
		"power": func(a *fakeAgent, err error) { a.err = err },
	} {
		t.Run(name, func(t *testing.T) {
			gs := newGS()
			gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
			h := newHarness(t, gs, newClass())
			h.reconcile(2)
			h.createPod(true)
			h.reconcile(2)
			h.agent.state = v1alpha1.ProcessRunning
			boom := errors.New("agent: unavailable")
			fail(h.agent, boom)

			h.updateGS(func(gs *v1alpha1.GameServer) {
				gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerStopped, Generation: 2, Kill: true}
			})
			if _, err := h.reconcileResult(); !errors.Is(err, boom) {
				t.Fatalf("reconcile error %v", err)
			}
			if h.pod() != nil {
				t.Fatal("the game pod stays while the agent fails")
			}
			if got := h.gs().Status.Power.ObservedGeneration; got != 1 {
				t.Fatalf("the kill is recorded as done: observed generation %d", got)
			}
		})
	}
}

// Nor does a failure before the agent is even reached: its token Secret
// cannot be read.
func TestKillBeforeTheAgentIsSetUpDeletesTheGamePod(t *testing.T) {
	gs := newGS()
	gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	h := newHarness(t, gs, newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	boom := errors.New("boom")
	armed := false
	h.r.Client = interceptor.NewClient(h.c.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			// Only the read of the token for the agent client, after the pods
			// were loaded; the Secret is also read earlier to ensure it exists.
			if _, ok := obj.(*corev1.Pod); ok && key.Name == names.AgentPod(uuid) {
				armed = true
			}
			if _, ok := obj.(*corev1.Secret); ok && armed && key.Name == names.AgentSecret(uuid) {
				return boom
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerStopped, Generation: 2, Kill: true}
	})
	if _, err := h.reconcileResult(); !errors.Is(err, boom) {
		t.Fatalf("reconcile error %v", err)
	}
	if h.pod() != nil {
		t.Fatal("the game pod stays when the agent cannot be set up")
	}
}

// A pod that cannot be deleted fails the reconcile, so the kill is tried
// again instead of being recorded as done.
func TestKillThatCannotDeleteThePodIsRetried(t *testing.T) {
	gs := newGS()
	gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	h := newHarness(t, gs, newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	h.agent.state = v1alpha1.ProcessRunning
	boom := errors.New("boom")
	h.r.Client = interceptor.NewClient(h.c.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*corev1.Pod); ok {
				return boom
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerStopped, Generation: 2, Kill: true}
	})
	if _, err := h.reconcileResult(); !errors.Is(err, boom) {
		t.Fatalf("reconcile error %v", err)
	}
	if got := h.gs().Status.Power.ObservedGeneration; got != 1 {
		t.Fatalf("the kill is recorded as done: observed generation %d", got)
	}
}

// A stop leaves the game pod until the process is offline.
func TestStopKeepsTheGamePodWhileTheProcessStops(t *testing.T) {
	gs := newGS()
	gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	h := newHarness(t, gs, newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	h.agent.state = v1alpha1.ProcessRunning

	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerStopped, Generation: 2}
	})
	h.reconcile(1)
	if h.pod() == nil {
		t.Fatal("a stop deleted the game pod before the process stopped")
	}
}
