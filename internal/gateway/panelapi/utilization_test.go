package panelapi

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/gateway/config"
	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
)

func TestNonNegative(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want uint64
	}{{-5, 0}, {0, 0}, {42, 42}} {
		if got := nonNegative(tc.in); got != tc.want {
			t.Errorf("nonNegative(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// Node capacity and server usage are summed; a negative usage sample counts as
// zero instead of wrapping around to a huge unsigned number.
func TestUtilizationSumsAndClamps(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceMemory:           resource.MustParse("8Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
		}},
	}
	server := func(name string, mem, disk int64) *v1alpha1.GameServer {
		return &v1alpha1.GameServer{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "pelican-servers"},
			Status:     v1alpha1.GameServerStatus{Usage: &v1alpha1.UsageStatus{MemoryBytes: mem, DiskBytes: disk}},
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, server("a", 1000, 2000), server("b", -1, -1)).Build()
	h := &Handler{Cfg: &config.Config{}, Store: store.New(c, "pelican-servers", "default")}

	w := httptest.NewRecorder()
	h.utilization(w, httptest.NewRequest("GET", "/api/system/utilization", nil))
	var out struct {
		MemoryTotal uint64 `json:"memory_total"`
		MemoryUsed  uint64 `json:"memory_used"`
		DiskTotal   uint64 `json:"disk_total"`
		DiskUsed    uint64 `json:"disk_used"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	if out.MemoryTotal != 8<<30 || out.DiskTotal != 100<<30 {
		t.Errorf("totals %d / %d", out.MemoryTotal, out.DiskTotal)
	}
	if out.MemoryUsed != 1000 || out.DiskUsed != 2000 {
		t.Errorf("used %d / %d, want 1000 / 2000", out.MemoryUsed, out.DiskUsed)
	}
}
