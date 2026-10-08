package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGatherNode(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	pod := func(name, node string, age time.Duration) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(t0.Add(-age))},
			Spec:       corev1.PodSpec{NodeName: node},
		}
	}
	deleting := pod("d1", "node-c", time.Hour)
	deleting.DeletionTimestamp = &metav1.Time{Time: t0}
	deleting2 := pod("d2", "node-c", time.Hour)
	deleting2.DeletionTimestamp = &metav1.Time{Time: t0}
	cases := []struct {
		name string
		pods []corev1.Pod
		want string
	}{
		{"none", nil, ""},
		{"pending only", []corev1.Pod{pod("p", "", time.Hour)}, ""},
		{"majority", []corev1.Pod{pod("a1", "node-a", time.Hour), pod("b1", "node-b", time.Minute), pod("b2", "node-b", time.Minute)}, "node-b"},
		{"tie to the oldest", []corev1.Pod{pod("a1", "node-a", time.Minute), pod("b1", "node-b", time.Hour)}, "node-b"},
		{"tie by name", []corev1.Pod{pod("b1", "node-b", time.Hour), pod("a1", "node-a", time.Hour)}, "node-a"},
		{"deleting pods do not count", []corev1.Pod{pod("a1", "node-a", time.Minute), deleting, deleting2}, "node-a"},
	}
	for _, c := range cases {
		if got := gatherNode(c.pods); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
