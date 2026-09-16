package render

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
)

// ExposureService renders the player-facing Service, or nil when the server
// has no allocation or uses HostPort exposure.
func ExposureService(in *Input) *corev1.Service {
	if !in.Settings.HasAllocation() {
		return nil
	}
	ex := in.Class.Spec.Exposure
	if ex.Mode == v1alpha1.ExposureHostPort {
		return nil
	}
	svc := &corev1.Service{
		ObjectMeta: in.Meta(names.ExposureService(in.UUID()), "exposure"),
		Spec: corev1.ServiceSpec{
			Selector:                 map[string]string{v1alpha1.LabelServerUUID: in.UUID(), v1alpha1.LabelComponent: "game"},
			PublishNotReadyAddresses: true,
		},
	}
	svc.Annotations = map[string]string{}
	for k, v := range ex.LoadBalancer.Annotations {
		svc.Annotations[k] = v
	}
	switch ex.Mode {
	case v1alpha1.ExposureNodePort:
		svc.Spec.Type = corev1.ServiceTypeNodePort
	default:
		svc.Spec.Type = corev1.ServiceTypeLoadBalancer
		if ex.LoadBalancer.IPAnnotation != "" {
			svc.Annotations[ex.LoadBalancer.IPAnnotation] = in.Settings.Allocations.Default.IP
		}
		if ex.LoadBalancer.SharingAnnotation != "" {
			svc.Annotations[ex.LoadBalancer.SharingAnnotation] = "pelican-" + in.Settings.Allocations.Default.IP
		}
	}
	if len(svc.Annotations) == 0 {
		svc.Annotations = nil
	}
	policy := ex.ExternalTrafficPolicy
	if policy == "" {
		policy = corev1.ServiceExternalTrafficPolicyLocal
	}
	svc.Spec.ExternalTrafficPolicy = policy
	for _, p := range in.Settings.Ports() {
		for _, proto := range []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP} {
			sp := corev1.ServicePort{Name: portName(p, proto), Port: p, TargetPort: intstr.FromInt32(p), Protocol: proto}
			if ex.Mode == v1alpha1.ExposureNodePort {
				sp.NodePort = p
			}
			svc.Spec.Ports = append(svc.Spec.Ports, sp)
		}
	}
	return svc
}

func portName(p int32, proto corev1.Protocol) string {
	if proto == corev1.ProtocolUDP {
		return fmt.Sprintf("udp-%d", p)
	}
	return fmt.Sprintf("tcp-%d", p)
}

// AgentService renders the headless Service for the agent's HTTP and SFTP ports.
func AgentService(in *Input) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: in.Meta(names.AgentService(in.UUID()), "agent"),
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			Selector:                 map[string]string{v1alpha1.LabelServerUUID: in.UUID(), v1alpha1.LabelComponent: "game"},
			PublishNotReadyAddresses: true,
			Ports: []corev1.ServicePort{
				{Name: "agent", Port: AgentPort, TargetPort: intstr.FromInt(AgentPort), Protocol: corev1.ProtocolTCP},
				{Name: "sftp", Port: SFTPPort, TargetPort: intstr.FromInt(SFTPPort), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}
