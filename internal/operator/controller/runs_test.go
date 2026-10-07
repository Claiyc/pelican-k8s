package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
)

// replicas returns the replicas of a StatefulSet.
func (h *harness) replicas(name string) int32 {
	h.t.Helper()
	sts := &appsv1.StatefulSet{}
	if !h.get(sts, name) {
		h.t.Fatalf("statefulset %s missing", name)
	}
	return *sts.Spec.Replicas
}

// settleGamePod plays the game StatefulSet controller: it removes the game
// pod when the StatefulSet is scaled to 0.
func (h *harness) settleGamePod() {
	h.t.Helper()
	if h.replicas(names.StatefulSet(uuid)) == 0 {
		if p := h.pod(); p != nil {
			if err := h.c.Delete(context.Background(), p); err != nil {
				h.t.Fatal(err)
			}
		}
	}
}

func (h *harness) condReason(t string) string {
	h.t.Helper()
	c := meta.FindStatusCondition(h.gs().Status.Conditions, t)
	if c == nil {
		return ""
	}
	return c.Reason
}

// A stopped server runs an agent pod and no game pod; a start creates the game
// pod and starts the process once its shim is attached.
func TestStartFromStopped(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	h.createAgentPod(true)
	h.reconcile(2)
	if got := h.replicas(names.StatefulSet(uuid)); got != 0 {
		t.Fatalf("a stopped server has no game pod: replicas %d", got)
	}
	if got := h.replicas(names.AgentStatefulSet(uuid)); got != 1 {
		t.Fatalf("the agent always runs: replicas %d", got)
	}
	if gs := h.gs(); gs.Status.Phase != v1alpha1.PhaseStopped || h.condReason(v1alpha1.ConditionGamePodReady) != "NotRequested" {
		t.Fatalf("phase %s, GamePodReady %s", gs.Status.Phase, h.condReason(v1alpha1.ConditionGamePodReady))
	}

	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	})
	h.reconcile(1)
	if got := h.replicas(names.StatefulSet(uuid)); got != 1 {
		t.Fatalf("a start scales the game StatefulSet to 1: replicas %d", got)
	}
	if calls := h.agent.Calls(); len(calls) != 0 {
		t.Fatalf("no start before the game pod exists: %v", calls)
	}
	if gs := h.gs(); gs.Status.Phase != v1alpha1.PhaseStarting || gs.Status.Power.ObservedGeneration != 0 {
		t.Fatalf("phase %s, observed %d", gs.Status.Phase, gs.Status.Power.ObservedGeneration)
	}

	game := h.createGamePod()
	h.reconcile(2)
	if got := strings.Join(h.agent.Calls(), ","); got != "power:start" {
		t.Fatalf("calls %s", got)
	}
	st := h.gs().Status
	if st.Power.ObservedGeneration != 1 || st.Game.PodUID != string(game.UID) || st.Power.LastAction.PodUID != string(game.UID) {
		t.Fatalf("status %+v", st)
	}
}

// A Panel stop stops the process; the game pod goes once it is offline.
func TestStopRemovesTheGamePod(t *testing.T) {
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
	if got := strings.Join(h.agent.Calls(), ","); got != "power:start,power:stop" {
		t.Fatalf("calls %s", got)
	}
	if got := h.replicas(names.StatefulSet(uuid)); got != 1 {
		t.Fatalf("the game pod stays while the process stops: replicas %d", got)
	}

	h.agent.state = v1alpha1.ProcessOffline
	h.reconcile(1)
	if got := h.replicas(names.StatefulSet(uuid)); got != 0 {
		t.Fatalf("an offline stopped server loses its game pod: replicas %d", got)
	}
	h.settleGamePod()
	h.reconcile(2)
	if h.agentPod() == nil {
		t.Fatal("the agent pod stays")
	}
	if got := strings.Join(h.agent.Calls(), ","); got != "power:start,power:stop" {
		t.Fatalf("nothing more to do: %s", got)
	}
	if gs := h.gs(); gs.Status.Phase != v1alpha1.PhaseStopped {
		t.Fatalf("phase %s", gs.Status.Phase)
	}
}

// The stop command typed into the console: Wings reports stopping → offline,
// the gateway sets desired Stopped without a new generation, and the game pod goes.
func TestConsoleStopRemovesTheGamePod(t *testing.T) {
	gs := newGS()
	gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	h := newHarness(t, gs, newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	h.agent.state = v1alpha1.ProcessOffline
	h.updateGS(func(gs *v1alpha1.GameServer) { gs.Spec.Power.Desired = v1alpha1.PowerStopped })
	h.reconcile(1)
	if got := h.replicas(names.StatefulSet(uuid)); got != 0 {
		t.Fatalf("replicas %d", got)
	}
	if got := strings.Join(h.agent.Calls(), ","); got != "power:start" {
		t.Fatalf("no power call for a stop that already happened: %s", got)
	}
}

// A crash leaves desired Running: Wings' crash handler restarts the process in
// the same game pod, and the operator neither starts it nor removes the pod.
func TestCrashKeepsTheGamePod(t *testing.T) {
	gs := newGS()
	gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	h := newHarness(t, gs, newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	h.agent.state = v1alpha1.ProcessOffline
	h.patchStatus(func(gs *v1alpha1.GameServer) { gs.Status.Process.State = v1alpha1.ProcessOffline })
	h.reconcile(3)
	if got := h.replicas(names.StatefulSet(uuid)); got != 1 {
		t.Fatalf("replicas %d", got)
	}
	if got := strings.Join(h.agent.Calls(), ","); got != "power:start" {
		t.Fatalf("the operator must not restart a crashed process: %s", got)
	}
	if gs := h.gs(); gs.Status.Phase != v1alpha1.PhaseStopped {
		t.Fatalf("a settled crash is Stopped, not Starting: %s", gs.Status.Phase)
	}
}

// A suspension stops the process through the agent's sync; the game pod goes
// once it is offline, and with suspendScalesToZero the agent pod after it.
func TestSuspensionRemovesThePods(t *testing.T) {
	cls := newClass()
	cls.Spec.SuspendScalesToZero = true
	gs := newGS()
	gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	h := newHarness(t, gs, cls)
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	h.agent.state = v1alpha1.ProcessRunning
	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Panel.Settings = settingsJSON(2048, 200, 5120, "ghcr.io/pelican-eggs/yolks:java_21", true)
		gs.Spec.Panel.PanelRevision = "rev2"
	})
	h.reconcile(1)
	if got := strings.Join(h.agent.Calls(), ","); got != "power:start,sync" {
		t.Fatalf("calls %s", got)
	}
	if h.replicas(names.StatefulSet(uuid)) != 1 || h.replicas(names.AgentStatefulSet(uuid)) != 1 {
		t.Fatal("both pods stay while the process stops")
	}
	h.agent.state = v1alpha1.ProcessOffline
	h.reconcile(1)
	if got := h.replicas(names.StatefulSet(uuid)); got != 0 {
		t.Fatalf("game replicas %d", got)
	}
	if got := h.replicas(names.AgentStatefulSet(uuid)); got != 1 {
		t.Fatalf("the agent pod waits for the game pod to go: replicas %d", got)
	}
	h.settleGamePod()
	h.reconcile(1)
	if got := h.replicas(names.AgentStatefulSet(uuid)); got != 0 {
		t.Fatalf("agent replicas %d", got)
	}
	if gs := h.gs(); gs.Status.Phase != v1alpha1.PhaseSuspended {
		t.Fatalf("phase %s", gs.Status.Phase)
	}
}

// An install on a stopped server needs only the agent pod.
func TestInstallWithoutGamePod(t *testing.T) {
	gs := newGS()
	gs.Spec.Install = v1alpha1.InstallSpec{Generation: 1, ScriptConfigMap: names.InstallConfigMap(uuid, 1)}
	h := newHarness(t, gs, newClass())
	h.reconcile(2)
	h.createAgentPod(true)
	h.reconcile(2)
	if got := strings.Join(h.agent.Calls(), ","); got != "install:false" {
		t.Fatalf("calls %s", got)
	}
	if h.pod() != nil || h.replicas(names.StatefulSet(uuid)) != 0 {
		t.Fatal("an install runs without a game pod")
	}
}

// A template change on a stopped server needs no action: the next start
// creates the game pod from the current template.
func TestImageChangeWhileStopped(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	h.createAgentPod(true)
	h.reconcile(2)
	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Panel.Settings = settingsJSON(2048, 200, 5120, "ghcr.io/pelican-eggs/yolks:java_25", false)
	})
	h.reconcile(1)
	if h.condReason(v1alpha1.ConditionRecreatePending) != "UpToDate" {
		t.Fatalf("RecreatePending %s", h.condReason(v1alpha1.ConditionRecreatePending))
	}
	if h.agentPod() == nil {
		t.Fatal("the agent pod is not part of the game template")
	}
	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	})
	h.reconcile(1)
	game := h.createGamePod()
	if img := game.Spec.Containers[0].Image; !strings.HasPrefix(img, "ghcr.io/pelican-eggs/yolks:java_25@") {
		t.Fatalf("the new game pod runs %s", img)
	}
	h.reconcile(1)
	if got := strings.Join(h.agent.Calls(), ","); got != "power:start" {
		t.Fatalf("calls %s", got)
	}
}

