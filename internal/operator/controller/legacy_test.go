package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/internal/operator/render"
)

// legacyServer creates a server of the 1.x layout: its StatefulSet and its
// pod, which holds a finalizer so that it stays terminating until released.
func legacyServer(t *testing.T, desired v1alpha1.PowerState, process string) *harness {
	t.Helper()
	gs := newGS()
	gs.Spec.Power.Desired = desired
	gs.Status.Process.State = process
	labels := map[string]string{v1alpha1.LabelServerUUID: uuid, v1alpha1.LabelComponent: render.ComponentGame}
	spec := corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "prepare"}, {Name: render.AgentContainer}},
		Containers:     []corev1.Container{{Name: render.GameContainer}},
	}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: names.StatefulSet(uuid), Namespace: ns},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: names.AgentService(uuid),
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			Template:    corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: spec},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: names.Pod(uuid), Namespace: ns, Labels: labels, Finalizers: []string{"test/hold"}},
		Spec:       spec,
	}
	return newHarness(t, gs, newClass(), sts, pod)
}

// The 1.x pod is deleted without its StatefulSet recreating it, and nothing
// of the 2.0 layout appears next to it while it terminates.
func TestLegacyPodIsReplaced(t *testing.T) {
	h := legacyServer(t, v1alpha1.PowerRunning, v1alpha1.ProcessRunning)
	h.reconcile(3)
	pod := h.pod()
	if pod == nil || pod.DeletionTimestamp.IsZero() {
		t.Fatalf("the legacy pod must be terminating, got %+v", pod)
	}
	if !hasEvent(recordedEvents(h), "LegacyPodDeleted") {
		t.Fatal("LegacyPodDeleted event expected")
	}
	if h.get(&appsv1.StatefulSet{}, names.StatefulSet(uuid)) {
		t.Fatal("the 1.x StatefulSet must be deleted")
	}
	if h.get(&appsv1.StatefulSet{}, names.AgentStatefulSet(uuid)) {
		t.Fatal("no agent pod may start while the legacy pod terminates")
	}
	if h.get(&networkingv1.NetworkPolicy{}, names.ExposureService(uuid)) {
		t.Fatal("the legacy pod's NetworkPolicy must stay as it is while it terminates")
	}

	pod.Finalizers = nil
	if err := h.c.Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	h.reconcile(2)
	if !h.get(&networkingv1.NetworkPolicy{}, names.ExposureService(uuid)) {
		t.Fatal("the game pod's NetworkPolicy must be written once the legacy pod is gone")
	}
	if !h.get(&appsv1.StatefulSet{}, names.AgentStatefulSet(uuid)) {
		t.Fatal("the agent StatefulSet must be created once the legacy pod is gone")
	}
	game := &appsv1.StatefulSet{}
	if !h.get(game, names.StatefulSet(uuid)) {
		t.Fatal("the game StatefulSet must be created once the legacy pod is gone")
	}
	if game.Spec.ServiceName != names.ExposureService(uuid) {
		t.Fatalf("game StatefulSet serviceName %q, want %q", game.Spec.ServiceName, names.ExposureService(uuid))
	}
}

// A 1.x StatefulSet goes also when its pod is already terminating or gone, as
// for a server 1.x had stopped, so the 2.0 StatefulSet gets its serviceName.
func TestLegacyStatefulSetWithoutLivePod(t *testing.T) {
	for _, name := range []string{"terminating", "gone"} {
		t.Run(name, func(t *testing.T) {
			h := legacyServer(t, v1alpha1.PowerStopped, v1alpha1.ProcessOffline)
			pod := h.pod()
			if name == "gone" {
				pod.Finalizers = nil
				if err := h.c.Update(context.Background(), pod); err != nil {
					t.Fatal(err)
				}
			}
			if err := h.c.Delete(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
			h.reconcile(3)
			if sts := (&appsv1.StatefulSet{}); h.get(sts, names.StatefulSet(uuid)) && sts.Spec.ServiceName == names.AgentService(uuid) {
				t.Fatal("the 1.x StatefulSet must be deleted")
			}
			if name == "terminating" {
				if h.get(&appsv1.StatefulSet{}, names.AgentStatefulSet(uuid)) {
					t.Fatal("no agent pod may start while the legacy pod terminates")
				}
				pod = h.pod()
				pod.Finalizers = nil
				if err := h.c.Update(context.Background(), pod); err != nil {
					t.Fatal(err)
				}
			}
			h.reconcile(2)
			game := &appsv1.StatefulSet{}
			if !h.get(game, names.StatefulSet(uuid)) || game.Spec.ServiceName != names.ExposureService(uuid) {
				t.Fatalf("game StatefulSet with serviceName %q expected, got %q", names.ExposureService(uuid), game.Spec.ServiceName)
			}
			if !h.get(&appsv1.StatefulSet{}, names.AgentStatefulSet(uuid)) {
				t.Fatal("the agent StatefulSet must be created")
			}
		})
	}
}

// 1.x set desired Stopped on every Panel restart and left desired Running
// after a crash; the upgrade keeps each server doing what its process did.
func TestLegacyPowerFollowsTheProcess(t *testing.T) {
	cases := []struct {
		desired v1alpha1.PowerState
		process string
		want    v1alpha1.PowerState
	}{
		{v1alpha1.PowerStopped, v1alpha1.ProcessRunning, v1alpha1.PowerRunning},
		{v1alpha1.PowerStopped, v1alpha1.ProcessStarting, v1alpha1.PowerRunning},
		{v1alpha1.PowerRunning, v1alpha1.ProcessOffline, v1alpha1.PowerStopped},
		{v1alpha1.PowerRunning, v1alpha1.ProcessRunning, v1alpha1.PowerRunning},
		{v1alpha1.PowerRunning, "", v1alpha1.PowerRunning},
	}
	for _, tc := range cases {
		t.Run(string(tc.desired)+"/"+tc.process, func(t *testing.T) {
			h := legacyServer(t, tc.desired, tc.process)
			h.reconcile(3)
			if got := h.gs().Spec.Power.Desired; got != tc.want {
				t.Fatalf("desired %s, want %s", got, tc.want)
			}
			if adopted := hasEvent(recordedEvents(h), "LegacyPowerAdopted"); adopted != (tc.desired != tc.want) {
				t.Fatalf("LegacyPowerAdopted event: %v", adopted)
			}
		})
	}
}
