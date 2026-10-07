package render

import (
	"fmt"
	"path"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/internal/pki"
	"github.com/Claiyc/pelican-k8s/internal/shim/protocol"
)

// GameAffinity is the game pod's pod affinity toward its agent pod (section 7.7).
type GameAffinity int

const (
	// GameAffinityNone gives the agent's node no advantage.
	GameAffinityNone GameAffinity = iota
	// GameAffinityPreferred prefers the agent's node when the game pod fits there.
	GameAffinityPreferred
	// GameAffinityRequired keeps the game pod Pending until it fits on the agent's node.
	GameAffinityRequired
)

// Component label values of the two pods.
const (
	ComponentAgent = "agent"
	ComponentGame  = "game"
)

// AgentStatefulSet renders the agent pod controller (section 7.4).
func AgentStatefulSet(in *Input) *appsv1.StatefulSet {
	return statefulSet(in, names.AgentStatefulSet(in.UUID()), names.AgentService(in.UUID()), ComponentAgent, AgentPodTemplate(in))
}

// GameStatefulSet renders the game pod controller (section 7.4).
func GameStatefulSet(in *Input) *appsv1.StatefulSet {
	return statefulSet(in, names.StatefulSet(in.UUID()), names.ExposureService(in.UUID()), ComponentGame, GamePodTemplate(in))
}

func statefulSet(in *Input, name, serviceName, component string, tmpl corev1.PodTemplateSpec) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: in.Meta(name, component),
		Spec: appsv1.StatefulSetSpec{
			Replicas:            int32Ptr(1),
			ServiceName:         serviceName,
			Selector:            &metav1.LabelSelector{MatchLabels: map[string]string{v1alpha1.LabelServerUUID: in.UUID(), v1alpha1.LabelComponent: component}},
			Template:            tmpl,
			UpdateStrategy:      appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType},
			PodManagementPolicy: appsv1.ParallelPodManagement,
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
		},
	}
}

// TemplateHash hashes the pod template parts that require a pod recreate. It
// leaves out the game container's resources, which are resized in place, and
// the placement toward the other pod (the game pod's pod affinity, the agent
// pod's node affinity), which is set per start (section 7.7). The game pod's
// node affinity to the allocation nodes stays in.
func TemplateHash(tmpl corev1.PodTemplateSpec) string {
	c := tmpl.DeepCopy()
	for i := range c.Spec.Containers {
		if c.Spec.Containers[i].Name == GameContainer {
			c.Spec.Containers[i].Resources = corev1.ResourceRequirements{}
		}
	}
	if a := c.Spec.Affinity; a != nil {
		a.PodAffinity = nil
		if c.Labels[v1alpha1.LabelComponent] == ComponentAgent {
			a.NodeAffinity = nil
		}
		if a.NodeAffinity == nil && a.PodAntiAffinity == nil {
			c.Spec.Affinity = nil
		}
	}
	delete(c.Annotations, AnnotationTemplateHash)
	return Hash(c)
}

// podSpec returns the pod-level settings both pods share (section 7.5).
func podSpec(in *Input) corev1.PodSpec {
	cls := in.Class.Spec
	uid := in.UID
	spec := corev1.PodSpec{
		AutomountServiceAccountToken:  boolPtr(false),
		EnableServiceLinks:            boolPtr(false),
		TerminationGracePeriodSeconds: int64Ptr(gracePeriod(cls)),
		RestartPolicy:                 corev1.RestartPolicyAlways,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:        boolPtr(true),
			RunAsUser:           int64Ptr(uid),
			RunAsGroup:          int64Ptr(uid),
			FSGroup:             int64Ptr(uid),
			FSGroupChangePolicy: fsGroupPolicy(corev1.FSGroupChangeOnRootMismatch),
			SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		NodeSelector: cls.NodeSelector,
		Tolerations:  cls.Tolerations,
	}
	for _, s := range cls.ImageResolution.PullSecrets {
		spec.ImagePullSecrets = append(spec.ImagePullSecrets, corev1.LocalObjectReference{Name: s})
	}
	return spec
}

func podTemplate(in *Input, component string, spec corev1.PodSpec) corev1.PodTemplateSpec {
	annotations := map[string]string{}
	if in.Settings.Meta.Name != "" {
		annotations[v1alpha1.AnnotationPanelName] = in.Settings.Meta.Name
	}
	tmpl := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: in.Labels(component), Annotations: annotations},
		Spec:       spec,
	}
	tmpl.Annotations[AnnotationTemplateHash] = TemplateHash(tmpl)
	return tmpl
}

func gracePeriod(cls v1alpha1.GameServerClassSpec) int64 {
	if cls.TerminationGracePeriodSeconds <= 0 {
		return 660
	}
	return cls.TerminationGracePeriodSeconds
}

func pullPolicy(cls v1alpha1.GameServerClassSpec) corev1.PullPolicy {
	if cls.Images.PullPolicy == "" {
		return corev1.PullIfNotPresent
	}
	return cls.Images.PullPolicy
}

func restrictedContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

func dataVolume(uuid string) corev1.Volume {
	return corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: names.PVC(uuid)}}}
}

// AgentPodTemplate renders the agent pod template. In.AgentNode, when set,
// becomes a required node affinity: the agent follows its game pod.
func AgentPodTemplate(in *Input) corev1.PodTemplateSpec {
	cls := in.Class.Spec
	uuid := in.UUID()
	sa := cls.AgentServiceAccountName
	if sa == "" {
		sa = "pelican-agent"
	}
	priority := cls.AgentPriorityClassName
	if priority == "" {
		priority = "pelican-agent"
	}
	agentCfg := cls.AgentConfigMap
	if agentCfg == "" {
		agentCfg = "pelican-agent-config"
	}
	restricted := restrictedContext()
	pvcSize := PVCSize(in.Settings.Build, cls.Storage)

	prepare := corev1.Container{
		Name:            "prepare",
		Image:           cls.Images.Shim,
		ImagePullPolicy: pullPolicy(cls),
		Command:         []string{"/shim", "prepare", "--data", "/data", "--uuid", uuid},
		SecurityContext: restricted,
		VolumeMounts:    []corev1.VolumeMount{{Name: "data", MountPath: "/data"}},
		Resources:       smallResources(),
	}
	agent := corev1.Container{
		Name:            AgentContainer,
		Image:           cls.Images.Agent,
		ImagePullPolicy: pullPolicy(cls),
		Args:            []string{"--config", "/etc/pelican/config.yml", "--shim-listen", fmt.Sprintf(":%d", ShimPort)},
		Ports: []corev1.ContainerPort{
			{Name: "agent", ContainerPort: AgentPort, Protocol: corev1.ProtocolTCP},
			{Name: "sftp", ContainerPort: SFTPPort, Protocol: corev1.ProtocolTCP},
			{Name: "shim", ContainerPort: ShimPort, Protocol: corev1.ProtocolTCP},
		},
		StartupProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/internal/v1/healthz", Port: intstr.FromString("agent")}}, PeriodSeconds: 2, FailureThreshold: 60},
		LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/internal/v1/healthz", Port: intstr.FromString("agent")}}, PeriodSeconds: 10, FailureThreshold: 6},
		Lifecycle:     &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/internal/v1/prestop", Port: intstr.FromString("agent")}}},
		Env: []corev1.EnvVar{
			{Name: "PELICAN_SERVER_UUID", Value: uuid},
			{Name: "WINGS_TOKEN_ID", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: names.AgentSecret(uuid)}, Key: "token_id"}}},
			{Name: "WINGS_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: names.AgentSecret(uuid)}, Key: "token"}}},
			shimTokenEnv(uuid),
		},
		SecurityContext: restricted,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: AgentRoot},
			{Name: "scratch", MountPath: ScratchDir},
			{Name: "agent-config", MountPath: "/etc/pelican", ReadOnly: true},
			{Name: "tmp", MountPath: "/tmp"},
		},
		Resources: agentResources(cls.Resources.Agent),
	}

	if in.TLS {
		agent.Args = append(agent.Args, "--tls-dir", AgentTLSDir)
		for _, p := range []*corev1.Probe{agent.StartupProbe, agent.LivenessProbe} {
			p.HTTPGet.Scheme = corev1.URISchemeHTTPS
		}
		agent.Lifecycle.PreStop.HTTPGet.Scheme = corev1.URISchemeHTTPS
		agent.VolumeMounts = append(agent.VolumeMounts, corev1.VolumeMount{Name: "tls", MountPath: AgentTLSDir, ReadOnly: true})
	}

	spec := podSpec(in)
	spec.ServiceAccountName = sa
	spec.PriorityClassName = priority
	spec.InitContainers = []corev1.Container{prepare}
	spec.Containers = []corev1.Container{agent}
	spec.Volumes = []corev1.Volume{
		dataVolume(uuid),
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "agent-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: agentCfg}}}},
		scratchVolume(cls.Storage, pvcSize),
	}
	if in.TLS {
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: names.TLSSecret(uuid)}}})
	}
	if in.AgentNode != "" {
		spec.Affinity = &corev1.Affinity{NodeAffinity: nodeNameAffinity([]string{in.AgentNode})}
	}
	return podTemplate(in, ComponentAgent, spec)
}

// GamePodTemplate renders the game pod template. In.GameAffinity sets its pod
// affinity toward the agent pod.
func GamePodTemplate(in *Input) corev1.PodTemplateSpec {
	cls := in.Class.Spec
	uuid := in.UUID()
	uid := in.UID
	sa := cls.ServiceAccountName
	if sa == "" {
		sa = "pelican-game"
	}
	if cls.Exposure.Mode == v1alpha1.ExposureHostPort {
		sa += "-hostport"
	}
	tmpMiB := cls.Resources.TmpSizeMiB
	if tmpMiB <= 0 {
		tmpMiB = 100
	}
	gamePull := corev1.PullAlways
	if in.NeverPull || strings.Contains(in.Image, "@sha256:") {
		gamePull = corev1.PullIfNotPresent
	}
	generatePasswd := cls.Security.GeneratePasswdEntry == nil || *cls.Security.GeneratePasswdEntry
	restricted := restrictedContext()

	prepare := corev1.Container{
		Name:            "prepare",
		Image:           cls.Images.Shim,
		ImagePullPolicy: pullPolicy(cls),
		Command:         []string{"/shim", "prepare", "--bin", "/pelican/bin/shim", "--shared", "/pelican"},
		SecurityContext: restricted,
		VolumeMounts:    []corev1.VolumeMount{{Name: "pelican", MountPath: "/pelican"}},
		Resources:       smallResources(),
	}

	probeCmd := []string{"/pelican/bin/shim", "probe", "--out", ArgvFile, "--name", "container", "--home", ContainerHome, "--uid", fmtUID(uid), "--gid", fmtUID(uid)}
	if generatePasswd {
		probeCmd = append(probeCmd, "--passwd", PasswdFile, "--group", GroupFile)
	}
	if len(in.Argv) > 0 {
		probeCmd = append(append(probeCmd, "--"), in.Argv...)
	}
	probe := corev1.Container{
		Name:            "probe-entrypoint",
		Image:           in.Image,
		ImagePullPolicy: gamePull,
		Command:         probeCmd,
		SecurityContext: restricted,
		VolumeMounts:    []corev1.VolumeMount{{Name: "pelican", MountPath: "/pelican"}},
		Resources:       smallResources(),
	}

	gameMounts := []corev1.VolumeMount{
		{Name: "data", MountPath: ContainerHome, SubPath: "volumes/" + uuid},
		{Name: "data", MountPath: "/etc/machine-id", SubPath: "machine-id", ReadOnly: true},
		{Name: "pelican", MountPath: "/pelican/bin", SubPath: "bin", ReadOnly: true},
		{Name: "pelican", MountPath: "/pelican/etc", SubPath: "etc", ReadOnly: true},
		// The shim's readiness file.
		{Name: "pelican", MountPath: "/pelican/run", SubPath: "run"},
		{Name: "tmp", MountPath: "/tmp"},
	}
	if generatePasswd {
		gameMounts = append(gameMounts,
			corev1.VolumeMount{Name: "pelican", MountPath: "/etc/passwd", SubPath: "etc/passwd", ReadOnly: true},
			corev1.VolumeMount{Name: "pelican", MountPath: "/etc/group", SubPath: "etc/group", ReadOnly: true},
		)
	}
	agentAddr := fmt.Sprintf("%s:%d", names.AgentService(uuid), ShimPort)
	gameCmd := []string{"/pelican/bin/shim", "run", "--agent", agentAddr, "--argv-file", ArgvFile, "--dir", ContainerHome, "--grace-period", fmt.Sprintf("%ds", gracePeriod(cls))}
	if in.TLS {
		// Only the CA bundle: the agent's key never enters the game pod.
		gameCmd = append(gameCmd, "--agent-ca", GameCAFile)
		gameMounts = append(gameMounts, corev1.VolumeMount{Name: "agent-ca", MountPath: path.Dir(GameCAFile), ReadOnly: true})
	}
	gameCmd = append(gameCmd, "--")
	game := corev1.Container{
		Name:            GameContainer,
		Image:           in.Image,
		ImagePullPolicy: gamePull,
		Command:         gameCmd,
		Args:            in.Argv,
		Stdin:           false,
		Ports:           gamePorts(in),
		Resources:       GameResources(in.Settings.Build, cls.Resources),
		ResizePolicy: []corev1.ContainerResizePolicy{
			{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.NotRequired},
			{ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.NotRequired},
		},
		// Ready means what the Panel calls "running" (the agent saw the egg's
		// done line and told the shim), so a starting or stopping server is
		// not ready. Readiness never restarts anything: only liveness and
		// startup probes do, and the game container must never get either.
		// Nothing else may depend on it: the Services publish not-ready
		// addresses and the StatefulSet is OnDelete + Parallel, so an unready
		// pod never blocks a recreate. There is no preStop hook: on SIGTERM
		// the shim stops the process itself with the stop configuration.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"/pelican/bin/shim", "ready"}}},
			PeriodSeconds:    5,
			TimeoutSeconds:   3,
			FailureThreshold: 1,
		},
		SecurityContext: restricted,
		Env: []corev1.EnvVar{
			{Name: "HOME", Value: ContainerHome},
			{Name: "USER", Value: "container"},
			// The shim sends the pod UID in its handshake, so the agent and
			// the operator know which game pod holds the connection.
			{Name: "PELICAN_POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}},
			{Name: "INTERNAL_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
			// Read by the shim, which removes it from the game process environment.
			shimTokenEnv(uuid),
		},
		VolumeMounts: gameMounts,
	}

	spec := podSpec(in)
	spec.ServiceAccountName = sa
	spec.PriorityClassName = cls.PriorityClassName
	spec.InitContainers = []corev1.Container{prepare, probe}
	spec.Containers = []corev1.Container{game}
	spec.Volumes = []corev1.Volume{
		dataVolume(uuid),
		{Name: "pelican", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: resource.NewQuantity(int64(tmpMiB)*1024*1024, resource.BinarySI)}}},
	}
	if in.TLS {
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "agent-ca", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: names.TLSSecret(uuid),
			Items:      []corev1.KeyToPath{{Key: pki.CAFile, Path: path.Base(GameCAFile)}},
		}}})
	}
	affinity := &corev1.Affinity{}
	if len(in.NodeNames) > 0 {
		affinity.NodeAffinity = nodeNameAffinity(in.NodeNames)
	}
	agentTerm := corev1.PodAffinityTerm{
		LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{v1alpha1.LabelServerUUID: uuid, v1alpha1.LabelComponent: ComponentAgent}},
		TopologyKey:   corev1.LabelHostname,
	}
	switch in.GameAffinity {
	case GameAffinityRequired:
		affinity.PodAffinity = &corev1.PodAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{agentTerm}}
	case GameAffinityPreferred:
		affinity.PodAffinity = &corev1.PodAffinity{PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{Weight: 100, PodAffinityTerm: agentTerm}}}
	}
	if affinity.NodeAffinity != nil || affinity.PodAffinity != nil {
		spec.Affinity = affinity
	}
	return podTemplate(in, ComponentGame, spec)
}

func nodeNameAffinity(nodes []string) *corev1.NodeAffinity {
	return &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: nodes}},
		}}},
	}
}

func shimTokenEnv(uuid string) corev1.EnvVar {
	return corev1.EnvVar{Name: protocol.TokenEnv, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: names.ShimSecret(uuid)}, Key: ShimTokenKey,
	}}}
}

func gamePorts(in *Input) []corev1.ContainerPort {
	var out []corev1.ContainerPort
	hostPort := in.Class.Spec.Exposure.Mode == v1alpha1.ExposureHostPort
	for _, p := range in.Settings.Ports() {
		for _, proto := range []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP} {
			cp := corev1.ContainerPort{Name: portName(p, proto), ContainerPort: p, Protocol: proto}
			if hostPort {
				cp.HostPort = p
			}
			out = append(out, cp)
		}
	}
	return out
}

func scratchVolume(s v1alpha1.StorageSpec, pvcSize resource.Quantity) corev1.Volume {
	if s.Scratch.Type == v1alpha1.ScratchEmptyDir {
		size := ScratchSize(pvcSize, s)
		return corev1.Volume{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &size}}}
	}
	spec := corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: ScratchSize(pvcSize, s)}},
	}
	sc := s.Scratch.StorageClassName
	if sc == "" {
		sc = s.StorageClassName
	}
	if sc != "" {
		spec.StorageClassName = &sc
	}
	return corev1.Volume{Name: "scratch", VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{
		VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{LabelPartOf: PartOfValue, v1alpha1.LabelComponent: "scratch"}},
			Spec:       spec,
		},
	}}}
}

func agentResources(r v1alpha1.ContainerResources) corev1.ResourceRequirements {
	cpu, mem, memLimit := r.CPU, r.Memory, r.MemoryLimit
	if cpu.IsZero() {
		cpu = resource.MustParse("50m")
	}
	if mem.IsZero() {
		mem = resource.MustParse("128Mi")
	}
	if memLimit.IsZero() {
		memLimit = resource.MustParse("512Mi")
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: mem},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: memLimit},
	}
}

func smallResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
	}
}

func fsGroupPolicy(p corev1.PodFSGroupChangePolicy) *corev1.PodFSGroupChangePolicy { return &p }

// GameContainerIndex returns the index of the game container in a pod spec.
func GameContainerIndex(spec *corev1.PodSpec) int {
	for i, c := range spec.Containers {
		if c.Name == GameContainer {
			return i
		}
	}
	return -1
}

// ContainerImageRef renders a digest-pinned reference.
func ContainerImageRef(image, digest string) string {
	if digest == "" {
		return image
	}
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	return fmt.Sprintf("%s@%s", image, digest)
}
