// Command hardcap runs the Hardcap gateway.
//
// Right now (Phase 1) it forwards OpenAI-compatible chat requests to one
// provider, streams the replies back, and logs the tokens each request used.
package main

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/GitDaksh/hardcap/internal/env"
	"github.com/GitDaksh/hardcap/internal/gateway"
	"github.com/GitDaksh/hardcap/internal/serve"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("hardcap stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	var cfg env.Loader
	// localhost by default: reachable only from this machine. Set
	// HARDCAP_ADDR=:8080 to listen on every interface (e.g. in a container).
	addr := cfg.String("HARDCAP_ADDR", "localhost:8080")
	upstreamURL := cfg.String("HARDCAP_UPSTREAM_URL", "http://localhost:8090/v1")
	upstreamKey := cfg.String("HARDCAP_UPSTREAM_KEY", "")
	if err := cfg.Err(); err != nil {
		return err
	}

	gw, err := gateway.New(gateway.Config{
		UpstreamURL: upstreamURL,
		UpstreamKey: upstreamKey,
		Logger:      logger,
	})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 10 * time.Second, // blocks slow-header (Slowloris) attacks
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout on purpose: streamed replies can run for minutes.
	}
	logger.Info("hardcap listening", "addr", addr, "upstream", upstreamURL)
	return serve.Run(srv, logger)
}