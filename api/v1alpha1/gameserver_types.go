package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PowerState is the desired process state of a server.
// +kubebuilder:validation:Enum=Running;Stopped
type PowerState string

const (
	PowerRunning PowerState = "Running"
	PowerStopped PowerState = "Stopped"
)

// Phase is a coarse summary of a GameServer for kubectl output.
type Phase string

const (
	PhasePending    Phase = "Pending"
	PhaseInstalling Phase = "Installing"
	PhaseStopped    Phase = "Stopped"
	PhaseStarting   Phase = "Starting"
	PhaseRunning    Phase = "Running"
	PhaseStopping   Phase = "Stopping"
	PhaseSuspended  Phase = "Suspended"
	PhaseError      Phase = "Error"
)

// Process states as reported by Wings.
const (
	ProcessOffline  = "offline"
	ProcessStarting = "starting"
	ProcessRunning  = "running"
	ProcessStopping = "stopping"
)

// Install results.
const (
	InstallRunning   = "Running"
	InstallSucceeded = "Succeeded"
	InstallFailed    = "Failed"
)

// Condition types set on a GameServer.
const (
	ConditionVolumeReady     = "VolumeReady"
	ConditionExposureReady   = "ExposureReady"
	ConditionAgentReady      = "AgentReady"
	ConditionInstallPrepared = "InstallPrepared"
	ConditionInstalled       = "Installed"
	ConditionResizePending   = "ResizePending"
	ConditionRecreatePending = "RecreatePending"
	ConditionNodeLost        = "NodeLost"
	ConditionOrphaned        = "Orphaned"
	ConditionDiskShrink      = "DiskShrinkRefused"
)

// PanelSpec mirrors the server as the Panel describes it. It is written by the
// gateway and must not be edited by hand.
type PanelSpec struct {
	// UUID is the Panel server UUID.
	// +kubebuilder:validation:MinLength=36
	// +kubebuilder:validation:MaxLength=36
	UUID string `json:"uuid"`
	// UUIDShort is the first eight characters of the UUID (the SFTP username suffix).
	UUIDShort string `json:"uuidShort"`
	// Settings is the raw `settings` object from GET /api/remote/servers/{uuid}
	// minus `environment`. The agent consumes it unchanged as its Wings server
	// configuration; the operator reads only suspended, container.image, build.*
	// and allocations.*.
	// +kubebuilder:pruning:PreserveUnknownFields
	Settings apiextensionsv1.JSON `json:"settings"`
	// EnvironmentSecretRef names the Secret holding the egg variables (`settings.environment`).
	EnvironmentSecretRef corev1.LocalObjectReference `json:"environmentSecretRef"`
	// ProcessConfiguration is the raw `process_configuration` object from the Panel.
	// +kubebuilder:pruning:PreserveUnknownFields
	ProcessConfiguration apiextensionsv1.JSON `json:"processConfiguration"`
	// PanelRevision is a hash of the settings and process configuration last applied.
	PanelRevision string `json:"panelRevision,omitempty"`
}

// PowerSpec is the desired process state.
type PowerSpec struct {
	// Desired is Running or Stopped.
	// +kubebuilder:default=Stopped
	Desired PowerState `json:"desired,omitempty"`
	// Generation is bumped together with Desired=Running to request a (re)start.
	// The operator acts once per generation.
	// +kubebuilder:validation:Minimum=0
	Generation int64 `json:"generation,omitempty"`
	// Kill requests SIGKILL instead of the graceful stop procedure when Desired is Stopped.
	Kill bool `json:"kill,omitempty"`
	// RestartRequest is bumped to force a pod recreate at the next safe point.
	// +kubebuilder:validation:Minimum=0
	RestartRequest int64 `json:"restartRequest,omitempty"`
}

// InstallSpec describes the pending or last install request.
type InstallSpec struct {
	// Generation is bumped to run a new install Job.
	// +kubebuilder:validation:Minimum=0
	Generation int64 `json:"generation,omitempty"`
	// Reinstall marks the request as a reinstall (the server is stopped first and
	// the Panel is told the result was a reinstall).
	Reinstall bool `json:"reinstall,omitempty"`
	// ScriptConfigMap names the ConfigMap holding install.sh for this generation.
	ScriptConfigMap string `json:"scriptConfigMap,omitempty"`
	// Image is the installer container image from the egg.
	Image string `json:"image,omitempty"`
	// Entrypoint is the installer entrypoint (bash, ash, ...).
	Entrypoint string `json:"entrypoint,omitempty"`
	// StartOnInstall requests a start once this generation has succeeded
	// (`start_on_completion` from the Panel's create call).
	StartOnInstall bool `json:"startOnInstall,omitempty"`
}

// GameServerSpec is the desired state of a GameServer.
type GameServerSpec struct {
	Panel PanelSpec `json:"panel"`
	// +kubebuilder:default={desired: Stopped}
	Power PowerSpec `json:"power,omitempty"`
	Install InstallSpec `json:"install,omitempty"`
	// ClassName references the GameServerClass supplying cluster-side policy.
	// +kubebuilder:default=default
	ClassName string `json:"className,omitempty"`
}

// ExitStatus is the last observed process exit.
type ExitStatus struct {
	Code      int32       `json:"code"`
	OOMKilled bool        `json:"oomKilled,omitempty"`
	At        metav1.Time `json:"at,omitempty"`
}

// ProcessStatus is what the agent reported about the process.
type ProcessStatus struct {
	// State is offline, starting, running or stopping.
	State    string       `json:"state,omitempty"`
	Since    *metav1.Time `json:"since,omitempty"`
	LastExit *ExitStatus  `json:"lastExit,omitempty"`
}

// PowerActionRecord is the last power action the operator issued.
type PowerActionRecord struct {
	Action string      `json:"action"`
	At     metav1.Time `json:"at"`
	PodUID string      `json:"podUID,omitempty"`
}

// PowerStatus tracks the operator's power actions.
type PowerStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	LastAction         *PowerActionRecord `json:"lastAction,omitempty"`
	// ObservedRestartRequest is the spec.power.restartRequest last acted on.
	ObservedRestartRequest int64 `json:"observedRestartRequest,omitempty"`
}

// AgentStatus tracks the agent the operator is driving.
type AgentStatus struct {
	// PodUID is the pod whose agent the operator last drove.
	PodUID string `json:"podUID,omitempty"`
	// SftpHostKey is the agent's SSH host key fingerprint pinned by the gateway.
	SftpHostKey string `json:"sftpHostKey,omitempty"`
	// RelayedExit identifies the last game container termination forwarded to the agent.
	RelayedExit string `json:"relayedExit,omitempty"`
	// SyncedRevision is the spec.panel.panelRevision last synced into the agent.
	SyncedRevision string `json:"syncedRevision,omitempty"`
	// SyncedEnvVersion is the resourceVersion of the env Secret last synced into the agent.
	SyncedEnvVersion string `json:"syncedEnvVersion,omitempty"`
}

// UsageStatus is a throttled resource usage summary.
type UsageStatus struct {
	MemoryBytes int64  `json:"memoryBytes,omitempty"`
	CPUPercent  string `json:"cpuPercent,omitempty"`
	DiskBytes   int64  `json:"diskBytes,omitempty"`
	UpdatedAt   *metav1.Time `json:"updatedAt,omitempty"`
}

// InstallStatus tracks install generations.
type InstallStatus struct {
	ObservedGeneration int64        `json:"observedGeneration,omitempty"`
	PreparedGeneration int64        `json:"preparedGeneration,omitempty"`
	Result             string       `json:"result,omitempty"`
	FinishedAt         *metav1.Time `json:"finishedAt,omitempty"`
	// JobName is the install Job of the current generation, if any.
	JobName string `json:"jobName,omitempty"`
	// RequestedGeneration is the generation the operator asked the agent to install.
	RequestedGeneration int64        `json:"requestedGeneration,omitempty"`
	RequestedAt         *metav1.Time `json:"requestedAt,omitempty"`
	// ReportedGeneration is the generation whose result the gateway reported to the Panel.
	ReportedGeneration int64 `json:"reportedGeneration,omitempty"`
}

// SnapshotStatus tracks scheduled VolumeSnapshots.
type SnapshotStatus struct {
	LastAt   *metav1.Time `json:"lastAt,omitempty"`
	LastName string       `json:"lastName,omitempty"`
}

// PendingBackup is a backup the agent is allowed to report on.
type PendingBackup struct {
	UUID      string      `json:"uuid"`
	StartedAt metav1.Time `json:"startedAt"`
}

// BackupsStatus tracks backups in flight.
type BackupsStatus struct {
	Pending []PendingBackup `json:"pending,omitempty"`
}

// Endpoint is an externally reachable game port.
type Endpoint struct {
	IP        string   `json:"ip"`
	Port      int32    `json:"port"`
	Protocols []string `json:"protocols,omitempty"`
}

// GameServerStatus is the observed state of a GameServer.
type GameServerStatus struct {
	ObservedGeneration int64         `json:"observedGeneration,omitempty"`
	Phase              Phase         `json:"phase,omitempty"`
	Process            ProcessStatus `json:"process,omitempty"`
	Power              PowerStatus   `json:"power,omitempty"`
	Agent              AgentStatus   `json:"agent,omitempty"`
	Usage              *UsageStatus  `json:"usage,omitempty"`
	Install            InstallStatus `json:"install,omitempty"`
	Backups            BackupsStatus `json:"backups,omitempty"`
	Endpoints          []Endpoint    `json:"endpoints,omitempty"`
	// PodImage is the digest-pinned image of the current pod.
	PodImage string `json:"podImage,omitempty"`
	// TemplateHash is the pod template hash the current StatefulSet carries.
	TemplateHash string          `json:"templateHash,omitempty"`
	Snapshot     *SnapshotStatus `json:"snapshot,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// GameServer is one Panel server: one pod, one PVC, one CR.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=gs
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Process",type=string,JSONPath=`.status.process.state`
// +kubebuilder:printcolumn:name="Desired",type=string,JSONPath=`.spec.power.desired`
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.className`
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.metadata.annotations.pelican-k8s\.io/panel-name`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type GameServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GameServerSpec   `json:"spec"`
	Status GameServerStatus `json:"status,omitempty"`
}

// GameServerList contains a list of GameServer.
//
// +kubebuilder:object:root=true
type GameServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GameServer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GameServer{}, &GameServerList{})
}
