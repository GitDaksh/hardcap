package admin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/GitDaksh/hardcap/internal/ledger"
	"github.com/GitDaksh/hardcap/internal/loadgen"
)

// Demo runs stampedes: a crowd of clients hammering one project through
// the real gateway, over real HTTP, so the dashboard can show the cap hold.
type Demo struct {
	gatewayURL string
	ledger     *ledger.Ledger
	logger     *slog.Logger

	mu     sync.Mutex
	runs   map[int64]*loadgen.Stats // by project; kept after finishing so the result stays visible
	active map[int64]bool           // projects with a stampede starting or running
}

// NewDemo returns a Demo that sends its requests to gatewayURL.
func NewDemo(gatewayURL string, l *ledger.Ledger, logger *slog.Logger) *Demo {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Demo{gatewayURL: gatewayURL, ledger: l, logger: logger,
		runs: map[int64]*loadgen.Stats{}, active: map[int64]bool{}}
}

// Start launches a stampede on a project. It returns at once; the stampede
// runs in the background for the given number of seconds.
func (d *Demo) Start(ctx context.Context, projectID int64, workers, seconds int, model string) error {
	d.mu.Lock()
	if d.active[projectID] {
		d.mu.Unlock()
		return fail(http.StatusConflict, "a stampede is already running on this project")
	}
	stats := &loadgen.Stats{}
	d.runs[projectID] = stats
	d.active[projectID] = true
	d.mu.Unlock()
	finish := func() {
		d.mu.Lock()
		delete(d.active, projectID)
		d.mu.Unlock()
	}

	// A throwaway key, revoked when the stampede ends.
	key, secret, err := d.ledger.CreateKey(ctx, projectID, ledger.KeyOptions{
		Name: fmt.Sprintf("stampede %s", time.Now().Format("15:04:05")),
	})
	if err != nil {
		finish()
		return err
	}

	d.logger.Info("stampede started", "project_id", projectID, "workers", workers, "seconds", seconds, "model", model)
	go func() {
		defer finish()
		loadgen.Run(context.Background(), loadgen.Config{
			URL: d.gatewayURL, APIKey: secret, Model: model,
			Workers: workers, Duration: time.Duration(seconds) * time.Second, Stream: true,
		}, stats)
		if err := d.ledger.RevokeKey(context.Background(), key.ID); err != nil {
			d.logger.Warn("couldn't revoke the stampede key", "key_id", key.ID, "error", err)
		}
		s := stats.Snapshot()
		d.logger.Info("stampede finished", "project_id", projectID, "sent", s.Sent, "ok", s.OK, "refused", s.Refused)
	}()
	return nil
}

// Status returns the latest stampede on a project, running or finished.
func (d *Demo) Status(projectID int64) (loadgen.Snapshot, bool) {
	d.mu.Lock()
	s, ok := d.runs[projectID]
	active := d.active[projectID]
	d.mu.Unlock()
	if !ok {
		return loadgen.Snapshot{}, false
	}
	snap := s.Snapshot()
	snap.Running = active
	return snap, true
}

// Running reports whether a stampede is starting or running on a project.
func (d *Demo) Running(projectID int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.active[projectID]
}
