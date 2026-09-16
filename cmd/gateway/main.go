// Command gateway presents itself to the Pelican Panel as one Wings node and
// bridges it to GameServer objects and agents. See ARCHITECTURE.md section 5.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/Claiyc/pelican-k8s/internal/gateway/app"
	"github.com/Claiyc/pelican-k8s/internal/gateway/config"
	"github.com/Claiyc/pelican-k8s/internal/version"
)

func main() {
	debug := flag.Bool("debug", false, "debug logging")
	flag.Parse()
	lvl := slog.LevelInfo
	if *debug {
		lvl = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(logger)

	cfg, err := config.FromEnv()
	if err != nil {
		logger.Error("configuration", "error", err)
		os.Exit(1)
	}
	rc, err := ctrl.GetConfig()
	if err != nil {
		logger.Error("kubernetes config", "error", err)
		os.Exit(1)
	}
	app.SetRestConfig(rc)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	logger.Info("pelican-k8s gateway starting", "version", version.Version, "panel", cfg.PanelURL, "namespace", cfg.ServersNamespace)
	g, err := app.New(ctx, cfg, rc, logger)
	if err != nil {
		logger.Error("gateway setup failed", "error", err)
		os.Exit(1)
	}
	if err := g.Run(ctx); err != nil {
		logger.Error("gateway exited", "error", err)
		os.Exit(1)
	}
}
