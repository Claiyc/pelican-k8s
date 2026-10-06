package controller

import (
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/internal/operator/render"
)

// VolumeSnapshotGVK is the CSI snapshot kind used for SnapshotThenDelete and schedules.
var VolumeSnapshotGVK = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}

// finalize implements deletion (ARCHITECTURE.md 8.8).
func (r *GameServerReconciler) finalize(s *scope) (ctrl.Result, error) {
	gs := s.gs
	if !controllerutil.ContainsFinalizer(gs, v1alpha1.Finalizer) {
		return ctrl.Result{}, nil
	}
	uuid := gs.Spec.Panel.UUID
	policy := v1alpha1.DeletionDelete
	var snapshotClass string
	cls := &v1alpha1.GameServerClass{}
	name := gs.Spec.ClassName
	if name == "" {
		name = r.DefaultClass
	}
	if err := r.Get(s.ctx, types.NamespacedName{Name: name}, cls); err == nil {
		if cls.Spec.Storage.DeletionPolicy != "" {
			policy = cls.Spec.Storage.DeletionPolicy
		}
		snapshotClass = cls.Spec.Storage.VolumeSnapshotClassName
	}

	// 1. Stop or destroy through the agent while the pod still exists.
	if err := r.loadPodByUUID(s, uuid); err != nil {
		return ctrl.Result{}, err
	}
	if s.pod != nil && s.pod.DeletionTimestamp.IsZero() {
		if ready, ip := agentReady(s.pod); ready {
			sec := &corev1.Secret{}
			if err := r.Get(s.ctx, types.NamespacedName{Namespace: gs.Namespace, Name: names.AgentSecret(uuid)}, sec); err == nil {
				agent := r.newAgent(fmt.Sprintf("http://%s:%d", ip, render.AgentPort), string(sec.Data["token"]))
				ctx, cancel := contextWithTimeout(s, 60*time.Second)
				if policy == v1alpha1.DeletionDelete {
					if err := agent.Delete(ctx, uuid); err != nil {
						r.event(s, corev1.EventTypeWarning, "AgentDeleteFailed", "agent delete failed, continuing: %v", err)
					}
				} else if err := agent.Power(ctx, uuid, "kill"); err != nil {
					r.event(s, corev1.EventTypeWarning, "AgentKillFailed", "agent kill failed, continuing: %v", err)
				}
				cancel()
			}
		}
	}

	// 2. Remove the workload and network objects.
	for _, obj := range []client.Object{
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: gs.Namespace, Name: names.StatefulSet(uuid)}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: gs.Namespace, Name: names.ExposureService(uuid)}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: gs.Namespace, Name: names.AgentService(uuid)}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: gs.Namespace, Name: names.NetworkPolicy(uuid)}},
	} {
		if err := r.Delete(s.ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	jobs := &batchv1.JobList{}
	if err := r.List(s.ctx, jobs, client.InNamespace(gs.Namespace), client.MatchingLabels{v1alpha1.LabelServerUUID: uuid}); err == nil {
		bg := metav1.DeletePropagationBackground
		for i := range jobs.Items {
			_ = r.Delete(s.ctx, &jobs.Items[i], &client.DeleteOptions{PropagationPolicy: &bg})
		}
	}
	if s.pod != nil {
		// Wait for the pod to go so the volume is released before the PVC is handled.
		return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
	}

	// 3. Apply the deletion policy to the PVC.
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(s.ctx, types.NamespacedName{Namespace: gs.Namespace, Name: names.PVC(uuid)}, pvc)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if err == nil {
		switch policy {
		case v1alpha1.DeletionRetain:
			patch := client.MergeFrom(pvc.DeepCopy())
			render.RetainPVC(pvc, s.now)
			if err := r.Patch(s.ctx, pvc, patch); err != nil {
				return ctrl.Result{}, err
			}
			r.event(s, corev1.EventTypeNormal, "VolumeRetained", "volume claim %s retained", pvc.Name)
		case v1alpha1.DeletionSnapshotThenDelete:
			done, err := r.snapshotBeforeDelete(s, pvc, snapshotClass)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !done {
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			fallthrough
		default:
			if err := r.Delete(s.ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	}

	controllerutil.RemoveFinalizer(gs, v1alpha1.Finalizer)
	if err := r.Update(s.ctx, gs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, nil
}

func (r *GameServerReconciler) loadPodByUUID(s *scope, uuid string) error {
	pod := &corev1.Pod{}
	err := r.Get(s.ctx, types.NamespacedName{Namespace: s.gs.Namespace, Name: names.Pod(uuid)}, pod)
	if apierrors.IsNotFound(err) {
		s.pod = nil
		return nil
	}
	if err != nil {
		return err
	}
	s.pod = pod
	return nil
}

// snapshotBeforeDelete creates the final VolumeSnapshot and reports whether it is ready.
func (r *GameServerReconciler) snapshotBeforeDelete(s *scope, pvc *corev1.PersistentVolumeClaim, snapshotClass string) (bool, error) {
	name := pvc.Name + "-final"
	snap := newVolumeSnapshot(s.gs.Namespace, name, pvc.Name, snapshotClass, map[string]string{v1alpha1.LabelServerUUID: s.gs.Spec.Panel.UUID, v1alpha1.LabelComponent: "final-snapshot"})
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(VolumeSnapshotGVK)
	err := r.Get(s.ctx, types.NamespacedName{Namespace: s.gs.Namespace, Name: name}, existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(s.ctx, snap); err != nil {
			return false, fmt.Errorf("create final snapshot: %w", err)
		}
		r.event(s, corev1.EventTypeNormal, "SnapshotCreated", "final snapshot %s created before deleting the volume", name)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	ready, _, _ := unstructured.NestedBool(existing.Object, "status", "readyToUse")
	return ready, nil
}

func newVolumeSnapshot(namespace, name, pvcName, class string, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(VolumeSnapshotGVK)
	u.SetNamespace(namespace)
	u.SetName(name)
	u.SetLabels(labels)
	spec := map[string]any{"source": map[string]any{"persistentVolumeClaimName": pvcName}}
	if class != "" {
		spec["volumeSnapshotClassName"] = class
	}
	u.Object["spec"] = spec
	return u
}
