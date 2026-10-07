package controller

import (
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/agentclient"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/internal/operator/render"
)

// loadPods reads the agent pod and the game pod. A pod of the 1.x layout, a
// game pod with the agent as a sidecar, is deleted on sight: its replacement
// is the two-pod layout.
func (r *GameServerReconciler) loadPods(s *scope) error {
	uuid := s.in.UUID()
	agent, err := r.getPod(s, names.AgentPod(uuid))
	if err != nil {
		return err
	}
	game, err := r.getPod(s, names.Pod(uuid))
	if err != nil {
		return err
	}
	if game != nil && legacyPod(game) {
		if game.DeletionTimestamp.IsZero() {
			r.event(s, corev1.EventTypeNormal, "LegacyPodDeleted", "deleting pod %s of the single-pod layout", game.Name)
			if err := r.Delete(s.ctx, game); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
		s.requeue = requeueFast
		game = nil
	}
	s.agentPod, s.pod = agent, game
	return nil
}

func (r *GameServerReconciler) getPod(s *scope, name string) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	err := r.Get(s.ctx, types.NamespacedName{Namespace: s.gs.Namespace, Name: name}, pod)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return pod, nil
}

// legacyPod reports a pod of the single-pod layout (1.x): the agent ran as an
// init container sidecar of the game pod.
func legacyPod(pod *corev1.Pod) bool {
	for _, c := range pod.Spec.InitContainers {
		if c.Name == render.AgentContainer {
			return true
		}
	}
	return false
}

// sftpIdleAfter is how long after the last relayed SFTP data the agent counts
// as idle.
const sftpIdleAfter = time.Minute

// place pins the agent pod to the game pod's node while a game pod has one and
// is not going (ARCHITECTURE.md 7.7). Without a game pod the agent may run
// anywhere; the StatefulSet is OnDelete, so the change leaves a running agent
// pod alone.
func (r *GameServerReconciler) place(s *scope) {
	s.in.AgentNode = ""
	if s.pod != nil && s.pod.DeletionTimestamp.IsZero() {
		s.in.AgentNode = s.pod.Spec.NodeName
	}
}

// placeGame picks the game pod's affinity toward the agent pod: required while
// the agent has in-flight work, so the work is not broken by a move, preferred
// or none otherwise as the class says.
func (r *GameServerReconciler) placeGame(s *scope) {
	s.work = r.agentWork(s)
	switch {
	case len(s.work) > 0:
		s.in.GameAffinity = render.GameAffinityRequired
	case s.class.Spec.Scheduling.PrefersAgentNode():
		s.in.GameAffinity = render.GameAffinityPreferred
	default:
		s.in.GameAffinity = render.GameAffinityNone
	}
}

// agentWork lists the in-flight work a move of the agent pod would break. An
// agent pod without a node, or one already going, has none.
func (r *GameServerReconciler) agentWork(s *scope) []string {
	pod, gs := s.agentPod, s.gs
	if pod == nil || pod.Spec.NodeName == "" || !pod.DeletionTimestamp.IsZero() {
		return nil
	}
	var work []string
	if len(gs.Status.Backups.Pending) > 0 {
		work = append(work, "backup")
	}
	if gs.Spec.Install.Generation > gs.Status.Install.ObservedGeneration && gs.Status.Install.Result != v1alpha1.InstallFailed {
		work = append(work, "install")
	}
	if at := gs.Status.Agent.SftpActiveAt; at != nil && s.now.Sub(at.Time) < sftpIdleAfter {
		work = append(work, "sftp")
	}
	if s.agent != nil {
		act, err := s.agent.Activity(s.ctx)
		switch {
		case err != nil:
			// Unknown work is treated as work: a move is only ever delayed.
			work = append(work, "unknown")
		case act.Busy && len(act.Reasons) == 0:
			work = append(work, "agent")
		case act.Busy:
			work = append(work, act.Reasons...)
		}
	}
	return work
}

// relocate moves the agent pod to the game pod's node and replaces pending
// pods whose placement no longer applies (ARCHITECTURE.md 7.7).
func (r *GameServerReconciler) relocate(s *scope) error {
	game := s.pod
	if game != nil && game.DeletionTimestamp.IsZero() && game.Spec.NodeName == "" && requiresAgentNode(game) && s.in.GameAffinity != render.GameAffinityRequired {
		r.event(s, corev1.EventTypeNormal, "Replace", "replacing pending game pod %s: the agent has no in-flight work any more", game.Name)
		if err := r.deletePod(s, game); err != nil {
			return err
		}
		s.pod = nil
	}

	// The game pod's node, unless the game pod went in this reconcile.
	node := ""
	if s.pod != nil && s.pod.DeletionTimestamp.IsZero() {
		node = s.pod.Spec.NodeName
	}
	agent := s.agentPod
	if agent == nil || !agent.DeletionTimestamp.IsZero() {
		if node == "" {
			r.setCondition(s, v1alpha1.ConditionAgentRelocating, metav1.ConditionFalse, "NoGamePod", "")
		}
		return nil
	}
	if agent.Spec.NodeName == "" {
		// A pending agent pod holds no work; one bound to the wrong node, or
		// to none while it should follow the game pod, is replaced.
		if pinnedNode(agent) != node {
			r.event(s, corev1.EventTypeNormal, "Replace", "replacing pending agent pod %s: its node affinity is out of date", agent.Name)
			if err := r.deletePod(s, agent); err != nil {
				return err
			}
			s.agentPod, s.agent = nil, nil
		}
		if node == "" {
			r.setCondition(s, v1alpha1.ConditionAgentRelocating, metav1.ConditionFalse, "NoGamePod", "")
		}
		return nil
	}
	switch {
	case node == "":
		r.setCondition(s, v1alpha1.ConditionAgentRelocating, metav1.ConditionFalse, "NoGamePod", "")
	case agent.Spec.NodeName == node:
		r.setCondition(s, v1alpha1.ConditionAgentRelocating, metav1.ConditionFalse, "SameNode", "")
	case len(s.work) > 0:
		r.setCondition(s, v1alpha1.ConditionAgentRelocating, metav1.ConditionTrue, "WaitingForWork",
			fmt.Sprintf("the agent pod moves from %s to %s once its in-flight work ends (%s)", agent.Spec.NodeName, node, strings.Join(s.work, ", ")))
		s.requeue = requeueFast
	default:
		r.setCondition(s, v1alpha1.ConditionAgentRelocating, metav1.ConditionTrue, "Relocating",
			fmt.Sprintf("moving the agent pod from %s to %s", agent.Spec.NodeName, node))
		r.event(s, corev1.EventTypeNormal, "AgentRelocating", "deleting agent pod %s on %s to move it to the game pod's node %s", agent.Name, agent.Spec.NodeName, node)
		if err := r.deletePod(s, agent); err != nil {
			return err
		}
		s.agentPod, s.agent = nil, nil
	}
	return nil
}

func (r *GameServerReconciler) deletePod(s *scope, pod *corev1.Pod) error {
	if err := r.Delete(s.ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	s.requeue = requeueFast
	return nil
}

// requiresAgentNode reports a pod with a required pod affinity.
func requiresAgentNode(pod *corev1.Pod) bool {
	a := pod.Spec.Affinity
	return a != nil && a.PodAffinity != nil && len(a.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) > 0
}

// pinnedNode returns the node a pod's required node affinity names, if it
// names exactly one by name.
func pinnedNode(pod *corev1.Pod) string {
	a := pod.Spec.Affinity
	if a == nil || a.NodeAffinity == nil || a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return ""
	}
	for _, t := range a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, m := range t.MatchFields {
			if m.Key == "metadata.name" && m.Operator == corev1.NodeSelectorOpIn && len(m.Values) == 1 {
				return m.Values[0]
			}
		}
	}
	return ""
}

// ensureAgentStatefulSet applies the agent StatefulSet: one replica from the
// creation of the GameServer to its deletion, none for a suspended server
// whose class scales it to zero once its game pod is gone.
func (r *GameServerReconciler) ensureAgentStatefulSet(s *scope) error {
	agent := render.AgentStatefulSet(s.in)
	if s.settings.Suspended && s.class.Spec.SuspendScalesToZero && s.pod == nil {
		agent.Spec.Replicas = new(int32)
	}
	if err := r.applyStatefulSet(s, agent); err != nil {
		return err
	}
	s.gs.Status.Agent.TemplateHash = agent.Spec.Template.Annotations[render.AnnotationTemplateHash]
	return nil
}

// ensureGameStatefulSet applies the game StatefulSet (ARCHITECTURE.md 7.6
// "Workloads"). Its template is rendered from the current spec and class on
// every reconcile, so a new game pod always gets the current image, limits,
// ports and placement.
func (r *GameServerReconciler) ensureGameStatefulSet(s *scope) error {
	game := render.GameStatefulSet(s.in)
	replicas, err := r.gameReplicas(s)
	if err != nil {
		return err
	}
	game.Spec.Replicas = &replicas
	if err := r.applyStatefulSet(s, game); err != nil {
		return err
	}
	s.gs.Status.TemplateHash = game.Spec.Template.Annotations[render.AnnotationTemplateHash]
	s.gs.Status.PodImage = s.in.Image
	return nil
}

// gameReplicas is 1 while the server should run, and after that until its
// process is offline: a game pod goes only once nothing runs in it.
func (r *GameServerReconciler) gameReplicas(s *scope) (int32, error) {
	if s.gs.Spec.Power.Desired == v1alpha1.PowerRunning && !s.settings.Suspended {
		return 1, nil
	}
	if s.pod == nil {
		return 0, nil
	}
	if !s.pod.DeletionTimestamp.IsZero() {
		// Already going; scaling down only keeps the StatefulSet from
		// recreating it.
		return 0, nil
	}
	if s.agent == nil {
		// Without the agent nothing tells whether the process still runs.
		return 1, nil
	}
	if !gameContainerStarted(s.pod) {
		return 0, nil
	}
	st, err := r.serverState(s)
	if err != nil {
		return 0, err
	}
	if st.State != v1alpha1.ProcessOffline {
		return 1, nil
	}
	return 0, nil
}

func (r *GameServerReconciler) applyStatefulSet(s *scope, desired *appsv1.StatefulSet) error {
	existing := &appsv1.StatefulSet{}
	err := r.Get(s.ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(s.gs, desired, r.Scheme()); err != nil {
			return err
		}
		if err := client.IgnoreAlreadyExists(r.Create(s.ctx, desired)); err != nil {
			return err
		}
		s.requeue = requeueFast
		return nil
	}
	if err != nil {
		return err
	}
	if render.Hash(existing.Spec.Template) == render.Hash(desired.Spec.Template) && ptrEq(existing.Spec.Replicas, desired.Spec.Replicas) {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec.Template = desired.Spec.Template
	existing.Spec.Replicas = desired.Spec.Replicas
	existing.Labels = desired.Labels
	return r.Patch(s.ctx, existing, patch)
}

func ptrEq(a, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// reconcilePods handles both pods: agent readiness, node loss, in-place
// resize, exit relay and recreates (ARCHITECTURE.md 7.6 "Resources").
func (r *GameServerReconciler) reconcilePods(s *scope) error {
	gs := s.gs
	agentPod, game := s.agentPod, s.pod
	restart := gs.Spec.Power.RestartRequest > gs.Status.Power.ObservedRestartRequest
	agentOutdated := agentPod != nil && agentPod.Annotations[render.AnnotationTemplateHash] != gs.Status.Agent.TemplateHash
	gameOutdated := game != nil && game.Annotations[render.AnnotationTemplateHash] != gs.Status.TemplateHash

	lost := false
	if err := r.reconcileAgentPod(s, &lost); err != nil {
		return err
	}
	r.placeGame(s)
	if err := r.ensureGameStatefulSet(s); err != nil {
		return err
	}
	resizeRecreate := false
	if err := r.reconcileGamePod(s, &lost, &resizeRecreate); err != nil {
		return err
	}
	if lost {
		r.setCondition(s, v1alpha1.ConditionNodeLost, metav1.ConditionTrue, "NodeNotReady", "a pod is terminating on a NotReady node")
	} else {
		r.setCondition(s, v1alpha1.ConditionNodeLost, metav1.ConditionFalse, "NodeReady", "")
	}

	switch {
	case restart:
		r.setCondition(s, v1alpha1.ConditionRecreatePending, metav1.ConditionTrue, "RestartRequested", "restart requested; the pods are recreated once the process is offline")
	case resizeRecreate:
		r.setCondition(s, v1alpha1.ConditionRecreatePending, metav1.ConditionTrue, "ResizeNeedsRecreate", "resources cannot be applied in place; the game pod is recreated once the process is offline")
	case gameOutdated:
		r.setCondition(s, v1alpha1.ConditionRecreatePending, metav1.ConditionTrue, "TemplateChanged", "game pod template changed; the game pod is recreated once the process is offline")
	case agentOutdated:
		r.setCondition(s, v1alpha1.ConditionRecreatePending, metav1.ConditionTrue, "AgentTemplateChanged", "agent pod template changed; the agent pod is recreated once the process is offline")
	default:
		r.setCondition(s, v1alpha1.ConditionRecreatePending, metav1.ConditionFalse, "UpToDate", "")
		return r.relocate(s)
	}
	if err := r.recreate(s, restart, agentOutdated, gameOutdated || resizeRecreate); err != nil {
		return err
	}
	return r.relocate(s)
}

// reconcileAgentPod tracks the agent pod and connects to its agent once ready.
func (r *GameServerReconciler) reconcileAgentPod(s *scope, lost *bool) error {
	pod := s.agentPod
	if pod == nil {
		r.setCondition(s, v1alpha1.ConditionAgentReady, metav1.ConditionFalse, "NoPod", "agent pod does not exist yet")
		s.gs.Status.Agent.PodUID = ""
		s.requeue = requeueFast
		return nil
	}
	if pod.Spec.NodeName != "" {
		s.gs.Status.Agent.Node = pod.Spec.NodeName
	}
	if !pod.DeletionTimestamp.IsZero() {
		r.setCondition(s, v1alpha1.ConditionAgentReady, metav1.ConditionFalse, "Terminating", "agent pod is terminating")
		return r.fence(s, pod, lost)
	}
	ready, ip := agentReady(pod)
	if !ready {
		r.setCondition(s, v1alpha1.ConditionAgentReady, metav1.ConditionFalse, "Starting", "agent container is not ready")
		s.requeue = requeueFast
		return nil
	}
	token, err := r.agentToken(s)
	if err != nil {
		return err
	}
	s.agent = r.newAgent(render.AgentURL(ip), token)
	r.setCondition(s, v1alpha1.ConditionAgentReady, metav1.ConditionTrue, "Ready", "")
	return nil
}

// reconcileGamePod tracks the game pod: GamePodReady, node loss, in-place
// resize and the relay of game container terminations.
func (r *GameServerReconciler) reconcileGamePod(s *scope, lost, resizeRecreate *bool) error {
	pod := s.pod
	if pod == nil {
		if s.gs.Spec.Power.Desired == v1alpha1.PowerRunning && !s.settings.Suspended {
			r.setCondition(s, v1alpha1.ConditionGamePodReady, metav1.ConditionFalse, "NoPod", "game pod does not exist yet")
			s.requeue = requeueFast
		} else {
			r.setCondition(s, v1alpha1.ConditionGamePodReady, metav1.ConditionFalse, "NotRequested", "the server is stopped")
		}
		return nil
	}
	if pod.Spec.NodeName != "" {
		s.gs.Status.Game.Node = pod.Spec.NodeName
	}
	if !pod.DeletionTimestamp.IsZero() {
		r.setCondition(s, v1alpha1.ConditionGamePodReady, metav1.ConditionFalse, "Terminating", "game pod is terminating")
		return r.fence(s, pod, lost)
	}
	// Always run the resize phase: it owns the ResizePending condition and the
	// in-place resize, whether or not a recreate is already pending.
	*resizeRecreate = r.reconcileResize(s, pod)

	switch {
	case unschedulable(pod) != "":
		r.setCondition(s, v1alpha1.ConditionGamePodReady, metav1.ConditionFalse, "Unschedulable", truncate(unschedulable(pod)))
		s.requeue = requeueFast
	case waitingForVolume(pod):
		r.setCondition(s, v1alpha1.ConditionGamePodReady, metav1.ConditionFalse, "WaitingForVolume", "waiting for the volume to attach and mount on "+pod.Spec.NodeName)
		s.requeue = requeueFast
	case s.agent == nil:
		r.setCondition(s, v1alpha1.ConditionGamePodReady, metav1.ConditionFalse, "AgentNotReady", "waiting for the agent")
	case !gameContainerRunning(pod):
		r.setCondition(s, v1alpha1.ConditionGamePodReady, metav1.ConditionFalse, "Starting", "game container is not running yet")
		s.requeue = requeueFast
	default:
		shim, err := s.agent.Shim(s.ctx)
		if err != nil {
			return fmt.Errorf("shim status: %w", err)
		}
		s.shim = shim
		if shim.Attached && shim.PodUID == string(pod.UID) {
			r.setCondition(s, v1alpha1.ConditionGamePodReady, metav1.ConditionTrue, "Ready", "")
		} else {
			r.setCondition(s, v1alpha1.ConditionGamePodReady, metav1.ConditionFalse, "ShimNotAttached", "waiting for the shim to connect to the agent")
			s.requeue = requeueFast
		}
	}
	if s.agent == nil {
		return nil
	}
	return r.relayTermination(s, pod)
}

// fence force-deletes a pod that is terminating on a node that has been
// NotReady for longer than the class allows.
func (r *GameServerReconciler) fence(s *scope, pod *corev1.Pod, lost *bool) error {
	s.requeue = requeueFast
	fd := s.class.Spec.Failover.ForceDeleteAfter
	if fd == nil || fd.Duration <= 0 || pod.Spec.NodeName == "" {
		return nil
	}
	node := &corev1.Node{}
	if err := r.Get(s.ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); err != nil {
		return client.IgnoreNotFound(err)
	}
	if nodeReady(node) {
		return nil
	}
	*lost = true
	if s.now.Sub(pod.DeletionTimestamp.Time) <= fd.Duration {
		return nil
	}
	r.event(s, corev1.EventTypeWarning, "ForceDelete", "force-deleting pod %s stuck on NotReady node %s", pod.Name, pod.Spec.NodeName)
	grace := int64(0)
	if err := r.Delete(s.ctx, pod, &client.DeleteOptions{GracePeriodSeconds: &grace}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// recreate applies a pending recreate. A pod is deleted once the process is
// offline; a pod that cannot hold the process (a game pod whose container
// never started, an agent pod that never became ready) is replaced at once.
// A restart request is acknowledged once both pods are gone.
func (r *GameServerReconciler) recreate(s *scope, restart, agent, game bool) error {
	offline := false
	if s.agent != nil {
		st, err := r.serverState(s)
		if err != nil {
			return err
		}
		offline = st.State == v1alpha1.ProcessOffline
	}
	gameDone, agentDone := s.pod == nil || !s.pod.DeletionTimestamp.IsZero(), s.agentPod == nil || !s.agentPod.DeletionTimestamp.IsZero()
	if (game || restart) && !gameDone && (offline || !gameContainerStarted(s.pod)) {
		if err := r.deleteForRecreate(s, s.pod, "game"); err != nil {
			return err
		}
		s.pod, gameDone = nil, true
	}
	if (agent || restart) && !agentDone {
		ready, _ := agentReady(s.agentPod)
		if offline || (!ready && !restart) {
			if err := r.deleteForRecreate(s, s.agentPod, "agent"); err != nil {
				return err
			}
			s.agentPod, s.agent, agentDone = nil, nil, true
		}
	}
	if restart && gameDone && agentDone {
		s.gs.Status.Power.ObservedRestartRequest = s.gs.Spec.Power.RestartRequest
	}
	return nil
}

func (r *GameServerReconciler) deleteForRecreate(s *scope, pod *corev1.Pod, kind string) error {
	r.event(s, corev1.EventTypeNormal, "Recreate", "deleting the %s pod to apply the new template", kind)
	return r.deletePod(s, pod)
}

// serverState returns the agent's process state, asking once per reconcile.
func (r *GameServerReconciler) serverState(s *scope) (*agentclient.State, error) {
	if s.state != nil {
		return s.state, nil
	}
	st, err := s.agent.GetServer(s.ctx, s.in.UUID())
	if err != nil {
		return nil, err
	}
	s.state = st
	return st, nil
}

// limitsRemoved names the container limits the pod carries and the desired
// resources drop. The API server rejects removing a limit through the resize
// subresource, so such a change is only applied by recreating the pod.
func limitsRemoved(want, have corev1.ResourceRequirements) []string {
	var out []string
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		if _, ok := have.Limits[name]; !ok {
			continue
		}
		if _, ok := want.Limits[name]; !ok {
			out = append(out, string(name))
		}
	}
	return out
}

// reconcileResize applies an in-place resource change to the game container and
// reports whether the change can only land on a fresh pod.
func (r *GameServerReconciler) reconcileResize(s *scope, pod *corev1.Pod) (recreate bool) {
	i := render.GameContainerIndex(&pod.Spec)
	if i < 0 {
		return false
	}
	want := render.GameResources(s.settings.Build, s.class.Spec.Resources)
	have := pod.Spec.Containers[i].Resources
	if resourcesEqual(want, have) {
		if deferred := resizeDeferred(pod); deferred != "" {
			r.setCondition(s, v1alpha1.ConditionResizePending, metav1.ConditionTrue, deferred, "in-place resize is pending; it is applied at the next pod recreate")
		} else {
			r.setCondition(s, v1alpha1.ConditionResizePending, metav1.ConditionFalse, "Applied", "")
		}
		return false
	}
	// Kubernetes refuses to remove a container limit in place, so a
	// Panel change to "unlimited" can only land on a fresh pod.
	// Attempting the resize would fail on every reconcile forever.
	if removed := limitsRemoved(want, have); len(removed) > 0 {
		r.setCondition(s, v1alpha1.ConditionResizePending, metav1.ConditionTrue, "RecreateRequired",
			fmt.Sprintf("removing the %s limit needs a new game pod; it is applied once the process is offline", strings.Join(removed, " and ")))
		return true
	}
	resized := pod.DeepCopy()
	resized.Spec.Containers[i].Resources = want
	if err := r.SubResource("resize").Update(s.ctx, resized); err != nil {
		r.setCondition(s, v1alpha1.ConditionResizePending, metav1.ConditionTrue, "ResizeFailed", truncate(err.Error()))
	} else {
		r.event(s, corev1.EventTypeNormal, "Resized", "applied in-place resource change")
		r.setCondition(s, v1alpha1.ConditionResizePending, metav1.ConditionFalse, "Applied", "")
	}
	return false
}

// relayTermination relays a game container termination (e.g. OOM) to the agent.
func (r *GameServerReconciler) relayTermination(s *scope, pod *corev1.Pod) error {
	key, code, oom := lastTermination(pod)
	if key == "" || s.gs.Status.Agent.RelayedExit == key {
		return nil
	}
	if err := s.agent.ExitState(s.ctx, code, oom); err != nil {
		return fmt.Errorf("relay exit state: %w", err)
	}
	s.gs.Status.Agent.RelayedExit = key
	s.gs.Status.Process.LastExit = &v1alpha1.ExitStatus{Code: code, OOMKilled: oom, At: s.now}
	r.event(s, corev1.EventTypeWarning, "GameContainerTerminated", "game container terminated (code %d, oomKilled=%v); relayed to the agent", code, oom)
	return nil
}

func (r *GameServerReconciler) newAgent(base, token string) AgentAPI {
	if r.NewAgent != nil {
		return r.NewAgent(base, token)
	}
	return agentclient.New(base, token)
}

func (r *GameServerReconciler) agentToken(s *scope) (string, error) {
	sec := &corev1.Secret{}
	if err := r.Get(s.ctx, types.NamespacedName{Namespace: s.gs.Namespace, Name: names.AgentSecret(s.in.UUID())}, sec); err != nil {
		return "", err
	}
	return string(sec.Data["token"]), nil
}

// agentReady reports whether the agent container of the agent pod is ready,
// and the pod address.
func agentReady(pod *corev1.Pod) (bool, string) {
	if pod.Status.PodIP == "" || pod.Status.Phase != corev1.PodRunning {
		return false, ""
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == render.AgentContainer {
			started := cs.Started != nil && *cs.Started
			return started && cs.Ready, pod.Status.PodIP
		}
	}
	return false, ""
}

// unschedulable returns the scheduler's message for a pod it cannot place.
func unschedulable(pod *corev1.Pod) string {
	if pod.Spec.NodeName != "" {
		return ""
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			if c.Message != "" {
				return c.Message
			}
			return c.Reason
		}
	}
	return ""
}

// waitingForVolume reports a scheduled pod whose sandbox kubelet has not
// created yet: kubelet attaches and mounts the volumes first, so this is a
// volume still attached to another node, or slow to mount.
func waitingForVolume(pod *corev1.Pod) bool {
	if pod.Spec.NodeName == "" {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReadyToStartContainers {
			return c.Status == corev1.ConditionFalse
		}
	}
	return false
}

// gameContainerRunning reports whether the game container runs now.
func gameContainerRunning(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == render.GameContainer {
			return cs.State.Running != nil
		}
	}
	return false
}
