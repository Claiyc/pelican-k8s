// Package routes adds the agent-only HTTP routes next to Wings' router. They
// are served by a plain mux in front of gin because Wings registers its
// authorization middleware engine-wide, which would also guard these paths.
package routes

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/router/downloader"
	"github.com/pelican/wings/server"

	"github.com/Claiyc/pelican-k8s/internal/agent/shimenv"
	"github.com/Claiyc/pelican-k8s/internal/pki"
	"github.com/Claiyc/pelican-k8s/internal/version"
)

// Paths kubelet calls: the probes and the preStop hook. They carry no token
// and, with TLS, no client certificate.
const (
	HealthzPath = "/internal/v1/healthz"
	PreStopPath = "/internal/v1/prestop"
)

// PrestopShimWait is how long the agent's preStop hook waits for the shim to
// report that it is terminating too (a drain that takes both pods).
var PrestopShimWait = 10 * time.Second

// Handler returns the combined handler: /internal/v1/* here, everything else
// to wings (the gin engine). File transfers through wings are counted as
// in-flight work for /internal/v1/activity.
func Handler(wings http.Handler, m *server.Manager, reg *shimenv.Registry) http.Handler {
	act := &Activity{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+HealthzPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version.Version, "servers": m.Len()})
	})
	// Called by kubelet's preStop hook of the agent container.
	mux.HandleFunc(PreStopPath, func(w http.ResponseWriter, r *http.Request) { prestop(w, r, m, reg) })
	// Called by the operator: the game pod whose shim holds the connection.
	mux.HandleFunc("GET /internal/v1/shim", authorized(func(w http.ResponseWriter, r *http.Request) {
		info := shimenv.ShimInfo{}
		for _, e := range reg.All() {
			info = e.Shim()
		}
		writeJSON(w, http.StatusOK, info)
	}))
	// Called by the operator before it moves the agent pod.
	mux.HandleFunc("GET /internal/v1/activity", authorized(func(w http.ResponseWriter, r *http.Request) {
		reasons := act.Reasons(m)
		writeJSON(w, http.StatusOK, map[string]any{"busy": len(reasons) > 0, "reasons": reasons})
	}))
	// Called by the operator when the game container terminated underneath the agent.
	mux.HandleFunc("POST /internal/v1/exit-state", authorized(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Code      int  `json:"code"`
			OOMKilled bool `json:"oomKilled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		for _, s := range m.All() {
			if e, ok := reg.Get(s.ID()); ok {
				e.InjectExit(body.Code, body.OOMKilled)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.Handle("/", act.Track(wings))
	return mux
}

// authorized guards a route with the agent's own Wings token, as the Panel's
// calls to Wings are guarded.
func authorized(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(auth), []byte(config.Get().Token.Token)) != 1 {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "You are not authorized to access this endpoint."})
			return
		}
		h(w, r)
	}
}

// Activity counts in-flight file requests: uploads, downloads, compress,
// decompress and the rest of the file API. Remote pulls, restores and
// installs run in Wings after their request returned and are read from Wings'
// own state.
type Activity struct {
	mu    sync.Mutex
	files int
}

// isFileRequest reports whether a request moves server files.
func isFileRequest(path string) bool {
	if strings.HasPrefix(path, "/download/") || strings.HasPrefix(path, "/upload/") {
		return true
	}
	rest, ok := strings.CutPrefix(path, "/api/servers/")
	if !ok {
		return false
	}
	_, sub, _ := strings.Cut(rest, "/")
	return strings.HasPrefix(sub, "files/")
}

// Track counts the file requests that pass through h.
func (a *Activity) Track(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isFileRequest(r.URL.Path) {
			h.ServeHTTP(w, r)
			return
		}
		a.mu.Lock()
		a.files++
		a.mu.Unlock()
		defer func() {
			a.mu.Lock()
			a.files--
			a.mu.Unlock()
		}()
		h.ServeHTTP(w, r)
	})
}

// Reasons lists the in-flight work a move of the agent would break.
func (a *Activity) Reasons(m *server.Manager) []string {
	set := map[string]bool{}
	a.mu.Lock()
	if a.files > 0 {
		set["files"] = true
	}
	a.mu.Unlock()
	for _, s := range m.All() {
		if len(downloader.ByServer(s.ID())) > 0 {
			set["pull"] = true
		}
		if s.IsRestoring() {
			set["restore"] = true
		}
		if s.IsInstalling() {
			set["install"] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// prestop returns at once unless the shim reports within PrestopShimWait that
// its container is being terminated too (a drain takes both pods); then it
// returns once the process is offline, so the agent sees the shutdown
// through. Otherwise the process keeps running under the shim until the next
// agent attaches.
func prestop(w http.ResponseWriter, r *http.Request, m *server.Manager, reg *shimenv.Registry) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Minute)
	defer cancel()
	terminating := func() bool {
		for _, s := range m.All() {
			e, ok := reg.Get(s.ID())
			if ok && e.Shim().Terminating {
				return true
			}
		}
		return false
	}
	offline := func() bool {
		for _, s := range m.All() {
			if s.Environment.State() != environment.ProcessOfflineState {
				return false
			}
		}
		return true
	}
	if offline() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "waited": false})
		return
	}
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	seen := time.After(PrestopShimWait)
	for !terminating() && !offline() {
		select {
		case <-seen:
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "waited": false})
			return
		case <-ctx.Done():
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "waited": false})
			return
		case <-t.C:
		}
	}
	for _, s := range m.All() {
		s.PublishConsoleOutputFromDaemon("Pod is being terminated, waiting for the server to stop...")
	}
	for !offline() {
		select {
		case <-ctx.Done():
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "waited": true})
			return
		case <-t.C:
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "waited": true})
}

// RequireClientCert rejects requests without a verified client certificate
// of the gateway or the operator, except the ones kubelet makes (HealthzPath,
// PreStopPath). The name check matters with a shared cert-manager issuer,
// whose other certificates the agent's bundle also verifies. The bearer
// token is still checked behind it.
func RequireClientCert(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == HealthzPath || r.URL.Path == PreStopPath || trustedClient(r) {
			next.ServeHTTP(w, r)
			return
		}
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "a client certificate is required"})
	})
}

func trustedClient(r *http.Request) bool {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return false
	}
	cn := r.TLS.VerifiedChains[0][0].Subject.CommonName
	return cn == pki.GatewayName || cn == pki.OperatorName
}
