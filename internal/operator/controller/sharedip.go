package controller

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/render"
)

// reasonSharedIPNodeMismatch marks a game pod that runs apart from the other
// game pods sharing its LoadBalancer address (ARCHITECTURE.md 9.3).
const reasonSharedIPNodeMismatch = "SharedIPNodeMismatch"

// placeSharedIP places a game pod that shares its LoadBalancer address under
// externalTrafficPolicy Local (ARCHITECTURE.md 9.3). The address is announced
// from one node and only pods there receive traffic, so all game pods on it
// gather on one node. It sets In.SharedIPNode, the node of the other pods,
// which a new game pod is pinned to, and s.sharedIPElsewhere when the
// server's running game pod is off the node they gather on (relabelled in
// place, or placed before the others).
func (r *GameServerReconciler) placeSharedIP(s *scope) error {
	s.in.SharedIPNode, s.sharedIPElsewhere = "", ""
	shared := render.SharedIP(s.in)
	if shared == "" {
		return nil
	}
	pods := &corev1.PodList{}
	if err := r.List(s.ctx, pods, client.InNamespace(s.gs.Namespace),
		client.MatchingLabels{v1alpha1.LabelSharedIP: shared, v1alpha1.LabelComponent: render.ComponentGame}); err != nil {
		return err
	}
	// Pods on a NotReady node do not count: the address moves to a node with
	// ready endpoints, and the pods there must not follow the lost ones.
	ready := map[string]bool{}
	var all, others []corev1.Pod
	for _, p := range pods.Items {
		n := p.Spec.NodeName
		if n == "" {
			continue
		}
		if _, ok := ready[n]; !ok {
			node := &corev1.Node{}
			err := r.Get(s.ctx, types.NamespacedName{Name: n}, node)
			if err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			ready[n] = err == nil && nodeReady(node)
		}
		if !ready[n] {
			continue
		}
		all = append(all, p)
		if p.Labels[v1alpha1.LabelServerUUID] != s.in.UUID() {
			others = append(others, p)
		}
	}
	s.in.SharedIPNode = gatherNode(others)
	if pod := s.pod; pod != nil && ready[pod.Spec.NodeName] && pod.DeletionTimestamp.IsZero() {
		if n := gatherNode(all); n != "" && n != pod.Spec.NodeName {
			s.sharedIPElsewhere = n
		}
	}
	return nil
}

// gatherNode picks the node that runs most of the given pods, a tie going to
// the node of the oldest pod. Every server on an address computes the same
// node, so only the pods elsewhere move.
func gatherNode(pods []corev1.Pod) string {
	count := map[string]int{}
	oldest := map[string]*corev1.Pod{}
	for i := range pods {
		p := &pods[i]
		n := p.Spec.NodeName
		if n == "" || !p.DeletionTimestamp.IsZero() {
			continue
		}
		count[n]++
		if o := oldest[n]; o == nil || olderPod(p, o) {
			oldest[n] = p
		}
	}
	best := ""
	for n, c := range count {
		if best == "" || c > count[best] || c == count[best] && olderPod(oldest[n], oldest[best]) {
			best = n
		}
	}
	return best
}

func olderPod(a, b *corev1.Pod) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}

// stopToMove stops a running process so the game pod can be recreated on the
// node of the other pods sharing its address; the fresh-pod rule starts it
// again there. A pod off that node receives no traffic, so the restart costs
// the players little.
func (r *GameServerReconciler) stopToMove(s *scope, node string) error {
	if s.agent == nil {
		return nil
	}
	st, err := r.serverState(s)
	if err != nil {
		return err
	}
	s.requeue = requeueFast
	if st.State != v1alpha1.ProcessRunning && st.State != v1alpha1.ProcessStarting {
		return nil
	}
	r.event(s, corev1.EventTypeNormal, "SharedIPMove", "stopping the server to move its game pod to %s, where the other servers on its address run", node)
	if err := r.power(s, "stop"); err != nil {
		return fmt.Errorf("stop to move: %w", err)
	}
	return nil
}
