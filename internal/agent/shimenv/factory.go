package shimenv

import (
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/server"

	"github.com/Claiyc/pelican-k8s/internal/shim/protocol"
)

// Registry keeps the environments created by the factory so that the agent's
// own routes (exit-state injection, prestop) can find them.
type Registry struct {
	ln       *protocol.Listener
	extraEnv []string
	logger   *slog.Logger
	mu       sync.Mutex
	envs     map[string]*Environment
}

// NewRegistry returns a registry whose environments take their shim
// connections from ln.
func NewRegistry(ln *protocol.Listener, extraEnv []string, logger *slog.Logger) *Registry {
	return &Registry{ln: ln, extraEnv: extraEnv, logger: logger, envs: map[string]*Environment{}}
}

// Factory is the server.EnvironmentFactory registered with the Wings manager.
func (r *Registry) Factory(s *server.Server, cfg *environment.Configuration) (environment.ProcessEnvironment, error) {
	e := New(s.ID(), cfg, Options{
		Connect:    r.ln.Accept,
		SocketPath: r.ln.Path(),
		RunLog:     filepath.Join(config.Get().System.LogDirectory, "console", s.ID()+".log"),
		ExtraEnv:   r.extraEnv,
		Logger:     r.logger,
	})
	e.SetImage(s.Config().Container.Image)
	if pc := s.ProcessConfiguration(); pc != nil {
		e.SetStopConfiguration(pc.Stop)
	}
	r.mu.Lock()
	// A server object built again (e.g. a retried manager load) replaces the
	// previous environment, which must not keep taking shim connections.
	if prev, ok := r.envs[s.ID()]; ok {
		prev.Close()
	}
	r.envs[s.ID()] = e
	r.mu.Unlock()
	return e, nil
}

// Get returns the environment of a server.
func (r *Registry) Get(id string) (*Environment, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.envs[id]
	return e, ok
}
