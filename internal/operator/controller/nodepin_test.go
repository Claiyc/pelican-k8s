package controller

import (
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
)

func testNode(name string, addrs ...corev1.NodeAddress) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{Addresses: addrs}}
}

func internalIP(ip string) corev1.NodeAddress {
	return corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ip}
}

func externalIP(ip string) corev1.NodeAddress {
	return corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: ip}
}

func TestAllocationNodes(t *testing.T) {
	a := *testNode("a", internalIP("10.0.0.1"), externalIP("203.0.113.1"), corev1.NodeAddress{Type: corev1.NodeHostName, Address: "198.51.100.9"})
	b := *testNode("b", internalIP("10.0.0.2"), externalIP("203.0.113.2"), internalIP("2001:db8::2"))
	c := *testNode("c", internalIP("10.0.0.3"), externalIP("203.0.113.2"))
	cluster := []corev1.Node{c, b, a}
	for _, tc := range []struct {
		name    string
		nodes   []corev1.Node
		ips     []string
		want    []string
		problem bool
	}{
		{name: "internal IP", nodes: cluster, ips: []string{"10.0.0.1"}, want: []string{"a"}},
		{name: "external IP", nodes: cluster, ips: []string{"203.0.113.1"}, want: []string{"a"}},
		{name: "shared address, sorted", nodes: cluster, ips: []string{"203.0.113.2"}, want: []string{"b", "c"}},
		{name: "every allocation IP", nodes: cluster, ips: []string{"10.0.0.2", "203.0.113.2"}, want: []string{"b"}},
		{name: "IPv6 normalised", nodes: cluster, ips: []string{"2001:DB8:0::2"}, want: []string{"b"}},
		{name: "unspecified and loopback name no node", nodes: cluster, ips: []string{"0.0.0.0", "127.0.0.1"}},
		{name: "hostname addresses do not count", nodes: cluster, ips: []string{"198.51.100.9"}, problem: true},
		{name: "IPs on different nodes", nodes: cluster, ips: []string{"10.0.0.1", "10.0.0.2"}, problem: true},
		{name: "unknown IP", nodes: cluster, ips: []string{"192.0.2.10"}, problem: true},
		{name: "single node needs no pin", nodes: []corev1.Node{a}, ips: []string{"192.0.2.10"}},
		{name: "no nodes", ips: []string{"192.0.2.10"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, problem := allocationNodes(tc.nodes, tc.ips, v1alpha1.ExposureNodePort)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("nodes %v, want %v", got, tc.want)
			}
			if (problem != "") != tc.problem {
				t.Fatalf("problem %q, want problem=%v", problem, tc.problem)
			}
		})
	}
	_, problem := allocationNodes(cluster, []string{"192.0.2.10"}, v1alpha1.ExposureHostPort)
	if !strings.Contains(problem, "192.0.2.10") || strings.Contains(problem, "externalTrafficPolicy") {
		t.Fatalf("HostPort message %q", problem)
	}
}

func TestPinsToAllocationNode(t *testing.T) {
	for _, tc := range []struct {
		ex   v1alpha1.ExposureSpec
		want bool
	}{
		{v1alpha1.ExposureSpec{Mode: v1alpha1.ExposureNodePort}, true},
		{v1alpha1.ExposureSpec{Mode: v1alpha1.ExposureNodePort, ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal}, true},
		{v1alpha1.ExposureSpec{Mode: v1alpha1.ExposureNodePort, ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyCluster}, false},
		{v1alpha1.ExposureSpec{Mode: v1alpha1.ExposureHostPort, ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyCluster}, true},
		{v1alpha1.ExposureSpec{Mode: v1alpha1.ExposureLoadBalancer}, false},
	} {
		if got := pinsToAllocationNode(tc.ex); got != tc.want {
			t.Fatalf("%+v: %v, want %v", tc.ex, got, tc.want)
		}
	}
}

func podNodeNames(t *testing.T, h *harness) []string {
	t.Helper()
	sts := &appsv1.StatefulSet{}
	if !h.get(sts, names.StatefulSet(uuid)) {
		t.Fatal("statefulset missing")
	}
	aff := sts.Spec.Template.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil {
		return nil
	}
	terms := aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchFields) != 1 || terms[0].MatchFields[0].Key != "metadata.name" {
		t.Fatalf("affinity %+v", aff)
	}
	return terms[0].MatchFields[0].Values
}

// NodePort on a multi-node cluster: the pod runs on the node that owns the
// allocation IP (192.0.2.10 in the test settings).
func TestNodePortPinsPodToAllocationNode(t *testing.T) {
	h := newHarness(t, newGS(), newClass(),
		testNode("node-a", internalIP("10.0.0.1")),
		testNode("node-b", internalIP("10.0.0.2"), externalIP("192.0.2.10")))
	h.reconcile(3)
	if got := podNodeNames(t, h); !reflect.DeepEqual(got, []string{"node-b"}) {
		t.Fatalf("pinned to %v, want [node-b]", got)
	}
	c := meta.FindStatusCondition(h.gs().Status.Conditions, v1alpha1.ConditionExposureReady)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != "Ready" {
		t.Fatalf("exposure condition %+v", c)
	}
}

// No node owns the allocation IP: the pod is not pinned and the condition
// names the address, without turning the server into an error.
func TestNodePortAllocationIPOnNoNode(t *testing.T) {
	h := newHarness(t, newGS(), newClass(),
		testNode("node-a", internalIP("10.0.0.1")),
		testNode("node-b", internalIP("10.0.0.2")))
	h.reconcile(3)
	if got := podNodeNames(t, h); got != nil {
		t.Fatalf("pinned to %v without a matching node", got)
	}
	gs := h.gs()
	c := meta.FindStatusCondition(gs.Status.Conditions, v1alpha1.ConditionExposureReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonAllocationIPNotOnNode || !strings.Contains(c.Message, "192.0.2.10") {
		t.Fatalf("exposure condition %+v", c)
	}
	if gs.Status.Phase == v1alpha1.PhaseError {
		t.Fatal("a missing node address must not mark the server as failed")
	}

	// The address appears on a node: the pod is pinned and the condition clears.
	node := &corev1.Node{}
	if err := h.c.Get(t.Context(), clientKey("node-a"), node); err != nil {
		t.Fatal(err)
	}
	node.Status.Addresses = append(node.Status.Addresses, externalIP("192.0.2.10"))
	if err := h.c.Status().Update(t.Context(), node); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	if got := podNodeNames(t, h); !reflect.DeepEqual(got, []string{"node-a"}) {
		t.Fatalf("pinned to %v, want [node-a]", got)
	}
	if c := meta.FindStatusCondition(h.gs().Status.Conditions, v1alpha1.ConditionExposureReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("exposure condition did not recover: %+v", c)
	}
}

// With externalTrafficPolicy Cluster every node forwards the NodePort, so the
// pod is not pinned; HostPort is always pinned.
func TestNodePinFollowsExposure(t *testing.T) {
	nodes := []*corev1.Node{testNode("node-a", internalIP("192.0.2.10")), testNode("node-b", internalIP("10.0.0.2"))}
	cluster := newClass()
	cluster.Spec.Exposure.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
	h := newHarness(t, newGS(), cluster, nodes[0], nodes[1])
	h.reconcile(3)
	if got := podNodeNames(t, h); got != nil {
		t.Fatalf("externalTrafficPolicy Cluster pinned to %v", got)
	}

	hostPort := newClass()
	hostPort.Spec.Exposure.Mode = v1alpha1.ExposureHostPort
	h = newHarness(t, newGS(), hostPort, nodes[0].DeepCopy(), nodes[1].DeepCopy())
	h.reconcile(3)
	if got := podNodeNames(t, h); !reflect.DeepEqual(got, []string{"node-a"}) {
		t.Fatalf("HostPort pinned to %v, want [node-a]", got)
	}
	if c := meta.FindStatusCondition(h.gs().Status.Conditions, v1alpha1.ConditionExposureReady); c == nil || c.Reason != "HostPort" {
		t.Fatalf("HostPort exposure condition %+v", c)
	}
}

func clientKey(name string) client.ObjectKey { return client.ObjectKey{Name: name} }
