// Package panelapi serves the node-facing Wings HTTP API to the Panel and
// browsers (ARCHITECTURE.md 5.2).
package panelapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/pelican/wings/system"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/gateway/agents"
	"github.com/Claiyc/pelican-k8s/internal/gateway/config"
	"github.com/Claiyc/pelican-k8s/internal/gateway/jwtx"
	"github.com/Claiyc/pelican-k8s/internal/gateway/serversync"
	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
	"github.com/Claiyc/pelican-k8s/internal/operator/settings"
	"github.com/Claiyc/pelican-k8s/internal/version"
)

// Handler serves the Panel-facing API.
type Handler struct {
	Cfg    *config.Config
	Store  *store.Store
	Agents *agents.Resolver
	Sync   *serversync.Syncer
	Log    *slog.Logger
	// WS handles websocket upgrades (set by the wsproxy package).
	WS http.Handler
	// Diagnostics renders the /api/diagnostics report.
	Diagnostics func(ctx context.Context) string
	// ClusterVersion is shown in /api/system.
	ClusterVersion string
}

// Routes returns the HTTP handler.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /download/backup", h.signedProxy("backup-download"))
	mux.HandleFunc("GET /download/file", h.signedProxy("file-download"))
	mux.HandleFunc("POST /upload/file", h.signedProxy("file-upload"))
	mux.HandleFunc("GET /api/servers/{server}/ws", h.websocket)
	mux.HandleFunc("POST /api/transfers", h.unsupported)

	auth := h.requireNodeToken
	mux.HandleFunc("POST /api/update", auth(h.update))
	mux.HandleFunc("GET /api/system", auth(h.systemInfo))
	mux.HandleFunc("GET /api/diagnostics", auth(h.diagnostics))
	mux.HandleFunc("GET /api/system/docker/disk", auth(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, system.DockerDiskUsage{}) }))
	mux.HandleFunc("DELETE /api/system/docker/image/prune", auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ImagesDeleted": nil, "SpaceReclaimed": 0})
	}))
	mux.HandleFunc("GET /api/system/ips", auth(h.systemIPs))
	mux.HandleFunc("GET /api/system/utilization", auth(h.utilization))
	mux.HandleFunc("GET /api/servers", auth(h.listServers))
	mux.HandleFunc("POST /api/servers", auth(h.createServer))
	mux.HandleFunc("DELETE /api/transfers/{server}", auth(h.unsupported))
	mux.HandleFunc("POST /api/deauthorize-user", auth(h.deauthorize))

	mux.HandleFunc("GET /api/servers/{server}", auth(h.serverExists(h.getServer)))
	mux.HandleFunc("DELETE /api/servers/{server}", auth(h.serverExists(h.deleteServer)))
	mux.HandleFunc("POST /api/servers/{server}/sync", auth(h.serverExists(h.syncServer)))
	mux.HandleFunc("POST /api/servers/{server}/install", auth(h.serverExists(h.install(false))))
	mux.HandleFunc("POST /api/servers/{server}/reinstall", auth(h.serverExists(h.install(true))))
	mux.HandleFunc("POST /api/servers/{server}/power", auth(h.serverExists(h.power)))
	mux.HandleFunc("POST /api/servers/{server}/transfer", auth(h.serverExists(h.unsupported)))
	mux.HandleFunc("DELETE /api/servers/{server}/transfer", auth(h.serverExists(h.unsupported)))
	mux.HandleFunc("POST /api/servers/{server}/backup", auth(h.serverExists(h.backupProxy)))
	mux.HandleFunc("POST /api/servers/{server}/backup/{backup}/restore", auth(h.serverExists(h.backupProxy)))
	// Everything else under a server is proxied by prefix.
	mux.HandleFunc("/api/servers/{server}/", auth(h.serverExists(h.proxy)))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "The requested resource does not exist.")
	})
	return h.wrap(mux)
}

// wrap sets the Wings User-Agent on every response and recovers panics.
func (h *Handler) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("User-Agent", h.Cfg.UserAgent())
		defer func() {
			if rec := recover(); rec != nil {
				h.Log.Error("panic in panel api", "error", rec, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) requireNodeToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
		if len(auth) != 2 || auth[0] != "Bearer" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "The required authorization heads were not present in the request.")
			return
		}
		if subtle.ConstantTimeCompare([]byte(auth[1]), []byte(h.Cfg.NodeToken)) != 1 {
			writeError(w, http.StatusForbidden, "You are not authorized to access this endpoint.")
			return
		}
		next(w, r)
	}
}

func (h *Handler) serverExists(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.Store.Exists(r.Context(), r.PathValue("server")) {
			writeError(w, http.StatusNotFound, "The requested resource does not exist on this instance.")
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (h *Handler) unsupported(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "Server transfers are not supported by pelican-k8s in this version.")
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"applied": false})
}

func (h *Handler) systemInfo(w http.ResponseWriter, r *http.Request) {
	kernel := "unknown"
	if b, err := readFile("/proc/sys/kernel/osrelease"); err == nil {
		kernel = strings.TrimSpace(b)
	}
	info := system.Information{
		Version: h.Cfg.AdvertisedVersion,
		Docker: system.DockerInformation{
			Version:    "kubernetes " + h.ClusterVersion,
			Cgroups:    system.DockerCgroups{Driver: "systemd", Version: "2"},
			Containers: system.DockerContainers{},
			Storage:    system.DockerStorage{Driver: "csi", Filesystem: "pvc"},
			Runc:       system.DockerRunc{Version: "pelican-k8s " + version.Version},
		},
		System: system.System{Architecture: runtime.GOARCH, CPUThreads: runtime.NumCPU(), KernelVersion: kernel, OS: "kubernetes", OSType: "linux"},
	}
	if n, err := h.countServers(r.Context()); err == nil {
		info.Docker.Containers.Total = n.total
		info.Docker.Containers.Running = n.running
		info.Docker.Containers.Stopped = n.total - n.running
	}
	if r.URL.Query().Get("v") == "2" {
		writeJSON(w, 200, info)
		return
	}
	writeJSON(w, 200, map[string]any{
		"architecture": info.System.Architecture, "cpu_count": info.System.CPUThreads, "kernel_version": info.System.KernelVersion,
		"os": info.System.OSType, "version": info.Version,
	})
}

type counts struct{ total, running int }

func (h *Handler) countServers(ctx context.Context) (counts, error) {
	list, err := h.Store.List(ctx)
	if err != nil {
		return counts{}, err
	}
	c := counts{total: len(list)}
	for _, gs := range list {
		if gs.Status.Process.State == v1alpha1.ProcessRunning || gs.Status.Process.State == v1alpha1.ProcessStarting {
			c.running++
		}
	}
	return c, nil
}

func (h *Handler) diagnostics(w http.ResponseWriter, r *http.Request) {
	var report string
	if h.Diagnostics != nil {
		report = h.Diagnostics(r.Context())
	} else {
		report = "pelican-k8s gateway " + version.Version + "\n"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(report))
}

func (h *Handler) systemIPs(w http.ResponseWriter, r *http.Request) {
	ips := h.Cfg.ExternalIPs
	if len(ips) == 0 {
		cls, err := h.Store.Class(r.Context(), nil)
		if err == nil {
			ips = cls.Spec.Exposure.ExternalIPs
		}
	}
	if len(ips) == 0 {
		nodes := &corev1.NodeList{}
		if err := h.Store.Client.List(r.Context(), nodes); err == nil {
			for _, n := range nodes.Items {
				for _, a := range n.Status.Addresses {
					if a.Type == corev1.NodeExternalIP || a.Type == corev1.NodeInternalIP {
						ips = append(ips, a.Address)
					}
				}
			}
		}
	}
	if ips == nil {
		ips = []string{}
	}
	writeJSON(w, 200, system.IpAddresses{IpAddresses: ips})
}

func (h *Handler) utilization(w http.ResponseWriter, r *http.Request) {
	u := system.Utilization{DiskDetails: []system.DiskInfo{}}
	nodes := &corev1.NodeList{}
	if err := h.Store.Client.List(r.Context(), nodes); err == nil {
		for _, n := range nodes.Items {
			u.MemoryTotal += uint64(n.Status.Allocatable.Memory().Value())
			u.DiskTotal += uint64(n.Status.Allocatable.StorageEphemeral().Value())
		}
	}
	if list, err := h.Store.List(r.Context()); err == nil {
		for _, gs := range list {
			if gs.Status.Usage != nil {
				u.MemoryUsed += uint64(gs.Status.Usage.MemoryBytes)
				u.DiskUsed += uint64(gs.Status.Usage.DiskBytes)
			}
		}
	}
	writeJSON(w, 200, u)
}

// apiResponse mirrors Wings' server.APIResponse for servers without a reachable agent.
func (h *Handler) fallbackState(ctx context.Context, gs *v1alpha1.GameServer) map[string]any {
	state := gs.Status.Process.State
	if state == "" {
		state = v1alpha1.ProcessOffline
	}
	pod, _ := h.Store.Pod(ctx, gs.Spec.Panel.UUID)
	if pod == nil {
		state = "missing"
	}
	st, _ := settings.Parse(gs.Spec.Panel.Settings)
	var cfg any = json.RawMessage(gs.Spec.Panel.Settings.Raw)
	suspended := st != nil && st.Suspended
	return map[string]any{
		"state":        state,
		"is_suspended": suspended,
		"utilization":  map[string]any{"memory_bytes": 0, "memory_limit_bytes": 0, "cpu_absolute": 0, "network": map[string]any{"rx_bytes": 0, "tx_bytes": 0}, "disk_io": map[string]any{"read_bytes": 0, "write_bytes": 0}, "uptime": 0, "state": state, "disk_bytes": 0},
		"configuration": cfg,
	}
}

func (h *Handler) serverState(ctx context.Context, gs *v1alpha1.GameServer) json.RawMessage {
	t, err := h.Agents.Resolve(ctx, gs.Spec.Panel.UUID)
	if err == nil {
		if body, err := h.Agents.State(ctx, t, false); err == nil {
			return body
		}
	}
	b, _ := json.Marshal(h.fallbackState(ctx, gs))
	return b
}

func (h *Handler) listServers(w http.ResponseWriter, r *http.Request) {
	list, err := h.Store.List(r.Context())
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	out := make([]json.RawMessage, 0, len(list))
	for i := range list {
		out = append(out, h.serverState(r.Context(), &list[i]))
	}
	writeJSON(w, 200, out)
}

func (h *Handler) getServer(w http.ResponseWriter, r *http.Request) {
	gs, err := h.Store.Get(r.Context(), r.PathValue("server"))
	if err != nil {
		writeError(w, 404, "The requested resource does not exist on this instance.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(h.serverState(r.Context(), gs))
}

func (h *Handler) createServer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UUID              string `json:"uuid"`
		StartOnCompletion bool   `json:"start_on_completion"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.UUID) != 36 {
		writeError(w, http.StatusUnprocessableEntity, "The data provided in the request could not be validated.")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := h.Sync.Create(ctx, body.UUID, body.StartOnCompletion); err != nil {
		h.Log.Error("create server failed", "uuid", body.UUID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create server: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) deleteServer(w http.ResponseWriter, r *http.Request) {
	if err := h.Sync.Delete(r.Context(), r.PathValue("server")); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	h.Agents.Invalidate(r.PathValue("server"))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) syncServer(w http.ResponseWriter, r *http.Request) {
	if err := h.Sync.Sync(r.Context(), r.PathValue("server")); err != nil {
		h.Log.Error("sync failed", "uuid", r.PathValue("server"), "error", err)
		writeError(w, 500, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) install(reinstall bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uuid := r.PathValue("server")
		if reinstall {
			if gs, err := h.Store.Get(r.Context(), uuid); err == nil && gs.Status.Power.LastAction != nil && time.Since(gs.Status.Power.LastAction.At.Time) < 3*time.Second {
				writeError(w, http.StatusConflict, "Cannot execute server reinstall event while another power action is running.")
				return
			}
		}
		if err := h.Sync.RequestInstall(r.Context(), uuid, reinstall); err != nil {
			h.Log.Error("install request failed", "uuid", uuid, "error", err)
			writeError(w, 500, err.Error())
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

func (h *Handler) power(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "invalid body")
		return
	}
	switch body.Action {
	case "start", "stop", "restart", "kill":
	default:
		writeError(w, http.StatusUnprocessableEntity, `The power action provided was not valid, should be one of "stop", "start", "restart", "kill"`)
		return
	}
	uuid := r.PathValue("server")
	if body.Action == "start" || body.Action == "restart" {
		if gs, err := h.Store.Get(r.Context(), uuid); err == nil {
			if st, err := settings.Parse(gs.Spec.Panel.Settings); err == nil && st.Suspended {
				writeError(w, http.StatusBadRequest, "Cannot start or restart a server that is suspended.")
				return
			}
		}
	}
	if err := h.Sync.Power(r.Context(), uuid, body.Action); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) deauthorize(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var data struct {
		User    string   `json:"user"`
		Servers []string `json:"servers"`
	}
	_ = json.Unmarshal(body, &data)
	targets := data.Servers
	if len(targets) == 0 {
		list, _ := h.Store.List(r.Context())
		for _, gs := range list {
			targets = append(targets, gs.Spec.Panel.UUID)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, uuid := range targets {
		t, err := h.Agents.Resolve(ctx, uuid)
		if err != nil {
			continue
		}
		payload, _ := json.Marshal(map[string]any{"user": data.User, "servers": []string{uuid}})
		if _, _, err := h.Agents.Do(ctx, t, http.MethodPost, "/api/deauthorize-user", strings.NewReader(string(payload)), "application/json"); err != nil {
			h.Log.Warn("deauthorize forward failed", "uuid", uuid, "error", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// proxy forwards a server-scoped request to its agent.
func (h *Handler) proxy(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("server")
	t, err := h.Agents.Resolve(r.Context(), uuid)
	if err != nil {
		h.agentUnavailable(w, err)
		return
	}
	h.Agents.Proxy(w, r, t, nil)
}

// backupProxy records the backup as pending so the agent's remote-API calls
// for it are allowed, then proxies.
func (h *Handler) backupProxy(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("server")
	backup := r.PathValue("backup")
	if backup == "" {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		var data struct {
			UUID string `json:"uuid"`
		}
		_ = json.Unmarshal(body, &data)
		backup = data.UUID
	}
	if backup != "" {
		gs, err := h.Store.Get(r.Context(), uuid)
		if err == nil {
			pending := append([]v1alpha1.PendingBackup(nil), gs.Status.Backups.Pending...)
			found := false
			for _, p := range pending {
				if p.UUID == backup {
					found = true
				}
			}
			if !found {
				pending = append(pending, v1alpha1.PendingBackup{UUID: backup, StartedAt: nowMeta()})
				_ = h.Store.PatchStatus(r.Context(), uuid, map[string]any{"backups": map[string]any{"pending": pending}})
			}
		}
	}
	h.proxy(w, r)
}

func (h *Handler) agentUnavailable(w http.ResponseWriter, err error) {
	if errors.Is(err, agents.ErrUnavailable) {
		writeError(w, http.StatusServiceUnavailable, "server pod unavailable")
		return
	}
	if apierrors.IsNotFound(err) {
		writeError(w, http.StatusNotFound, "The requested resource does not exist on this instance.")
		return
	}
	writeError(w, http.StatusBadGateway, err.Error())
}

// signedProxy verifies the Panel-signed JWT in ?token=, re-signs it for the
// target agent and proxies the request (section 8.4).
func (h *Handler) signedProxy(scope string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		claims, raw, err := jwtx.Verify([]byte(token), []byte(h.Cfg.NodeToken))
		if err != nil {
			writeError(w, http.StatusForbidden, "jwt: "+err.Error())
			return
		}
		if claims.ServerUUID == "" || !strings.Contains(" "+claims.Scope+" ", " "+scope+" ") {
			writeError(w, http.StatusNotFound, "The requested resource was not found on this server.")
			return
		}
		t, err := h.Agents.Resolve(r.Context(), claims.ServerUUID)
		if err != nil {
			h.agentUnavailable(w, err)
			return
		}
		resigned, err := jwtx.Resign(raw, []byte(t.Token))
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		h.Agents.Proxy(w, r, t, func(out *http.Request) {
			q := out.URL.Query()
			q.Set("token", string(resigned))
			out.URL.RawQuery = q.Encode()
		})
	}
}

func (h *Handler) websocket(w http.ResponseWriter, r *http.Request) {
	if h.WS == nil {
		writeError(w, 500, "websocket proxy not configured")
		return
	}
	h.WS.ServeHTTP(w, r)
}

// ClientIP returns the caller address honouring trusted proxies.
func ClientIP(r *http.Request, trusted []string) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" && isTrusted(host, trusted) {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	return host
}

func isTrusted(host string, cidrs []string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil && n.Contains(ip) {
			return true
		} else if err != nil && c == host {
			return true
		}
	}
	return false
}

var _ = client.IgnoreNotFound
var _ = fmt.Sprintf
