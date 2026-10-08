package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/agentclient"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/internal/operator/render"
)

// bind plays the scheduler: it puts a pod on a node.
func (h *harness) bind(pod *corev1.Pod, node string) {
	h.t.Helper()
	p := &corev1.Pod{}
	if !h.get(p, pod.Name) {
		h.t.Fatalf("pod %s missing", pod.Name)
	}
	p.Spec.NodeName = node
	if err := h.c.Update(context.Background(), p); err != nil {
		h.t.Fatal(err)
	}
}

// unschedulable marks a pod the scheduler could not place.
func (h *harness) unschedulable(pod *corev1.Pod) {
	h.t.Helper()
	p := &corev1.Pod{}
	if !h.get(p, pod.Name) {
		h.t.Fatalf("pod %s missing", pod.Name)
	}
	p.Status = corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: "0/3 nodes are available",
	}}}
	if err := h.c.Status().Update(context.Background(), p); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) template(name string) *corev1.PodTemplateSpec {
	h.t.Helper()
	sts := &appsv1.StatefulSet{}
	if !h.get(sts, name) {
		h.t.Fatalf("statefulset %s missing", name)
	}
	return &sts.Spec.Template
}

// gameAffinity reads the game StatefulSet's pod affinity toward the agent.
func (h *harness) gameAffinity() render.GameAffinity {
	h.t.Helper()
	pod := &corev1.Pod{Spec: h.template(names.StatefulSet(uuid)).Spec}
	switch {
	case requiresAgentNode(pod):
		return render.GameAffinityRequired
	case pod.Spec.Affinity != nil && pod.Spec.Affinity.PodAffinity != nil:
		return render.GameAffinityPreferred
	}
	return render.GameAffinityNone
}

// agentPinned reads the node the agent StatefulSet pins its pod to.
func (h *harness) agentPinned() string {
	h.t.Helper()
	return pinnedNode(&corev1.Pod{Spec: h.template(names.AgentStatefulSet(uuid)).Spec})
}

func (h *harness) condition(t string) *metav1.Condition {
	h.t.Helper()
	return meta.FindStatusCondition(h.gs().Status.Conditions, t)
}

func runningGS() *v1alpha1.GameServer {
	gs := newGS()
	gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	return gs
}

// startOn runs a started server with its agent pod on agentNode and its game
// pod on gameNode, as the scheduler placed them.
func startOn(t *testing.T, cls *v1alpha1.GameServerClass, agentNode, gameNode string) *harness {
	t.Helper()
	h := newHarness(t, runningGS(), cls)
	h.reconcile(2)
	h.bind(h.createAgentPod(true), agentNode)
	h.reconcile(1)
	h.bind(h.createGamePod(), gameNode)
	return h
}

func TestPlacementSameNode(t *testing.T) {
	h := startOn(t, newClass(), "node-a", "node-a")
	if got := h.gameAffinity(); got != render.GameAffinityPreferred {
		t.Fatalf("game affinity %v, want preferred", got)
	}
	agent := h.agentPod()
	h.reconcile(2)
	if got := h.agentPinned(); got != "node-a" {
		t.Fatalf("agent pinned to %q", got)
	}
	if p := h.agentPod(); p == nil || p.UID != agent.UID {
		t.Fatal("the agent pod on the game pod's node stays")
	}
	if got := h.condReason(v1alpha1.ConditionAgentRelocating); got != "SameNode" {
		t.Fatalf("AgentRelocating %s", got)
	}
	if got := strings.Join(h.agent.Calls(), ","); got != "power:start" {
		t.Fatalf("calls %s", got)
	}
}

func TestPlacementWithoutPreference(t *testing.T) {
	cls := newClass()
	cls.Spec.Scheduling.PreferAgentNode = new(bool)
	h := newHarness(t, runningGS(), cls)
	h.reconcile(2)
	h.bind(h.createAgentPod(true), "node-a")
	h.reconcile(1)
	if got := h.gameAffinity(); got != render.GameAffinityNone {
		t.Fatalf("game affinity %v, want none", got)
	}
}

// The scheduler put the game pod elsewhere: the agent pod is deleted and
// comes back pinned to the game pod's node.
func TestPlacementOtherNode(t *testing.T) {
	h := startOn(t, newClass(), "node-a", "node-b")
	recordedEvents(h)
	h.reconcile(1)
	if h.agentPod() != nil {
		t.Fatal("the agent pod on the other node is deleted")
	}
	if got := h.agentPinned(); got != "node-b" {
		t.Fatalf("agent pinned to %q", got)
	}
	if c := h.condition(v1alpha1.ConditionAgentRelocating); c == nil || c.Status != metav1.ConditionTrue || c.Reason != "Relocating" {
		t.Fatalf("AgentRelocating %+v", c)
	}
	if !hasEvent(recordedEvents(h), "AgentRelocating") {
		t.Fatal("no AgentRelocating event")
	}

	// Recreated from the pinned template, the agent pod lands on node-b.
	agent := h.createAgentPod(true)
	if got := pinnedNode(agent); got != "node-b" {
		t.Fatalf("new agent pod pinned to %q", got)
	}
	h.reconcile(1)
	if c := h.condition(v1alpha1.ConditionAgentRelocating); c.Status != metav1.ConditionTrue {
		t.Fatal("a pending agent pod is still relocating")
	}
	if h.agentPod() == nil {
		t.Fatal("a pending agent pod pinned to the game pod's node is left to the scheduler")
	}
	h.bind(agent, "node-b")
	h.reconcile(2)
	if got := h.condReason(v1alpha1.ConditionAgentRelocating); got != "SameNode" {
		t.Fatalf("AgentRelocating %s", got)
	}
}

// An agent with in-flight work keeps its node: the game pod requires it, and
// an agent pod already apart from its game pod moves once the work ends.
func TestPlacementBusyAgent(t *testing.T) {
	h := newHarness(t, runningGS(), newClass())
	h.reconcile(2)
	h.bind(h.createAgentPod(true), "node-a")
	h.agent.busy = []string{"transfer"}
	h.reconcile(1)
	if got := h.gameAffinity(); got != render.GameAffinityRequired {
		t.Fatalf("game affinity %v, want required", got)
	}

	// The work began after the game pod was placed on another node.
	h.bind(h.createGamePod(), "node-b")
	h.reconcile(2)
	if h.agentPod() == nil {
		t.Fatal("a busy agent pod is not moved")
	}
	if calls := h.agent.Calls(); len(calls) != 0 {
		t.Fatalf("no start while the agent is on another node: %v", calls)
	}
	c := h.condition(v1alpha1.ConditionAgentRelocating)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != "WaitingForWork" || !strings.Contains(c.Message, "transfer") {
		t.Fatalf("AgentRelocating %+v", c)
	}

	h.agent.mu.Lock()
	h.agent.busy = nil
	h.agent.mu.Unlock()
	h.reconcile(1)
	if h.agentPod() != nil {
		t.Fatal("the agent pod moves once its work ended")
	}
	if got := h.gameAffinity(); got != render.GameAffinityPreferred {
		t.Fatalf("game affinity %v, want preferred", got)
	}
}

// The agent's node has no room for a game pod that must join it: the pod
// stays Pending until the work ends, then is replaced by one free to go
// anywhere.
func TestPlacementBusyAgentOnFullNode(t *testing.T) {
	h := newHarness(t, runningGS(), newClass())
	h.reconcile(2)
	h.bind(h.createAgentPod(true), "node-a")
	h.agent.busy = []string{"backup"}
	h.reconcile(1)
	game := h.createGamePod()
	h.unschedulable(game)
	h.reconcile(2)
	if p := h.pod(); p == nil || p.UID != game.UID {
		t.Fatal("the pending game pod waits while the agent is busy")
	}
	if got := h.condReason(v1alpha1.ConditionGamePodReady); got != "Unschedulable" {
		t.Fatalf("GamePodReady %s", got)
	}

	h.agent.mu.Lock()
	h.agent.busy = nil
	h.agent.mu.Unlock()
	recordedEvents(h)
	h.reconcile(1)
	if h.pod() != nil {
		t.Fatal("the pending game pod with the stale requirement is replaced")
	}
	if !hasEvent(recordedEvents(h), "Replace") {
		t.Fatal("no Replace event")
	}
	if p := h.createGamePod(); requiresAgentNode(p) {
		t.Fatal("the replacement does not require the agent's node")
	}
}

// The agent pod goes while the game runs: its replacement is pinned to the
// game pod's node.
func TestPlacementLostAgentPod(t *testing.T) {
	h := startOn(t, newClass(), "node-a", "node-a")
	h.reconcile(2)
	if err := h.c.Delete(context.Background(), h.agentPod()); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	if got := h.agentPinned(); got != "node-a" {
		t.Fatalf("agent pinned to %q", got)
	}
	if got := pinnedNode(h.createAgentPod(true)); got != "node-a" {
		t.Fatalf("new agent pod pinned to %q", got)
	}
	h.reconcile(1)
	if got := h.condReason(v1alpha1.ConditionAgentRelocating); got != "SameNode" {
		t.Fatalf("AgentRelocating %s", got)
	}
}

// The game pod goes: the agent stays where it is, unpinned, and the next game
// pod prefers its node again.
func TestPlacementLostGamePod(t *testing.T) {
	h := startOn(t, newClass(), "node-a", "node-a")
	h.reconcile(2)
	agent := h.agentPod()
	if err := h.c.Delete(context.Background(), h.pod()); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	if got := h.agentPinned(); got != "" {
		t.Fatalf("agent still pinned to %q", got)
	}
	if p := h.agentPod(); p == nil || p.UID != agent.UID {
		t.Fatal("unpinning does not restart the agent")
	}
	if got := h.condReason(v1alpha1.ConditionAgentRelocating); got != "NoGamePod" {
		t.Fatalf("AgentRelocating %s", got)
	}
	if got := h.replicas(names.StatefulSet(uuid)); got != 1 {
		t.Fatalf("the game pod comes back: replicas %d", got)
	}
	if got := h.gameAffinity(); got != render.GameAffinityPreferred {
		t.Fatalf("game affinity %v", got)
	}
}

// A drain evicts both pods. The StatefulSet controller recreated the agent pod
// from the template still pinned to the drained node; it cannot schedule, and
// is replaced by one free to go anywhere.
func TestPlacementDrain(t *testing.T) {
	h := startOn(t, newClass(), "node-a", "node-a")
	h.reconcile(2)
	if err := h.deletePods(); err != nil {
		t.Fatal(err)
	}
	stale := h.createAgentPod(false)
	h.unschedulable(stale)
	if got := pinnedNode(stale); got != "node-a" {
		t.Fatalf("stale agent pod pinned to %q", got)
	}
	recordedEvents(h)
	h.reconcile(1)
	if h.agentPod() != nil {
		t.Fatal("the pending agent pod pinned to the drained node is replaced")
	}
	if !hasEvent(recordedEvents(h), "Replace") {
		t.Fatal("no Replace event")
	}
	agent := h.createAgentPod(true)
	if got := pinnedNode(agent); got != "" {
		t.Fatalf("replacement pinned to %q", got)
	}
	h.bind(agent, "node-c")
	h.reconcile(1)
	h.bind(h.createGamePod(), "node-c")
	h.reconcile(2)
	if got := h.condReason(v1alpha1.ConditionAgentRelocating); got != "SameNode" {
		t.Fatalf("AgentRelocating %s", got)
	}
}

type activityAgent struct {
	*fakeAgent
	act *agentclient.Activity
	err error
}

func (a activityAgent) Activity(context.Context) (*agentclient.Activity, error) { return a.act, a.err }

func TestAgentWork(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	scheduled := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "agent-1"}, Spec: corev1.PodSpec{NodeName: "node-a"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: render.AgentContainer, ContainerID: "cri://b"}}}}
	live := []v1alpha1.PendingBackup{{UUID: "b-1", Agent: "agent-1/cri://b"}}
	for _, tc := range []struct {
		name   string
		pod    *corev1.Pod
		mutate func(*v1alpha1.GameServer)
		agent  AgentAPI
		want   []string
	}{
		{name: "idle", pod: scheduled, agent: activityAgent{act: &agentclient.Activity{}}},
		{name: "no agent pod", mutate: func(gs *v1alpha1.GameServer) { gs.Status.Backups.Pending = []v1alpha1.PendingBackup{{}} }},
		{name: "unscheduled agent pod", pod: &corev1.Pod{}, mutate: func(gs *v1alpha1.GameServer) { gs.Status.Backups.Pending = []v1alpha1.PendingBackup{{}} }},
		{name: "backup", pod: scheduled, mutate: func(gs *v1alpha1.GameServer) { gs.Status.Backups.Pending = live }, want: []string{"backup"}},
		// A backup does not survive its agent: an entry of an earlier agent pod
		// or agent container run is no work.
		{name: "backup of an earlier agent", pod: scheduled, mutate: func(gs *v1alpha1.GameServer) {
			gs.Status.Backups.Pending = []v1alpha1.PendingBackup{{UUID: "b-1", Agent: "agent-0/cri://b"}, {UUID: "b-2", Agent: "agent-1/cri://a"}, {UUID: "b-3"}}
		}},
		{name: "install", pod: scheduled, mutate: func(gs *v1alpha1.GameServer) { gs.Spec.Install.Generation = 1 }, want: []string{"install"}},
		{name: "failed install", pod: scheduled, mutate: func(gs *v1alpha1.GameServer) {
			gs.Spec.Install.Generation = 1
			gs.Status.Install.Result = v1alpha1.InstallFailed
		}},
		{name: "recent sftp", pod: scheduled, mutate: func(gs *v1alpha1.GameServer) {
			gs.Status.Agent.SftpActiveAt = &metav1.Time{Time: now.Add(-30 * time.Second)}
		}, want: []string{"sftp"}},
		{name: "old sftp", pod: scheduled, mutate: func(gs *v1alpha1.GameServer) {
			gs.Status.Agent.SftpActiveAt = &metav1.Time{Time: now.Add(-2 * time.Minute)}
		}},
		{name: "agent reasons", pod: scheduled, agent: activityAgent{act: &agentclient.Activity{Busy: true, Reasons: []string{"transfer", "pull"}}}, want: []string{"transfer", "pull"}},
		{name: "agent busy", pod: scheduled, agent: activityAgent{act: &agentclient.Activity{Busy: true}}, want: []string{"agent"}},
		{name: "activity error", pod: scheduled, agent: activityAgent{err: errors.New("down")}, want: []string{"unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gs := newGS()
			if tc.mutate != nil {
				tc.mutate(gs)
			}
			s := &scope{ctx: context.Background(), gs: gs, agentPod: tc.pod, agent: tc.agent, now: metav1.NewTime(now)}
			if got := (&GameServerReconciler{}).agentWork(s); !slices.Equal(got, tc.want) {
				t.Fatalf("work %v, want %v", got, tc.want)
			}
		})
	}
}

// A game pod sharing its LoadBalancer address requires the node of the other
// game pods on it, not its agent's: a pending one is not taken for a stale
// agent requirement and replaced on every reconcile.
func TestPlacementSharedIPPendingGamePodStays(t *testing.T) {
	cls := newClass()
	cls.Spec.Exposure = v1alpha1.ExposureSpec{
		Mode:         v1alpha1.ExposureLoadBalancer,
		LoadBalancer: v1alpha1.LoadBalancerSpec{Provider: v1alpha1.LoadBalancerMetalLB},
	}
	h := newHarness(t, runningGS(), cls)
	h.reconcile(2)
	h.bind(h.createAgentPod(true), "node-a")
	h.reconcile(1)
	game := h.createGamePod()
	if requiresAgentNode(game) {
		t.Fatal("the shared-IP term is not an agent requirement")
	}
	h.unschedulable(game)
	recordedEvents(h)
	h.reconcile(2)
	if p := h.pod(); p == nil || p.UID != game.UID {
		t.Fatal("the pending game pod stays")
	}
	if hasEvent(recordedEvents(h), "Replace") {
		t.Fatal("unexpected Replace event")
	}
}

// A running game pod from before the server shared its address is relabelled
// in place, so the shared selector reaches it without a restart; the label
// goes again when the class stops sharing.
func TestSharedIPLabelFollowsTheClass(t *testing.T) {
	cls := newClass()
	cls.Spec.Exposure = v1alpha1.ExposureSpec{Mode: v1alpha1.ExposureLoadBalancer}
	h := startOn(t, cls, "node-a", "node-a")
	h.agent.mu.Lock()
	h.agent.state = v1alpha1.ProcessRunning
	h.agent.mu.Unlock()
	h.reconcile(1)
	game := h.pod()
	if _, ok := game.Labels[v1alpha1.LabelSharedIP]; ok {
		t.Fatal("no shared-IP label without a sharing annotation")
	}
	cls = &v1alpha1.GameServerClass{}
	if err := h.c.Get(context.Background(), client.ObjectKey{Name: "default"}, cls); err != nil {
		t.Fatal(err)
	}
	cls.Spec.Exposure.LoadBalancer.Provider = v1alpha1.LoadBalancerMetalLB
	if err := h.c.Update(context.Background(), cls); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	p := h.pod()
	if p == nil || p.UID != game.UID || p.Labels[v1alpha1.LabelSharedIP] != "192.0.2.10" {
		t.Fatalf("running pod labels %v", p.Labels)
	}
	cls.Spec.Exposure.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
	if err := h.c.Update(context.Background(), cls); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	if p := h.pod(); p == nil || p.UID != game.UID {
		t.Fatal("the running pod stays")
	} else if _, ok := p.Labels[v1alpha1.LabelSharedIP]; ok {
		t.Fatal("the label goes when the address is no longer shared under Local")
	}
}
