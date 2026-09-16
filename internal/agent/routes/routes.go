// Package routes adds the agent-only HTTP routes next to Wings' router. They
// are served by a plain mux in front of gin because Wings registers its
// authorization middleware engine-wide, which would also guard these paths.
package routes

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/server"

	"github.com/Claiyc/pelican-k8s/internal/agent/shimenv"
	"github.com/Claiyc/pelican-k8s/internal/version"
)

// Handler returns the combined handler: /internal/v1/* here, everything else
// to wings (the gin engine).
func Handler(wings http.Handler, m *server.Manager, reg *shimenv.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version.Version, "servers": m.Len()})
	})
	// Called by kubelet's preStop hooks; returns once the process is offline.
	mux.HandleFunc("/internal/v1/prestop", func(w http.ResponseWriter, r *http.Request) { prestop(w, r, m) })
	// Called by the operator when the game container terminated underneath the agent.
	mux.HandleFunc("POST /internal/v1/exit-state", func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(auth), []byte(config.Get().Token.Token)) != 1 {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "You are not authorized to access this endpoint."})
			return
		}
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
	})
	mux.Handle("/", wings)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func prestop(w http.ResponseWriter, r *http.Request, m *server.Manager) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	for _, s := range m.All() {
		if s.Environment.State() == environment.ProcessOfflineState {
			continue
		}
		s.Log().Info("prestop: stopping server process before pod termination")
		s.PublishConsoleOutputFromDaemon("Pod is being terminated, stopping the server...")
		err := s.HandlePowerAction(server.PowerActionStop, 30)
		if err != nil && (errors.Is(err, server.ErrServerIsInstalling) || errors.Is(err, server.ErrServerIsRestoring) || errors.Is(err, server.ErrServerIsTransferring)) {
			continue
		}
		if err != nil {
			s.Log().WithField("error", err).Warn("prestop: graceful stop failed, terminating")
			_ = s.Environment.WaitForStop(ctx, time.Minute, true)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
