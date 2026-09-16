// Package settings parses the parts of the Panel's raw `settings` object that
// the operator and gateway act on. Everything else stays opaque.
package settings

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// Settings is the typed subset of the Panel server settings.
type Settings struct {
	ID          int64          `json:"id"`
	UUID        string         `json:"uuid"`
	Meta        Meta           `json:"meta"`
	Suspended   bool           `json:"suspended"`
	Invocation  string         `json:"invocation"`
	Build       Build          `json:"build"`
	Container   Container      `json:"container"`
	Allocations Allocations    `json:"allocations"`
	Egg         Egg            `json:"egg"`
	Environment map[string]any `json:"environment,omitempty"`
}

// Meta is the server name and description.
type Meta struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Build are the Panel resource limits (0 = unlimited).
type Build struct {
	MemoryLimit int64  `json:"memory_limit"`
	Swap        int64  `json:"swap"`
	IOWeight    int64  `json:"io_weight"`
	CPULimit    int64  `json:"cpu_limit"`
	Threads     string `json:"threads"`
	DiskSpace   int64  `json:"disk_space"`
	OOMKiller   bool   `json:"oom_killer"`
}

// Container carries the image.
type Container struct {
	Image           string `json:"image"`
	RequiresRebuild bool   `json:"requires_rebuild"`
}

// Allocation is an ip:port pair.
type Allocation struct {
	IP   string `json:"ip"`
	Port int32  `json:"port"`
}

// Allocations are the server's ports.
type Allocations struct {
	ForceOutgoingIP bool               `json:"force_outgoing_ip"`
	Default         Allocation         `json:"default"`
	Mappings        map[string][]int32 `json:"mappings"`
}

// Egg carries the egg id.
type Egg struct {
	ID string `json:"id"`
}

// Parse decodes the raw settings JSON.
func Parse(raw apiextensionsv1.JSON) (*Settings, error) {
	if len(raw.Raw) == 0 {
		return nil, fmt.Errorf("settings: empty")
	}
	var s Settings
	if err := json.Unmarshal(raw.Raw, &s); err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	if s.UUID == "" {
		return nil, fmt.Errorf("settings: missing uuid")
	}
	return &s, nil
}

// HasAllocation reports whether the server has a network allocation. Servers
// without one arrive as default 127.0.0.1:0.
func (s *Settings) HasAllocation() bool {
	return s.Allocations.Default.Port != 0
}

// Ports returns the sorted, de-duplicated set of allocation ports across all IPs.
func (s *Settings) Ports() []int32 {
	set := map[int32]struct{}{}
	for _, ports := range s.Allocations.Mappings {
		for _, p := range ports {
			if p >= 1 && p <= 65535 {
				set[p] = struct{}{}
			}
		}
	}
	if s.HasAllocation() {
		set[s.Allocations.Default.Port] = struct{}{}
	}
	out := make([]int32, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// IPs returns the sorted allocation IPs.
func (s *Settings) IPs() []string {
	set := map[string]struct{}{}
	for ip := range s.Allocations.Mappings {
		if ip != "" {
			set[ip] = struct{}{}
		}
	}
	if s.HasAllocation() && s.Allocations.Default.IP != "" {
		set[s.Allocations.Default.IP] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for ip := range set {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

// Image returns the container image without the Wings "never pull" prefix and
// whether that prefix was present.
func (s *Settings) Image() (image string, neverPull bool) {
	img := strings.TrimSpace(s.Container.Image)
	if strings.HasPrefix(img, "~") {
		return strings.TrimPrefix(img, "~"), true
	}
	return img, false
}

// RewriteForPod returns a copy of the raw settings with the allocation default
// IP replaced by bindIP (0.0.0.0) as described in ARCHITECTURE.md 9.4, keeping
// every other field untouched.
func RewriteForPod(raw []byte, bindIP string) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	allocs, _ := m["allocations"].(map[string]any)
	if allocs == nil {
		return raw, nil
	}
	def, _ := allocs["default"].(map[string]any)
	if def == nil {
		return raw, nil
	}
	if port, _ := def["port"].(float64); port != 0 {
		def["ip"] = bindIP
	}
	return json.Marshal(m)
}
