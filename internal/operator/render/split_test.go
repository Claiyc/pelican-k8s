package render

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
)

// Where servers share a LoadBalancer address under Local, each protocol gets
// its own Service: OVN-Kubernetes forwards only one protocol of a port that
// one Service carries as both.
func TestExposureIsSplitByProtocolOnASharedAddress(t *testing.T) {
	in := sharedInput(t, uuid, 25565, nil)
	tcp, udp := ExposureService(in), ExposureServiceUDP(in)
	if udp == nil {
		t.Fatal("no UDP Service for a shared address")
	}
	if tcp.Name != names.ExposureService(uuid) || udp.Name != names.ExposureServiceUDP(uuid) {
		t.Fatalf("names %s / %s", tcp.Name, udp.Name)
	}
	for _, c := range []struct {
		svc   *corev1.Service
		proto corev1.Protocol
	}{{tcp, corev1.ProtocolTCP}, {udp, corev1.ProtocolUDP}} {
		if len(c.svc.Spec.Ports) != 1 || c.svc.Spec.Ports[0].Protocol != c.proto || c.svc.Spec.Ports[0].Port != 25565 {
			t.Fatalf("%s ports %+v", c.svc.Name, c.svc.Spec.Ports)
		}
	}
	// MetalLB puts two Services on one address only with equal sharing keys
	// and, under Local, equal selectors.
	if tcp.Annotations[v1alpha1.MetalLBSharingAnnotation] == "" || tcp.Annotations[v1alpha1.MetalLBSharingAnnotation] != udp.Annotations[v1alpha1.MetalLBSharingAnnotation] {
		t.Fatalf("sharing keys %v / %v", tcp.Annotations, udp.Annotations)
	}
	if tcp.Annotations[v1alpha1.MetalLBIPAnnotation] != "203.0.113.10" || udp.Annotations[v1alpha1.MetalLBIPAnnotation] != "203.0.113.10" {
		t.Fatalf("addresses %v / %v", tcp.Annotations, udp.Annotations)
	}
	if labels.Set(tcp.Spec.Selector).String() != labels.Set(udp.Spec.Selector).String() {
		t.Fatalf("selectors %v / %v", tcp.Spec.Selector, udp.Spec.Selector)
	}
	if tcp.Spec.Type != corev1.ServiceTypeLoadBalancer || udp.Spec.Type != corev1.ServiceTypeLoadBalancer ||
		udp.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal || !udp.Spec.PublishNotReadyAddresses {
		t.Fatalf("udp spec %+v", udp.Spec)
	}
	pod := GamePodTemplate(in)
	checkSharedService(t, udp, pod, GamePodTemplate(sharedInput(t, "9f8e7d6c-5b4a-4f3e-8d2c-1b0a9f8e7d6c", 25566, nil)))
}

// Without a shared address one Service carries both protocols: two Services
// could not get the same address.
func TestExposureIsNotSplitWithoutASharedAddress(t *testing.T) {
	cases := map[string]func(*Input){
		"cluster policy": func(i *Input) {
			i.Class.Spec.Exposure.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
		},
		"no sharing annotation": func(i *Input) { i.Class.Spec.Exposure.LoadBalancer = v1alpha1.LoadBalancerSpec{} },
		"node port":             func(i *Input) { i.Class.Spec.Exposure.Mode = v1alpha1.ExposureNodePort },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := sharedInput(t, uuid, 25565, mutate)
			if ExposureServiceUDP(in) != nil {
				t.Fatal("a UDP Service without a shared address")
			}
			svc := ExposureService(in)
			if len(svc.Spec.Ports) != 2 || svc.Spec.Ports[0].Protocol != corev1.ProtocolTCP || svc.Spec.Ports[1].Protocol != corev1.ProtocolUDP {
				t.Fatalf("ports %+v", svc.Spec.Ports)
			}
		})
	}
	if ExposureServiceUDP(testInput(t, func(i *Input) { i.Class.Spec.Exposure.Mode = v1alpha1.ExposureHostPort })) != nil {
		t.Fatal("a UDP Service with HostPort exposure")
	}
}
