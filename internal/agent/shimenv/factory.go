package shimenv

import (
	"log/slog"
	"path/filepath"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/server"
)

// Registry keeps the environments created by the factory so that the agent's
// own routes (exit-state injection, prestop) can find them.
type Registry struct {
	socket   string
	extraEnv []string
	logger   *slog.Logger
	envs     map[string]*Environment
}

// NewRegistry returns a registry creating environments on socketPath.
func NewRegistry(socketPath string, extraEnv []string, logger *slog.Logger) *Registry {
	return &Registry{socket: socketPath, extraEnv: extraEnv, logger: logger, envs: map[string]*Environment{}}
}

// Factory is the server.EnvironmentFactory registered with the Wings manager.
func (r *Registry) Factory(s *server.Server, cfg *environment.Configuration) (environment.ProcessEnvironment, error) {
	e := New(s.ID(), cfg, Options{
		SocketPath: r.socket,
		RunLog:     filepath.Join(config.Get().System.LogDirectory, "console", s.ID()+".log"),
		ExtraEnv:   r.extraEnv,
		Logger:     r.logger,
	})
	e.SetImage(s.Config().Container.Image)
	if pc := s.ProcessConfiguration(); pc != nil {
		e.SetStopConfiguration(pc.Stop)
	}
	r.envs[s.ID()] = e
	return e, nil
}

// Get returns the environment of a server.
func (r *Registry) Get(id string) (*Environment, bool) {
	e, ok := r.envs[id]
	return e, ok
}
