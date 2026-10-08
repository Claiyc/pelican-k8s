package render

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
)

// NetworkPolicy renders the game pod's policy (section 12.4); an open one
// when the class disables policies.
func NetworkPolicy(in *Input) *networkingv1.NetworkPolicy {
	net := in.Class.Spec.Network
	if !policiesEnabled(net) {
		return openPolicy(in, names.NetworkPolicy(in.UUID()), ComponentGame)
	}
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: in.Meta(names.NetworkPolicy(in.UUID()), ComponentGame),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: ownPod(in, ComponentGame),
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

	np.Spec.Egress = append(np.Spec.Egress, dnsEgress(), internetEgress(net))
	// Egress: the shim connection to its own agent.
	shim := intstr.FromInt(ShimPort)
	agent := ownPod(in, ComponentAgent)
	np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
		To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &agent}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &shim}},
	})
	// Egress: other game servers (proxies) and explicit in-cluster destinations.
	if net.InClusterEgress.GameServers == nil || *net.InClusterEgress.GameServers {
		np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{LabelPartOf: PartOfValue, v1alpha1.LabelComponent: ComponentGame}}}},
		})
	}
	np.Spec.Egress = append(np.Spec.Egress, additionalEgress(net)...)
	return np
}

// AgentNetworkPolicy renders the agent pod's policy (section 12.4); an open
// one when the class disables policies.
func AgentNetworkPolicy(in *Input) *networkingv1.NetworkPolicy {
	net := in.Class.Spec.Network
	if !policiesEnabled(net) {
		return openPolicy(in, names.AgentNetworkPolicy(in.UUID()), ComponentAgent)
	}
	tcp := corev1.ProtocolTCP
	systemPeer := networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": in.SystemNamespace}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{LabelPartOf: PartOfValue}},
	}
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: in.Meta(names.AgentNetworkPolicy(in.UUID()), ComponentAgent),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: ownPod(in, ComponentAgent),
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
	// Ingress: agent ports from the gateway and operator, plus node CIDRs for kubelet probes.
	agentPort, sftpPort, shimPort := intstr.FromInt(AgentPort), intstr.FromInt(SFTPPort), intstr.FromInt(ShimPort)
	np.Spec.Ingress = append(np.Spec.Ingress, networkingv1.NetworkPolicyIngressRule{
		From:  []networkingv1.NetworkPolicyPeer{systemPeer},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &agentPort}, {Protocol: &tcp, Port: &sftpPort}},
	})
	if len(net.NodeCIDRs) > 0 {
		rule := networkingv1.NetworkPolicyIngressRule{Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &agentPort}}}
		for _, c := range net.NodeCIDRs {
			rule.From = append(rule.From, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: c}})
		}
		np.Spec.Ingress = append(np.Spec.Ingress, rule)
	}
	// Ingress: the shim of its own game pod.
	game := ownPod(in, ComponentGame)
	np.Spec.Ingress = append(np.Spec.Ingress, networkingv1.NetworkPolicyIngressRule{
		From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &game}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &shimPort}},
	})

	// Egress: DNS, the internet (remote pulls, S3 uploads) and the gateway's remote API.
	np.Spec.Egress = append(np.Spec.Egress, dnsEgress(), internetEgress(net))
	gw := intstr.FromInt(GatewayPort)
	np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
		To:    []networkingv1.NetworkPolicyPeer{systemPeer},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &gw}},
	})
	np.Spec.Egress = append(np.Spec.Egress, additionalEgress(net)...)
	return np
}

// ownPod selects this server's pod of one component.
func ownPod(in *Input, component string) metav1.LabelSelector {
	return metav1.LabelSelector{MatchLabels: map[string]string{v1alpha1.LabelServerUUID: in.UUID(), v1alpha1.LabelComponent: component}}
}

// dnsEgress allows DNS anywhere in the cluster. Policies match the pod port
// after service DNAT, so both 53 and the 5353 OpenShift/CoreDNS listens on are needed.
func dnsEgress() networkingv1.NetworkPolicyEgressRule {
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	dns, dnsAlt := intstr.FromInt(53), intstr.FromInt(5353)
	return networkingv1.NetworkPolicyEgressRule{
		To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}, {Protocol: &udp, Port: &dnsAlt}, {Protocol: &tcp, Port: &dnsAlt}},
	}
}

// internetEgress allows the internet minus link-local and the blocked (by
// default the private and shared) ranges. The DNS, gateway, shim and
// in-cluster rules are separate allow rules, so they still apply to
// destinations in those ranges.
func internetEgress(net v1alpha1.NetworkSpec) networkingv1.NetworkPolicyEgressRule {
	except := append([]string{v1alpha1.LinkLocalCIDR}, BlockedEgressCIDRs(net)...)
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: except}}},
	}
}

// additionalEgress renders the class's explicit in-cluster destinations.
func additionalEgress(net v1alpha1.NetworkSpec) []networkingv1.NetworkPolicyEgressRule {
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	var out []networkingv1.NetworkPolicyEgressRule
	for _, r := range net.InClusterEgress.Additional {
		rule := networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: r.CIDR}}}}
		for _, p := range r.Ports {
			pp := intstr.FromInt32(p)
			rule.Ports = append(rule.Ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &pp}, networkingv1.NetworkPolicyPort{Protocol: &udp, Port: &pp})
		}
		out = append(out, rule)
	}
	return out
}

// BlockedEgressCIDRs returns the class's blocked egress ranges, or the default
// private and shared ranges when the field is unset. An explicit empty list
// blocks only link-local.
func BlockedEgressCIDRs(net v1alpha1.NetworkSpec) []string {
	if net.BlockedEgressCIDRs == nil {
		return v1alpha1.DefaultBlockedEgressCIDRs
	}
	return net.BlockedEgressCIDRs
}

func policiesEnabled(net v1alpha1.NetworkSpec) bool {
	return net.Enabled == nil || *net.Enabled
}

// openPolicy admits all traffic to and from one of the server's pods. The
// chart's default-deny selects every pod in the servers namespace, so a class
// that turns its policies off needs this to leave its pods unrestricted.
func openPolicy(in *Input, name, component string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: in.Meta(name, component),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: ownPod(in, component),
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{{}},
			Egress:      []networkingv1.NetworkPolicyEgressRule{{}},
		},
	}
}
