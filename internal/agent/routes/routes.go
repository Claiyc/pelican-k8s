// Package routes adds the agent-only HTTP routes to Wings' router.
package routes

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/router/middleware"
	"github.com/pelican/wings/server"

	"github.com/Claiyc/pelican-k8s/internal/agent/shimenv"
	"github.com/Claiyc/pelican-k8s/internal/version"
)

// Register adds /internal/v1/* to the engine.
func Register(r *gin.Engine, m *server.Manager, reg *shimenv.Registry) {
	g := r.Group("/internal/v1")
	g.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true, "version": version.Version, "servers": m.Len()})
	})
	// Called by kubelet's preStop hook; returns once the process is offline.
	g.POST("/prestop", func(c *gin.Context) { prestop(c, m) })
	g.GET("/prestop", func(c *gin.Context) { prestop(c, m) })
	// Called by the operator when the game container terminated underneath the agent.
	g.POST("/exit-state", middleware.RequireAuthorization(), func(c *gin.Context) {
		var body struct {
			Code      int  `json:"code"`
			OOMKilled bool `json:"oomKilled"`
		}
		if err := c.BindJSON(&body); err != nil {
			return
		}
		for _, s := range m.All() {
			if e, ok := reg.Get(s.ID()); ok {
				e.InjectExit(body.Code, body.OOMKilled)
			}
		}
		c.Status(http.StatusNoContent)
	})
}

func prestop(c *gin.Context, m *server.Manager) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Minute)
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
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
