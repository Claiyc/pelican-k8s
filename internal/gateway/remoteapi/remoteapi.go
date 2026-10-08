// Package remoteapi serves the Wings remote API to agents: each agent sees a
// Panel that owns exactly its own server (ARCHITECTURE.md 5.7).
package remoteapi

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pelican/wings/remote"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/gateway/agents"
	"github.com/Claiyc/pelican-k8s/internal/gateway/panel"
	"github.com/Claiyc/pelican-k8s/internal/gateway/serversync"
	"github.com/Claiyc/pelican-k8s/internal/gateway/store"
)

// SftpSessions answers the agents' SFTP auth calls from the relay's records.
type SftpSessions interface {
	// Lookup returns the recorded Panel response for a relayed login.
	Lookup(username, password string) (*remote.SftpAuthResponse, bool)
}

// Handler serves /api/remote/* to agents.
type Handler struct {
	Store  *store.Store
	Panel  *panel.Client
	Sync   *serversync.Syncer
	Agents *agents.Resolver
	Sftp   SftpSessions
	Log    *slog.Logger
	// Metrics counters (optional).
	OnUnmatched func(method, path string)

	// statusMu serialises the state posts of one server (striped by UUID).
	statusMu [32]sync.Mutex
	// RepollEvery and RepollFor pace the polls after a state post the agent
	// did not answer for (default every second for 30 s).
	RepollEvery, RepollFor time.Duration
	// repolling holds the servers whose state is being polled again.
	repolling sync.Map
}

type ctxKey struct{}

// Routes returns the handler.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/remote/servers", h.listServers)
	mux.HandleFunc("POST /api/remote/servers/reset", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /api/remote/activity", h.activity)
	mux.HandleFunc("POST /api/remote/sftp/auth", h.sftpAuth)
	mux.HandleFunc("GET /api/remote/servers/{uuid}", h.own(h.getServer))
	mux.HandleFunc("GET /api/remote/servers/{uuid}/install", h.own(h.getInstall))
	mux.HandleFunc("POST /api/remote/servers/{uuid}/install", h.own(h.installResult))
	mux.HandleFunc("POST /api/remote/servers/{uuid}/container/status", h.own(h.containerStatus))
	mux.HandleFunc("POST /api/remote/servers/{uuid}/install/prepared", h.own(h.installPrepared))
	mux.HandleFunc("GET /api/remote/servers/{uuid}/install/state", h.own(h.installState))
	mux.HandleFunc("GET /api/remote/backups/{backup}", h.backup)
	mux.HandleFunc("POST /api/remote/backups/{backup}", h.backup)
	mux.HandleFunc("POST /api/remote/backups/{backup}/restore", h.backup)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if h.OnUnmatched != nil {
			h.OnUnmatched(r.Method, r.URL.Path)
		}
		h.Log.Warn("unmatched remote api call from agent", "method", r.Method, "path", r.URL.Path)
		writeErr(w, http.StatusNotFound, "NotFoundHttpException", "The requested resource does not exist.")
	})
	return h.auth(mux)
}

// auth maps the agent token to its server and stores the UUID in the context.
func (h *Handler) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		uuid, ok := h.Store.ServerByAgentToken(r.Context(), bearer)
		if !ok {
			writeErr(w, http.StatusForbidden, "AccessDeniedHttpException", "invalid agent credentials")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, uuid)))
	})
}

func caller(r *http.Request) string {
	u, _ := r.Context().Value(ctxKey{}).(string)
	return u
}

// own ensures the path UUID belongs to the calling agent.
func (h *Handler) own(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("uuid") != caller(r) {
			writeErr(w, http.StatusNotFound, "NotFoundHttpException", "The requested resource does not exist.")
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

func writeErr(w http.ResponseWriter, code int, kind, detail string) {
	writeJSON(w, code, map[string]any{"errors": []map[string]string{{"code": kind, "status": strconv.Itoa(code), "detail": detail}}})
}

func (h *Handler) payload(ctx context.Context, uuid string) (map[string]any, error) {
	gs, err := h.Store.Get(ctx, uuid)
	if err != nil {
		return nil, err
	}
	settings, proc, err := h.Sync.AgentConfiguration(ctx, gs)
	if err != nil {
		return nil, err
	}
	return map[string]any{"uuid": uuid, "settings": settings, "process_configuration": proc}, nil
}

func (h *Handler) listServers(w http.ResponseWriter, r *http.Request) {
	p, err := h.payload(r.Context(), caller(r))
	if err != nil {
		writeErr(w, 500, "ServerError", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"data": []any{p}, "meta": remote.Pagination{CurrentPage: 1, From: 1, LastPage: 1, PerPage: 50, To: 1, Total: 1}})
}

func (h *Handler) getServer(w http.ResponseWriter, r *http.Request) {
	p, err := h.payload(r.Context(), caller(r))
	if err != nil {
		writeErr(w, 500, "ServerError", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"settings": p["settings"], "process_configuration": p["process_configuration"]})
}

func (h *Handler) getInstall(w http.ResponseWriter, r *http.Request) {
	gs, err := h.Store.Get(r.Context(), caller(r))
	if err != nil {
		writeErr(w, 404, "NotFoundHttpException", err.Error())
		return
	}
	script, err := h.Store.InstallScript(r.Context(), gs.Spec.Panel.UUID, gs.Spec.Install.Generation)
	if err != nil {
		writeErr(w, 404, "NotFoundHttpException", "no install script for the current generation")
		return
	}
	writeJSON(w, 200, remote.InstallationScript{ContainerImage: gs.Spec.Install.Image, Entrypoint: gs.Spec.Install.Entrypoint, Script: script})
}

func (h *Handler) installPrepared(w http.ResponseWriter, r *http.Request) {
	uuid := caller(r)
	gs, err := h.Store.Get(r.Context(), uuid)
	if err != nil {
		writeErr(w, 404, "NotFoundHttpException", err.Error())
		return
	}
	gen := gs.Spec.Install.Generation
	if err := h.Store.PatchStatus(r.Context(), uuid, map[string]any{"install": map[string]any{"preparedGeneration": gen, "result": v1alpha1.InstallRunning}}); err != nil {
		writeErr(w, 500, "ServerError", err.Error())
		return
	}
	strict := h.Store.ClassSpec(r.Context(), gs).Install.StrictExitCode
	writeJSON(w, 200, map[string]any{"generation": gen, "strict_exit_code": strict})
}

func (h *Handler) installState(w http.ResponseWriter, r *http.Request) {
	gs, err := h.Store.Get(r.Context(), caller(r))
	if err != nil {
		writeErr(w, 404, "NotFoundHttpException", err.Error())
		return
	}
	gen, _ := strconv.ParseInt(r.URL.Query().Get("generation"), 10, 64)
	result := gs.Status.Install.Result
	if gen != 0 && gen != gs.Spec.Install.Generation {
		result = "Superseded"
	}
	writeJSON(w, 200, map[string]any{"generation": gs.Spec.Install.Generation, "result": result, "job_finished": result != v1alpha1.InstallRunning && result != ""})
}

func (h *Handler) installResult(w http.ResponseWriter, r *http.Request) {
	uuid := caller(r)
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req remote.InstallStatusRequest
	_ = json.Unmarshal(body, &req)
	gs, err := h.Store.Get(r.Context(), uuid)
	if err != nil {
		writeErr(w, 404, "NotFoundHttpException", err.Error())
		return
	}
	result := v1alpha1.InstallFailed
	if req.Successful {
		result = v1alpha1.InstallSucceeded
	}
	gen := gs.Spec.Install.Generation
	status := map[string]any{"install": map[string]any{"result": result, "finishedAt": metav1.Now(), "reportedGeneration": gen}}
	if err := h.Store.PatchStatus(r.Context(), uuid, status); err != nil {
		h.Log.Warn("install result status patch failed", "uuid", uuid, "error", err)
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	if err := h.Panel.SetInstallationStatus(fctx, uuid, req.Successful, req.Reinstall); err != nil {
		h.Log.Warn("forwarding install result to panel failed", "uuid", uuid, "error", err)
	}
	if req.Successful && gs.Spec.Install.StartOnInstall {
		if err := h.Sync.Power(fctx, uuid, "start"); err != nil {
			h.Log.Warn("start after install failed", "uuid", uuid, "error", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// containerStatus records the observed state and derives the intentional stop
// (section 4.2), then forwards to the Panel.
func (h *Handler) containerStatus(w http.ResponseWriter, r *http.Request) {
	uuid := caller(r)
	var body struct {
		Data remote.ServerStateChange `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "BadRequest", "invalid body")
		return
	}
	sc := body.Data
	h.Agents.Invalidate(uuid)
	h.recordState(r.Context(), uuid, sc.NewState)
	if sc.PrevState == v1alpha1.ProcessStopping && sc.NewState == v1alpha1.ProcessOffline && !h.podTerminating(r.Context(), uuid) {
		// Conditional on the version the rule was checked against: a power
		// action another replica writes meanwhile is checked again, not overwritten.
		if err := h.Store.UpdateSpec(r.Context(), uuid, func(gs *v1alpha1.GameServer) (map[string]any, error) {
			if !intentionalStop(gs) {
				return nil, nil
			}
			return map[string]any{"power": map[string]any{"desired": string(v1alpha1.PowerStopped), "kill": false}}, nil
		}); err != nil {
			h.Log.Warn("cannot record the intentional stop", "uuid", uuid, "error", err)
		}
	}
	// Forward with a detached context: the agent's own client timeout must not
	// cancel the Panel call halfway.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	if err := h.Panel.PushServerStateChange(fctx, uuid, sc.PrevState, sc.NewState); err != nil {
		h.Log.Warn("forwarding container status to panel failed", "uuid", uuid, "error", err)
	}
	go h.recordUsage(context.WithoutCancel(r.Context()), uuid)
	w.WriteHeader(http.StatusNoContent)
}

// recordState writes a posted process state. The agent sends each change from
// its own goroutine, so posts can arrive out of order, and with several
// gateway replicas at different replicas. The state written is the one the
// agent reports on a fresh poll, and the write applies only if the GameServer
// has not changed since it was read before the poll: a poll that lost the race
// to a newer one is retried rather than written over it. Each replica also
// handles a server's posts one at a time. When the poll fails the posted state
// is written and the agent is polled again in the background until it answers,
// because no further post comes while its state stays put.
func (h *Handler) recordState(ctx context.Context, uuid, posted string) {
	mu := h.stateLock(uuid)
	mu.Lock()
	defer mu.Unlock()
	h.writeState(ctx, uuid, func(state string, err error) (string, bool) {
		if err == nil {
			return state, true
		}
		h.Log.Debug("cannot poll the agent, recording the posted state", "uuid", uuid, "error", err)
		if _, busy := h.repolling.LoadOrStore(uuid, true); !busy {
			go h.repollState(context.WithoutCancel(ctx), uuid)
		}
		return posted, true
	})
}

// repollState writes the agent's state once it answers a poll.
func (h *Handler) repollState(ctx context.Context, uuid string) {
	defer h.repolling.Delete(uuid)
	every, total := h.RepollEvery, h.RepollFor
	if every <= 0 {
		every = time.Second
	}
	if total <= 0 {
		total = 30 * time.Second
	}
	mu := h.stateLock(uuid)
	for deadline := time.Now().Add(total); time.Now().Before(deadline); {
		time.Sleep(every)
		mu.Lock()
		answered := false
		h.writeState(ctx, uuid, func(state string, err error) (string, bool) {
			answered = err == nil
			return state, answered
		})
		mu.Unlock()
		if answered {
			return
		}
	}
	h.Log.Warn("the agent did not answer a state poll; status.process keeps the last posted state", "uuid", uuid)
}

func (h *Handler) stateLock(uuid string) *sync.Mutex {
	f := fnv.New32a()
	_, _ = f.Write([]byte(uuid))
	return &h.statusMu[f.Sum32()%uint32(len(h.statusMu))]
}

// agentState polls the agent for its process state. The poll is short: it
// runs under the server's stripe of statusMu, which other servers share.
func (h *Handler) agentState(ctx context.Context, uuid string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	t, err := h.Agents.Resolve(ctx, uuid)
	if err != nil {
		return "", err
	}
	body, err := h.Agents.State(ctx, t, true)
	if err != nil {
		return "", err
	}
	var st struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return "", err
	}
	if st.State == "" {
		return "", errors.New("the agent reported no state")
	}
	return st.State, nil
}

// stateWriteBackoff paces the retries of a state write that lost to another
// write of the GameServer (another replica's, or any other status change).
var stateWriteBackoff = wait.Backoff{Duration: 20 * time.Millisecond, Factor: 2, Jitter: 0.1, Steps: 8, Cap: time.Second}

// writeState reads the GameServer, polls the agent, and writes the state pick
// chooses from the poll's result, conditional on the version read. A write
// that conflicts starts over with a fresh read and poll. pick returns false
// to write nothing.
func (h *Handler) writeState(ctx context.Context, uuid string, pick func(state string, err error) (string, bool)) {
	err := retry.OnError(stateWriteBackoff, apierrors.IsConflict, func() error {
		gs, err := h.Store.Get(ctx, uuid)
		if err != nil {
			return err
		}
		state, ok := pick(h.agentState(ctx, uuid))
		if !ok {
			return nil
		}
		status := map[string]any{"process": map[string]any{"state": state, "since": metav1.Now()}}
		return h.Store.PatchStatusAt(ctx, uuid, gs.ResourceVersion, status)
	})
	if err != nil {
		h.Log.Warn("status patch failed", "uuid", uuid, "error", err)
	}
}

// restartWindow is how long after the operator issued a restart its stop
// half is not taken for an intentional stop: Wings waits up to 10 minutes for
// the process to stop before it starts it again.
const restartWindow = 10 * time.Minute

// podTerminating reports a game or agent pod that is terminating: a drain or
// a recreate stops the process without changing what the user wants.
func (h *Handler) podTerminating(ctx context.Context, uuid string) bool {
	for _, get := range []func(context.Context, string) (*corev1.Pod, error){h.Store.Pod, h.Store.AgentPod} {
		if pod, _ := get(ctx, uuid); pod != nil && !pod.DeletionTimestamp.IsZero() {
			return true
		}
	}
	return false
}

// intentionalStop reports whether a stopping → offline transition of a server
// whose pods are not terminating is a stop the Panel should see as the
// server's new desired state: the stop command typed into the console, a
// suspension, or a stop the operator issued for desired Stopped. A stop the
// operator issued for a pending power generation (a restart into a new game
// pod under RecreatePending) or the stop half of a restart does not change
// what the user wants. A console stop within restartWindow of a restart is
// then settled by crashwatch.
func intentionalStop(gs *v1alpha1.GameServer) bool {
	if gs.Spec.Power.Desired == v1alpha1.PowerStopped {
		return false
	}
	if la := gs.Status.Power.LastAction; la != nil && la.Action == "restart" && time.Since(la.At.Time) < restartWindow {
		return false
	}
	return gs.Spec.Power.Generation == gs.Status.Power.ObservedGeneration
}

// recordUsage samples the agent's utilization into status.usage (throttled by the caller pattern).
func (h *Handler) recordUsage(parent context.Context, uuid string) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	t, err := h.Agents.Resolve(ctx, uuid)
	if err != nil {
		return
	}
	body, err := h.Agents.State(ctx, t, true)
	if err != nil {
		return
	}
	var st struct {
		Utilization struct {
			Memory    int64   `json:"memory_bytes"`
			CPU       float64 `json:"cpu_absolute"`
			DiskBytes int64   `json:"disk_bytes"`
		} `json:"utilization"`
	}
	if json.Unmarshal(body, &st) != nil {
		return
	}
	_ = h.Store.PatchStatus(ctx, uuid, map[string]any{"usage": map[string]any{
		"memoryBytes": st.Utilization.Memory, "cpuPercent": strconv.FormatFloat(st.Utilization.CPU, 'f', 1, 64), "diskBytes": st.Utilization.DiskBytes, "updatedAt": metav1.Now(),
	}})
}

// activity forwards rows that belong to the caller.
func (h *Handler) activity(w http.ResponseWriter, r *http.Request) {
	uuid := caller(r)
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	var in struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, 400, "BadRequest", "invalid body")
		return
	}
	var keep []json.RawMessage
	for _, row := range in.Data {
		var meta struct {
			Server string `json:"server"`
		}
		if json.Unmarshal(row, &meta) == nil && meta.Server == uuid {
			keep = append(keep, row)
		}
	}
	if len(keep) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	out, _ := json.Marshal(map[string]any{"data": keep})
	status, res, err := h.Panel.Do(r.Context(), http.MethodPost, "/activity", out, nil)
	if err != nil {
		writeErr(w, 502, "BadGateway", err.Error())
		return
	}
	if status == http.StatusUnprocessableEntity {
		// Wings retries a rejected batch forever and never gets past it. Drop
		// rows the Panel refuses (they cannot become valid) and log them.
		h.Log.Warn("panel rejected activity rows; dropping the batch", "uuid", uuid, "response", strings.TrimSpace(string(res)))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(res)
}

// backup forwards backup calls for backups the gateway saw being started.
func (h *Handler) backup(w http.ResponseWriter, r *http.Request) {
	uuid := caller(r)
	backup := r.PathValue("backup")
	gs, err := h.Store.Get(r.Context(), uuid)
	if err != nil {
		writeErr(w, 404, "NotFoundHttpException", err.Error())
		return
	}
	allowed := false
	for _, p := range gs.Status.Backups.Pending {
		if p.UUID == backup {
			allowed = true
		}
	}
	if !allowed {
		h.Log.Warn("agent referenced an unknown backup", "uuid", uuid, "backup", backup)
		writeErr(w, http.StatusNotFound, "NotFoundHttpException", "unknown backup for this server")
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	path := strings.TrimPrefix(r.URL.Path, "/api/remote")
	query := map[string]string{}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			query[k] = v[0]
		}
	}
	status, res, err := h.Panel.Do(r.Context(), r.Method, path, body, query)
	if err != nil {
		writeErr(w, 502, "BadGateway", err.Error())
		return
	}
	// Completion posts clear the pending record.
	if r.Method == http.MethodPost {
		if err := h.Store.UpdateBackups(r.Context(), uuid, func(live []v1alpha1.PendingBackup, _ string) []v1alpha1.PendingBackup {
			out := []v1alpha1.PendingBackup{}
			for _, p := range live {
				if p.UUID != backup {
					out = append(out, p)
				}
			}
			return out
		}); err != nil {
			h.Log.Warn("cannot clear the pending backup", "uuid", uuid, "backup", backup, "error", err)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(res)
}

// sftpAuth answers from the relay's session records; never forwarded.
func (h *Handler) sftpAuth(w http.ResponseWriter, r *http.Request) {
	var req remote.SftpAuthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "BadRequest", "invalid body")
		return
	}
	if h.Sftp == nil {
		writeErr(w, http.StatusForbidden, "AccessDeniedHttpException", "sftp relay not configured")
		return
	}
	res, ok := h.Sftp.Lookup(req.User, req.Pass)
	if !ok || res.Server != caller(r) {
		writeErr(w, http.StatusForbidden, "AccessDeniedHttpException", "invalid credentials")
		return
	}
	writeJSON(w, 200, res)
}
