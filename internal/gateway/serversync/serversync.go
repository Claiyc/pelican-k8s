// Package serversync turns Panel intents into GameServer spec changes and
// assembles the Wings configuration served to agents (ARCHITECTURE.md 5.7,
// 8.1, 8.2, 8.6, 13).
package serversync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/pelican/wings/remote"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/gateway/panel"
	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/internal/operator/render"
	"github.com/Claiyc/pelican-k8s/internal/operator/settings"
)

// BindIP is what game processes bind to inside the pod (ARCHITECTURE.md 9.4).
const BindIP = "0.0.0.0"

// Syncer owns the Panel -> spec direction.
type Syncer struct {
	Store    *store.Store
	Panel    *panel.Client
	Timezone string
	Log      *slog.Logger
}

// splitSettings separates the environment map from the raw settings.
func splitSettings(raw json.RawMessage) (settingsNoEnv []byte, env map[string]any, err error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, fmt.Errorf("settings: %w", err)
	}
	env, _ = m["environment"].(map[string]any)
	delete(m, "environment")
	b, err := json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	return b, env, nil
}

// Revision hashes a Panel configuration payload.
func Revision(settings []byte, proc []byte) string {
	h := sha256.New()
	h.Write(settings)
	h.Write([]byte{0})
	h.Write(proc)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))[:32]
}

// Create builds the GameServer, its env Secret and install ConfigMap for a
// new Panel server (section 8.1).
func (s *Syncer) Create(ctx context.Context, uuid string, startOnCompletion bool) error {
	cfg, err := s.Panel.GetServerConfiguration(ctx, uuid)
	if err != nil {
		return fmt.Errorf("fetch configuration: %w", err)
	}
	script, err := s.Panel.GetInstallationScript(ctx, uuid)
	if err != nil {
		return fmt.Errorf("fetch install script: %w", err)
	}
	gs, envData, err := s.build(uuid, cfg)
	if err != nil {
		return err
	}
	gs.Spec.Install = v1alpha1.InstallSpec{Generation: 1, ScriptConfigMap: names.InstallConfigMap(uuid, 1), Image: script.ContainerImage, Entrypoint: script.Entrypoint, StartOnInstall: startOnCompletion}
	if err := s.Store.Client.Create(ctx, gs); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Re-create after a failed earlier attempt: treat as sync + reinstall request.
			s.Log.Warn("server already exists, syncing instead", "uuid", uuid)
			if err := s.Sync(ctx, uuid); err != nil {
				return err
			}
			return s.RequestInstall(ctx, uuid, false)
		}
		return err
	}
	if err := s.writeEnvSecret(ctx, gs, envData); err != nil {
		return err
	}
	if err := s.writeInstallScript(ctx, gs, 1, script.Script); err != nil {
		return err
	}
	return nil
}

// Adopt creates a GameServer for a server the Panel knows but the cluster
// does not, without installing it (drift resync).
func (s *Syncer) Adopt(ctx context.Context, uuid string, cfg *remote.ServerConfigurationResponse) error {
	gs, envData, err := s.build(uuid, cfg)
	if err != nil {
		return err
	}
	if err := s.Store.Client.Create(ctx, gs); err != nil {
		return client.IgnoreAlreadyExists(err)
	}
	return s.writeEnvSecret(ctx, gs, envData)
}

func (s *Syncer) build(uuid string, cfg *remote.ServerConfigurationResponse) (*v1alpha1.GameServer, map[string]string, error) {
	raw, env, err := splitSettings(cfg.Settings)
	if err != nil {
		return nil, nil, err
	}
	st, err := settings.Parse(apiextensionsv1.JSON{Raw: raw})
	if err != nil {
		return nil, nil, err
	}
	if st.UUID != uuid {
		return nil, nil, fmt.Errorf("panel returned settings for %q, expected %q", st.UUID, uuid)
	}
	proc, err := json.Marshal(cfg.ProcessConfiguration)
	if err != nil {
		return nil, nil, err
	}
	gs := &v1alpha1.GameServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:        names.ForUUID(uuid),
			Namespace:   s.Store.Namespace,
			Labels:      map[string]string{v1alpha1.LabelServerUUID: uuid, v1alpha1.LabelEggUUID: st.Egg.ID},
			Annotations: map[string]string{v1alpha1.AnnotationPanelName: st.Meta.Name},
		},
		Spec: v1alpha1.GameServerSpec{
			Panel: v1alpha1.PanelSpec{
				UUID:                 uuid,
				UUIDShort:            names.Short(uuid),
				Settings:             apiextensionsv1.JSON{Raw: raw},
				EnvironmentSecretRef: corev1.LocalObjectReference{Name: names.EnvSecret(uuid)},
				ProcessConfiguration: apiextensionsv1.JSON{Raw: proc},
				PanelRevision:        Revision(raw, proc),
			},
			Power:     v1alpha1.PowerSpec{Desired: v1alpha1.PowerStopped},
			ClassName: s.Store.DefaultClass,
		},
	}
	return gs, s.envData(st, env), nil
}

// envData renders the env Secret: egg variables plus the variables Wings adds
// (parsed STARTUP, SERVER_*), so install Jobs see what Wings' installer sees.
func (s *Syncer) envData(st *settings.Settings, env map[string]any) map[string]string {
	out := map[string]string{}
	vars := map[string]string{}
	for k, v := range env {
		vars[strings.ToUpper(k)] = stringify(v)
	}
	for k, v := range vars {
		out[k] = v
	}
	memory := st.Build.MemoryLimit
	port := int(st.Allocations.Default.Port)
	ip := st.Allocations.Default.IP
	out["STARTUP"] = ParseInvocation(st.Invocation, vars, memory, port, ip)
	out["SERVER_MEMORY"] = strconv.FormatInt(memory, 10)
	out["SERVER_IP"] = ip
	out["SERVER_PORT"] = strconv.Itoa(port)
	out["SERVER_PUBLIC_IP"] = ip
	if _, ok := out["TZ"]; !ok {
		out["TZ"] = s.Timezone
	}
	return out
}

// ParseInvocation mirrors Wings' server.parseInvocation.
func ParseInvocation(invocation string, vars map[string]string, memory int64, port int, ip string) string {
	invocation = strings.ReplaceAll(invocation, "{{", "${")
	invocation = strings.ReplaceAll(invocation, "}}", "}")
	for k, v := range vars {
		invocation = strings.ReplaceAll(invocation, "${"+k+"}", v)
	}
	invocation = strings.ReplaceAll(invocation, "${SERVER_PORT}", strconv.Itoa(port))
	invocation = strings.ReplaceAll(invocation, "${SERVER_MEMORY}", strconv.FormatInt(memory, 10))
	invocation = strings.ReplaceAll(invocation, "${SERVER_IP}", ip)
	return invocation
}

func stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func (s *Syncer) writeEnvSecret(ctx context.Context, gs *v1alpha1.GameServer, data map[string]string) error {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.EnvSecret(gs.Spec.Panel.UUID), Namespace: gs.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, s.Store.Client, sec, func() error {
		sec.Labels = map[string]string{v1alpha1.LabelServerUUID: gs.Spec.Panel.UUID, v1alpha1.LabelComponent: "env", render.LabelPartOf: render.PartOfValue}
		sec.Type = corev1.SecretTypeOpaque
		sec.Data = nil
		sec.StringData = data
		return controllerutil.SetControllerReference(gs, sec, s.Store.Client.Scheme())
	})
	return err
}

func (s *Syncer) writeInstallScript(ctx context.Context, gs *v1alpha1.GameServer, gen int64, script string) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: names.InstallConfigMap(gs.Spec.Panel.UUID, gen), Namespace: gs.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, s.Store.Client, cm, func() error {
		cm.Labels = map[string]string{v1alpha1.LabelServerUUID: gs.Spec.Panel.UUID, v1alpha1.LabelComponent: "install", render.LabelPartOf: render.PartOfValue}
		cm.Data = map[string]string{"install.sh": strings.ReplaceAll(script, "\r\n", "\n")}
		return controllerutil.SetControllerReference(gs, cm, s.Store.Client.Scheme())
	})
	return err
}

// Sync re-fetches the Panel configuration into spec.panel and the env Secret (section 8.6).
func (s *Syncer) Sync(ctx context.Context, uuid string) error {
	cfg, err := s.Panel.GetServerConfiguration(ctx, uuid)
	if err != nil {
		return fmt.Errorf("fetch configuration: %w", err)
	}
	return s.Apply(ctx, uuid, cfg)
}

// Apply writes a fetched configuration into the CR and Secret.
func (s *Syncer) Apply(ctx context.Context, uuid string, cfg *remote.ServerConfigurationResponse) error {
	gs, err := s.Store.Get(ctx, uuid)
	if err != nil {
		return err
	}
	raw, env, err := splitSettings(cfg.Settings)
	if err != nil {
		return err
	}
	st, err := settings.Parse(apiextensionsv1.JSON{Raw: raw})
	if err != nil {
		return err
	}
	proc, err := json.Marshal(cfg.ProcessConfiguration)
	if err != nil {
		return err
	}
	if err := s.writeEnvSecret(ctx, gs, s.envData(st, env)); err != nil {
		return err
	}
	rev := Revision(raw, proc)
	if gs.Spec.Panel.PanelRevision == rev {
		return nil
	}
	patch := client.MergeFrom(gs.DeepCopy())
	gs.Spec.Panel.Settings = apiextensionsv1.JSON{Raw: raw}
	gs.Spec.Panel.ProcessConfiguration = apiextensionsv1.JSON{Raw: proc}
	gs.Spec.Panel.PanelRevision = rev
	if gs.Annotations == nil {
		gs.Annotations = map[string]string{}
	}
	gs.Annotations[v1alpha1.AnnotationPanelName] = st.Meta.Name
	if gs.Labels == nil {
		gs.Labels = map[string]string{}
	}
	gs.Labels[v1alpha1.LabelEggUUID] = st.Egg.ID
	return s.Store.Client.Patch(ctx, gs, patch)
}

// RequestInstall fetches the install payload and bumps spec.install (section 8.2).
func (s *Syncer) RequestInstall(ctx context.Context, uuid string, reinstall bool) error {
	gs, err := s.Store.Get(ctx, uuid)
	if err != nil {
		return err
	}
	script, err := s.Panel.GetInstallationScript(ctx, uuid)
	if err != nil {
		return fmt.Errorf("fetch install script: %w", err)
	}
	gen := gs.Spec.Install.Generation + 1
	if err := s.writeInstallScript(ctx, gs, gen, script.Script); err != nil {
		return err
	}
	return s.Store.PatchSpec(ctx, uuid, map[string]any{"install": map[string]any{
		"generation": gen, "reinstall": reinstall, "scriptConfigMap": names.InstallConfigMap(uuid, gen),
		"image": script.ContainerImage, "entrypoint": script.Entrypoint, "startOnInstall": false,
	}})
}

// Power patches spec.power for a Panel or websocket power action (section 8.3).
func (s *Syncer) Power(ctx context.Context, uuid, action string) error {
	gs, err := s.Store.Get(ctx, uuid)
	if err != nil {
		return err
	}
	var power map[string]any
	switch action {
	case "start", "restart":
		power = map[string]any{"desired": string(v1alpha1.PowerRunning), "generation": gs.Spec.Power.Generation + 1, "kill": false}
	case "stop":
		power = map[string]any{"desired": string(v1alpha1.PowerStopped), "generation": gs.Spec.Power.Generation + 1, "kill": false}
	case "kill":
		power = map[string]any{"desired": string(v1alpha1.PowerStopped), "generation": gs.Spec.Power.Generation + 1, "kill": true}
	default:
		return fmt.Errorf("invalid power action %q", action)
	}
	return s.Store.PatchSpec(ctx, uuid, map[string]any{"power": power})
}

// Delete removes the CR (section 8.8); the operator's finalizer does the rest.
func (s *Syncer) Delete(ctx context.Context, uuid string) error {
	gs := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Namespace: s.Store.Namespace, Name: names.ForUUID(uuid)}}
	return client.IgnoreNotFound(s.Store.Client.Delete(ctx, gs))
}

// AgentConfiguration assembles the Wings server configuration served to the
// agent of a server (section 5.7 and 9.4): spec.panel plus the env Secret,
// the default allocation IP rewritten to the bind address and unlimited
// memory replaced by the class default so SERVER_MEMORY is usable.
func (s *Syncer) AgentConfiguration(ctx context.Context, gs *v1alpha1.GameServer) (json.RawMessage, json.RawMessage, error) {
	env, err := s.Store.EnvSecret(ctx, gs.Spec.Panel.UUID)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(gs.Spec.Panel.Settings.Raw, &m); err != nil {
		return nil, nil, err
	}
	envAny := map[string]any{}
	for k, v := range env {
		envAny[k] = v
	}
	m["environment"] = envAny
	if allocs, ok := m["allocations"].(map[string]any); ok {
		if def, ok := allocs["default"].(map[string]any); ok {
			if port, _ := def["port"].(float64); port != 0 {
				def["ip"] = BindIP
			}
		}
	}
	if build, ok := m["build"].(map[string]any); ok {
		if mem, _ := build["memory_limit"].(float64); mem == 0 {
			cls := s.Store.ClassSpec(ctx, gs)
			if cls.Resources.UnlimitedMemoryMiB > 0 {
				build["memory_limit"] = cls.Resources.UnlimitedMemoryMiB
			}
		}
	}
	settingsJSON, err := json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	return settingsJSON, json.RawMessage(gs.Spec.Panel.ProcessConfiguration.Raw), nil
}

// Resync compares the Panel's server list with the cluster (section 13).
func (s *Syncer) Resync(ctx context.Context) error {
	servers, err := s.Panel.ListServers(ctx)
	if err != nil {
		return err
	}
	panelSet := map[string]remote.RawServerData{}
	for _, srv := range servers {
		panelSet[srv.Uuid] = srv
	}
	existing, err := s.Store.List(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for i := range existing {
		gs := &existing[i]
		uuid := gs.Spec.Panel.UUID
		seen[uuid] = true
		srv, ok := panelSet[uuid]
		if !ok {
			_ = s.Store.SetCondition(ctx, uuid, metav1.Condition{Type: v1alpha1.ConditionOrphaned, Status: metav1.ConditionTrue, Reason: "NotOnPanel", Message: "the Panel no longer lists this server on this node; it is never deleted automatically"})
			continue
		}
		var proc remote.ProcessConfiguration
		_ = json.Unmarshal(srv.ProcessConfiguration, &proc)
		cfg := &remote.ServerConfigurationResponse{Settings: srv.Settings, ProcessConfiguration: &proc}
		raw, _, err := splitSettings(cfg.Settings)
		if err != nil {
			continue
		}
		procJSON, _ := json.Marshal(cfg.ProcessConfiguration)
		if Revision(raw, procJSON) != gs.Spec.Panel.PanelRevision {
			s.Log.Info("resync: panel configuration changed", "uuid", uuid)
			if err := s.Apply(ctx, uuid, cfg); err != nil {
				s.Log.Warn("resync: apply failed", "uuid", uuid, "error", err)
			}
		}
		if c := findCondition(gs, v1alpha1.ConditionOrphaned); c != nil && c.Status == metav1.ConditionTrue {
			_ = s.Store.SetCondition(ctx, uuid, metav1.Condition{Type: v1alpha1.ConditionOrphaned, Status: metav1.ConditionFalse, Reason: "OnPanel"})
		}
	}
	for uuid, srv := range panelSet {
		if seen[uuid] {
			continue
		}
		s.Log.Info("resync: adopting server known to the Panel but missing in the cluster", "uuid", uuid)
		var proc remote.ProcessConfiguration
		_ = json.Unmarshal(srv.ProcessConfiguration, &proc)
		if err := s.Adopt(ctx, uuid, &remote.ServerConfigurationResponse{Settings: srv.Settings, ProcessConfiguration: &proc}); err != nil {
			s.Log.Warn("resync: adopt failed", "uuid", uuid, "error", err)
		}
	}
	return nil
}

func findCondition(gs *v1alpha1.GameServer, t string) *metav1.Condition {
	for i := range gs.Status.Conditions {
		if gs.Status.Conditions[i].Type == t {
			return &gs.Status.Conditions[i]
		}
	}
	return nil
}

// RunResync runs Resync at startup and every interval until ctx ends.
func (s *Syncer) RunResync(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.Resync(ctx); err != nil {
			s.Log.Warn("resync failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
