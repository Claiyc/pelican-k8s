package controller

import (
	"strings"
	"testing"

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
