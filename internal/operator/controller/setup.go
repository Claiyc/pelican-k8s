package controller

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
)

// SetupWithManager registers the reconciler and its watches.
func (r *GameServerReconciler) SetupWithManager(mgr ctrl.Manager, concurrency int) error {
	if concurrency <= 0 {
		concurrency = 4
	}
	byServerLabel := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		uuid := o.GetLabels()[v1alpha1.LabelServerUUID]
		if uuid == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: names.ForUUID(uuid)}}}
	})
	byClass := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		list := &v1alpha1.GameServerList{}
		if err := mgr.GetClient().List(ctx, list); err != nil {
			return nil
		}
		var out []reconcile.Request
		for _, gs := range list.Items {
			name := gs.Spec.ClassName
			if name == "" {
				name = r.DefaultClass
			}
			if name == o.GetName() {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&gs)})
			}
		}
		return out
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.GameServer{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.Secret{}).
		Watches(&corev1.Pod{}, byServerLabel, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return o.GetLabels()[v1alpha1.LabelComponent] == "game"
		}))).
		Watches(&v1alpha1.GameServerClass{}, byClass).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrency}).
		Complete(r)
}
