package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
)

func contextWithTimeout(s *scope, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(s.ctx, d)
}

// reconcileSnapshots takes scheduled crash-consistent VolumeSnapshots (ARCHITECTURE.md 10.4).
func (r *GameServerReconciler) reconcileSnapshots(s *scope) error {
	schedule := s.class.Spec.Storage.SnapshotSchedule
	if schedule == "" {
		return nil
	}
	sched, err := cron.ParseStandard(schedule)
	if err != nil {
		r.event(s, corev1.EventTypeWarning, "InvalidSnapshotSchedule", "class snapshotSchedule %q: %v", schedule, err)
		return nil
	}
	last := s.gs.CreationTimestamp.Time
	if s.gs.Status.Snapshot != nil && s.gs.Status.Snapshot.LastAt != nil {
		last = s.gs.Status.Snapshot.LastAt.Time
	}
	next := sched.Next(last)
	if s.now.Time.Before(next) {
		if wait := next.Sub(s.now.Time); wait < s.requeue {
			s.requeue = wait
		}
		return nil
	}
	name := fmt.Sprintf("%s-%s", names.PVC(s.in.UUID()), s.now.UTC().Format("20060102-150405"))
	snap := newVolumeSnapshot(s.gs.Namespace, name, names.PVC(s.in.UUID()), s.class.Spec.Storage.VolumeSnapshotClassName,
		map[string]string{v1alpha1.LabelServerUUID: s.in.UUID(), v1alpha1.LabelComponent: "scheduled-snapshot"})
	if err := r.Create(s.ctx, snap); err != nil && !apierrors.IsAlreadyExists(err) {
		r.event(s, corev1.EventTypeWarning, "SnapshotFailed", "scheduled snapshot failed: %v", err)
		return nil
	}
	s.gs.Status.Snapshot = &v1alpha1.SnapshotStatus{LastAt: &s.now, LastName: name}
	r.event(s, corev1.EventTypeNormal, "SnapshotCreated", "scheduled snapshot %s created", name)
	return r.pruneSnapshots(s)
}

func (r *GameServerReconciler) pruneSnapshots(s *scope) error {
	retain := int(s.class.Spec.Storage.SnapshotRetain)
	if retain <= 0 {
		retain = 7
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(VolumeSnapshotGVK)
	list.SetKind("VolumeSnapshotList")
	if err := r.List(s.ctx, list, client.InNamespace(s.gs.Namespace), client.MatchingLabels{v1alpha1.LabelServerUUID: s.in.UUID(), v1alpha1.LabelComponent: "scheduled-snapshot"}); err != nil {
		return client.IgnoreNotFound(err)
	}
	items := list.Items
	sort.Slice(items, func(i, j int) bool {
		return items[i].GetCreationTimestamp().Time.Before(items[j].GetCreationTimestamp().Time)
	})
	for len(items) > retain {
		old := items[0]
		items = items[1:]
		if err := r.Delete(s.ctx, &old); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

var _ = metav1.Now
