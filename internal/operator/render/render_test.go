package render

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/settings"
)

const uuid = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"

func testInput(t *testing.T, mutate func(*Input)) *Input {
	t.Helper()
	raw := `{"id":1,"uuid":"` + uuid + `","meta":{"name":"Survival SMP"},"suspended":false,
"build":{"memory_limit":4096,"cpu_limit":200,"disk_space":10240,"oom_killer":true},
"container":{"image":"ghcr.io/pelican-eggs/yolks:java_21"},
"allocations":{"default":{"ip":"203.0.113.10","port":25565},"mappings":{"203.0.113.10":[25565,25575]}},
"egg":{"id":"egg-1"}}`
	s, err := settings.Parse(apiextensionsv1.JSON{Raw: []byte(raw)})
	if err != nil {
		t.Fatal(err)
	}
	gs := &v1alpha1.GameServer{
		ObjectMeta: metav1.ObjectMeta{Name: "gs-" + uuid, Namespace: "pelican-servers"},
		Spec: v1alpha1.GameServerSpec{
			Panel:   v1alpha1.PanelSpec{UUID: uuid, Settings: apiextensionsv1.JSON{Raw: []byte(raw)}},
			Install: v1alpha1.InstallSpec{Generation: 2, ScriptConfigMap: "gs-" + uuid + "-install-2", Image: "ghcr.io/pelican-eggs/installers:alpine", Entrypoint: "ash"},
		},
	}
	cls := &v1alpha1.GameServerClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: v1alpha1.GameServerClassSpec{
			Storage:   v1alpha1.StorageSpec{StorageClassName: "main", DefaultSizeGiB: 20, OverheadPercent: 10},
			Exposure:  v1alpha1.ExposureSpec{Mode: v1alpha1.ExposureNodePort},
			Resources: v1alpha1.ResourcesSpec{MemoryOverheadPercent: 5, CPURequestPercentOfLimit: 25, UnlimitedMemoryMiB: 4096, MinCPU: resource.MustParse("100m"), TmpSizeMiB: 100},
			Security:  v1alpha1.SecuritySpec{RunAsUser: 1000},
			Images:    v1alpha1.ImagesSpec{Shim: "ghcr.io/claiyc/pelican-k8s/shim:v1", Agent: "ghcr.io/claiyc/pelican-k8s/agent:v1"},
		},
	}
	in := &Input{GS: gs, Class: cls, Settings: s, UID: 1000, Image: "ghcr.io/pelican-eggs/yolks:java_21@sha256:abc", SystemNamespace: "pelican-system", EnvSecretExists: true}
	if mutate != nil {
		mutate(in)
	}
	return in
}

func TestGameResources(t *testing.T) {
	r := v1alpha1.ResourcesSpec{MemoryOverheadPercent: 5, CPURequestPercentOfLimit: 25, UnlimitedMemoryMiB: 2048, MinCPU: resource.MustParse("100m")}
	got := GameResources(settings.Build{MemoryLimit: 4096, CPULimit: 200}, r)
	if got.Requests.Memory().String() != "4Gi" || got.Limits.Memory().Value() != 4300*1024*1024 {
		t.Fatalf("memory %v / %v", got.Requests.Memory(), got.Limits.Memory())
	}
	if got.Limits.Cpu().MilliValue() != 2000 || got.Requests.Cpu().MilliValue() != 500 {
		t.Fatalf("cpu %v / %v", got.Requests.Cpu(), got.Limits.Cpu())
	}
	// Unlimited memory and CPU: class defaults, no CPU limit, min request.
	got = GameResources(settings.Build{}, r)
	if got.Requests.Memory().String() != "2Gi" {
		t.Fatalf("unlimited memory %v", got.Requests.Memory())
	}
	if _, ok := got.Limits[corev1.ResourceCPU]; ok {
		t.Fatal("no cpu limit expected")
	}
	if got.Requests.Cpu().MilliValue() != 100 {
		t.Fatalf("min cpu %v", got.Requests.Cpu())
	}
	// Tiny CPU limit: request is clamped to the limit, not the minimum.
	got = GameResources(settings.Build{MemoryLimit: 512, CPULimit: 5}, r)
	if got.Limits.Cpu().MilliValue() != 50 || got.Requests.Cpu().MilliValue() != 50 {
		t.Fatalf("clamped cpu %v / %v", got.Requests.Cpu(), got.Limits.Cpu())
	}
	// Unlimited CPU with class cap.
	r.UnlimitedCPUPercent = 400
	got = GameResources(settings.Build{MemoryLimit: 512}, r)
	if got.Limits.Cpu().MilliValue() != 4000 || got.Requests.Cpu().MilliValue() != 1000 {
		t.Fatalf("capped cpu %v / %v", got.Requests.Cpu(), got.Limits.Cpu())
	}
}

func TestPVCSize(t *testing.T) {
	s := v1alpha1.StorageSpec{DefaultSizeGiB: 20, OverheadPercent: 10}
	if got := PVCSize(settings.Build{DiskSpace: 10240}, s); got.Value() != 11264*1024*1024 {
		t.Fatalf("size %v", got.String())
	}
	if got := PVCSize(settings.Build{}, s); got.Value() != 22528*1024*1024 {
		t.Fatalf("default size %v", got.String())
	}
	if got := ScratchSize(resource.MustParse("11Gi"), v1alpha1.StorageSpec{Scratch: v1alpha1.ScratchSpec{SizeGiB: 2}}); got.String() != "2Gi" {
		t.Fatalf("scratch %v", got.String())
	}
}

func TestInstallResources(t *testing.T) {
	r := v1alpha1.ResourcesSpec{MemoryOverheadPercent: 0, CPURequestPercentOfLimit: 25, MinCPU: resource.MustParse("100m")}
	i := v1alpha1.InstallJobSpec{Resources: v1alpha1.ContainerResources{CPU: resource.MustParse("1"), Memory: resource.MustParse("1Gi")}}
	got := InstallResources(settings.Build{MemoryLimit: 128, CPULimit: 50}, r, i)
	if got.Limits.Memory().String() != "1Gi" || got.Limits.Cpu().String() != "1" {
		t.Fatalf("install limits %v", got.Limits)
	}
	got = InstallResources(settings.Build{MemoryLimit: 8192, CPULimit: 400}, r, i)
	if got.Limits.Memory().String() != "8Gi" || got.Limits.Cpu().String() != "4" {
		t.Fatalf("install limits %v", got.Limits)
	}
}

func TestStatefulSetTemplate(t *testing.T) {
	in := testInput(t, nil)
	sts := StatefulSet(in)
	if *sts.Spec.Replicas != 1 || sts.Spec.UpdateStrategy.Type != "OnDelete" || sts.Spec.ServiceName != "gs-"+uuid+"-agent" {
		t.Fatalf("sts spec %+v", sts.Spec)
	}
	spec := sts.Spec.Template.Spec
	if len(spec.InitContainers) != 3 || spec.InitContainers[2].Name != "agent" || spec.InitContainers[2].RestartPolicy == nil || *spec.InitContainers[2].RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Fatalf("init containers %+v", spec.InitContainers)
	}
	if len(spec.Containers) != 1 || spec.Containers[0].Name != "game" || spec.Containers[0].Image != in.Image {
		t.Fatalf("containers %+v", spec.Containers)
	}
	game := spec.Containers[0]
	if len(game.Ports) != 4 || game.Ports[0].ContainerPort != 25565 || game.Ports[0].HostPort != 0 {
		t.Fatalf("ports %+v", game.Ports)
	}
	if *spec.SecurityContext.RunAsUser != 1000 || *spec.SecurityContext.FSGroup != 1000 || !*spec.SecurityContext.RunAsNonRoot || spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod security %+v", spec.SecurityContext)
	}
	if *spec.AutomountServiceAccountToken || *spec.TerminationGracePeriodSeconds != 660 || spec.ServiceAccountName != "pelican-game" {
		t.Fatalf("pod spec %+v", spec)
	}
	if !*game.SecurityContext.ReadOnlyRootFilesystem || game.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("game security %+v", game.SecurityContext)
	}
	var home, machineID, passwd bool
	for _, m := range game.VolumeMounts {
		switch m.MountPath {
		case "/home/container":
			home = m.SubPath == "volumes/"+uuid && m.Name == "data"
		case "/etc/machine-id":
			machineID = m.SubPath == "machine-id" && m.ReadOnly
		case "/etc/passwd":
			passwd = m.SubPath == "etc/passwd"
		}
	}
	if !home || !machineID || !passwd {
		t.Fatalf("mounts %+v", game.VolumeMounts)
	}
	// Readiness tracks the game process; a stopped server must never be
	// restarted or blocked for being unready.
	if game.ReadinessProbe == nil || game.ReadinessProbe.HTTPGet == nil || game.ReadinessProbe.HTTPGet.Path != "/internal/v1/ready" || game.ReadinessProbe.HTTPGet.Port.IntValue() != 8080 {
		t.Fatalf("readiness probe %+v", game.ReadinessProbe)
	}
	if game.LivenessProbe != nil || game.StartupProbe != nil {
		t.Fatal("the game container must not have a liveness or startup probe: a stopped server would be restarted")
	}
	if sts.Spec.PodManagementPolicy != "Parallel" || sts.Spec.MinReadySeconds != 0 {
		t.Fatalf("an unready pod must not block the StatefulSet: %+v", sts.Spec)
	}
	if game.Lifecycle.PreStop.HTTPGet.Path != "/internal/v1/prestop" || game.Lifecycle.PreStop.HTTPGet.Port.IntValue() != 8080 {
		t.Fatalf("prestop %+v", game.Lifecycle)
	}
	if game.Resources.Limits.Memory().Value() != 4300*1024*1024 {
		t.Fatalf("resources %+v", game.Resources)
	}
	if h := sts.Spec.Template.Annotations[AnnotationTemplateHash]; h == "" || h != TemplateHash(sts.Spec.Template) {
		t.Fatal("template hash missing or unstable")
	}
	// The hash ignores resources but not the image.
	in2 := testInput(t, func(i *Input) { i.Settings.Build.MemoryLimit = 8192 })
	if TemplateHash(PodTemplate(in2)) != TemplateHash(sts.Spec.Template) {
		t.Fatal("resource change must not change the template hash")
	}
	in3 := testInput(t, func(i *Input) { i.Image = "ghcr.io/pelican-eggs/yolks:java_17@sha256:def" })
	if TemplateHash(PodTemplate(in3)) == TemplateHash(sts.Spec.Template) {
		t.Fatal("image change must change the template hash")
	}
	// Scratch defaults to a generic ephemeral volume the size of the PVC.
	var scratch *corev1.Volume
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == "scratch" {
			scratch = &spec.Volumes[i]
		}
	}
	if scratch == nil || scratch.Ephemeral == nil || scratch.Ephemeral.VolumeClaimTemplate.Spec.Resources.Requests.Storage().Value() != 11264*1024*1024 {
		t.Fatalf("scratch %+v", scratch)
	}
	agent := spec.InitContainers[2]
	if agent.Lifecycle.PreStop.HTTPGet.Path != "/internal/v1/prestop" || agent.StartupProbe.HTTPGet.Path != "/internal/v1/healthz" {
		t.Fatalf("agent probes %+v", agent)
	}
	var tokenEnv bool
	for _, e := range agent.Env {
		if e.Name == "WINGS_TOKEN" && e.ValueFrom.SecretKeyRef.Name == "gs-"+uuid+"-agent" {
			tokenEnv = true
		}
	}
	if !tokenEnv {
		t.Fatalf("agent env %+v", agent.Env)
	}
}

func TestHostPortAndArgv(t *testing.T) {
	in := testInput(t, func(i *Input) {
		i.Class.Spec.Exposure.Mode = v1alpha1.ExposureHostPort
		i.Argv = []string{"/usr/bin/tini", "-g", "--", "/entrypoint.sh"}
	})
	tmpl := PodTemplate(in)
	game := tmpl.Spec.Containers[0]
	if game.Ports[0].HostPort != 25565 || tmpl.Spec.ServiceAccountName != "pelican-game-hostport" {
		t.Fatalf("hostport %+v", game.Ports)
	}
	if strings.Join(game.Args, " ") != "/usr/bin/tini -g -- /entrypoint.sh" {
		t.Fatalf("args %v", game.Args)
	}
	probe := tmpl.Spec.InitContainers[1]
	if !strings.Contains(strings.Join(probe.Command, " "), "-- /usr/bin/tini -g -- /entrypoint.sh") {
		t.Fatalf("probe command %v", probe.Command)
	}
	if ExposureService(in) != nil {
		t.Fatal("no exposure service in HostPort mode")
	}
}

func TestServices(t *testing.T) {
	in := testInput(t, nil)
	svc := ExposureService(in)
	if svc.Spec.Type != corev1.ServiceTypeNodePort || len(svc.Spec.Ports) != 4 || svc.Spec.Ports[0].NodePort != 25565 || svc.Spec.Ports[1].Protocol != corev1.ProtocolUDP {
		t.Fatalf("nodeport service %+v", svc.Spec)
	}
	if svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal || !svc.Spec.PublishNotReadyAddresses {
		t.Fatalf("service policy %+v", svc.Spec)
	}
	lb := testInput(t, func(i *Input) {
		i.Class.Spec.Exposure = v1alpha1.ExposureSpec{Mode: v1alpha1.ExposureLoadBalancer, LoadBalancer: v1alpha1.LoadBalancerSpec{IPAnnotation: "metallb.io/loadBalancerIPs", SharingAnnotation: "metallb.io/allow-shared-ip"}}
	})
	svc = ExposureService(lb)
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer || svc.Annotations["metallb.io/loadBalancerIPs"] != "203.0.113.10" || svc.Annotations["metallb.io/allow-shared-ip"] == "" || svc.Spec.Ports[0].NodePort != 0 {
		t.Fatalf("lb service %+v %+v", svc.Annotations, svc.Spec)
	}
	none := testInput(t, func(i *Input) {
		i.Settings.Allocations = settings.Allocations{Default: settings.Allocation{IP: "127.0.0.1"}}
	})
	if ExposureService(none) != nil {
		t.Fatal("servers without allocation get no exposure service")
	}
	agent := AgentService(in)
	if agent.Spec.ClusterIP != "None" || len(agent.Spec.Ports) != 2 || agent.Spec.Ports[1].Port != 2022 {
		t.Fatalf("agent service %+v", agent.Spec)
	}
}

func TestNetworkPolicy(t *testing.T) {
	in := testInput(t, func(i *Input) {
		i.Class.Spec.Network = v1alpha1.NetworkSpec{
			BlockedEgressCIDRs: []string{"10.128.0.0/14", "172.30.0.0/16", "192.0.2.0/24"},
			NodeCIDRs:          []string{"192.0.2.10/32"},
			InClusterEgress:    v1alpha1.InClusterEgressSpec{Additional: []v1alpha1.EgressRule{{CIDR: "172.30.5.5/32", Ports: []int32{9000}}}},
		}
	})
	np := NetworkPolicy(in)
	if len(np.Spec.Ingress) != 3 {
		t.Fatalf("ingress rules %d", len(np.Spec.Ingress))
	}
	if np.Spec.Ingress[0].From[0].IPBlock.CIDR != "0.0.0.0/0" || len(np.Spec.Ingress[0].Ports) != 4 {
		t.Fatalf("game ingress %+v", np.Spec.Ingress[0])
	}
	if np.Spec.Ingress[1].From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "pelican-system" {
		t.Fatalf("agent ingress %+v", np.Spec.Ingress[1])
	}
	if len(np.Spec.Egress) != 5 {
		t.Fatalf("egress rules %d: %+v", len(np.Spec.Egress), np.Spec.Egress)
	}
	internet := np.Spec.Egress[1].To[0].IPBlock
	if internet.CIDR != "0.0.0.0/0" || len(internet.Except) != 4 || internet.Except[0] != "169.254.0.0/16" {
		t.Fatalf("internet egress %+v", internet)
	}
	if np.Spec.Egress[4].To[0].IPBlock.CIDR != "172.30.5.5/32" || len(np.Spec.Egress[4].Ports) != 2 {
		t.Fatalf("additional egress %+v", np.Spec.Egress[4])
	}
	off := testInput(t, func(i *Input) { i.Class.Spec.Network.Enabled = boolPtr(false) })
	if NetworkPolicy(off) != nil {
		t.Fatal("disabled network policy")
	}
}

func TestInstallJob(t *testing.T) {
	in := testInput(t, nil)
	job := InstallJob(in, 2)
	if job.Name != "gs-"+uuid+"-install-2" || *job.Spec.BackoffLimit != 0 || *job.Spec.ActiveDeadlineSeconds != 3600 {
		t.Fatalf("job %+v", job.Spec)
	}
	spec := job.Spec.Template.Spec
	if *spec.SecurityContext.RunAsUser != 0 || spec.ServiceAccountName != "pelican-installer" || *spec.AutomountServiceAccountToken {
		t.Fatalf("job pod %+v", spec)
	}
	c := spec.Containers[0]
	if c.Image != "ghcr.io/pelican-eggs/installers:alpine" || !strings.HasSuffix(strings.Join(c.Command, " "), "-- ash /mnt/install/install.sh") || !strings.Contains(strings.Join(c.Command, " "), "--chown 1000:1000") {
		t.Fatalf("install command %v", c.Command)
	}
	if c.EnvFrom[0].SecretRef.Name != "gs-"+uuid+"-env" {
		t.Fatalf("envFrom %+v", c.EnvFrom)
	}
	var server, install, script bool
	for _, m := range c.VolumeMounts {
		switch m.MountPath {
		case "/mnt/server":
			server = m.SubPath == "volumes/"+uuid
		case "/pelican/install":
			install = m.SubPath == "install/2"
		case "/mnt/install":
			script = m.ReadOnly
		}
	}
	if !server || !install || !script {
		t.Fatalf("mounts %+v", c.VolumeMounts)
	}
	if pi := spec.InitContainers[0]; *pi.SecurityContext.RunAsUser != 1000 || pi.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("prepare must run as the game uid: %+v", pi.SecurityContext)
	}
	if spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].TopologyKey != "kubernetes.io/hostname" {
		t.Fatal("affinity to the server pod is required")
	}
	if c.Resources.Limits.Memory().Value() < 4096*1024*1024 {
		t.Fatalf("install resources %+v", c.Resources)
	}
}

func TestContainerImageRef(t *testing.T) {
	if ContainerImageRef("img:tag", "sha256:abc") != "img:tag@sha256:abc" || ContainerImageRef("img:tag@sha256:old", "sha256:new") != "img:tag@sha256:new" || ContainerImageRef("img:tag", "") != "img:tag" {
		t.Fatal("image ref")
	}
}

// The provider spares a class the exact annotation keys: a mistyped key is
// ignored by the load balancer and the mistake only shows as a pending IP.
func TestLoadBalancerProviderSuppliesAnnotationKeys(t *testing.T) {
	in := testInput(t, func(i *Input) {
		i.Class.Spec.Exposure = v1alpha1.ExposureSpec{
			Mode:         v1alpha1.ExposureLoadBalancer,
			LoadBalancer: v1alpha1.LoadBalancerSpec{Provider: v1alpha1.LoadBalancerMetalLB},
		}
	})
	svc := ExposureService(in)
	if svc.Annotations[v1alpha1.MetalLBIPAnnotation] != "203.0.113.10" {
		t.Fatalf("ip annotation %+v", svc.Annotations)
	}
	if svc.Annotations[v1alpha1.MetalLBSharingAnnotation] == "" {
		t.Fatalf("sharing annotation %+v", svc.Annotations)
	}
}

// An explicit key must still win, so a fork or a newer provider version can be
// pointed at a different annotation without waiting for a release.
func TestLoadBalancerExplicitKeysOverrideProvider(t *testing.T) {
	in := testInput(t, func(i *Input) {
		i.Class.Spec.Exposure = v1alpha1.ExposureSpec{
			Mode: v1alpha1.ExposureLoadBalancer,
			LoadBalancer: v1alpha1.LoadBalancerSpec{
				Provider:     v1alpha1.LoadBalancerMetalLB,
				IPAnnotation: "example.com/address",
			},
		}
	})
	svc := ExposureService(in)
	if svc.Annotations["example.com/address"] != "203.0.113.10" {
		t.Fatalf("explicit key ignored: %+v", svc.Annotations)
	}
	if _, ok := svc.Annotations[v1alpha1.MetalLBIPAnnotation]; ok {
		t.Fatalf("provider key must not be added too: %+v", svc.Annotations)
	}
	// The sharing key is still filled in by the provider.
	if svc.Annotations[v1alpha1.MetalLBSharingAnnotation] == "" {
		t.Fatalf("sharing annotation %+v", svc.Annotations)
	}
}

// Without a provider nothing is guessed: an unknown load balancer gets a plain
// Service and picks its own address.
func TestLoadBalancerWithoutProviderAddsNoAnnotations(t *testing.T) {
	in := testInput(t, func(i *Input) {
		i.Class.Spec.Exposure = v1alpha1.ExposureSpec{Mode: v1alpha1.ExposureLoadBalancer}
	})
	if svc := ExposureService(in); len(svc.Annotations) != 0 {
		t.Fatalf("annotations %+v", svc.Annotations)
	}
}
