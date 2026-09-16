// Command agent runs Wings as a library for exactly one server inside the
// game pod. See ARCHITECTURE.md section 6.3.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/apex/log"
	"github.com/apex/log/handlers/json"

	"github.com/Claiyc/pelican-k8s/internal/agent/app"
)

func main() {
	configPath := flag.String("config", "/etc/pelican/config.yml", "Wings configuration file")
	socket := flag.String("shim-socket", "/pelican/run/shim.sock", "shim unix socket")
	debug := flag.Bool("debug", false, "debug logging")
	flag.Parse()

	lvl := slog.LevelInfo
	if *debug {
		lvl = slog.LevelDebug
		log.SetLevel(log.DebugLevel)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(logger)
	log.SetHandler(json.New(os.Stdout))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := app.Run(ctx, app.Options{ConfigPath: *configPath, ShimSocket: *socket, Logger: logger}); err != nil {
		logger.Error("agent failed", "error", err)
		os.Exit(1)
	}
}
