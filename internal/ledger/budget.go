package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/GitDaksh/hardcap/internal/pricing"
)

// Hold is money set aside for one running request.
type Hold struct {
	RequestID string
	KeyID     int64
	ProjectID int64
	Model     string
	Amount    pricing.Micros
	Mode      Mode
}

// BudgetError means a request doesn't fit under a cap. It becomes an HTTP
// 402 with a message saying which cap and how much is left.
type BudgetError struct {
	Scope    string // "key" or "project"
	Cap      pricing.Micros
	Spent    pricing.Micros
	Reserved pricing.Micros
	Needed   pricing.Micros
}

// Free is what's left under the cap, counting money held by running requests.
func (e *BudgetError) Free() pricing.Micros {
	return max(e.Cap-e.Spent-e.Reserved, 0)
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("%s budget exhausted: this request could cost up to %s, but only %s of the %s cap is free",
		e.Scope, e.Needed, e.Free(), e.Cap)
}

// Reserve sets aside amount for one request, or returns a *BudgetError if it
// doesn't fit. The hold expires after ttl: if this gateway dies before
// settling, the sweeper charges the hold in full once it expires.
func (l *Ledger) Reserve(ctx context.Context, key Key, requestID, model string, amount pricing.Micros, ttl time.Duration) (Hold, error) {
	h := Hold{RequestID: requestID, KeyID: key.ID, ProjectID: key.ProjectID, Model: model, Amount: amount, Mode: key.Mode}
	if key.Mode == ModeNaive {
		return h, l.checkNaive(ctx, key, amount)
	}

	var status string
	var cap, spent, reserved pricing.Micros
	err := l.pool.QueryRow(ctx,
		`SELECT out_status, out_cap, out_spent, out_reserved FROM hardcap_reserve($1, $2, $3, $4, $5)`,
		requestID, key.ID, amount, model, ttl.Seconds(),
	).Scan(&status, &cap, &spent, &reserved)
	if err != nil {
		return Hold{}, fmt.Errorf("reserve: %w", err)
	}
	switch status {
	case "ok":
		return h, nil
	case "invalid_key":
		l.cache.clear()
		return Hold{}, ErrInvalidKey
	case "key_cap", "project_cap":
		scope := "project"
		if status == "key_cap" {
			scope = "key"
		}
		return Hold{}, &BudgetError{Scope: scope, Cap: cap, Spent: spent, Reserved: reserved, Needed: amount}
	default:
		return Hold{}, fmt.Errorf("reserve: unexpected status %q", status)
	}
}

// checkNaive is the budget check most apps write: read the balance, compare,
// carry on, and add the real cost afterwards. Nothing is set aside, so every
// request that reads the balance before the others have been charged sees
// the same "plenty left" and goes ahead. That's how a $1 cap ends up at
// $1.40 under load. Hardcap's tests and the stampede demo prove it.
func (l *Ledger) checkNaive(ctx context.Context, key Key, amount pricing.Micros) error {
	var revoked bool
	var keyCap *pricing.Micros
	var keySpent, cap, spent pricing.Micros
	err := l.pool.QueryRow(ctx, `
		SELECT k.revoked_at IS NOT NULL, k.cap_micros, k.spent_micros, p.cap_micros, p.spent_micros
		  FROM api_keys k JOIN projects p ON p.id = k.project_id
		 WHERE k.id = $1`, key.ID).Scan(&revoked, &keyCap, &keySpent, &cap, &spent)
	if errors.Is(err, pgx.ErrNoRows) || revoked {
		return ErrInvalidKey
	}
	if err != nil {
		return fmt.Errorf("naive check: %w", err)
	}
	if keyCap != nil && keySpent+amount > *keyCap {
		return &BudgetError{Scope: "key", Cap: *keyCap, Spent: keySpent, Needed: amount}
	}
	if spent+amount > cap {
		return &BudgetError{Scope: "project", Cap: cap, Spent: spent, Needed: amount}
	}
	return nil
}

// Settlement is how a request ended and what it really cost.
type Settlement struct {
	Provider         string
	Outcome          string
	PromptTokens     int
	CompletionTokens int
	UsageKnown       bool
	Cost             pricing.Micros
	Duration         time.Duration
}

// Settle releases a hold and charges the real cost. It's idempotent (a
// second call for the same request changes nothing), which is what makes it
// safe to retry: if a try fails after Postgres committed it, the retry just
// finds the receipt and stops. It retries up to 3 times within ctx.
func (l *Ledger) Settle(ctx context.Context, h Hold, s Settlement) error {
	var err error
	for attempt, wait := 0, 50*time.Millisecond; attempt < 3; attempt, wait = attempt+1, wait*4 {
		if attempt > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return fmt.Errorf("settle %s: %w (last error: %v)", h.RequestID, ctx.Err(), err)
			}
		}
		var status string
		err = l.pool.QueryRow(ctx,
			`SELECT hardcap_settle($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			h.RequestID, h.KeyID, h.ProjectID, h.Model, s.Provider, s.Outcome,
			s.PromptTokens, s.CompletionTokens, s.UsageKnown, s.Cost, durationMS(s.Duration),
		).Scan(&status)
		if err == nil {
			return nil // "settled" or "duplicate": either way the books are right
		}
	}
	return fmt.Errorf("settle %s: %w", h.RequestID, err)
}

// SweepExpired charges up to limit expired holds in full and returns how
// many it charged. Every gateway runs it on a timer.
func (l *Ledger) SweepExpired(ctx context.Context, limit int) (int, error) {
	n := 0
	for n < limit {
		var requestID *string
		if err := l.pool.QueryRow(ctx, `SELECT hardcap_capture_expired()`).Scan(&requestID); err != nil {
			return n, fmt.Errorf("sweep: %w", err)
		}
		if requestID == nil {
			break
		}
		n++
	}
	return n, nil
}

func durationMS(d time.Duration) int {
	return int(min(d.Milliseconds(), 1<<31-1))
}
