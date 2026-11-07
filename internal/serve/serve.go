// Package serve runs an HTTP server and shuts it down gracefully.
package serve

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Run serves until Ctrl+C (or SIGTERM, which hosting platforms send before
// stopping a process), then gives in-flight requests up to 10 seconds to
// finish instead of cutting them off mid-reply.
func Run(srv *http.Server, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		return err // it never started, e.g. the port is already in use
	case <-ctx.Done():
	}

	logger.Info("shutting down; letting in-flight requests finish")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close() // time's up: cut off whatever is still running
		return fmt.Errorf("graceful shutdown timed out: %w", err)
	}
	return nil
}