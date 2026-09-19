package render

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
)

// StatefulSet renders the game pod controller (section 7.5).
func StatefulSet(in *Input) *appsv1.StatefulSet {
	tmpl := PodTemplate(in)
	sts := &appsv1.StatefulSet{
		ObjectMeta: in.Meta(names.StatefulSet(in.UUID()), "game"),
		Spec: appsv1.StatefulSetSpec{
			Replicas:            int32Ptr(1),
			ServiceName:         names.AgentService(in.UUID()),
			Selector:            &metav1.LabelSelector{MatchLabels: map[string]string{v1alpha1.LabelServerUUID: in.UUID(), v1alpha1.LabelComponent: "game"}},
			Template:            tmpl,
			UpdateStrategy:      appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType},
			PodManagementPolicy: appsv1.ParallelPodManagement,
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
		},
	}
	return sts
}

// TemplateHash hashes the pod template parts that require a pod recreate
// (everything except the resources, which are resized in place).
func TemplateHash(tmpl corev1.PodTemplateSpec) string {
	c := tmpl.DeepCopy()
	for i := range c.Spec.Containers {
		c.Spec.Containers[i].Resources = corev1.ResourceRequirements{}
	}
	delete(c.Annotations, AnnotationTemplateHash)
	return Hash(c)
}

// PodTemplate renders the game pod template.
func PodTemplate(in *Input) corev1.PodTemplateSpec {
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
	grace := cls.TerminationGracePeriodSeconds
	if grace <= 0 {
		grace = 660
	}
	tmpMiB := cls.Resources.TmpSizeMiB
	if tmpMiB <= 0 {
		tmpMiB = 100
	}
	agentCfg := cls.AgentConfigMap
	if agentCfg == "" {
		agentCfg = "pelican-agent-config"
	}
	pullPolicy := cls.Images.PullPolicy
	if pullPolicy == "" {
		pullPolicy = corev1.PullIfNotPresent
	}
	gamePull := corev1.PullAlways
	if in.NeverPull || strings.Contains(in.Image, "@sha256:") {
		gamePull = corev1.PullIfNotPresent
	}
	generatePasswd := cls.Security.GeneratePasswdEntry == nil || *cls.Security.GeneratePasswdEntry

	restricted := &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}

	pvcSize := PVCSize(in.Settings.Build, cls.Storage)
	volumes := []corev1.Volume{
		{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: names.PVC(uuid)}}},
		{Name: "pelican", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: resource.NewQuantity(int64(tmpMiB)*1024*1024, resource.BinarySI)}}},
		{Name: "agent-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: agentCfg}}}},
		scratchVolume(cls.Storage, pvcSize),
	}

	prepare := corev1.Container{
		Name:            "prepare",
		Image:           cls.Images.Shim,
		ImagePullPolicy: pullPolicy,
		Command:         []string{"/shim", "prepare", "--bin", "/pelican/bin/shim", "--shared", "/pelican", "--data", "/data", "--uuid", uuid},
		SecurityContext: restricted,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "pelican", MountPath: "/pelican"},
			{Name: "data", MountPath: "/data"},
		},
		Resources: smallResources(),
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

	always := corev1.ContainerRestartPolicyAlways
	agent := corev1.Container{
		Name:            AgentContainer,
		Image:           cls.Images.Agent,
		ImagePullPolicy: pullPolicy,
		RestartPolicy:   &always,
		Args:            []string{"--config", "/etc/pelican/config.yml", "--shim-socket", ShimSocket},
		Ports: []corev1.ContainerPort{
			{Name: "agent", ContainerPort: AgentPort, Protocol: corev1.ProtocolTCP},
			{Name: "sftp", ContainerPort: SFTPPort, Protocol: corev1.ProtocolTCP},
		},
		StartupProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/internal/v1/healthz", Port: intstr.FromString("agent")}}, PeriodSeconds: 2, FailureThreshold: 60},
		LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/internal/v1/healthz", Port: intstr.FromString("agent")}}, PeriodSeconds: 10, FailureThreshold: 6},
		Lifecycle:     &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/internal/v1/prestop", Port: intstr.FromString("agent")}}},
		Env: []corev1.EnvVar{
			{Name: "PELICAN_SERVER_UUID", Value: uuid},
			{Name: "PELICAN_POD_IMAGE", Value: in.Image},
			{Name: "PELICAN_POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
			{Name: "WINGS_TOKEN_ID", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: names.AgentSecret(uuid)}, Key: "token_id"}}},
			{Name: "WINGS_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: names.AgentSecret(uuid)}, Key: "token"}}},
		},
		SecurityContext: restricted,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: AgentRoot},
			{Name: "scratch", MountPath: ScratchDir},
			{Name: "pelican", MountPath: "/pelican/run", SubPath: "run"},
			{Name: "agent-config", MountPath: "/etc/pelican", ReadOnly: true},
			{Name: "tmp", MountPath: "/tmp"},
		},
		Resources: agentResources(cls.Resources.Agent),
	}

	gameMounts := []corev1.VolumeMount{
		{Name: "data", MountPath: ContainerHome, SubPath: "volumes/" + uuid},
		{Name: "data", MountPath: "/etc/machine-id", SubPath: "machine-id", ReadOnly: true},
		{Name: "pelican", MountPath: "/pelican/bin", SubPath: "bin", ReadOnly: true},
		{Name: "pelican", MountPath: "/pelican/etc", SubPath: "etc", ReadOnly: true},
		{Name: "pelican", MountPath: "/pelican/run", SubPath: "run"},
		{Name: "tmp", MountPath: "/tmp"},
	}
	if generatePasswd {
		gameMounts = append(gameMounts,
			corev1.VolumeMount{Name: "pelican", MountPath: "/etc/passwd", SubPath: "etc/passwd", ReadOnly: true},
			corev1.VolumeMount{Name: "pelican", MountPath: "/etc/group", SubPath: "etc/group", ReadOnly: true},
		)
	}
	gameCmd := []string{"/pelican/bin/shim", "run", "--socket", ShimSocket, "--argv-file", ArgvFile, "--dir", ContainerHome, "--"}
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
		// done line), so a stopped or still starting server shows 1/2 instead of
		// looking healthy. Readiness never restarts anything: only
		// liveness and startup probes do, and the game container must never get
		// either (a stopped server would be killed in a loop). Nothing else may
		// depend on it: both Services publish not-ready addresses, the operator
		// judges the agent by its own container status, and the StatefulSet is
		// OnDelete + Parallel so an unready pod never blocks a recreate.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/internal/v1/ready", Port: intstr.FromInt(AgentPort)}},
			PeriodSeconds:    5,
			TimeoutSeconds:   3,
			FailureThreshold: 1,
		},
		Lifecycle:       &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/internal/v1/prestop", Port: intstr.FromInt(AgentPort)}}},
		SecurityContext: restricted,
		Env: []corev1.EnvVar{
			{Name: "HOME", Value: ContainerHome},
			{Name: "USER", Value: "container"},
		},
		VolumeMounts: gameMounts,
	}

	labels := in.Labels("game")
	annotations := map[string]string{}
	if in.Settings.Meta.Name != "" {
		annotations[v1alpha1.AnnotationPanelName] = in.Settings.Meta.Name
	}
	tmpl := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken:  boolPtr(false),
			ServiceAccountName:            sa,
			EnableServiceLinks:            boolPtr(false),
			TerminationGracePeriodSeconds: int64Ptr(grace),
			RestartPolicy:                 corev1.RestartPolicyAlways,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:        boolPtr(true),
				RunAsUser:           int64Ptr(uid),
				RunAsGroup:          int64Ptr(uid),
				FSGroup:             int64Ptr(uid),
				FSGroupChangePolicy: fsGroupPolicy(corev1.FSGroupChangeOnRootMismatch),
				SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			InitContainers:    []corev1.Container{prepare, probe, agent},
			Containers:        []corev1.Container{game},
			Volumes:           volumes,
			NodeSelector:      cls.NodeSelector,
			Tolerations:       cls.Tolerations,
			PriorityClassName: cls.PriorityClassName,
		},
	}
	for _, s := range cls.ImageResolution.PullSecrets {
		tmpl.Spec.ImagePullSecrets = append(tmpl.Spec.ImagePullSecrets, corev1.LocalObjectReference{Name: s})
	}
	tmpl.Annotations[AnnotationTemplateHash] = TemplateHash(tmpl)
	return tmpl
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
