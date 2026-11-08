// Package serve runs an HTTP server and shuts it down gracefully.
package serve

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Run serves until ctx ends (main cancels it on Ctrl+C or SIGTERM, which
// hosting platforms send before stopping a process). Then it stops taking
// new connections and gives in-flight requests up to grace to finish,
// instead of cutting them off mid-reply.
func Run(ctx context.Context, srv *http.Server, logger *slog.Logger, grace time.Duration) error {
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		return err // it never started, e.g. the port is already in use
	case <-ctx.Done():
	}

	logger.Info("shutting down; letting in-flight requests finish", "grace", grace)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close() // time's up: cut off whatever is still running
		return fmt.Errorf("graceful shutdown timed out: %w", err)
	}
	return nil
}
