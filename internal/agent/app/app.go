// Package app boots Wings as a library for one server: configuration, remote
// client, activity database, the server manager with the shim environment and
// Job installer, cron, SFTP and the HTTP router. See ARCHITECTURE.md 6.3.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/pelican/wings/boot"
	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/router"
	"github.com/pelican/wings/server"
	"github.com/pelican/wings/sftp"

	"github.com/Claiyc/pelican-k8s/internal/agent/gatewayclient"
	"github.com/Claiyc/pelican-k8s/internal/agent/installer"
	"github.com/Claiyc/pelican-k8s/internal/agent/routes"
	"github.com/Claiyc/pelican-k8s/internal/agent/shimenv"
	"github.com/Claiyc/pelican-k8s/internal/version"
)

// Options configure the agent.
type Options struct {
	ConfigPath string
	ShimSocket string
	Logger     *slog.Logger
	// Ready, when non-nil, is closed once the HTTP server is listening.
	Ready chan struct{}
}

// Run boots the agent and blocks until ctx is cancelled or a fatal error occurs.
func Run(ctx context.Context, o Options) error {
	configPath, socket, logger := o.ConfigPath, o.ShimSocket, o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := config.FromFile(configPath); err != nil {
		return fmt.Errorf("load config %s: %w", configPath, err)
	}
	// The agent runs as the pod's pinned UID; Wings' chown logic must use it.
	config.Update(func(c *config.Configuration) {
		c.System.User.Uid = os.Getuid()
		c.System.User.Gid = os.Getgid()
		c.System.User.Rootless.Enabled = true
		c.System.User.Rootless.ContainerUID = os.Getuid()
		c.System.User.Rootless.ContainerGID = os.Getgid()
	})
	cfg := config.Get()
	if cfg.Token.ID == "" || cfg.Token.Token == "" {
		return errors.New("WINGS_TOKEN_ID and WINGS_TOKEN must be set")
	}
	if err := config.ConfigureTimezone(); err != nil {
		return err
	}
	if err := config.ConfigureDirectories(); err != nil {
		return fmt.Errorf("configure directories: %w", err)
	}
	for _, d := range []string{cfg.System.ArchiveDirectory, cfg.System.BackupDirectory, filepath.Join(cfg.System.LogDirectory, "install")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	logger.Info("pelican-k8s agent starting", "version", version.Version, "remote", cfg.PanelLocation, "root", cfg.System.RootDirectory)

	client := remote.New(cfg.PanelLocation,
		remote.WithCredentials(cfg.Token.ID, cfg.Token.Token),
		remote.WithCustomHeaders(cfg.RemoteQuery.CustomHeaders),
		remote.WithHttpClient(&http.Client{Timeout: time.Second * time.Duration(cfg.RemoteQuery.Timeout)}),
	)
	if err := boot.InitializeDatabase(); err != nil {
		return fmt.Errorf("initialize activity database: %w", err)
	}

	extraEnv := []string{}
	if ip := os.Getenv("PELICAN_POD_IP"); ip != "" {
		extraEnv = append(extraEnv, "INTERNAL_IP="+ip)
	}
	registry := shimenv.NewRegistry(socket, extraEnv, logger)
	gw := gatewayclient.New(cfg.PanelLocation, cfg.Token.ID, cfg.Token.Token)
	inst := &installer.Installer{Gateway: gw, Root: cfg.System.RootDirectory, LogDir: cfg.System.LogDirectory, Logger: logger}

	// Retry the initial server fetch: the gateway may not have the CR yet.
	var manager *server.Manager
	for attempt := 1; ; attempt++ {
		var err error
		manager, err = server.NewManager(ctx, client, server.WithEnvironmentFactory(registry.Factory), server.WithInstaller(inst))
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logger.Warn("failed to load server configuration from gateway, retrying", "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(time.Duration(attempt)*2*time.Second, 30*time.Second)):
		}
	}
	if manager.Len() == 0 {
		return errors.New("gateway returned no server for this agent token")
	}
	for _, s := range manager.All() {
		if err := s.EnsureDataDirectoryExists(); err != nil {
			return err
		}
		// Never auto-start here: the operator drives spec.power. The shim
		// environment re-attaches on its own if the game already runs.
		if s.Environment.State() == environment.ProcessOfflineState {
			s.Log().Info("server loaded, waiting for the operator")
		}
	}

	if sched, err := boot.Scheduler(ctx, manager); err != nil {
		return fmt.Errorf("cron: %w", err)
	} else {
		sched.Start()
		defer func() { _ = sched.Shutdown() }()
	}

	go func() {
		if err := sftp.New(manager).Run(); err != nil {
			logger.Error("sftp server failed", "error", err)
			cancel()
		}
	}()

	engine := router.Configure(manager, client)
	routes.Register(engine, manager, registry)
	srv := &http.Server{
		Addr:              cfg.Api.Host + ":" + strconv.Itoa(cfg.Api.Port),
		Handler:           engine,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		_ = srv.Shutdown(sctx)
		for _, s := range manager.All() {
			s.CtxCancel()
		}
	}()
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	logger.Info("agent listening", "addr", ln.Addr().String(), "sftp", cfg.System.Sftp.Port)
	if o.Ready != nil {
		close(o.Ready)
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
