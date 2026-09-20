package panelapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/gateway/config"
	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
)

type fakePools []string

func (f fakePools) Addresses(context.Context) []string { return f }

func ipsHandler(t *testing.T, cfgIPs []string, classIPs []string, pools AddressPools) *Handler {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	cls := &v1alpha1.GameServerClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec:       v1alpha1.GameServerClassSpec{Exposure: v1alpha1.ExposureSpec{ExternalIPs: classIPs}},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "198.51.100.1"}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cls, node).Build()
	return &Handler{Cfg: &config.Config{ExternalIPs: cfgIPs}, Store: store.New(c, "pelican-servers", "default"), Pools: pools}
}

func ips(t *testing.T, h *Handler) []string {
	t.Helper()
	w := httptest.NewRecorder()
	h.systemIPs(w, httptest.NewRequest("GET", "/api/system/ips", nil))
	var out struct {
		IPAddresses []string `json:"ip_addresses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	return out.IPAddresses
}

// Discovered pool addresses stand in for the node addresses, which are what a
// LoadBalancer deployment must not offer: an allocation on a node IP produces a
// Service that never gets an ingress address.
func TestSystemIPsPrefersPoolsOverNodes(t *testing.T) {
	h := ipsHandler(t, nil, nil, fakePools{"192.0.2.10", "192.0.2.11"})
	if got := ips(t, h); strings.Join(got, ",") != "192.0.2.10,192.0.2.11" {
		t.Fatalf("got %v", got)
	}
}

// Explicit configuration is an operator's decision and outranks discovery.
func TestSystemIPsConfigWinsOverPools(t *testing.T) {
	h := ipsHandler(t, []string{"203.0.113.5"}, nil, fakePools{"192.0.2.10"})
	if got := ips(t, h); strings.Join(got, ",") != "203.0.113.5" {
		t.Fatalf("got %v", got)
	}
}

func TestSystemIPsClassWinsOverPools(t *testing.T) {
	h := ipsHandler(t, nil, []string{"203.0.113.6"}, fakePools{"192.0.2.10"})
	if got := ips(t, h); strings.Join(got, ",") != "203.0.113.6" {
		t.Fatalf("got %v", got)
	}
}

// Discovery is a convenience: an empty result falls through to node addresses.
func TestSystemIPsFallsBackToNodesWhenPoolsAreEmpty(t *testing.T) {
	h := ipsHandler(t, nil, nil, fakePools{})
	if got := ips(t, h); strings.Join(got, ",") != "198.51.100.1" {
		t.Fatalf("got %v", got)
	}
}

func TestSystemIPsWithoutPoolsIsUnchanged(t *testing.T) {
	h := ipsHandler(t, nil, nil, nil)
	if got := ips(t, h); strings.Join(got, ",") != "198.51.100.1" {
		t.Fatalf("got %v", got)
	}
}
