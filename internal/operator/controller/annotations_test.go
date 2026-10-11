package controller

import (
	"context"
	"errors"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
)

// A Secret the operator may not delete does not keep it from driving the
// server: the cleanup is tried again, the reconcile goes on.
func TestUndeletableTLSSecretDoesNotFailTheReconcile(t *testing.T) {
	gs := newGS()
	gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	h := newHarness(t, gs, newClass())
	h.enableTLS()
	h.reconcile(2)
	h.r.PKI = nil
	h.r.Client = interceptor.NewClient(h.c.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				return apierrors.NewForbidden(corev1.Resource("secrets"), obj.GetName(), errors.New("cannot delete"))
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	if !h.get(&corev1.Secret{}, names.TLSSecret(uuid)) {
		t.Fatal("the Secret went although its deletion is refused")
	}
	if calls := h.agent.Calls(); len(calls) == 0 || calls[len(calls)-1] != "power:start" {
		t.Fatalf("the server is not started: %v", calls)
	}
	if gs := h.gs(); gs.Status.Phase == v1alpha1.PhaseError {
		t.Fatalf("phase %s: %s", gs.Status.Phase, h.condReason(v1alpha1.ConditionAgentReady))
	}
}

// A load balancer's own annotations stay on the Service; the operator only
// adds, changes and removes the ones it set.
func TestServiceKeepsForeignAnnotations(t *testing.T) {
	const foreign, mine, stale = "metallb.io/ip-allocated-from-pool", "metallb.io/loadBalancerIPs", "example.com/old"
	stored := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "lb", Annotations: map[string]string{
			foreign: "games", mine: "192.0.2.1", stale: "x", annotationManaged: mine + "," + stale,
		}},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
	}
	h := newHarness(t, newGS(), newClass(), stored)
	s := &scope{ctx: context.Background(), gs: h.gs(), requeue: requeueSlow}
	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "lb", Annotations: map[string]string{mine: "192.0.2.10"}},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Ports: []corev1.ServicePort{{Name: "tcp-1", Port: 1, Protocol: corev1.ProtocolTCP}}},
	}
	if err := h.r.applyService(s, desired); err != nil {
		t.Fatal(err)
	}
	var svc corev1.Service
	h.get(&svc, "lb")
	want := map[string]string{foreign: "games", mine: "192.0.2.10", annotationManaged: mine}
	if len(svc.Annotations) != len(want) {
		t.Fatalf("annotations %v, want %v", svc.Annotations, want)
	}
	for k, v := range want {
		if svc.Annotations[k] != v {
			t.Fatalf("annotations %v, want %v", svc.Annotations, want)
		}
	}

	// A second pass changes nothing: no write, so nothing to react to.
	version := svc.ResourceVersion
	if err := h.r.applyService(s, desired); err != nil {
		t.Fatal(err)
	}
	h.get(&svc, "lb")
	if svc.ResourceVersion != version {
		t.Fatalf("an unchanged Service was written again: %s, was %s", svc.ResourceVersion, version)
	}

	// Without annotations of its own the operator leaves only the others'.
	desired.Annotations = nil
	if err := h.r.applyService(s, desired); err != nil {
		t.Fatal(err)
	}
	h.get(&svc, "lb")
	if len(svc.Annotations) != 1 || svc.Annotations[foreign] != "games" {
		t.Fatalf("annotations %v", svc.Annotations)
	}
	if got := mergeAnnotations(nil, nil); got != nil {
		t.Fatalf("mergeAnnotations(nil, nil) = %v", got)
	}
}

// A Service from before the operator recorded its keys keeps every
// annotation it has: nothing tells whose they are.
func TestServiceWithoutRecordedKeysKeepsItsAnnotations(t *testing.T) {
	got := mergeAnnotations(map[string]string{"a": "1", "b": "2"}, map[string]string{"b": "3"})
	if len(got) != 3 || got["a"] != "1" || got["b"] != "3" || got[annotationManaged] != "b" {
		t.Fatalf("annotations %v", got)
	}
}

// A game pod kubelet has not taken up yet is not announced as pulling its
// image: its volume comes first.
func TestStartNoticeWaitsForTheSandbox(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	h.reconcile(2)
	h.bind(h.createAgentPod(true), "node-a")
	h.reconcile(2)
	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Power = v1alpha1.PowerSpec{Desired: v1alpha1.PowerRunning, Generation: 1}
	})
	h.reconcile(1)
	game := h.createGamePod()
	p := &corev1.Pod{}
	h.get(p, game.Name)
	p.Status = corev1.PodStatus{Phase: corev1.PodPending}
	if err := h.c.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	if n := h.agent.Notices(); !slices.Equal(n, []string{"Creating the game pod..."}) {
		t.Fatalf("notices %v", n)
	}
	h.bind(game, "node-a")
	h.get(p, game.Name)
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReadyToStartContainers, Status: corev1.ConditionFalse}}
	if err := h.c.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	h.get(p, game.Name)
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReadyToStartContainers, Status: corev1.ConditionTrue}}
	if err := h.c.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	want := []string{"Creating the game pod...", "Waiting for the volume to attach and mount...", "Pulling the image and starting the game container..."}
	if n := h.agent.Notices(); !slices.Equal(n, want) {
		t.Fatalf("notices %v", n)
	}
}
