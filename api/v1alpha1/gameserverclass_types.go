package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ScratchVolumeType selects how the agent's scratch volume is provisioned.
// +kubebuilder:validation:Enum=Ephemeral;EmptyDir
type ScratchVolumeType string

const (
	ScratchEphemeral ScratchVolumeType = "Ephemeral"
	ScratchEmptyDir  ScratchVolumeType = "EmptyDir"
)

// DeletionPolicy selects what happens to the server PVC when the CR is deleted.
// +kubebuilder:validation:Enum=Delete;Retain;SnapshotThenDelete
type DeletionPolicy string

const (
	DeletionDelete             DeletionPolicy = "Delete"
	DeletionRetain             DeletionPolicy = "Retain"
	DeletionSnapshotThenDelete DeletionPolicy = "SnapshotThenDelete"
)

// ExposureMode selects how game ports are exposed.
// +kubebuilder:validation:Enum=LoadBalancer;NodePort;HostPort
type ExposureMode string

const (
	ExposureLoadBalancer ExposureMode = "LoadBalancer"
	ExposureNodePort     ExposureMode = "NodePort"
	ExposureHostPort     ExposureMode = "HostPort"
)

// ScratchSpec configures the scratch volume holding backup archives and agent temp files.
type ScratchSpec struct {
	// +kubebuilder:default=Ephemeral
	Type             ScratchVolumeType `json:"type,omitempty"`
	StorageClassName string            `json:"storageClassName,omitempty"`
	// SizeGiB is the scratch volume size; 0 means the size of the server PVC.
	// +kubebuilder:validation:Minimum=0
	SizeGiB int32 `json:"sizeGiB,omitempty"`
}

// StorageSpec configures per-server storage.
type StorageSpec struct {
	// StorageClassName must allow volume expansion. Empty selects the cluster default.
	StorageClassName string `json:"storageClassName,omitempty"`
	// DefaultSizeGiB is used when the Panel disk_space is 0 (unlimited).
	// +kubebuilder:default=20
	// +kubebuilder:validation:Minimum=1
	DefaultSizeGiB int32 `json:"defaultSizeGiB,omitempty"`
	// OverheadPercent grows the PVC beyond disk_space for logs, the activity database and install output.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	OverheadPercent int32       `json:"overheadPercent,omitempty"`
	Scratch         ScratchSpec `json:"scratch,omitempty"`
	// +kubebuilder:default=Delete
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
	// VolumeSnapshotClassName is required for SnapshotThenDelete and snapshotSchedule.
	VolumeSnapshotClassName string `json:"volumeSnapshotClassName,omitempty"`
	// SnapshotSchedule is an optional cron expression for crash-consistent VolumeSnapshots.
	SnapshotSchedule string `json:"snapshotSchedule,omitempty"`
	// SnapshotRetain is how many scheduled snapshots to keep per server.
	// +kubebuilder:default=7
	// +kubebuilder:validation:Minimum=1
	SnapshotRetain int32 `json:"snapshotRetain,omitempty"`
}

// LoadBalancerProvider names a load balancer implementation whose annotation
// keys are known, so a class does not have to spell them out. A wrong key is
// silently ignored by the provider, which is hard to notice.
// +kubebuilder:validation:Enum="";metallb
type LoadBalancerProvider string

const (
	// LoadBalancerMetalLB fills in the MetalLB annotation keys.
	LoadBalancerMetalLB LoadBalancerProvider = "metallb"
)

// LoadBalancerSpec carries implementation specific annotation keys.
type LoadBalancerSpec struct {
	// Provider supplies the annotation keys of a known implementation.
	// "metallb" means metallb.io/loadBalancerIPs and metallb.io/allow-shared-ip.
	// An explicit ipAnnotation or sharingAnnotation always wins.
	Provider LoadBalancerProvider `json:"provider,omitempty"`
	// IPAnnotation pins the Service to the allocation IP (e.g. metallb.io/loadBalancerIPs).
	IPAnnotation string `json:"ipAnnotation,omitempty"`
	// SharingAnnotation lets several Services share one IP (e.g. metallb.io/allow-shared-ip).
	SharingAnnotation string `json:"sharingAnnotation,omitempty"`
	// Annotations are added verbatim to every exposure Service.
	Annotations map[string]string `json:"annotations,omitempty"`
}

// MetalLB annotation keys.
const (
	MetalLBIPAnnotation      = "metallb.io/loadBalancerIPs"
	MetalLBSharingAnnotation = "metallb.io/allow-shared-ip"
)

// IPKey returns the annotation that pins a Service to an address.
func (l LoadBalancerSpec) IPKey() string {
	if l.IPAnnotation != "" {
		return l.IPAnnotation
	}
	if l.Provider == LoadBalancerMetalLB {
		return MetalLBIPAnnotation
	}
	return ""
}

// SharingKey returns the annotation that lets Services share an address.
func (l LoadBalancerSpec) SharingKey() string {
	if l.SharingAnnotation != "" {
		return l.SharingAnnotation
	}
	if l.Provider == LoadBalancerMetalLB {
		return MetalLBSharingAnnotation
	}
	return ""
}

// ExposureSpec configures how game ports reach players.
type ExposureSpec struct {
	// +kubebuilder:default=LoadBalancer
	Mode ExposureMode `json:"mode,omitempty"`
	// +kubebuilder:default=Local
	ExternalTrafficPolicy corev1.ServiceExternalTrafficPolicy `json:"externalTrafficPolicy,omitempty"`
	LoadBalancer          LoadBalancerSpec                    `json:"loadBalancer,omitempty"`
	// ExternalIPs are the addresses advertised to the Panel through /api/system/ips.
	// Empty means node addresses (NodePort, HostPort) or the observed LoadBalancer ingress IPs.
	ExternalIPs []string `json:"externalIPs,omitempty"`
}

// EgressRule allows game pods to reach an in-cluster destination.
type EgressRule struct {
	CIDR  string  `json:"cidr"`
	Ports []int32 `json:"ports,omitempty"`
}

// InClusterEgressSpec lists in-cluster destinations game pods may reach.
type InClusterEgressSpec struct {
	// GameServers allows traffic to other GameServer pods on their allocation ports.
	// +kubebuilder:default=true
	GameServers *bool        `json:"gameServers,omitempty"`
	Additional  []EgressRule `json:"additional,omitempty"`
}

// NetworkSpec configures NetworkPolicies.
type NetworkSpec struct {
	InClusterEgress InClusterEgressSpec `json:"inClusterEgress,omitempty"`
	// BlockedEgressCIDRs are excluded from the default 0.0.0.0/0 egress of game pods
	// (pod CIDR, service CIDR, node and LAN ranges). Link-local is always blocked.
	BlockedEgressCIDRs []string `json:"blockedEgressCIDRs,omitempty"`
	// NodeCIDRs are admitted on the agent HTTP port for kubelet probes and hooks.
	NodeCIDRs []string `json:"nodeCIDRs,omitempty"`
	// Enabled controls whether per-server NetworkPolicies are created.
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"`
}

// ContainerResources are simple resource amounts.
type ContainerResources struct {
	CPU         resource.Quantity `json:"cpu,omitempty"`
	Memory      resource.Quantity `json:"memory,omitempty"`
	MemoryLimit resource.Quantity `json:"memoryLimit,omitempty"`
}

// ResourcesSpec maps Panel build limits to pod resources.
type ResourcesSpec struct {
	// MemoryOverheadPercent grows the memory limit beyond memory_limit (Wings docker.overhead).
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=0
	MemoryOverheadPercent int32 `json:"memoryOverheadPercent,omitempty"`
	// CPURequestPercentOfLimit is the overcommit knob.
	// +kubebuilder:default=25
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	CPURequestPercentOfLimit int32 `json:"cpuRequestPercentOfLimit,omitempty"`
	// UnlimitedMemoryMiB is used when memory_limit is 0.
	// +kubebuilder:default=4096
	// +kubebuilder:validation:Minimum=1
	UnlimitedMemoryMiB int32 `json:"unlimitedMemoryMiB,omitempty"`
	// UnlimitedCPUPercent is used when cpu_limit is 0; 0 means no limit.
	// +kubebuilder:validation:Minimum=0
	UnlimitedCPUPercent int32 `json:"unlimitedCpuPercent,omitempty"`
	// +kubebuilder:default="100m"
	MinCPU resource.Quantity `json:"minCpu,omitempty"`
	// +kubebuilder:default=100
	TmpSizeMiB int32              `json:"tmpSizeMiB,omitempty"`
	Agent      ContainerResources `json:"agent,omitempty"`
}

// SecuritySpec pins the UID game pods run as.
type SecuritySpec struct {
	// RunAsUser is the pinned non-root UID and GID.
	// +kubebuilder:default=1000
	// +kubebuilder:validation:Minimum=1
	RunAsUser int64 `json:"runAsUser,omitempty"`
	// GeneratePasswdEntry generates an /etc/passwd entry for the UID inside the game container.
	// +kubebuilder:default=true
	GeneratePasswdEntry *bool `json:"generatePasswdEntry,omitempty"`
	// UseNamespaceUIDRange reads the OpenShift UID range annotation of the namespace and
	// uses its start instead of RunAsUser.
	UseNamespaceUIDRange bool `json:"useNamespaceUIDRange,omitempty"`
}

// InstallJobSpec configures install Jobs.
type InstallJobSpec struct {
	// +kubebuilder:default=pelican-installer
	ServiceAccountName string             `json:"serviceAccountName,omitempty"`
	Resources          ContainerResources `json:"resources,omitempty"`
	// StrictExitCode makes a non-zero script exit code fail the install (Wings ignores it).
	StrictExitCode bool `json:"strictExitCode,omitempty"`
	// +kubebuilder:default=3600
	ActiveDeadlineSeconds int64 `json:"activeDeadlineSeconds,omitempty"`
	// PrepareTimeoutSeconds bounds how long the operator waits for the agent to report prepared.
	// +kubebuilder:default=600
	PrepareTimeoutSeconds int64 `json:"prepareTimeoutSeconds,omitempty"`
	// RunAsRoot runs the install container as UID 0 (egg scripts expect it).
	// +kubebuilder:default=true
	RunAsRoot *bool `json:"runAsRoot,omitempty"`
	// DisableSeccomp leaves the seccomp profile unset on install pods. OpenShift's
	// anyuid SCC (needed for root installs) rejects pods that set one.
	DisableSeccomp bool `json:"disableSeccomp,omitempty"`
}

// FailoverSpec configures node-loss handling.
type FailoverSpec struct {
	// ForceDeleteAfter force-deletes a pod stuck Terminating on a NotReady node (e.g. 5m). Empty = never.
	ForceDeleteAfter *metav1.Duration `json:"forceDeleteAfter,omitempty"`
}

// ImageResolutionSpec configures how the operator finds the image ENTRYPOINT/CMD.
type ImageResolutionSpec struct {
	// RegistryLookup resolves the image config from the registry.
	RegistryLookup bool     `json:"registryLookup,omitempty"`
	PullSecrets    []string `json:"pullSecrets,omitempty"`
	// EntrypointOverrides maps an image reference glob to the argv to run.
	EntrypointOverrides map[string][]string `json:"entrypointOverrides,omitempty"`
	// PinDigest resolves tags to digests on every pod creation (Wings' pull-on-start).
	// +kubebuilder:default=true
	PinDigest *bool `json:"pinDigest,omitempty"`
}

// ImagesSpec names the pelican-k8s images injected into game pods.
type ImagesSpec struct {
	Shim  string `json:"shim,omitempty"`
	Agent string `json:"agent,omitempty"`
	// +kubebuilder:default=IfNotPresent
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
}

// GameServerClassSpec is the cluster-side policy for a set of GameServers.
type GameServerClassSpec struct {
	Storage         StorageSpec         `json:"storage,omitempty"`
	Exposure        ExposureSpec        `json:"exposure,omitempty"`
	Network         NetworkSpec         `json:"network,omitempty"`
	Resources       ResourcesSpec       `json:"resources,omitempty"`
	Security        SecuritySpec        `json:"security,omitempty"`
	Install         InstallJobSpec      `json:"install,omitempty"`
	Failover        FailoverSpec        `json:"failover,omitempty"`
	ImageResolution ImageResolutionSpec `json:"imageResolution,omitempty"`
	Images          ImagesSpec          `json:"images,omitempty"`
	// ServiceAccountName is the ServiceAccount of game pods.
	// +kubebuilder:default=pelican-game
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
	// AgentConfigMap names the ConfigMap holding the agent's Wings config.yml.
	// +kubebuilder:default=pelican-agent-config
	AgentConfigMap string `json:"agentConfigMap,omitempty"`
	// SuspendScalesToZero deletes the pod of a suspended server.
	SuspendScalesToZero bool `json:"suspendScalesToZero,omitempty"`
	// TerminationGracePeriodSeconds must exceed Wings' 10 minute stop wait.
	// +kubebuilder:default=660
	TerminationGracePeriodSeconds int64 `json:"terminationGracePeriodSeconds,omitempty"`
	// NodeSelector, Tolerations and PriorityClassName are applied to game pods.
	NodeSelector      map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations       []corev1.Toleration `json:"tolerations,omitempty"`
	PriorityClassName string              `json:"priorityClassName,omitempty"`
}

// GameServerClass is admin-owned, cluster-scoped policy referenced by GameServer.spec.className.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=gsc
// +kubebuilder:printcolumn:name="Exposure",type=string,JSONPath=`.spec.exposure.mode`
// +kubebuilder:printcolumn:name="StorageClass",type=string,JSONPath=`.spec.storage.storageClassName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type GameServerClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec GameServerClassSpec `json:"spec,omitempty"`
}

// GameServerClassList contains a list of GameServerClass.
//
// +kubebuilder:object:root=true
type GameServerClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GameServerClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GameServerClass{}, &GameServerClassList{})
}
