package controller

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
func TestLegacyStatefulSetWithTerminatingPod(t *testing.T) {
	h := legacyServer(t, v1alpha1.PowerStopped, v1alpha1.ProcessOffline)
	if err := h.c.Delete(context.Background(), h.pod()); err != nil {
		t.Fatal(err)
	}
	h.reconcile(3)
	assertNoLegacyStatefulSet(t, h)
	if h.get(&appsv1.StatefulSet{}, names.AgentStatefulSet(uuid)) {
		t.Fatal("no agent pod may start while the legacy pod terminates")
	}
	releasePod(t, h)
	h.reconcile(2)
	assertMigrated(t, h)
}

func TestLegacyStatefulSetWithoutPod(t *testing.T) {
	h := legacyServer(t, v1alpha1.PowerStopped, v1alpha1.ProcessOffline)
	releasePod(t, h)
	if err := h.c.Delete(context.Background(), h.pod()); err != nil {
		t.Fatal(err)
	}
	h.reconcile(3)
	assertNoLegacyStatefulSet(t, h)
	h.reconcile(2)
	assertMigrated(t, h)
}

// A terminating 1.x StatefulSet holds the migration back until it is gone.
func TestLegacyStatefulSetTerminating(t *testing.T) {
	h := legacyServer(t, v1alpha1.PowerStopped, v1alpha1.ProcessOffline)
	releasePod(t, h)
	if err := h.c.Delete(context.Background(), h.pod()); err != nil {
		t.Fatal(err)
	}
	sts := &appsv1.StatefulSet{}
	h.get(sts, names.StatefulSet(uuid))
	sts.Finalizers = []string{"test/hold"}
	if err := h.c.Update(context.Background(), sts); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Delete(context.Background(), sts); err != nil {
		t.Fatal(err)
	}
	h.reconcile(2)
	if h.get(&appsv1.StatefulSet{}, names.AgentStatefulSet(uuid)) {
		t.Fatal("no agent pod may start while the 1.x StatefulSet terminates")
	}
	h.get(sts, names.StatefulSet(uuid))
	sts.Finalizers = nil
	if err := h.c.Update(context.Background(), sts); err != nil {
		t.Fatal(err)
	}
	h.reconcile(2)
	assertMigrated(t, h)
}

// A failing API call stops the migration and is returned, so it is retried.
func TestLegacyMigrationFailuresAreReturned(t *testing.T) {
	boom := errors.New("api unavailable")
	cases := map[string]interceptor.Funcs{
		"pod lookup": {
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					return boom
				}
				return c.Get(ctx, key, obj, opts...)
			},
		},
		"power patch": {
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*v1alpha1.GameServer); ok {
					return boom
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		},
		"statefulset lookup": {
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.StatefulSet); ok {
					return boom
				}
				return c.Get(ctx, key, obj, opts...)
			},
		},
		"statefulset delete": {
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*appsv1.StatefulSet); ok {
					return boom
				}
				return c.Delete(ctx, obj, opts...)
			},
		},
		"pod delete": {
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					return boom
				}
				return c.Delete(ctx, obj, opts...)
			},
		},
	}
	for name, funcs := range cases {
		t.Run(name, func(t *testing.T) {
			h := legacyServer(t, v1alpha1.PowerStopped, v1alpha1.ProcessRunning)
			h.reconcile(1) // adds the finalizer
			h.r.Client = interceptor.NewClient(h.c.(client.WithWatch), funcs)
			if _, err := h.reconcileResult(); !errors.Is(err, boom) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// A 1.x StatefulSet that is gone by the time it is deleted counts as gone.
func TestLegacyStatefulSetVanishes(t *testing.T) {
	h := legacyServer(t, v1alpha1.PowerStopped, v1alpha1.ProcessOffline)
	releasePod(t, h)
	if err := h.c.Delete(context.Background(), h.pod()); err != nil {
		t.Fatal(err)
	}
	h.r.Client = interceptor.NewClient(h.c.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if sts, ok := obj.(*appsv1.StatefulSet); ok && sts.Spec.ServiceName == names.AgentService(uuid) {
				if err := c.Delete(ctx, obj); err != nil {
					return err
				}
				return apierrors.NewNotFound(appsv1.Resource("statefulsets"), obj.GetName())
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	h.reconcile(3)
	assertMigrated(t, h)
}

func releasePod(t *testing.T, h *harness) {
	t.Helper()
	pod := h.pod()
	pod.Finalizers = nil
	if err := h.c.Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

func assertNoLegacyStatefulSet(t *testing.T, h *harness) {
	t.Helper()
	if sts := (&appsv1.StatefulSet{}); h.get(sts, names.StatefulSet(uuid)) && sts.Spec.ServiceName == names.AgentService(uuid) {
		t.Fatal("the 1.x StatefulSet must be deleted")
	}
}

func assertMigrated(t *testing.T, h *harness) {
	t.Helper()
	game := &appsv1.StatefulSet{}
	if !h.get(game, names.StatefulSet(uuid)) || game.Spec.ServiceName != names.ExposureService(uuid) {
		t.Fatalf("game StatefulSet with serviceName %q expected, got %q", names.ExposureService(uuid), game.Spec.ServiceName)
	}
	if !h.get(&appsv1.StatefulSet{}, names.AgentStatefulSet(uuid)) {
		t.Fatal("the agent StatefulSet must be created")
	}
}

// Deleting the server takes down a 1.x pod that has no StatefulSet left.
func TestFinalizeDeletesAnOrphanedLegacyPod(t *testing.T) {
	h := orphanedLegacyServer(t)
	h.reconcile(1)
	if pod := h.pod(); pod == nil || pod.DeletionTimestamp.IsZero() {
		t.Fatalf("the orphaned 1.x pod must be terminating, got %+v", pod)
	}
}

// orphanedLegacyServer is a 1.x server being deleted whose StatefulSet is
// already gone.
func orphanedLegacyServer(t *testing.T) *harness {
	t.Helper()
	h := legacyServer(t, v1alpha1.PowerRunning, v1alpha1.ProcessRunning)
	gs := h.gs()
	gs.Finalizers = []string{v1alpha1.Finalizer}
	if err := h.c.Update(context.Background(), gs); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Delete(context.Background(), &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: names.StatefulSet(uuid), Namespace: ns}}); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Delete(context.Background(), h.gs()); err != nil {
		t.Fatal(err)
	}
	return h
}

// A failing delete of the orphaned 1.x pod keeps the finalizer and is retried.
func TestFinalizeLegacyPodDeleteFailure(t *testing.T) {
	boom := errors.New("api unavailable")
	h := orphanedLegacyServer(t)
	h.r.Client = interceptor.NewClient(h.c.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*corev1.Pod); ok {
				return boom
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	if _, err := h.reconcileResult(); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if len(h.gs().Finalizers) == 0 {
		t.Fatal("the finalizer must stay while the teardown fails")
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

// A power action the operator has not acted on yet wins over the process state.
func TestLegacyPowerKeepsAPendingAction(t *testing.T) {
	h := legacyServer(t, v1alpha1.PowerStopped, v1alpha1.ProcessRunning)
	gs := h.gs()
	gs.Spec.Power.Generation = 4
	if err := h.c.Update(context.Background(), gs); err != nil {
		t.Fatal(err)
	}
	gs.Status.Power.ObservedGeneration = 3
	if err := h.c.Status().Update(context.Background(), gs); err != nil {
		t.Fatal(err)
	}
	h.reconcile(3)
	if got := h.gs().Spec.Power.Desired; got != v1alpha1.PowerStopped {
		t.Fatalf("desired %s, want the pending Stopped", got)
	}
	if hasEvent(recordedEvents(h), "LegacyPowerAdopted") {
		t.Fatal("no LegacyPowerAdopted event expected")
	}
}
