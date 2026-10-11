package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
)

// setPolicy changes the default class's externalTrafficPolicy.
func (h *harness) setPolicy(policy corev1.ServiceExternalTrafficPolicy) {
	h.t.Helper()
	cls := &v1alpha1.GameServerClass{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: "default"}, cls); err != nil {
		h.t.Fatal(err)
	}
	cls.Spec.Exposure.ExternalTrafficPolicy = policy
	if err := h.c.Update(context.Background(), cls); err != nil {
		h.t.Fatal(err)
	}
}

// assign plays the load balancer: it gives a Service its address.
func (h *harness) assign(name, ip string) {
	h.t.Helper()
	svc := &corev1.Service{}
	if !h.get(svc, name) {
		h.t.Fatalf("service %s missing", name)
	}
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: ip}}
	if err := h.c.Status().Update(context.Background(), svc); err != nil {
		h.t.Fatal(err)
	}
}

// A server on a shared LoadBalancer address gets a TCP and a UDP Service, and
// is exposed once both have the address.
func TestSplitExposureNeedsBothServices(t *testing.T) {
	h := newHarness(t, newGS(), metalLBClass())
	h.reconcile(2)
	tcp, udp := &corev1.Service{}, &corev1.Service{}
	if !h.get(tcp, names.ExposureService(uuid)) || !h.get(udp, names.ExposureServiceUDP(uuid)) {
		t.Fatal("both Services expected")
	}
	if len(tcp.Spec.Ports) != 1 || tcp.Spec.Ports[0].Protocol != corev1.ProtocolTCP || len(udp.Spec.Ports) != 1 || udp.Spec.Ports[0].Protocol != corev1.ProtocolUDP {
		t.Fatalf("ports %+v / %+v", tcp.Spec.Ports, udp.Spec.Ports)
	}
	if len(udp.OwnerReferences) != 1 || udp.Annotations[annotationManaged] == "" {
		t.Fatalf("udp Service metadata %+v", udp.ObjectMeta)
	}

	h.assign(names.ExposureService(uuid), "192.0.2.10")
	h.reconcile(1)
	if got := h.condReason(v1alpha1.ConditionExposureReady); got != "Pending" {
		t.Fatalf("ExposureReady %s with one half without an address", got)
	}
	h.assign(names.ExposureServiceUDP(uuid), "192.0.2.10")
	h.reconcile(1)
	if got := h.condReason(v1alpha1.ConditionExposureReady); got != "Ready" {
		t.Fatalf("ExposureReady %s", got)
	}
	if eps := h.gs().Status.Endpoints; len(eps) != 1 || eps[0].IP != "192.0.2.10" || eps[0].Port != 30565 {
		t.Fatalf("endpoints %+v", eps)
	}
}

// A Service that carried both protocols becomes the TCP half, and a class
// that stops sharing takes the UDP half away again.
func TestExposureSplitFollowsTheClass(t *testing.T) {
	cls := metalLBClass()
	cls.Spec.Exposure.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
	h := newHarness(t, newGS(), cls)
	h.reconcile(2)
	svc := &corev1.Service{}
	if !h.get(svc, names.ExposureService(uuid)) || len(svc.Spec.Ports) != 2 {
		t.Fatalf("one Service with both protocols expected: %+v", svc.Spec.Ports)
	}
	if h.get(&corev1.Service{}, names.ExposureServiceUDP(uuid)) {
		t.Fatal("a UDP Service under the Cluster policy")
	}

	h.setPolicy(corev1.ServiceExternalTrafficPolicyLocal)
	h.reconcile(1)
	h.get(svc, names.ExposureService(uuid))
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Protocol != corev1.ProtocolTCP {
		t.Fatalf("ports %+v", svc.Spec.Ports)
	}
	if !h.get(&corev1.Service{}, names.ExposureServiceUDP(uuid)) {
		t.Fatal("no UDP Service after the class shares the address")
	}

	h.setPolicy(corev1.ServiceExternalTrafficPolicyCluster)
	h.reconcile(1)
	if h.get(&corev1.Service{}, names.ExposureServiceUDP(uuid)) {
		t.Fatal("the UDP Service stays under the Cluster policy")
	}
	h.get(svc, names.ExposureService(uuid))
	if len(svc.Spec.Ports) != 2 {
		t.Fatalf("ports %+v", svc.Spec.Ports)
	}
}

// settingsWithoutAllocation is the test server's settings without any
// allocation.
func settingsWithoutAllocation() apiextensionsv1.JSON {
	b, _ := json.Marshal(map[string]any{
		"id": 1, "uuid": uuid, "meta": map[string]any{"name": "Test"},
		"build":     map[string]any{"memory_limit": 2048, "cpu_limit": 200, "disk_space": 5120},
		"container": map[string]any{"image": "ghcr.io/pelican-eggs/yolks:java_21"},
		"egg":       map[string]any{"id": "egg-1"},
	})
	return apiextensionsv1.JSON{Raw: b}
}

// failServices makes the harness' reconciler fail one verb on Services whose
// name matches.
func (h *harness) failServices(verb, name string, err error) {
	match := func(obj client.Object) bool {
		_, ok := obj.(*corev1.Service)
		return ok && obj.GetName() == name
	}
	h.r.Client = interceptor.NewClient(h.c.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if verb == "create" && match(obj) {
				return err
			}
			return c.Create(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if verb == "delete" && match(obj) {
				return err
			}
			return c.Delete(ctx, obj, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if verb == "get" && key.Name == name {
				if _, ok := obj.(*corev1.Service); ok {
					return err
				}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
}

// Two Services with different addresses do not expose the server: a port
// would answer TCP on one address and UDP on another.
func TestSplitExposureNeedsTheSameAddress(t *testing.T) {
	h := newHarness(t, newGS(), metalLBClass())
	h.reconcile(2)
	h.assign(names.ExposureService(uuid), "192.0.2.10")
	h.assign(names.ExposureServiceUDP(uuid), "192.0.2.10")
	h.reconcile(1)
	if eps := h.gs().Status.Endpoints; len(eps) != 1 {
		t.Fatalf("endpoints %+v", eps)
	}

	h.assign(names.ExposureServiceUDP(uuid), "192.0.2.11")
	h.reconcile(1)
	c := meta.FindStatusCondition(h.gs().Status.Conditions, v1alpha1.ConditionExposureReady)
	if c == nil || c.Status != "False" || c.Reason != reasonAddressMismatch {
		t.Fatalf("ExposureReady %+v", c)
	}
	if eps := h.gs().Status.Endpoints; len(eps) != 0 {
		t.Fatalf("endpoints are published with two addresses: %+v", eps)
	}

	// A half that loses its address takes the published endpoints with it.
	h.assign(names.ExposureServiceUDP(uuid), "192.0.2.10")
	h.reconcile(1)
	if eps := h.gs().Status.Endpoints; len(eps) != 1 {
		t.Fatalf("endpoints %+v", eps)
	}
	svc := &corev1.Service{}
	h.get(svc, names.ExposureServiceUDP(uuid))
	svc.Status.LoadBalancer.Ingress = nil
	if err := h.c.Status().Update(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	if got := h.condReason(v1alpha1.ConditionExposureReady); got != "Pending" {
		t.Fatalf("ExposureReady %s", got)
	}
	if eps := h.gs().Status.Endpoints; len(eps) != 0 {
		t.Fatalf("endpoints stay while a half has no address: %+v", eps)
	}
}

func TestSameAddresses(t *testing.T) {
	for _, c := range []struct {
		a, b []string
		want bool
	}{
		{[]string{"a"}, []string{"a"}, true},
		{[]string{"a", "b"}, []string{"b", "a"}, true},
		{[]string{"a"}, []string{"b"}, false},
		{[]string{"a"}, []string{"a", "b"}, false},
		{[]string{"a", "a"}, []string{"a", "b"}, false},
	} {
		if got := sameAddresses(c.a, c.b); got != c.want {
			t.Errorf("sameAddresses(%v, %v) = %v", c.a, c.b, got)
		}
	}
}

// A UDP Service that cannot be written is reported like the TCP one.
func TestSplitExposureReportsAFailedUDPService(t *testing.T) {
	boom := errors.New("boom")
	h := newHarness(t, newGS(), metalLBClass())
	h.failServices("create", names.ExposureServiceUDP(uuid), boom)
	h.reconcile(1)
	if _, err := h.reconcileResult(); !errors.Is(err, boom) {
		t.Fatalf("reconcile error %v", err)
	}
	c := meta.FindStatusCondition(h.gs().Status.Conditions, v1alpha1.ConditionExposureReady)
	if c == nil || c.Reason != "ServiceError" {
		t.Fatalf("ExposureReady %+v", c)
	}
}

// A published exposure that is split, and whose UDP Service cannot be
// created, publishes nothing: its ports are not served as UDP meanwhile.
func TestFailedSplitWithdrawsTheEndpoints(t *testing.T) {
	cls := metalLBClass()
	cls.Spec.Exposure.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
	h := newHarness(t, newGS(), cls)
	h.reconcile(2)
	h.assign(names.ExposureService(uuid), "192.0.2.10")
	h.reconcile(1)
	if eps := h.gs().Status.Endpoints; len(eps) != 1 {
		t.Fatalf("endpoints %+v", eps)
	}

	boom := errors.New("boom")
	h.setPolicy(corev1.ServiceExternalTrafficPolicyLocal)
	h.failServices("create", names.ExposureServiceUDP(uuid), boom)
	if _, err := h.reconcileResult(); !errors.Is(err, boom) {
		t.Fatalf("reconcile error %v", err)
	}
	if eps := h.gs().Status.Endpoints; len(eps) != 0 {
		t.Fatalf("endpoints stay while UDP is not served: %+v", eps)
	}
}

// The way back: the UDP Service is gone and the one that should carry both
// protocols again cannot be written.
func TestFailedMergeWithdrawsTheEndpoints(t *testing.T) {
	h := newHarness(t, newGS(), metalLBClass())
	h.reconcile(2)
	h.assign(names.ExposureService(uuid), "192.0.2.10")
	h.assign(names.ExposureServiceUDP(uuid), "192.0.2.10")
	h.reconcile(1)
	if eps := h.gs().Status.Endpoints; len(eps) != 1 {
		t.Fatalf("endpoints %+v", eps)
	}

	boom := errors.New("boom")
	h.setPolicy(corev1.ServiceExternalTrafficPolicyCluster)
	h.r.Client = interceptor.NewClient(h.c.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*corev1.Service); ok && obj.GetName() == names.ExposureService(uuid) {
				return boom
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	if _, err := h.reconcileResult(); !errors.Is(err, boom) {
		t.Fatalf("reconcile error %v", err)
	}
	if h.get(&corev1.Service{}, names.ExposureServiceUDP(uuid)) {
		t.Fatal("the UDP Service stays")
	}
	if eps := h.gs().Status.Endpoints; len(eps) != 0 {
		t.Fatalf("endpoints stay while UDP is not served: %+v", eps)
	}
}

// Nor is a UDP Service that cannot be read or deleted passed over.
func TestSplitExposureServiceErrorsFailTheReconcile(t *testing.T) {
	boom := errors.New("boom")

	// The address of the UDP half cannot be read.
	h := newHarness(t, newGS(), metalLBClass())
	h.reconcile(2)
	h.assign(names.ExposureService(uuid), "192.0.2.10")
	h.failServices("get", names.ExposureServiceUDP(uuid), boom)
	if _, err := h.reconcileResult(); !errors.Is(err, boom) {
		t.Fatalf("reconcile error %v", err)
	}

	// A class that stops sharing cannot delete the UDP half.
	h = newHarness(t, newGS(), metalLBClass())
	h.reconcile(2)
	h.setPolicy(corev1.ServiceExternalTrafficPolicyCluster)
	h.failServices("delete", names.ExposureServiceUDP(uuid), boom)
	if _, err := h.reconcileResult(); !errors.Is(err, boom) {
		t.Fatalf("reconcile error %v", err)
	}

	// A server that lost its allocation cannot delete its Service.
	h = newHarness(t, newGS(), metalLBClass())
	h.reconcile(2)
	h.updateGS(func(gs *v1alpha1.GameServer) {
		gs.Spec.Panel.Settings = settingsWithoutAllocation()
	})
	h.failServices("delete", names.ExposureService(uuid), boom)
	if _, err := h.reconcileResult(); !errors.Is(err, boom) {
		t.Fatalf("reconcile error %v", err)
	}
}

// A load balancer that hands out names instead of addresses exposes the
// server by them.
func TestSplitExposureWithHostnames(t *testing.T) {
	h := newHarness(t, newGS(), metalLBClass())
	h.reconcile(2)
	for _, name := range []string{names.ExposureService(uuid), names.ExposureServiceUDP(uuid)} {
		svc := &corev1.Service{}
		h.get(svc, name)
		svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{Hostname: "lb.example.com"}}
		if err := h.c.Status().Update(context.Background(), svc); err != nil {
			t.Fatal(err)
		}
	}
	h.reconcile(1)
	if eps := h.gs().Status.Endpoints; len(eps) != 1 || eps[0].IP != "lb.example.com" {
		t.Fatalf("endpoints %+v", eps)
	}
}

// Deleting the server removes both Services.
func TestSplitExposureIsDeletedWithTheServer(t *testing.T) {
	h := newHarness(t, newGS(), metalLBClass())
	h.reconcile(2)
	if err := h.c.Delete(context.Background(), h.gs()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := h.reconcileResult(); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{names.ExposureService(uuid), names.ExposureServiceUDP(uuid)} {
		if h.get(&corev1.Service{}, name) {
			t.Fatalf("service %s stays", name)
		}
	}
}
