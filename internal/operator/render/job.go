package render

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
)

// InstallJob renders the install Job for the given generation (section 8.2).
func InstallJob(in *Input, gen int64) *batchv1.Job {
	cls := in.Class.Spec
	uuid := in.UUID()
	name := names.InstallJob(uuid, gen)
	sa := cls.Install.ServiceAccountName
	if sa == "" {
		sa = "pelican-installer"
	}
	deadline := cls.Install.ActiveDeadlineSeconds
	if deadline <= 0 {
		deadline = 3600
	}
	pullPolicy := cls.Images.PullPolicy
	if pullPolicy == "" {
		pullPolicy = corev1.PullIfNotPresent
	}
	entrypoint := in.GS.Spec.Install.Entrypoint
	if entrypoint == "" {
		entrypoint = "bash"
	}
	image := in.GS.Spec.Install.Image
	if image == "" {
		image = "ghcr.io/pelican-eggs/installers:debian"
	}
	gen64 := gen
	labels := in.Labels("install")
	labels["pelican-k8s.io/install-generation"] = itoa(gen64)

	// The prepare step runs as the game UID (which owns the PVC layout) even
	// when the script itself runs as root.
	prepare := corev1.Container{
		Name:            "prepare",
		Image:           cls.Images.Shim,
		ImagePullPolicy: pullPolicy,
		Command:         []string{"/shim", "prepare", "--bin", "/pelican/bin/shim", "--shared", "/pelican", "--data", "/data", "--uuid", uuid},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolPtr(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			RunAsUser:                int64Ptr(in.UID),
			RunAsGroup:               int64Ptr(in.UID),
			RunAsNonRoot:             boolPtr(true),
		},
		VolumeMounts: []corev1.VolumeMount{{Name: "pelican", MountPath: "/pelican"}, {Name: "data", MountPath: "/data"}},
		Resources:    smallResources(),
	}

	chown := fmtUID(in.UID) + ":" + fmtUID(in.UID)
	install := corev1.Container{
		Name:            "install",
		Image:           image,
		ImagePullPolicy: corev1.PullAlways,
		Command: []string{"/pelican/bin/shim", "install-run",
			"--log", "/pelican/install/output.log", "--exit", "/pelican/install/exit-code",
			"--chown", chown, "--chown-path", "/mnt/server", "--", entrypoint, "/mnt/install/install.sh"},
		Env: []corev1.EnvVar{
			{Name: "HOME", Value: "/mnt/server"},
		},
		// Root keeps the runtime's default capability set (what Wings' Docker
		// installer gets); non-root installs drop everything.
		SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: boolPtr(false)},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: "/mnt/server", SubPath: "volumes/" + uuid},
			{Name: "data", MountPath: "/pelican/install", SubPath: "install/" + itoa(gen64)},
			{Name: "script", MountPath: "/mnt/install", ReadOnly: true},
			{Name: "pelican", MountPath: "/pelican/bin", SubPath: "bin", ReadOnly: true},
			{Name: "tmp", MountPath: "/tmp"},
		},
		Resources: InstallResources(in.Settings.Build, cls.Resources, cls.Install),
	}
	if in.EnvSecretExists {
		install.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: names.EnvSecret(uuid)}}}}
	}
	runAsRoot := cls.Install.RunAsRoot == nil || *cls.Install.RunAsRoot
	podSC := &corev1.PodSecurityContext{}
	if !cls.Install.DisableSeccomp {
		podSC.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	}
	if runAsRoot {
		podSC.RunAsUser = int64Ptr(0)
		podSC.RunAsGroup = int64Ptr(0)
	} else {
		podSC.RunAsUser = int64Ptr(in.UID)
		podSC.RunAsGroup = int64Ptr(in.UID)
		podSC.RunAsNonRoot = boolPtr(true)
		install.SecurityContext.Capabilities = &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}
	}

	tmpMiB := cls.Resources.TmpSizeMiB
	if tmpMiB <= 0 {
		tmpMiB = 100
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: in.GS.Namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32Ptr(0),
			ActiveDeadlineSeconds:   int64Ptr(deadline),
			TTLSecondsAfterFinished: int32Ptr(3600),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           sa,
					AutomountServiceAccountToken: boolPtr(false),
					EnableServiceLinks:           boolPtr(false),
					SecurityContext:              podSC,
					InitContainers:               []corev1.Container{prepare},
					Containers:                   []corev1.Container{install},
					Affinity: &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
							LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{v1alpha1.LabelServerUUID: uuid, v1alpha1.LabelComponent: "game"}},
							TopologyKey:   "kubernetes.io/hostname",
						}},
					}},
					Volumes: []corev1.Volume{
						{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: names.PVC(uuid)}}},
						{Name: "script", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: in.GS.Spec.Install.ScriptConfigMap}, Items: []corev1.KeyToPath{{Key: "install.sh", Path: "install.sh"}}}}},
						{Name: "pelican", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(int64(tmpMiB)*1024*1024, resource.BinarySI)}}},
					},
					NodeSelector: cls.NodeSelector,
					Tolerations:  cls.Tolerations,
				},
			},
		},
	}
	for _, s := range cls.ImageResolution.PullSecrets {
		job.Spec.Template.Spec.ImagePullSecrets = append(job.Spec.Template.Spec.ImagePullSecrets, corev1.LocalObjectReference{Name: s})
	}
	return job
}

func itoa(i int64) string { return fmtUID(i) }
