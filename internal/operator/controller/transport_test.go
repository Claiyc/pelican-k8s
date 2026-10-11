package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/internal/operator/render"
	"github.com/Claiyc/pelican-k8s/internal/pki"
)

// enableTLS turns tls on for the harness' reconciler, as a restart of the
// operator with tls.enabled does.
func (h *harness) enableTLS() {
	h.t.Helper()
	ca, _, err := pki.NewCA(h.now)
	if err != nil {
		h.t.Fatal(err)
	}
	if h.r.PKI, err = pki.NewIssuer(ca, nil, pki.OperatorName); err != nil {
		h.t.Fatal(err)
	}
}

// later moves the clock, so the next pod the harness creates gets a new UID.
func (h *harness) later() { h.now = h.now.Add(time.Second) }

// Turning tls on replaces the agent pod of a stopped server without calling
// the agent, which still serves plain HTTP.
func TestTLSTurnedOnReplacesAStoppedServersAgent(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	old := h.createAgentPod(true)
	h.reconcile(2)
	if render.PodTLS(old, uuid) {
		t.Fatal("a pod rendered without tls counts as tls")
	}

	h.enableTLS()
	h.agent.err, h.agent.getErr, h.agent.shimErr = errors.New("http: server gave HTTP response to HTTPS client"), errors.New("unreachable"), errors.New("unreachable")
	h.reconcile(1)
	if h.agentPod() != nil {
		t.Fatal("the agent pod of the old setting stays")
	}
	if calls := h.agent.Calls(); len(calls) != 0 {
		t.Fatalf("the agent of the old setting was called: %v", calls)
	}
	if got := h.condReason(v1alpha1.ConditionRecreatePending); got != reasonTransportChanged {
		t.Fatalf("RecreatePending %s", got)
	}
	if gs := h.gs(); gs.Status.Phase == v1alpha1.PhaseError {
		t.Fatal("a tls change is not an error")
	}

	h.agent.err, h.agent.getErr, h.agent.shimErr = nil, nil, nil
	h.later()
	fresh := h.createAgentPod(true)
	if !render.PodTLS(fresh, uuid) {
		t.Fatal("the new agent pod does not mount its certificate")
	}
	h.reconcile(2)
	if !condTrue(h.gs(), v1alpha1.ConditionAgentReady) || h.condReason(v1alpha1.ConditionRecreatePending) != "UpToDate" {
		t.Fatalf("AgentReady %s, RecreatePending %s", h.condReason(v1alpha1.ConditionAgentReady), h.condReason(v1alpha1.ConditionRecreatePending))
	}
	if h.agentPod() == nil {
		t.Fatal("the agent pod of the new setting was replaced again")
	}
}

// A running server is stopped through its game pod first, then its agent pod
// follows, and the process is started again in the new pods.
func TestTLSTurnedOnRestartsARunningServer(t *testing.T) {
	gs := newGS()
	gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	h := newHarness(t, gs, newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	h.agent.state = v1alpha1.ProcessRunning
	h.agent.shimRunning = true
	h.patchStatus(func(gs *v1alpha1.GameServer) { gs.Status.Process.State = v1alpha1.ProcessRunning })

	h.enableTLS()
	h.reconcile(1)
	if h.pod() != nil {
		t.Fatal("the game pod of the old setting stays")
	}
	if h.agentPod() == nil {
		t.Fatal("the agent pod goes before its game pod is gone")
	}
	if got := h.replicas(names.StatefulSet(uuid)); got != 1 {
		t.Fatalf("a server that should run keeps its game StatefulSet at 1: replicas %d", got)
	}
	if got := h.gs().Spec.Power.Desired; got != v1alpha1.PowerRunning {
		t.Fatalf("desired %s", got)
	}

	// The StatefulSet controller creates the game pod of the new setting.
	h.later()
	h.agent.shimRunning = false
	game := h.createGamePod()
	if !render.PodTLS(game, uuid) {
		t.Fatal("the new game pod does not mount the CA bundle")
	}
	h.agent.mu.Lock()
	h.agent.shimPod = ""
	h.agent.mu.Unlock()
	h.reconcile(1)
	if h.agentPod() != nil {
		t.Fatal("the agent pod of the old setting stays")
	}
	if h.pod() == nil {
		t.Fatal("the game pod of the new setting was deleted")
	}

	h.later()
	h.createAgentPod(true)
	h.agent.mu.Lock()
	h.agent.shimPod, h.agent.state = string(game.UID), v1alpha1.ProcessOffline
	h.agent.mu.Unlock()
	h.reconcile(2)
	if calls := h.agent.Calls(); len(calls) == 0 || calls[len(calls)-1] != "power:start" {
		t.Fatalf("the process is not started in the new pods: %v", calls)
	}
}

// A server that should stop does not get a game pod of the new setting.
func TestTransportChangeDoesNotRecreateAStoppingServersGamePod(t *testing.T) {
	gs := newGS()
	gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	h := newHarness(t, gs, newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerStopped, Generation: 2}
	})

	h.enableTLS()
	h.reconcile(1)
	if got := h.replicas(names.StatefulSet(uuid)); got != 0 {
		t.Fatalf("replicas %d", got)
	}
	if h.pod() != nil {
		t.Fatal("the game pod of the old setting stays")
	}
	h.reconcile(1)
	if h.agentPod() != nil {
		t.Fatal("the agent pod of the old setting stays")
	}
}

// Turning tls off replaces the pods the same way and removes the certificate
// Secret once nothing mounts it.
func TestTLSTurnedOffReplacesTheAgentAndDropsItsSecret(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.enableTLS()
	h.reconcile(2)
	h.createAgentPod(true)
	h.reconcile(2)
	if !h.get(&corev1.Secret{}, names.TLSSecret(uuid)) {
		t.Fatal("tls secret missing")
	}

	h.r.PKI = nil
	h.reconcile(1)
	if h.agentPod() != nil {
		t.Fatal("the agent pod of the old setting stays")
	}
	h.later()
	if fresh := h.createAgentPod(true); render.PodTLS(fresh, uuid) {
		t.Fatal("the new agent pod still mounts a certificate")
	}
	h.reconcile(2)
	if h.get(&corev1.Secret{}, names.TLSSecret(uuid)) {
		t.Fatal("the certificate Secret stays without tls")
	}
	if !condTrue(h.gs(), v1alpha1.ConditionAgentReady) {
		t.Fatalf("AgentReady %s", h.condReason(v1alpha1.ConditionAgentReady))
	}
}

// A Secret the GameServer does not own, as cert-manager writes it, is kept.
func TestForeignTLSSecretIsKept(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	sec := &corev1.Secret{}
	sec.Namespace, sec.Name = ns, names.TLSSecret(uuid)
	if err := h.c.Create(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	h.createAgentPod(true)
	h.reconcile(2)
	if !h.get(&corev1.Secret{}, names.TLSSecret(uuid)) {
		t.Fatal("a Secret the server does not own was deleted")
	}
}

// Without a game pod no process runs, whatever status.process still says.
func TestProcessIsOfflineWithoutAGamePod(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	h.createAgentPod(true)
	h.reconcile(2)
	h.patchStatus(func(gs *v1alpha1.GameServer) { gs.Status.Process.State = v1alpha1.ProcessRunning })

	h.reconcile(1)
	gs := h.gs()
	if gs.Status.Process.State != v1alpha1.ProcessOffline || gs.Status.Process.Since == nil || !gs.Status.Process.Since.Time.Equal(h.now) {
		t.Fatalf("process %+v", gs.Status.Process)
	}
	if gs.Status.Phase != v1alpha1.PhaseStopped {
		t.Fatalf("phase %s", gs.Status.Phase)
	}
}

// A reconcile that keeps failing does not restamp AgentReady: the status
// write would trigger the next reconcile at once.
func TestFailingReconcileKeepsTransitionTimes(t *testing.T) {
	gs := newGS()
	gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	h := newHarness(t, gs, newClass())
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)

	h.agent.shimErr = errors.New("agent: unavailable")
	if _, err := h.reconcileResult(); err == nil {
		t.Fatal("reconcile did not fail")
	}
	first := meta.FindStatusCondition(h.gs().Status.Conditions, v1alpha1.ConditionAgentReady)
	if first == nil || first.Reason != "Error" {
		t.Fatalf("AgentReady %+v", first)
	}
	version := h.gs().ResourceVersion

	h.now = h.now.Add(time.Minute)
	if _, err := h.reconcileResult(); err == nil {
		t.Fatal("reconcile did not fail")
	}
	second := meta.FindStatusCondition(h.gs().Status.Conditions, v1alpha1.ConditionAgentReady)
	if !second.LastTransitionTime.Equal(&first.LastTransitionTime) {
		t.Fatalf("transition time moved from %s to %s", first.LastTransitionTime, second.LastTransitionTime)
	}
	if got := h.gs().ResourceVersion; got != version {
		t.Fatalf("a repeated failure wrote the status again: resourceVersion %s, was %s", got, version)
	}
}

// A start prints what it waits for on the console, once per step.
func TestStartIsAnnouncedOnTheConsole(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	h.createAgentPod(true)
	h.reconcile(2)
	if n := h.agent.Notices(); len(n) != 0 {
		t.Fatalf("a stopped server prints nothing: %v", n)
	}

	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	})
	h.reconcile(3)
	if n := h.agent.Notices(); !slices.Equal(n, []string{"Creating the game pod..."}) {
		t.Fatalf("notices %v", n)
	}

	game := h.createGamePod()
	h.unschedulable(game)
	h.reconcile(2)
	n := h.agent.Notices()
	if len(n) != 2 || !strings.HasPrefix(n[1], "The game pod cannot be scheduled yet: 0/3 nodes") {
		t.Fatalf("notices %v", n)
	}
}
