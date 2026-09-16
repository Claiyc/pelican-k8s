package render

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
)

// NetworkPolicy renders the per-server policy (section 12.4), or nil when disabled.
func NetworkPolicy(in *Input) *networkingv1.NetworkPolicy {
	net := in.Class.Spec.Network
	if net.Enabled != nil && !*net.Enabled {
		return nil
	}
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	systemPeer := networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": in.SystemNamespace}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{LabelPartOf: PartOfValue}},
	}
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: in.Meta(names.NetworkPolicy(in.UUID()), "game"),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{v1alpha1.LabelServerUUID: in.UUID(), v1alpha1.LabelComponent: "game"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}

	// Ingress: game ports from anywhere.
	if ports := in.Settings.Ports(); len(ports) > 0 {
		rule := networkingv1.NetworkPolicyIngressRule{From: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}}}
		for _, p := range ports {
			pp := intstr.FromInt32(p)
			rule.Ports = append(rule.Ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &pp}, networkingv1.NetworkPolicyPort{Protocol: &udp, Port: &pp})
		}
		np.Spec.Ingress = append(np.Spec.Ingress, rule)
	}
	// Ingress: agent ports from the gateway and operator, plus node CIDRs for kubelet probes.
	agentPort, sftpPort := intstr.FromInt(AgentPort), intstr.FromInt(SFTPPort)
	agentRule := networkingv1.NetworkPolicyIngressRule{
		From:  []networkingv1.NetworkPolicyPeer{systemPeer},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &agentPort}, {Protocol: &tcp, Port: &sftpPort}},
	}
	np.Spec.Ingress = append(np.Spec.Ingress, agentRule)
	if len(net.NodeCIDRs) > 0 {
		rule := networkingv1.NetworkPolicyIngressRule{Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &agentPort}}}
		for _, c := range net.NodeCIDRs {
			rule.From = append(rule.From, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: c}})
		}
		np.Spec.Ingress = append(np.Spec.Ingress, rule)
	}

	// Egress: DNS anywhere in the cluster.
	dns := intstr.FromInt(53)
	np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
		To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}},
	})
	// Egress: the internet minus cluster and LAN ranges.
	except := append([]string{"169.254.0.0/16"}, net.BlockedEgressCIDRs...)
	np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: except}}},
	})
	// Egress: the gateway (remote API).
	gw := intstr.FromInt(GatewayPort)
	np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
		To:    []networkingv1.NetworkPolicyPeer{systemPeer},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &gw}},
	})
	// Egress: other game servers (proxies) and explicit in-cluster destinations.
	if net.InClusterEgress.GameServers == nil || *net.InClusterEgress.GameServers {
		np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{LabelPartOf: PartOfValue, v1alpha1.LabelComponent: "game"}}}},
		})
	}
	for _, r := range net.InClusterEgress.Additional {
		rule := networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: r.CIDR}}}}
		for _, p := range r.Ports {
			pp := intstr.FromInt32(p)
			rule.Ports = append(rule.Ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &pp}, networkingv1.NetworkPolicyPort{Protocol: &udp, Port: &pp})
		}
		np.Spec.Egress = append(np.Spec.Egress, rule)
	}
	return np
}
