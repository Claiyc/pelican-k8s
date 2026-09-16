// Package v1alpha1 contains the API types of the pelican-k8s.io API group.
//
// +kubebuilder:object:generate=true
// +groupName=pelican-k8s.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "pelican-k8s.io", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

const (
	// LabelServerUUID carries the Panel server UUID on every object owned by a GameServer.
	LabelServerUUID = "pelican-k8s.io/server-uuid"
	// LabelEggUUID carries the Panel egg UUID.
	LabelEggUUID = "pelican-k8s.io/egg-uuid"
	// LabelComponent identifies the role of an owned object (game, install, agent).
	LabelComponent = "pelican-k8s.io/component"
	// LabelOrphanedAt is set on a PVC that was retained after its GameServer was deleted.
	LabelOrphanedAt = "pelican-k8s.io/orphaned-at"
	// AnnotationPanelName carries the human readable Panel server name.
	AnnotationPanelName = "pelican-k8s.io/panel-name"
	// Finalizer protects a GameServer until the operator has cleaned up.
	Finalizer = "pelican-k8s.io/finalizer"
)
