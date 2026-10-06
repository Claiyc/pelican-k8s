package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/internal/operator/render"
)

// recordedEvents drains the fake recorder.
func recordedEvents(h *harness) []string {
	h.t.Helper()
	rec := h.r.Recorder.(*record.FakeRecorder)
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func listSnapshots(h *harness, component string) []unstructured.Unstructured {
	h.t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(VolumeSnapshotGVK)
	list.SetKind("VolumeSnapshotList")
	if err := h.c.List(context.Background(), list, client.InNamespace(ns), client.MatchingLabels{v1alpha1.LabelComponent: component}); err != nil {
		h.t.Fatal(err)
	}
	return list.Items
}

func (h *harness) reconcileResult() (ctrl.Result, error) {
	h.t.Helper()
	return h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: names.ForUUID(uuid)}})
}

// snapshotClass schedules hourly snapshots and keeps two.
func snapshotClass() *v1alpha1.GameServerClass {
	c := newClass()
	c.Spec.Storage.SnapshotSchedule = "0 * * * *"
	c.Spec.Storage.VolumeSnapshotClassName = "csi"
	c.Spec.Storage.SnapshotRetain = 2
	return c
}

// stampCreation gives created objects the harness clock so that retention
// pruning can order them.
func stampCreation(h **harness) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if ts := obj.GetCreationTimestamp(); ts.IsZero() {
				obj.SetCreationTimestamp(metav1.NewTime((*h).now))
			}
			return c.Create(ctx, obj, opts...)
		},
	}
}

func TestScheduledSnapshots(t *testing.T) {
	var h *harness
	h = newHarnessWith(t, stampCreation(&h), newGS(), snapshotClass())
	h.reconcile(3)

	// The first reconcile after creation takes the first snapshot.
	snaps := listSnapshots(h, "scheduled-snapshot")
	if len(snaps) != 1 {
		t.Fatalf("%d snapshots", len(snaps))
	}
	first := snaps[0]
	if first.GetName() != names.PVC(uuid)+"-20260916-120000" {
		t.Fatalf("name %q", first.GetName())
	}
	if first.GetLabels()[v1alpha1.LabelServerUUID] != uuid {
		t.Fatalf("labels %v", first.GetLabels())
	}
	pvc, _, _ := unstructured.NestedString(first.Object, "spec", "source", "persistentVolumeClaimName")
	class, _, _ := unstructured.NestedString(first.Object, "spec", "volumeSnapshotClassName")
	if pvc != names.PVC(uuid) || class != "csi" {
		t.Fatalf("spec %v", first.Object["spec"])
	}
	gs := h.gs()
	if gs.Status.Snapshot == nil || gs.Status.Snapshot.LastName != first.GetName() || !gs.Status.Snapshot.LastAt.Time.Equal(h.now) {
		t.Fatalf("status.snapshot = %+v", gs.Status.Snapshot)
	}
	if !hasEvent(recordedEvents(h), "SnapshotCreated") {
		t.Fatal("no SnapshotCreated event")
	}

	// Before the next slot nothing is taken, and the reconcile is requeued for it.
	h.now = time.Date(2026, 9, 16, 12, 59, 58, 0, time.UTC)
	res, err := h.reconcileResult()
	if err != nil {
		t.Fatal(err)
	}
	if len(listSnapshots(h, "scheduled-snapshot")) != 1 {
		t.Fatal("snapshot taken before its slot")
	}
	if res.RequeueAfter != 2*time.Second {
		t.Fatalf("requeue %v, want the 2s left until the slot", res.RequeueAfter)
	}

	// Two more slots: retention keeps the newest two.
	h.now = time.Date(2026, 9, 16, 13, 0, 0, 0, time.UTC)
	h.reconcile(1)
	h.now = time.Date(2026, 9, 16, 14, 0, 0, 0, time.UTC)
	h.reconcile(1)
	snaps = listSnapshots(h, "scheduled-snapshot")
	if len(snaps) != 2 {
		t.Fatalf("%d snapshots after pruning, want 2", len(snaps))
	}
	for _, s := range snaps {
		if s.GetName() == first.GetName() {
			t.Fatal("the oldest snapshot must have been pruned")
		}
	}
}

func TestPruneKeepsDefaultSevenAndOtherServers(t *testing.T) {
	var h *harness
	cls := snapshotClass()
	cls.Spec.Storage.SnapshotRetain = 0
	h = newHarnessWith(t, stampCreation(&h), newGS(), cls)
	h.reconcile(2) // first snapshot at 12:00
	other := newVolumeSnapshot(ns, "other-server-snap", "other-pvc", "csi", map[string]string{v1alpha1.LabelServerUUID: "other", v1alpha1.LabelComponent: "scheduled-snapshot"})
	if err := h.c.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 8; i++ {
		h.now = time.Date(2026, 9, 16, 12+i, 0, 0, 0, time.UTC)
		h.reconcile(1)
	}
	var mine int
	for _, s := range listSnapshots(h, "scheduled-snapshot") {
		if s.GetLabels()[v1alpha1.LabelServerUUID] == uuid {
			mine++
		}
	}
	if mine != 7 {
		t.Fatalf("%d snapshots kept, want the default of 7", mine)
	}
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "other-server-snap"}, snapshotObject()); err != nil {
		t.Fatalf("another server's snapshot must not be pruned: %v", err)
	}
}

func snapshotObject() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(VolumeSnapshotGVK)
	return u
}

func TestInvalidSnapshotScheduleWarns(t *testing.T) {
	cls := snapshotClass()
	cls.Spec.Storage.SnapshotSchedule = "every hour please"
	h := newHarness(t, newGS(), cls)
	h.reconcile(3)
	if len(listSnapshots(h, "scheduled-snapshot")) != 0 {
		t.Fatal("no snapshot may be taken with an invalid schedule")
	}
	if !hasEvent(recordedEvents(h), "InvalidSnapshotSchedule") {
		t.Fatal("no InvalidSnapshotSchedule warning")
	}
	if h.gs().Status.Phase == v1alpha1.PhaseError {
		t.Fatal("a bad schedule must not break the reconcile")
	}
}

func TestSnapshotCreateFailureIsNotFatal(t *testing.T) {
	failing := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetObjectKind().GroupVersionKind().Kind == "VolumeSnapshot" {
				return errors.New("snapshot CRD not installed")
			}
			return c.Create(ctx, obj, opts...)
		},
	}
	h := newHarnessWith(t, failing, newGS(), snapshotClass())
	h.reconcile(3)
	if gs := h.gs(); gs.Status.Snapshot != nil {
		t.Fatalf("a failed snapshot must not be recorded: %+v", gs.Status.Snapshot)
	}
	if !hasEvent(recordedEvents(h), "SnapshotFailed") {
		t.Fatal("no SnapshotFailed warning")
	}
	if h.gs().Status.Phase == v1alpha1.PhaseError {
		t.Fatal("a snapshot failure must not put the server into Error")
	}
}

func TestSnapshotAlreadyExistsCountsAsTaken(t *testing.T) {
	h := newHarness(t, newGS(), snapshotClass())
	h.reconcile(2) // takes the first snapshot at 12:00 and records it
	// The status write is lost (e.g. a conflict): the next reconcile in the same
	// second tries the same name again.
	h.patchStatus(func(gs *v1alpha1.GameServer) { gs.Status.Snapshot = nil })
	h.reconcile(1)
	if gs := h.gs(); gs.Status.Snapshot == nil {
		t.Fatal("an existing snapshot of the same name must be adopted")
	}
	if n := len(listSnapshots(h, "scheduled-snapshot")); n != 1 {
		t.Fatalf("%d snapshots", n)
	}
}

func TestPruneListErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		err     error
		wantErr bool
	}{
		"not found (CRD missing) is ignored": {apierrors.NewNotFound(schema.GroupResource{Group: "snapshot.storage.k8s.io", Resource: "volumesnapshots"}, ""), false},
		"other errors are returned":          {errors.New("boom"), true},
	} {
		t.Run(name, func(t *testing.T) {
			failList := false
			h := newHarnessWith(t, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*unstructured.UnstructuredList); ok && failList {
						return tc.err
					}
					return c.List(ctx, list, opts...)
				},
			}, newGS(), snapshotClass())
			h.reconcile(2) // finalizer, then everything up to the snapshot
			s := scopeFor(t, h)
			failList = true
			if err := h.r.pruneSnapshots(s); (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// scopeFor builds the reconcile scope the way the reconciler does.
func scopeFor(t *testing.T, h *harness) *scope {
	t.Helper()
	s := &scope{ctx: context.Background(), gs: h.gs(), now: metav1.NewTime(h.now), requeue: requeueSlow}
	s.orig = s.gs.DeepCopy()
	if err := h.r.loadClassAndSettings(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// SnapshotThenDelete keeps the volume until the final snapshot is ready.
func TestFinalizeSnapshotThenDelete(t *testing.T) {
	cls := newClass()
	cls.Spec.Storage.DeletionPolicy = v1alpha1.DeletionSnapshotThenDelete
	cls.Spec.Storage.VolumeSnapshotClassName = "csi"
	h := newHarness(t, newGS(), cls)
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	if err := h.c.Delete(context.Background(), h.gs()); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	if got := strings.Join(h.agent.Calls(), ","); got != "power:kill" {
		t.Fatalf("agent calls %s: the data must be kept, so the server is killed rather than deleted", got)
	}
	if err := h.c.Delete(context.Background(), h.pod()); err != nil {
		t.Fatal(err)
	}

	// The snapshot is requested and the volume claim waits for it.
	res, err := h.reconcileResult()
	if err != nil || res.RequeueAfter != 5*time.Second {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	snaps := listSnapshots(h, "final-snapshot")
	if len(snaps) != 1 || snaps[0].GetName() != names.PVC(uuid)+"-final" {
		t.Fatalf("final snapshots: %+v", snaps)
	}
	if class, _, _ := unstructured.NestedString(snaps[0].Object, "spec", "volumeSnapshotClassName"); class != "csi" {
		t.Fatalf("snapshot class %q", class)
	}
	var pvc corev1.PersistentVolumeClaim
	if !h.get(&pvc, names.PVC(uuid)) {
		t.Fatal("pvc deleted before the snapshot was ready")
	}
	if !hasEvent(recordedEvents(h), "SnapshotCreated") {
		t.Fatal("no SnapshotCreated event")
	}

	// Still not ready: keep waiting without creating another snapshot.
	res, err = h.reconcileResult()
	if err != nil || res.RequeueAfter != 5*time.Second {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(listSnapshots(h, "final-snapshot")) != 1 {
		t.Fatal("duplicate final snapshot")
	}
	if !h.get(&pvc, names.PVC(uuid)) {
		t.Fatal("pvc deleted while the snapshot is not ready")
	}

	// Ready: the claim goes and the finalizer is released.
	snap := snapshotObject()
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: names.PVC(uuid) + "-final"}, snap); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(snap.Object, true, "status", "readyToUse"); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Update(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	h.reconcile(1)
	if h.get(&pvc, names.PVC(uuid)) {
		t.Fatal("pvc must be deleted once the snapshot is ready")
	}
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: names.ForUUID(uuid)}, &v1alpha1.GameServer{}); !apierrors.IsNotFound(err) {
		t.Fatalf("gameserver should be gone: %v", err)
	}
	if len(listSnapshots(h, "final-snapshot")) != 1 {
		t.Fatal("the final snapshot must outlive the server")
	}
}

func TestFinalizeSnapshotWithoutClassUsesClusterDefault(t *testing.T) {
	snap := newVolumeSnapshot(ns, "s", "pvc", "", map[string]string{"a": "b"})
	if _, found, _ := unstructured.NestedString(snap.Object, "spec", "volumeSnapshotClassName"); found {
		t.Fatalf("an empty class must be omitted so the cluster default applies: %v", snap.Object["spec"])
	}
	if snap.GroupVersionKind() != VolumeSnapshotGVK || snap.GetNamespace() != ns || snap.GetName() != "s" || snap.GetLabels()["a"] != "b" {
		t.Fatalf("snapshot %+v", snap)
	}
}

func deletingGameServer(t *testing.T, cls *v1alpha1.GameServerClass, funcs interceptor.Funcs) *harness {
	t.Helper()
	h := newHarnessWith(t, funcs, newGS(), cls)
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	if err := h.c.Delete(context.Background(), h.gs()); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestFinalizeSnapshotErrors(t *testing.T) {
	cls := newClass()
	cls.Spec.Storage.DeletionPolicy = v1alpha1.DeletionSnapshotThenDelete
	cls.Spec.Storage.VolumeSnapshotClassName = "csi"

	t.Run("create fails", func(t *testing.T) {
		h := deletingGameServer(t, cls, interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == "VolumeSnapshot" {
					return errors.New("no snapshot support")
				}
				return c.Create(ctx, obj, opts...)
			},
		})
		h.reconcile(1)
		_ = h.c.Delete(context.Background(), h.pod())
		_, err := h.reconcileResult()
		if err == nil || !strings.Contains(err.Error(), "create final snapshot") {
			t.Fatalf("err = %v", err)
		}
		var pvc corev1.PersistentVolumeClaim
		if !h.get(&pvc, names.PVC(uuid)) {
			t.Fatal("the volume must survive a failed snapshot")
		}
	})

	t.Run("lookup fails", func(t *testing.T) {
		h := deletingGameServer(t, cls, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if u, ok := obj.(*unstructured.Unstructured); ok && u.GroupVersionKind().Kind == "VolumeSnapshot" {
					return errors.New("api down")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		h.reconcile(1)
		_ = h.c.Delete(context.Background(), h.pod())
		if _, err := h.reconcileResult(); err == nil || !strings.Contains(err.Error(), "api down") {
			t.Fatalf("err = %v", err)
		}
	})
}

// failingAgent fails the destructive calls the finalizer makes.
type failingAgent struct{ *fakeAgent }

func (f failingAgent) Delete(ctx context.Context, uuid string) error {
	f.record("delete")
	return errors.New("wings unreachable")
}

func (f failingAgent) Power(ctx context.Context, uuid, action string) error {
	f.record("power:" + action)
	return errors.New("wings unreachable")
}

func TestFinalizeContinuesWhenTheAgentFails(t *testing.T) {
	for name, policy := range map[string]v1alpha1.DeletionPolicy{
		"delete": v1alpha1.DeletionDelete,
		"retain": v1alpha1.DeletionRetain,
	} {
		t.Run(name, func(t *testing.T) {
			cls := newClass()
			cls.Spec.Storage.DeletionPolicy = policy
			h := deletingGameServer(t, cls, interceptor.Funcs{})
			h.r.NewAgent = func(base, token string) AgentAPI { return failingAgent{h.agent} }
			_ = recordedEvents(h)
			h.reconcile(1)

			want := map[string]string{"delete": "AgentDeleteFailed", "retain": "AgentKillFailed"}[name]
			if !hasEvent(recordedEvents(h), want) {
				t.Fatalf("no %s warning", want)
			}
			// The teardown went on regardless.
			var sts appsv1.StatefulSet
			if h.get(&sts, names.StatefulSet(uuid)) {
				t.Fatal("the statefulset must be deleted even though the agent failed")
			}
		})
	}
}

func TestFinalizeRemovesInstallJobsAndWaitsForThePod(t *testing.T) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "install-job", Namespace: ns, Labels: map[string]string{v1alpha1.LabelServerUUID: uuid}}}
	unrelated := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "other-job", Namespace: ns, Labels: map[string]string{v1alpha1.LabelServerUUID: "someone-else"}}}
	h := newHarness(t, newGS(), newClass(), job, unrelated)
	h.reconcile(2)
	h.createPod(true)
	h.reconcile(2)
	if err := h.c.Delete(context.Background(), h.gs()); err != nil {
		t.Fatal(err)
	}
	res, err := h.reconcileResult()
	if err != nil || res.RequeueAfter != 3*time.Second {
		t.Fatalf("with the pod still present the finalizer must wait: %+v %v", res, err)
	}
	var j batchv1.Job
	if h.get(&j, "install-job") {
		t.Fatal("the server's install job must be deleted")
	}
	if !h.get(&j, "other-job") {
		t.Fatal("another server's job must stay")
	}
	if len(h.gs().Finalizers) == 0 {
		t.Fatal("finalizer released while the pod still exists")
	}
}

func TestFinalizeFailuresAreReturnedAndRetried(t *testing.T) {
	boom := errors.New("api unavailable")
	cases := map[string]interceptor.Funcs{
		"statefulset delete": {
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*appsv1.StatefulSet); ok {
					return boom
				}
				return c.Delete(ctx, obj, opts...)
			},
		},
		"pod lookup": {
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Pod); ok && key.Name == names.Pod(uuid) {
					return boom
				}
				return c.Get(ctx, key, obj, opts...)
			},
		},
	}
	for name, funcs := range cases {
		t.Run(name, func(t *testing.T) {
			h := deletingGameServer(t, newClass(), interceptor.Funcs{})
			// Swap the interceptor in after the setup, which needs a working client.
			h.r.Client = interceptor.NewClient(h.c.(client.WithWatch), funcs)
			if _, err := h.reconcileResult(); !errors.Is(err, boom) {
				t.Fatalf("err = %v", err)
			}
			if len(h.gs().Finalizers) == 0 {
				t.Fatal("the finalizer must stay while the teardown fails")
			}
		})
	}
}

func TestFinalizePVCStepFailures(t *testing.T) {
	boom := errors.New("api unavailable")
	setup := func(t *testing.T, policy v1alpha1.DeletionPolicy, funcs interceptor.Funcs) *harness {
		cls := newClass()
		cls.Spec.Storage.DeletionPolicy = policy
		h := deletingGameServer(t, cls, interceptor.Funcs{})
		h.reconcile(1) // agent teardown, pod still present
		if err := h.c.Delete(context.Background(), h.pod()); err != nil {
			t.Fatal(err)
		}
		h.r.Client = interceptor.NewClient(h.c.(client.WithWatch), funcs)
		return h
	}

	t.Run("pvc lookup", func(t *testing.T) {
		h := setup(t, v1alpha1.DeletionDelete, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
					return boom
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		if _, err := h.reconcileResult(); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("pvc delete", func(t *testing.T) {
		h := setup(t, v1alpha1.DeletionDelete, interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
					return boom
				}
				return c.Delete(ctx, obj, opts...)
			},
		})
		if _, err := h.reconcileResult(); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if len(h.gs().Finalizers) == 0 {
			t.Fatal("finalizer released although the volume could not be deleted")
		}
	})
	t.Run("retain patch", func(t *testing.T) {
		h := setup(t, v1alpha1.DeletionRetain, interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
					return boom
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		})
		if _, err := h.reconcileResult(); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("finalizer removal tolerates a vanished object", func(t *testing.T) {
		h := setup(t, v1alpha1.DeletionDelete, interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*v1alpha1.GameServer); ok {
					return apierrors.NewNotFound(schema.GroupResource{Resource: "gameservers"}, obj.GetName())
				}
				return c.Update(ctx, obj, opts...)
			},
		})
		if _, err := h.reconcileResult(); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("finalizer removal error", func(t *testing.T) {
		h := setup(t, v1alpha1.DeletionDelete, interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*v1alpha1.GameServer); ok {
					return boom
				}
				return c.Update(ctx, obj, opts...)
			},
		})
		if _, err := h.reconcileResult(); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestFinalizeWithoutOurFinalizerDoesNothing(t *testing.T) {
	h := newHarness(t, newGS(), newClass())
	s := &scope{ctx: context.Background(), gs: newGS(), now: metav1.Now()}
	res, err := h.r.finalize(s)
	if err != nil || res != (ctrl.Result{}) {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(h.agent.Calls()) != 0 {
		t.Fatalf("agent called: %v", h.agent.Calls())
	}
}

func TestResolveUID(t *testing.T) {
	ranged := func(annotations map[string]string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Annotations: annotations}}
	}
	cases := map[string]struct {
		runAsUser int64
		useRange  bool
		namespace *corev1.Namespace
		want      int64
		wantErr   string
	}{
		"configured":                 {runAsUser: 4242, want: 4242},
		"default when unset":         {want: 1000},
		"range ignored when off":     {runAsUser: 7, namespace: ranged(map[string]string{uidRangeAnn: "1000680000/10000"}), want: 7},
		"namespace range start":      {useRange: true, namespace: ranged(map[string]string{uidRangeAnn: "1000680000/10000"}), want: 1000680000},
		"namespace without range":    {useRange: true, runAsUser: 5, namespace: ranged(nil), want: 5},
		"malformed range":            {useRange: true, namespace: ranged(map[string]string{uidRangeAnn: "abc/10"}), wantErr: "parse"},
		"namespace missing":          {useRange: true, wantErr: "not found"},
		"range without size section": {useRange: true, namespace: ranged(map[string]string{uidRangeAnn: "2000"}), want: 2000},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cls := newClass()
			cls.Spec.Security.RunAsUser = tc.runAsUser
			cls.Spec.Security.UseNamespaceUIDRange = tc.useRange
			objs := []client.Object{newGS(), cls}
			if tc.namespace != nil {
				objs = append(objs, tc.namespace)
			}
			h := newHarness(t, objs...)
			s := &scope{ctx: context.Background(), gs: h.gs(), class: cls}
			got, err := h.r.resolveUID(s)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("uid = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

func TestPodHelpers(t *testing.T) {
	t.Run("nodeReady", func(t *testing.T) {
		node := func(conds ...corev1.NodeCondition) *corev1.Node {
			return &corev1.Node{Status: corev1.NodeStatus{Conditions: conds}}
		}
		if !nodeReady(node(corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse}, corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue})) {
			t.Error("Ready=True")
		}
		if nodeReady(node(corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionUnknown})) {
			t.Error("Ready=Unknown (node lost)")
		}
		if nodeReady(node(corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionFalse})) {
			t.Error("Ready=False")
		}
		if nodeReady(node()) {
			t.Error("no Ready condition")
		}
	})
	t.Run("gameContainerStarted", func(t *testing.T) {
		pod := func(cs ...corev1.ContainerStatus) *corev1.Pod {
			return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: cs}}
		}
		game := func(mut func(*corev1.ContainerStatus)) *corev1.Pod {
			cs := corev1.ContainerStatus{Name: render.GameContainer}
			mut(&cs)
			return pod(cs)
		}
		if gameContainerStarted(pod()) {
			t.Error("no statuses")
		}
		if gameContainerStarted(pod(corev1.ContainerStatus{Name: "sidecar", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}})) {
			t.Error("only the game container counts")
		}
		if gameContainerStarted(game(func(cs *corev1.ContainerStatus) { cs.State.Waiting = &corev1.ContainerStateWaiting{} })) {
			t.Error("waiting")
		}
		if !gameContainerStarted(game(func(cs *corev1.ContainerStatus) { cs.State.Running = &corev1.ContainerStateRunning{} })) {
			t.Error("running")
		}
		if !gameContainerStarted(game(func(cs *corev1.ContainerStatus) { cs.State.Terminated = &corev1.ContainerStateTerminated{} })) {
			t.Error("terminated")
		}
		if !gameContainerStarted(game(func(cs *corev1.ContainerStatus) { cs.RestartCount = 1 })) {
			t.Error("restarted")
		}
		if !gameContainerStarted(game(func(cs *corev1.ContainerStatus) {
			cs.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{}
		})) {
			t.Error("last termination")
		}
	})
	t.Run("resizeDeferred", func(t *testing.T) {
		pod := func(conds ...corev1.PodCondition) *corev1.Pod {
			return &corev1.Pod{Status: corev1.PodStatus{Conditions: conds}}
		}
		if got := resizeDeferred(pod()); got != "" {
			t.Errorf("no conditions: %q", got)
		}
		if got := resizeDeferred(pod(corev1.PodCondition{Type: "PodResizePending", Status: corev1.ConditionFalse})); got != "" {
			t.Errorf("false condition: %q", got)
		}
		if got := resizeDeferred(pod(corev1.PodCondition{Type: "PodResizePending", Status: corev1.ConditionTrue, Reason: "Deferred"})); got != "Deferred" {
			t.Errorf("reason: %q", got)
		}
		if got := resizeDeferred(pod(corev1.PodCondition{Type: "PodResizeInProgress", Status: corev1.ConditionTrue})); got != "PodResizeInProgress" {
			t.Errorf("type fallback: %q", got)
		}
		if got := resizeDeferred(pod(corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue})); got != "" {
			t.Errorf("unrelated: %q", got)
		}
	})
	t.Run("truncate", func(t *testing.T) {
		if truncate("short") != "short" {
			t.Error("short")
		}
		long := strings.Repeat("x", 600)
		if got := truncate(long); len(got) != 515 || !strings.HasSuffix(got, "...") {
			t.Errorf("len %d", len(got))
		}
		if exact := strings.Repeat("y", 512); truncate(exact) != exact {
			t.Error("512 bytes fit")
		}
	})
	t.Run("ptrEq", func(t *testing.T) {
		one, other, two := int32(1), int32(1), int32(2)
		for _, tc := range []struct {
			a, b *int32
			want bool
		}{{nil, nil, true}, {&one, nil, false}, {nil, &one, false}, {&one, &other, true}, {&one, &two, false}} {
			if got := ptrEq(tc.a, tc.b); got != tc.want {
				t.Errorf("ptrEq(%v, %v) = %v", tc.a, tc.b, got)
			}
		}
	})
	t.Run("resourcesEqual", func(t *testing.T) {
		a := corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}}
		b := corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000m")}}
		if !resourcesEqual(a, b) {
			t.Error("1 and 1000m are the same quantity")
		}
		c := corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1")}}
		if resourcesEqual(a, c) {
			t.Error("different resource names")
		}
	})
}
