package render

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
)

// PVC renders the server volume claim.
func PVC(in *Input) *corev1.PersistentVolumeClaim {
	size := PVCSize(in.Settings.Build, in.Class.Spec.Storage)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: in.Meta(names.PVC(in.UUID()), "data"),
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		},
	}
	if sc := in.Class.Spec.Storage.StorageClassName; sc != "" {
		pvc.Spec.StorageClassName = &sc
	}
	return pvc
}

// AgentSecret renders the agent token Secret with the given credentials.
func AgentSecret(in *Input, tokenID, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: in.Meta(names.AgentSecret(in.UUID()), "agent"),
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"token_id": tokenID, "token": token},
	}
}

// RetainPVC marks a claim as orphaned after a Retain deletion.
func RetainPVC(pvc *corev1.PersistentVolumeClaim, now metav1.Time) {
	if pvc.Labels == nil {
		pvc.Labels = map[string]string{}
	}
	pvc.Labels[v1alpha1.LabelOrphanedAt] = now.UTC().Format("2006-01-02T15-04-05Z")
	var refs []metav1.OwnerReference
	for _, r := range pvc.OwnerReferences {
		if r.Kind != "GameServer" {
			refs = append(refs, r)
		}
	}
	pvc.OwnerReferences = refs
}
