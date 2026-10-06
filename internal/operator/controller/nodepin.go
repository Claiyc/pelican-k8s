package controller

import (
	"fmt"
	"net"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
)

// reasonAllocationIPNotOnNode marks NodePort (externalTrafficPolicy Local) and
// HostPort servers whose allocation IP belongs to no node of a multi-node
// cluster: the pod cannot be placed where that address delivers traffic.
const reasonAllocationIPNotOnNode = "AllocationIPNotOnNode"

// pinsToAllocationNode reports whether the class's exposure only serves the
// allocation IP from the node that owns it: HostPort, and NodePort with
// externalTrafficPolicy Local (kube-proxy then forwards only on nodes running
// the pod).
func pinsToAllocationNode(ex v1alpha1.ExposureSpec) bool {
	switch ex.Mode {
	case v1alpha1.ExposureHostPort:
		return true
	case v1alpha1.ExposureNodePort:
		return ex.ExternalTrafficPolicy == "" || ex.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal
	}
	return false
}

// pinIPs returns the allocation IPs that identify a node, normalised. The
// unspecified and loopback addresses do not name a node and are skipped.
func pinIPs(ips []string) []string {
	var out []string
	for _, raw := range ips {
		ip := net.ParseIP(raw)
		switch {
		case ip == nil:
			out = append(out, raw)
		case ip.IsUnspecified() || ip.IsLoopback():
		default:
			out = append(out, ip.String())
		}
	}
	return out
}

func nodeAddresses(n *corev1.Node) map[string]bool {
	out := map[string]bool{}
	for _, a := range n.Status.Addresses {
		if a.Type != corev1.NodeInternalIP && a.Type != corev1.NodeExternalIP {
			continue
		}
		if ip := net.ParseIP(a.Address); ip != nil {
			out[ip.String()] = true
		} else {
			out[a.Address] = true
		}
	}
	return out
}

// allocationNodes resolves the nodes the game pod must run on so that its
// allocation IP serves traffic: the nodes whose InternalIP or ExternalIP
// covers every allocation IP. It returns the sorted node names, or a message
// for the ExposureReady condition when no node qualifies. A single-node
// cluster needs no pin: every address that reaches the cluster (e.g. a public
// IP NATed to the node) reaches the pod's node.
func allocationNodes(nodes []corev1.Node, ips []string, mode v1alpha1.ExposureMode) ([]string, string) {
	want := pinIPs(ips)
	if len(want) == 0 || len(nodes) <= 1 {
		return nil, ""
	}
	var names []string
	for i := range nodes {
		addrs := nodeAddresses(&nodes[i])
		all := true
		for _, ip := range want {
			if !addrs[ip] {
				all = false
				break
			}
		}
		if all {
			names = append(names, nodes[i].Name)
		}
	}
	if len(names) > 0 {
		sort.Strings(names)
		return names, ""
	}
	fix := "use a node address as the allocation IP, give the node that receives the address that ExternalIP (k3s: --node-external-ip)"
	if mode == v1alpha1.ExposureNodePort {
		fix += ", set the class exposure.externalTrafficPolicy to Cluster"
	}
	return nil, fmt.Sprintf("no node has %s as its InternalIP or ExternalIP, so the pod cannot run where the allocation receives traffic; %s, or use LoadBalancer exposure",
		strings.Join(want, " and "), fix)
}
