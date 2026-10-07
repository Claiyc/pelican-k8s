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

func TestGameStatefulSet(t *testing.T) {
	in := testInput(t, nil)
	sts := GameStatefulSet(in)
	if sts.Name != "gs-"+uuid || *sts.Spec.Replicas != 1 || sts.Spec.UpdateStrategy.Type != "OnDelete" || sts.Spec.Selector.MatchLabels[v1alpha1.LabelComponent] != "game" {
		t.Fatalf("sts %+v", sts)
	}
	spec := sts.Spec.Template.Spec
	if len(spec.InitContainers) != 2 || spec.InitContainers[0].Name != "prepare" || spec.InitContainers[1].Name != "probe-entrypoint" {
		t.Fatalf("init containers %+v", spec.InitContainers)
	}
	// The game pod's prepare only copies the shim; the agent pod prepares the volume.
	if got := strings.Join(spec.InitContainers[0].Command, " "); got != "/shim prepare --bin /pelican/bin/shim --shared /pelican" {
		t.Fatalf("prepare command %q", got)
	}
	if len(spec.Containers) != 1 || spec.Containers[0].Name != "game" || spec.Containers[0].Image != in.Image {
		t.Fatalf("containers %+v", spec.Containers)
	}
	game := spec.Containers[0]
	if len(game.Ports) != 4 || game.Ports[0].ContainerPort != 25565 || game.Ports[0].HostPort != 0 {
		t.Fatalf("ports %+v", game.Ports)
	}
	if !strings.Contains(strings.Join(game.Command, " "), "--agent gs-"+uuid+"-agent:8082") {
		t.Fatalf("game command %v", game.Command)
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
		case AgentRoot:
			t.Fatal("the game pod must not mount the PVC root")
		}
	}
	if !home || !machineID || !passwd {
		t.Fatalf("mounts %+v", game.VolumeMounts)
	}
	env := map[string]corev1.EnvVar{}
	for _, e := range game.Env {
		env[e.Name] = e
	}
	if e := env["PELICAN_POD_UID"]; e.ValueFrom == nil || e.ValueFrom.FieldRef.FieldPath != "metadata.uid" {
		t.Fatalf("pod uid env %+v", e)
	}
	if e := env["INTERNAL_IP"]; e.ValueFrom == nil || e.ValueFrom.FieldRef.FieldPath != "status.podIP" {
		t.Fatalf("internal ip env %+v", e)
	}
	// Readiness tracks the game process; a stopped server must never be
	// restarted or blocked for being unready.
	if game.ReadinessProbe == nil || game.ReadinessProbe.Exec == nil || strings.Join(game.ReadinessProbe.Exec.Command, " ") != "/pelican/bin/shim ready" {
		t.Fatalf("readiness probe %+v", game.ReadinessProbe)
	}
	if game.LivenessProbe != nil || game.StartupProbe != nil {
		t.Fatal("the game container must not have a liveness or startup probe: a stopped server would be restarted")
	}
	if sts.Spec.PodManagementPolicy != "Parallel" || sts.Spec.MinReadySeconds != 0 {
		t.Fatalf("an unready pod must not block the StatefulSet: %+v", sts.Spec)
	}
	// The shim stops the process itself on SIGTERM.
	if game.Lifecycle != nil {
		t.Fatalf("the game container must not have a lifecycle hook: %+v", game.Lifecycle)
	}
	if game.Resources.Limits.Memory().Value() != 4300*1024*1024 {
		t.Fatalf("resources %+v", game.Resources)
	}
	for _, v := range spec.Volumes {
		if v.Name == "scratch" || v.Name == "agent-config" {
			t.Fatalf("game pod volume %q belongs to the agent pod", v.Name)
		}
	}
	if spec.Affinity != nil {
		t.Fatalf("no affinity without allocation nodes or agent placement: %+v", spec.Affinity)
	}
}

func TestAgentStatefulSet(t *testing.T) {
	in := testInput(t, nil)
	sts := AgentStatefulSet(in)
	if sts.Name != "gs-"+uuid+"-agent" || *sts.Spec.Replicas != 1 || sts.Spec.ServiceName != "gs-"+uuid+"-agent" || sts.Spec.UpdateStrategy.Type != "OnDelete" {
		t.Fatalf("sts %+v", sts)
	}
	if sts.Spec.Selector.MatchLabels[v1alpha1.LabelComponent] != "agent" || sts.Spec.Template.Labels[v1alpha1.LabelComponent] != "agent" {
		t.Fatalf("selector %+v labels %+v", sts.Spec.Selector, sts.Spec.Template.Labels)
	}
	spec := sts.Spec.Template.Spec
	if spec.ServiceAccountName != "pelican-agent" || spec.PriorityClassName != "pelican-agent" || *spec.AutomountServiceAccountToken {
		t.Fatalf("pod spec %+v", spec)
	}
	if *spec.SecurityContext.RunAsUser != 1000 || *spec.SecurityContext.FSGroup != 1000 {
		t.Fatalf("pod security %+v", spec.SecurityContext)
	}
	if len(spec.InitContainers) != 1 || strings.Join(spec.InitContainers[0].Command, " ") != "/shim prepare --data /data --uuid "+uuid {
		t.Fatalf("init containers %+v", spec.InitContainers)
	}
	if len(spec.Containers) != 1 || spec.Containers[0].Name != AgentContainer {
		t.Fatalf("containers %+v", spec.Containers)
	}
	agent := spec.Containers[0]
	if agent.Lifecycle.PreStop.HTTPGet.Path != "/internal/v1/prestop" || agent.StartupProbe.HTTPGet.Path != "/internal/v1/healthz" {
		t.Fatalf("agent probes %+v", agent)
	}
	if len(agent.Ports) != 3 || agent.Ports[2].ContainerPort != ShimPort {
		t.Fatalf("agent ports %+v", agent.Ports)
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
	if spec.Affinity != nil {
		t.Fatalf("no affinity without a game pod node: %+v", spec.Affinity)
	}

	custom := AgentPodTemplate(testInput(t, func(i *Input) {
		i.Class.Spec.AgentServiceAccountName = "agents"
		i.Class.Spec.AgentPriorityClassName = "high"
		i.AgentNode = "node-b"
	}))
	if custom.Spec.ServiceAccountName != "agents" || custom.Spec.PriorityClassName != "high" {
		t.Fatalf("custom agent pod %+v", custom.Spec)
	}
	terms := custom.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || terms[0].MatchFields[0].Values[0] != "node-b" {
		t.Fatalf("agent node affinity %+v", terms)
	}
}

func TestGameAffinity(t *testing.T) {
	cases := []struct {
		affinity  GameAffinity
		required  bool
		preferred bool
	}{
		{GameAffinityNone, false, false},
		{GameAffinityPreferred, false, true},
		{GameAffinityRequired, true, false},
	}
	for _, tc := range cases {
		spec := GamePodTemplate(testInput(t, func(i *Input) { i.GameAffinity = tc.affinity })).Spec
		var pa *corev1.PodAffinity
		if spec.Affinity != nil {
			pa = spec.Affinity.PodAffinity
		}
		required := pa != nil && len(pa.RequiredDuringSchedulingIgnoredDuringExecution) == 1
		preferred := pa != nil && len(pa.PreferredDuringSchedulingIgnoredDuringExecution) == 1
		if required != tc.required || preferred != tc.preferred {
			t.Fatalf("affinity %d: %+v", tc.affinity, pa)
		}
		var term corev1.PodAffinityTerm
		switch {
		case required:
			term = pa.RequiredDuringSchedulingIgnoredDuringExecution[0]
		case preferred:
			term = pa.PreferredDuringSchedulingIgnoredDuringExecution[0].PodAffinityTerm
		default:
			continue
		}
		if term.TopologyKey != "kubernetes.io/hostname" || term.LabelSelector.MatchLabels[v1alpha1.LabelComponent] != "agent" || term.LabelSelector.MatchLabels[v1alpha1.LabelServerUUID] != uuid {
			t.Fatalf("affinity term %+v", term)
		}
	}
	// The allocation node pin and the agent affinity combine.
	spec := GamePodTemplate(testInput(t, func(i *Input) {
		i.NodeNames = []string{"node-a"}
		i.GameAffinity = GameAffinityPreferred
	})).Spec
	if spec.Affinity.NodeAffinity == nil || spec.Affinity.PodAffinity == nil {
		t.Fatalf("combined affinity %+v", spec.Affinity)
	}
}

func TestTemplateHash(t *testing.T) {
	in := testInput(t, nil)
	game := GamePodTemplate(in)
	if h := game.Annotations[AnnotationTemplateHash]; h == "" || h != TemplateHash(game) {
		t.Fatal("template hash missing or unstable")
	}
	same := func(name string, a, b corev1.PodTemplateSpec, want bool) {
		t.Helper()
		if got := a.Annotations[AnnotationTemplateHash] == b.Annotations[AnnotationTemplateHash]; got != want {
			t.Fatalf("%s: same hash %v, want %v", name, got, want)
		}
	}
	same("resources", game, GamePodTemplate(testInput(t, func(i *Input) { i.Settings.Build.MemoryLimit = 8192 })), true)
	same("image", game, GamePodTemplate(testInput(t, func(i *Input) { i.Image = "ghcr.io/pelican-eggs/yolks:java_17@sha256:def" })), false)
	same("game affinity", game, GamePodTemplate(testInput(t, func(i *Input) { i.GameAffinity = GameAffinityRequired })), true)
	same("allocation nodes", game, GamePodTemplate(testInput(t, func(i *Input) { i.NodeNames = []string{"node-a"} })), false)

	agent := AgentPodTemplate(in)
	same("agent node", agent, AgentPodTemplate(testInput(t, func(i *Input) { i.AgentNode = "node-b" })), true)
	same("agent image", agent, AgentPodTemplate(testInput(t, func(i *Input) { i.Class.Spec.Images.Agent = "agent:v2" })), false)
	same("agent resources", agent, AgentPodTemplate(testInput(t, func(i *Input) { i.Class.Spec.Resources.Agent.Memory = resource.MustParse("256Mi") })), false)
	// The game image is not part of the agent pod.
	same("game image in the agent", agent, AgentPodTemplate(testInput(t, func(i *Input) { i.Image = "other@sha256:def" })), true)
	// Panel edits of the server do not replace its agent pod.
	same("disk size in the agent", agent, AgentPodTemplate(testInput(t, func(i *Input) { i.Settings.Build.DiskSpace = 99999 })), true)
	same("server name in the agent", agent, AgentPodTemplate(testInput(t, func(i *Input) { i.Settings.Meta.Name = "renamed" })), true)
	same("egg in the agent", agent, AgentPodTemplate(testInput(t, func(i *Input) { i.Settings.Egg.ID = "another-egg" })), true)
	same("disk size in the emptyDir agent", AgentPodTemplate(testInput(t, func(i *Input) { i.Class.Spec.Storage.Scratch.Type = v1alpha1.ScratchEmptyDir })),
		AgentPodTemplate(testInput(t, func(i *Input) {
			i.Class.Spec.Storage.Scratch.Type = v1alpha1.ScratchEmptyDir
			i.Settings.Build.DiskSpace = 99999
		})), true)
	same("scratch type", agent, AgentPodTemplate(testInput(t, func(i *Input) { i.Class.Spec.Storage.Scratch.Type = v1alpha1.ScratchEmptyDir })), false)
	// The game pod is still replaced for a rename while it runs.
	same("server name in the game", game, GamePodTemplate(testInput(t, func(i *Input) { i.Settings.Meta.Name = "renamed" })), false)
}

// The shim dials the agent over TCP and writes its readiness file to
// /pelican/run; both containers get the shim token, the game container
// nothing of the agent's.
func TestShimSocketIsolation(t *testing.T) {
	in := testInput(t, nil)
	agent, game := AgentPodTemplate(in).Spec.Containers[0], GamePodTemplate(in).Spec.Containers[0]
	runMount := func(c corev1.Container) *corev1.VolumeMount {
		for i := range c.VolumeMounts {
			if c.VolumeMounts[i].MountPath == "/pelican/run" {
				return &c.VolumeMounts[i]
			}
		}
		return nil
	}
	if m := runMount(game); m == nil || m.ReadOnly || m.SubPath != "run" {
		t.Fatalf("game /pelican/run mount %+v", m)
	}
	if m := runMount(agent); m != nil {
		t.Fatalf("agent /pelican/run mount %+v", m)
	}
	shimToken := func(c corev1.Container) bool {
		for _, e := range c.Env {
			if e.Name == "PELICAN_SHIM_TOKEN" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil &&
				e.ValueFrom.SecretKeyRef.Name == "gs-"+uuid+"-shim" && e.ValueFrom.SecretKeyRef.Key == ShimTokenKey {
				return true
			}
		}
		return false
	}
	if !shimToken(agent) || !shimToken(game) {
		t.Fatalf("shim token env: agent %+v game %+v", agent.Env, game.Env)
	}
	for _, e := range game.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name != "gs-"+uuid+"-shim" {
			t.Fatalf("game container references secret %q", e.ValueFrom.SecretKeyRef.Name)
		}
	}
	if sec := ShimSecret(testInput(t, nil), "tok"); sec.Name != "gs-"+uuid+"-shim" || sec.StringData[ShimTokenKey] != "tok" {
		t.Fatalf("shim secret %+v", sec)
	}
}

func TestHostPortAndArgv(t *testing.T) {
	in := testInput(t, func(i *Input) {
		i.Class.Spec.Exposure.Mode = v1alpha1.ExposureHostPort
		i.Argv = []string{"/usr/bin/tini", "-g", "--", "/entrypoint.sh"}
	})
	tmpl := GamePodTemplate(in)
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
	if svc.Spec.Selector[v1alpha1.LabelComponent] != "game" {
		t.Fatalf("exposure selector %+v", svc.Spec.Selector)
	}
	agent := AgentService(in)
	if agent.Spec.Type != corev1.ServiceTypeClusterIP || agent.Spec.ClusterIP != "None" || len(agent.Spec.Ports) != 3 || agent.Spec.Ports[1].Port != 2022 || agent.Spec.Ports[2].Port != ShimPort {
		t.Fatalf("agent service %+v", agent.Spec)
	}
	if agent.Spec.Selector[v1alpha1.LabelComponent] != "agent" || !agent.Spec.PublishNotReadyAddresses {
		t.Fatalf("agent service selector %+v", agent.Spec)
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
	if np.Name != "gs-"+uuid || np.Spec.PodSelector.MatchLabels[v1alpha1.LabelComponent] != "game" {
		t.Fatalf("game policy %+v", np.ObjectMeta)
	}
	// Only the game ports: the game pod serves nothing else.
	if len(np.Spec.Ingress) != 1 || np.Spec.Ingress[0].From[0].IPBlock.CIDR != "0.0.0.0/0" || len(np.Spec.Ingress[0].Ports) != 4 {
		t.Fatalf("game ingress %+v", np.Spec.Ingress)
	}
	if len(np.Spec.Egress) != 5 {
		t.Fatalf("egress rules %d: %+v", len(np.Spec.Egress), np.Spec.Egress)
	}
	internet := np.Spec.Egress[1].To[0].IPBlock
	if internet.CIDR != "0.0.0.0/0" || len(internet.Except) != 4 || internet.Except[0] != "169.254.0.0/16" {
		t.Fatalf("internet egress %+v", internet)
	}
	shim := np.Spec.Egress[2]
	if shim.To[0].PodSelector.MatchLabels[v1alpha1.LabelComponent] != "agent" || shim.To[0].PodSelector.MatchLabels[v1alpha1.LabelServerUUID] != uuid || shim.Ports[0].Port.IntValue() != ShimPort {
		t.Fatalf("shim egress %+v", shim)
	}
	for _, r := range np.Spec.Egress {
		for _, p := range r.Ports {
			if p.Port != nil && p.Port.IntValue() == GatewayPort {
				t.Fatalf("the game pod must not reach the gateway: %+v", r)
			}
		}
	}
	if np.Spec.Egress[4].To[0].IPBlock.CIDR != "172.30.5.5/32" || len(np.Spec.Egress[4].Ports) != 2 {
		t.Fatalf("additional egress %+v", np.Spec.Egress[4])
	}

	ap := AgentNetworkPolicy(in)
	if ap.Name != "gs-"+uuid+"-agent" || ap.Spec.PodSelector.MatchLabels[v1alpha1.LabelComponent] != "agent" {
		t.Fatalf("agent policy %+v", ap.ObjectMeta)
	}
	if len(ap.Spec.Ingress) != 3 {
		t.Fatalf("agent ingress rules %d", len(ap.Spec.Ingress))
	}
	if ap.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "pelican-system" || len(ap.Spec.Ingress[0].Ports) != 2 {
		t.Fatalf("system ingress %+v", ap.Spec.Ingress[0])
	}
	if ap.Spec.Ingress[1].From[0].IPBlock.CIDR != "192.0.2.10/32" {
		t.Fatalf("node ingress %+v", ap.Spec.Ingress[1])
	}
	if in := ap.Spec.Ingress[2]; in.From[0].PodSelector.MatchLabels[v1alpha1.LabelComponent] != "game" || in.Ports[0].Port.IntValue() != ShimPort {
		t.Fatalf("shim ingress %+v", in)
	}
	if len(ap.Spec.Egress) != 4 || ap.Spec.Egress[2].Ports[0].Port.IntValue() != GatewayPort || ap.Spec.Egress[3].To[0].IPBlock.CIDR != "172.30.5.5/32" {
		t.Fatalf("agent egress %+v", ap.Spec.Egress)
	}

	off := testInput(t, func(i *Input) { i.Class.Spec.Network.Enabled = boolPtr(false) })
	if NetworkPolicy(off) != nil || AgentNetworkPolicy(off) != nil {
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
	if term := spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0]; term.TopologyKey != "kubernetes.io/hostname" || term.LabelSelector.MatchLabels[v1alpha1.LabelComponent] != "agent" {
		t.Fatal("affinity to the agent pod is required")
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

// The Panel's memory_limit is a cap, not an estimate, so the default reserves
// all of it. A class may reserve less and overcommit deliberately.
func TestMemoryRequestPercentOfLimit(t *testing.T) {
	base := v1alpha1.ResourcesSpec{MemoryOverheadPercent: 5, CPURequestPercentOfLimit: 25, MinCPU: resource.MustParse("100m")}
	b := settings.Build{MemoryLimit: 8192, CPULimit: 400}

	full := GameResources(b, base)
	if got := full.Requests.Memory().Value(); got != 8192*1024*1024 {
		t.Fatalf("default request %d, want the full limit", got)
	}
	if got := full.Limits.Memory().Value(); got != 8601*1024*1024 {
		t.Fatalf("limit %d, want memory_limit plus overhead", got)
	}

	half := base
	half.MemoryRequestPercentOfLimit = 50
	got := GameResources(b, half)
	if v := got.Requests.Memory().Value(); v != 4096*1024*1024 {
		t.Fatalf("request %d, want half the limit", v)
	}
	// The limit is unaffected: overcommit changes what is reserved, not the cap.
	if v := got.Limits.Memory().Value(); v != 8601*1024*1024 {
		t.Fatalf("limit %d must not change with the request percentage", v)
	}

	// An out-of-range value falls back to reserving everything rather than
	// silently overcommitting.
	for _, pct := range []int32{0, -10, 200} {
		r := base
		r.MemoryRequestPercentOfLimit = pct
		res := GameResources(b, r)
		if v := res.Requests.Memory().Value(); v != 8192*1024*1024 {
			t.Fatalf("percent %d: request %d, want the full limit", pct, v)
		}
	}
}

// An install Job reserving the full limit would undo the class's overcommit.
func TestInstallResourcesFollowTheGameRequest(t *testing.T) {
	r := v1alpha1.ResourcesSpec{MemoryOverheadPercent: 0, CPURequestPercentOfLimit: 25, MinCPU: resource.MustParse("100m"), MemoryRequestPercentOfLimit: 50}
	i := v1alpha1.InstallJobSpec{Resources: v1alpha1.ContainerResources{CPU: resource.MustParse("1"), Memory: resource.MustParse("1Gi")}}
	got := InstallResources(settings.Build{MemoryLimit: 8192, CPULimit: 400}, r, i)
	if v := got.Requests.Memory().Value(); v != 4096*1024*1024 {
		t.Fatalf("install memory request %d, want half the limit", v)
	}
	if got.Limits.Memory().String() != "8Gi" {
		t.Fatalf("install memory limit %v must stay at the cap", got.Limits[corev1.ResourceMemory])
	}
}

func TestTLS(t *testing.T) {
	plain := testInput(t, nil)
	in := testInput(t, func(i *Input) { i.TLS = true })

	agentTmpl := AgentPodTemplate(in)
	agent := agentTmpl.Spec.Containers[0]
	if got := strings.Join(agent.Args, " "); !strings.HasSuffix(got, "--tls-dir "+AgentTLSDir) {
		t.Errorf("agent args %q", got)
	}
	for name, scheme := range map[string]corev1.URIScheme{
		"startup":  agent.StartupProbe.HTTPGet.Scheme,
		"liveness": agent.LivenessProbe.HTTPGet.Scheme,
		"preStop":  agent.Lifecycle.PreStop.HTTPGet.Scheme,
	} {
		if scheme != corev1.URISchemeHTTPS {
			t.Errorf("%s scheme %q", name, scheme)
		}
	}
	if !hasMount(agent.VolumeMounts, "tls", AgentTLSDir) {
		t.Errorf("agent mounts %+v", agent.VolumeMounts)
	}
	if v := volume(agentTmpl.Spec.Volumes, "tls"); v == nil || v.Secret == nil || v.Secret.SecretName != "gs-"+uuid+"-tls" || len(v.Secret.Items) != 0 {
		t.Errorf("agent tls volume %+v", v)
	}

	gameTmpl := GamePodTemplate(in)
	game := gameTmpl.Spec.Containers[0]
	if got := strings.Join(game.Command, " "); !strings.Contains(got, "--agent gs-"+uuid+"-agent:8082 ") || !strings.HasSuffix(got, " --agent-ca "+GameCAFile+" --") {
		t.Errorf("game command %q", got)
	}
	if !hasMount(game.VolumeMounts, "agent-ca", "/pelican/tls") {
		t.Errorf("game mounts %+v", game.VolumeMounts)
	}
	// The game pod gets the CA bundle only, never the agent's key.
	v := volume(gameTmpl.Spec.Volumes, "agent-ca")
	if v == nil || v.Secret == nil || v.Secret.SecretName != "gs-"+uuid+"-tls" || len(v.Secret.Items) != 1 || v.Secret.Items[0].Key != "ca.crt" || v.Secret.Items[0].Path != "ca.crt" {
		t.Errorf("game ca volume %+v", v)
	}

	// Without TLS nothing of it is rendered, and switching it recreates both pods.
	plainAgent, plainGame := AgentPodTemplate(plain), GamePodTemplate(plain)
	if strings.Contains(strings.Join(plainAgent.Spec.Containers[0].Args, " "), "--tls-dir") || volume(plainAgent.Spec.Volumes, "tls") != nil ||
		plainAgent.Spec.Containers[0].StartupProbe.HTTPGet.Scheme != "" {
		t.Errorf("plain agent pod has TLS: %+v", plainAgent.Spec)
	}
	if strings.Contains(strings.Join(plainGame.Spec.Containers[0].Command, " "), "--agent-ca") || volume(plainGame.Spec.Volumes, "agent-ca") != nil {
		t.Errorf("plain game pod has TLS: %+v", plainGame.Spec)
	}
	if TemplateHash(plainAgent) == TemplateHash(agentTmpl) || TemplateHash(plainGame) == TemplateHash(gameTmpl) {
		t.Error("switching TLS must change the template hashes")
	}

	sec := TLSSecret(in, map[string][]byte{"tls.crt": []byte("c")})
	if sec.Name != "gs-"+uuid+"-tls" || sec.Type != corev1.SecretTypeTLS || string(sec.Data["tls.crt"]) != "c" || sec.Labels[v1alpha1.LabelServerUUID] != uuid {
		t.Errorf("tls secret %+v", sec)
	}
}

func hasMount(mounts []corev1.VolumeMount, name, path string) bool {
	for _, m := range mounts {
		if m.Name == name && m.MountPath == path && m.ReadOnly {
			return true
		}
	}
	return false
}

func volume(vols []corev1.Volume, name string) *corev1.Volume {
	for i := range vols {
		if vols[i].Name == name {
			return &vols[i]
		}
	}
	return nil
}

func TestAgentURL(t *testing.T) {
	for ip, want := range map[string]string{
		"10.0.0.5": "http://10.0.0.5:8080",
		"fd00::5":  "http://[fd00::5]:8080",
	} {
		if got := AgentURL(ip); got != want {
			t.Errorf("AgentURL(%q) = %q, want %q", ip, got, want)
		}
	}
}
