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

// errorStringHandler renders error fields as their message: the JSON handler
// would marshal wrapped errors (emperror) as empty objects.
type errorStringHandler struct{ next log.Handler }

func (h errorStringHandler) HandleLog(e *log.Entry) error {
	for k, v := range e.Fields {
		if err, ok := v.(error); ok {
			e.Fields[k] = err.Error()
		}
	}
	return h.next.HandleLog(e)
}

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
	log.SetHandler(errorStringHandler{json.New(os.Stdout)})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := app.Run(ctx, app.Options{ConfigPath: *configPath, ShimSocket: *socket, Logger: logger}); err != nil {
		logger.Error("agent failed", "error", err)
		os.Exit(1)
	}
}
