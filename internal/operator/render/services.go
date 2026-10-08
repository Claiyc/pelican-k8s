package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strings"

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
			Selector:                 map[string]string{v1alpha1.LabelServerUUID: in.UUID(), v1alpha1.LabelComponent: ComponentGame},
			PublishNotReadyAddresses: true,
		},
	}
	shared := SharedIP(in)
	if shared != "" {
		// MetalLB lets Local Services share an address only when their
		// selectors are identical, so every Service on the address selects
		// all its game pods. Named target ports keep each port on the one
		// pod that declares it.
		svc.Spec.Selector = map[string]string{v1alpha1.LabelSharedIP: shared, v1alpha1.LabelComponent: ComponentGame}
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
		if k := ex.LoadBalancer.IPKey(); k != "" {
			svc.Annotations[k] = in.Settings.Allocations.Default.IP
		}
		if k := ex.LoadBalancer.SharingKey(); k != "" {
			svc.Annotations[k] = "pelican-" + in.Settings.Allocations.Default.IP
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
			if shared != "" {
				sp.TargetPort = intstr.FromString(containerPortName(in, p, proto))
			}
			if ex.Mode == v1alpha1.ExposureNodePort {
				sp.NodePort = p
			}
			svc.Spec.Ports = append(svc.Spec.Ports, sp)
		}
	}
	return svc
}

// SharedIP returns the LabelSharedIP value of a server whose LoadBalancer
// Service shares its allocation IP (a sharing annotation is set) under
// externalTrafficPolicy Local, or "" otherwise. Such an address is announced
// from one node and Local delivers only to pods on that node, so all game
// pods on the address run together on one node.
func SharedIP(in *Input) string {
	ex := in.Class.Spec.Exposure
	if !in.Settings.HasAllocation() || ex.Mode == v1alpha1.ExposureNodePort || ex.Mode == v1alpha1.ExposureHostPort {
		return ""
	}
	if ex.LoadBalancer.SharingKey() == "" || ex.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyCluster {
		return ""
	}
	return ipLabelValue(in.Settings.Allocations.Default.IP)
}

// ipLabelValue turns an address into a label value: IPv4 as is, IPv6 (whose
// colons a label value cannot hold) as hex, anything else hashed.
func ipLabelValue(raw string) string {
	ip := net.ParseIP(raw)
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	if ip != nil {
		return hex.EncodeToString(ip)
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:16])
}

// DeclaresGamePort reports whether the game container of pod declares one of
// the named ports a shared Service targets (or the server has no port). A game
// pod created before the names existed declares none and receives no traffic
// through the shared Service until it is recreated.
func DeclaresGamePort(in *Input, pod *corev1.Pod) bool {
	ports := in.Settings.Ports()
	if len(ports) == 0 {
		return true
	}
	for _, c := range pod.Spec.Containers {
		if c.Name != GameContainer {
			continue
		}
		for _, cp := range c.Ports {
			for _, p := range ports {
				if cp.Name == containerPortName(in, p, cp.Protocol) {
					return true
				}
			}
		}
	}
	return false
}

// containerPortName names a game container port after the port and the
// server, so a shared Service's named target port never resolves on another
// server's pod that still declares a port it has given up (at most 15
// characters: t25565-1a2b3c4d). The first 8 characters of a server UUID are
// its uuid_short, which the Panel keeps unique among its servers, so the
// names of two servers never collide.
func containerPortName(in *Input, p int32, proto corev1.Protocol) string {
	id := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, strings.ToLower(in.UUID()))
	if len(id) > 8 {
		id = id[:8]
	}
	prefix := "t"
	if proto == corev1.ProtocolUDP {
		prefix = "u"
	}
	return fmt.Sprintf("%s%d-%s", prefix, p, id)
}

func portName(p int32, proto corev1.Protocol) string {
	if proto == corev1.ProtocolUDP {
		return fmt.Sprintf("udp-%d", p)
	}
	return fmt.Sprintf("tcp-%d", p)
}

// AgentService renders the headless Service of the agent pod: HTTP, SFTP and
// the shim port the game pod dials.
func AgentService(in *Input) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: in.Meta(names.AgentService(in.UUID()), "agent"),
		Spec: corev1.ServiceSpec{
			// The API server's default, set anyway: the reconciler compares
			// the type against the stored Service, which always carries one.
			Type:                     corev1.ServiceTypeClusterIP,
			ClusterIP:                corev1.ClusterIPNone,
			Selector:                 map[string]string{v1alpha1.LabelServerUUID: in.UUID(), v1alpha1.LabelComponent: ComponentAgent},
			PublishNotReadyAddresses: true,
			Ports: []corev1.ServicePort{
				{Name: "agent", Port: AgentPort, TargetPort: intstr.FromInt(AgentPort), Protocol: corev1.ProtocolTCP},
				{Name: "sftp", Port: SFTPPort, TargetPort: intstr.FromInt(SFTPPort), Protocol: corev1.ProtocolTCP},
				{Name: "shim", Port: ShimPort, TargetPort: intstr.FromInt(ShimPort), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}
