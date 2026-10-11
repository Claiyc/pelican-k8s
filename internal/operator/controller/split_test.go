package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

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
