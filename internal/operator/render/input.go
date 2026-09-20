// Package render builds the Kubernetes objects owned by a GameServer from the
// CR, its class and the parsed Panel settings. Functions here are pure so that
// they can be unit tested without a cluster. See ARCHITECTURE.md 7.4-7.5, 8.2.
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/settings"
)

// Well-known ports and paths shared by the components.
const (
	AgentPort      = 8080
	SFTPPort       = 2022
	GatewayPort    = 8081
	ShimSocket     = "/pelican/run/shim.sock"
	ArgvFile       = "/pelican/etc/argv"
	PasswdFile     = "/pelican/etc/passwd"
	GroupFile      = "/pelican/etc/group"
	ContainerHome  = "/home/container"
	AgentRoot      = "/var/lib/pelican"
	ScratchDir     = "/scratch"
	GameContainer  = "game"
	AgentContainer = "agent"

	// AnnotationTemplateHash marks a pod with the hash of the template it was created from.
	AnnotationTemplateHash = "pelican-k8s.io/template-hash"
	// LabelPartOf is set on every pelican-k8s component pod.
	LabelPartOf = "app.kubernetes.io/part-of"
	PartOfValue = "pelican-k8s"
	LabelName   = "app.kubernetes.io/name"
)

// Input is everything needed to render the owned objects.
type Input struct {
	GS       *v1alpha1.GameServer
	Class    *v1alpha1.GameServerClass
	Settings *settings.Settings
	// UID is the resolved pinned UID/GID for the game pod.
	UID int64
	// Image is the game image to run, possibly digest pinned.
	Image     string
	NeverPull bool
	// Argv is the resolved entrypoint; nil lets the probe init container decide.
	Argv []string
	// SystemNamespace is where the gateway and operator run (for NetworkPolicies).
	SystemNamespace string
	// EnvSecretExists reports whether the egg variable Secret is present (install Job envFrom).
	EnvSecretExists bool
}

// UUID returns the server UUID.
func (in *Input) UUID() string { return in.Settings.UUID }

// Labels returns the common labels of owned objects.
func (in *Input) Labels(component string) map[string]string {
	l := map[string]string{
		v1alpha1.LabelServerUUID:       in.UUID(),
		LabelPartOf:                    PartOfValue,
		LabelName:                      "gameserver",
		"app.kubernetes.io/managed-by": "pelican-operator",
	}
	if component != "" {
		l[v1alpha1.LabelComponent] = component
	}
	if in.Settings.Egg.ID != "" {
		l[v1alpha1.LabelEggUUID] = in.Settings.Egg.ID
	}
	return l
}

// Meta returns object metadata in the GameServer namespace.
func (in *Input) Meta(name, component string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: in.GS.Namespace, Labels: in.Labels(component)}
}

// GameResources maps the Panel build limits to container resources (section 11).
func GameResources(b settings.Build, r v1alpha1.ResourcesSpec) corev1.ResourceRequirements {
	memMiB := b.MemoryLimit
	if memMiB <= 0 {
		memMiB = int64(r.UnlimitedMemoryMiB)
	}
	if memMiB <= 0 {
		memMiB = 4096
	}
	overhead := int64(r.MemoryOverheadPercent)
	if overhead < 0 {
		overhead = 0
	}
	limMiB := memMiB * (100 + overhead) / 100
	// The Panel's memory_limit is a cap, not an estimate. Reserving all of it
	// is the safe default; a class may reserve less and overcommit on purpose.
	reqPct := int64(r.MemoryRequestPercentOfLimit)
	if reqPct <= 0 || reqPct > 100 {
		reqPct = 100
	}
	reqMiB := memMiB * reqPct / 100
	if reqMiB < 1 {
		reqMiB = 1
	}
	req := corev1.ResourceList{corev1.ResourceMemory: *resource.NewQuantity(reqMiB*1024*1024, resource.BinarySI)}
	lim := corev1.ResourceList{corev1.ResourceMemory: *resource.NewQuantity(limMiB*1024*1024, resource.BinarySI)}

	minCPU := r.MinCPU
	if minCPU.IsZero() {
		minCPU = resource.MustParse("100m")
	}
	pct := b.CPULimit
	if pct <= 0 {
		pct = int64(r.UnlimitedCPUPercent)
	}
	if pct > 0 {
		limit := resource.NewMilliQuantity(pct*10, resource.DecimalSI)
		reqPct := int64(r.CPURequestPercentOfLimit)
		if reqPct <= 0 {
			reqPct = 25
		}
		reqm := pct * 10 * reqPct / 100
		request := resource.NewMilliQuantity(reqm, resource.DecimalSI)
		if request.Cmp(minCPU) < 0 {
			request = &minCPU
		}
		if request.Cmp(*limit) > 0 {
			request = limit
		}
		lim[corev1.ResourceCPU] = *limit
		req[corev1.ResourceCPU] = *request
	} else {
		req[corev1.ResourceCPU] = minCPU
	}
	return corev1.ResourceRequirements{Requests: req, Limits: lim}
}

// PVCSize returns the volume size for the Panel disk_space (section 10.1).
func PVCSize(b settings.Build, s v1alpha1.StorageSpec) resource.Quantity {
	diskMiB := b.DiskSpace
	if diskMiB <= 0 {
		gib := int64(s.DefaultSizeGiB)
		if gib <= 0 {
			gib = 20
		}
		diskMiB = gib * 1024
	}
	overhead := int64(s.OverheadPercent)
	if overhead < 0 {
		overhead = 0
	}
	sizeMiB := diskMiB * (100 + overhead) / 100
	return *resource.NewQuantity(sizeMiB*1024*1024, resource.BinarySI)
}

// ScratchSize returns the scratch volume size.
func ScratchSize(pvc resource.Quantity, s v1alpha1.StorageSpec) resource.Quantity {
	if s.Scratch.SizeGiB > 0 {
		return *resource.NewQuantity(int64(s.Scratch.SizeGiB)*1024*1024*1024, resource.BinarySI)
	}
	return pvc
}

// InstallResources returns the install Job resources: max(server, class) as Wings does.
func InstallResources(b settings.Build, r v1alpha1.ResourcesSpec, i v1alpha1.InstallJobSpec) corev1.ResourceRequirements {
	game := GameResources(b, r)
	mem := game.Limits[corev1.ResourceMemory]
	if !i.Resources.Memory.IsZero() && i.Resources.Memory.Cmp(mem) > 0 {
		mem = i.Resources.Memory
	}
	// The request follows the game container, so a class that overcommits
	// memory is not undone by every install Job reserving the full limit.
	memReq := game.Requests[corev1.ResourceMemory]
	if memReq.Cmp(mem) > 0 {
		memReq = mem
	}
	out := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: memReq},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: mem},
	}
	cpuLimit, hasLimit := game.Limits[corev1.ResourceCPU]
	if hasLimit {
		if !i.Resources.CPU.IsZero() && i.Resources.CPU.Cmp(cpuLimit) > 0 {
			cpuLimit = i.Resources.CPU
		}
		out.Limits[corev1.ResourceCPU] = cpuLimit
		out.Requests[corev1.ResourceCPU] = game.Requests[corev1.ResourceCPU]
	} else {
		out.Requests[corev1.ResourceCPU] = game.Requests[corev1.ResourceCPU]
	}
	return out
}

// Hash returns a short stable hash of an object.
func Hash(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "error"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

func boolPtr(b bool) *bool    { return &b }
func int64Ptr(i int64) *int64 { return &i }
func int32Ptr(i int32) *int32 { return &i }

func fmtUID(uid int64) string { return fmt.Sprintf("%d", uid) }
