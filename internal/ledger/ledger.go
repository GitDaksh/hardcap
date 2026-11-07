// Package ledger is Hardcap's bank: projects, API keys, budgets, and the
// hold → settle → capture flow that keeps spending under every cap.
//
// The flow, for one request:
//
//  1. Reserve: before calling the AI, set aside the most the request could
//     possibly cost (a "hold"). If that doesn't fit under the caps, refuse.
//  2. Settle: when the request ends, release the hold and charge what it
//     really cost, which is never more than the hold.
//  3. Capture: if a gateway crashes mid-request, its hold expires and the
//     sweeper charges it in full, because the tokens may have been used.
//
// Because every running request has already set aside its worst case,
// spent + reserved can never pass the cap, no matter how many requests run
// at the same moment or how many gateway instances share the database.
package ledger

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GitDaksh/hardcap/internal/pricing"
)

var (
	ErrNotFound   = errors.New("not found")
	ErrInvalidKey = errors.New("invalid or revoked API key")
	ErrNameTaken  = errors.New("that name is already taken")
)

// Mode is how a project enforces its cap.
type Mode string

const (
	// ModeHardcap reserves the worst case before each request (the safe way).
	ModeHardcap Mode = "hardcap"
	// ModeNaive checks the balance, then charges afterwards (the common way).
	// It exists to demonstrate overspending under concurrency.
	ModeNaive Mode = "naive"
)

// Valid reports whether m is a known mode.
func (m Mode) Valid() bool { return m == ModeHardcap || m == ModeNaive }

// Project is a budget shared by all of its API keys.
type Project struct {
	ID        int64          `json:"id"`
	Name      string         `json:"name"`
	Mode      Mode           `json:"mode"`
	Cap       pricing.Micros `json:"cap_micros"`
	Spent     pricing.Micros `json:"spent_micros"`
	Reserved  pricing.Micros `json:"reserved_micros"`
	Requests  int64          `json:"requests"`
	CreatedAt time.Time      `json:"created_at"`
}

// Key is an API key. The secret itself is only ever shown once, at creation.
type Key struct {
	ID        int64           `json:"id"`
	ProjectID int64           `json:"project_id"`
	Name      string          `json:"name"`
	Prefix    string          `json:"prefix"`
	Cap       *pricing.Micros `json:"cap_micros"` // nil: only the project's cap applies
	Spent     pricing.Micros  `json:"spent_micros"`
	Reserved  pricing.Micros  `json:"reserved_micros"`
	RPM       *int            `json:"rpm_limit"` // nil: the default from hardcap.yaml; 0: no limit
	TPM       *int            `json:"tpm_limit"`
	Revoked   bool            `json:"revoked"`
	CreatedAt time.Time       `json:"created_at"`

	// Mode is the project's mode, filled in by Authenticate.
	Mode Mode `json:"-"`
}

// KeyOptions are the optional settings for a new key.
type KeyOptions struct {
	Name string
	Cap  *pricing.Micros
	RPM  *int
	TPM  *int
}

// UsageEvent is the receipt for one finished request.
type UsageEvent struct {
	ID               int64          `json:"id"`
	RequestID        string         `json:"request_id"`
	KeyID            int64          `json:"key_id"`
	Model            string         `json:"model"`
	Provider         string         `json:"provider"`
	Outcome          string         `json:"outcome"`
	PromptTokens     int            `json:"prompt_tokens"`
	CompletionTokens int            `json:"completion_tokens"`
	UsageKnown       bool           `json:"usage_known"`
	Held             pricing.Micros `json:"held_micros"`
	Cost             pricing.Micros `json:"cost_micros"`
	DurationMS       int            `json:"duration_ms"`
	CreatedAt        time.Time      `json:"created_at"`
}

// Audit is an independent recount of a project's money, used by tests and
// the load tester to prove the books balance.
type Audit struct {
	Project      Project        `json:"project"`
	ReceiptCount int64          `json:"receipt_count"`
	ReceiptTotal pricing.Micros `json:"receipt_total_micros"` // sum of every receipt's cost
	OpenHolds    int64          `json:"open_holds"`
	HeldTotal    pricing.Micros `json:"held_total_micros"` // sum of every open hold
}

// Balanced reports whether the project's running totals match the receipts
// and holds they were built from.
func (a Audit) Balanced() bool {
	return a.ReceiptTotal == a.Project.Spent && a.HeldTotal == a.Project.Reserved
}

// Ledger reads and writes budgets in Postgres.
type Ledger struct {
	pool  *pgxpool.Pool
	cache keyCache
}

// New returns a Ledger that uses pool.
func New(pool *pgxpool.Pool) *Ledger {
	return &Ledger{pool: pool, cache: keyCache{ttl: 10 * time.Second}}
}

const projectColumns = `id, name, mode, cap_micros, spent_micros, reserved_micros, requests, created_at`

func scanProject(row pgx.Row) (Project, error) {
	var p Project
	err := row.Scan(&p.ID, &p.Name, &p.Mode, &p.Cap, &p.Spent, &p.Reserved, &p.Requests, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	return p, err
}

// CreateProject adds a project with a spending cap.
func (l *Ledger) CreateProject(ctx context.Context, name string, cap pricing.Micros, mode Mode) (Project, error) {
	if !mode.Valid() {
		return Project{}, fmt.Errorf("unknown mode %q", mode)
	}
	p, err := scanProject(l.pool.QueryRow(ctx,
		`INSERT INTO projects (name, cap_micros, mode) VALUES ($1, $2, $3) RETURNING `+projectColumns,
		name, cap, mode))
	if isUniqueViolation(err) {
		return Project{}, ErrNameTaken
	}
	return p, err
}

// Projects lists every project, oldest first.
func (l *Ledger) Projects(ctx context.Context) ([]Project, error) {
	rows, err := l.pool.Query(ctx, `SELECT `+projectColumns+` FROM projects ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Project, error) { return scanProject(row) })
}

// Project returns one project.
func (l *Ledger) Project(ctx context.Context, id int64) (Project, error) {
	return scanProject(l.pool.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = $1`, id))
}

// UpdateProject changes a project's cap and/or mode (nil means unchanged).
// Lowering the cap below what's already spent is allowed: every new request
// is then refused.
func (l *Ledger) UpdateProject(ctx context.Context, id int64, cap *pricing.Micros, mode *Mode) (Project, error) {
	if mode != nil && !mode.Valid() {
		return Project{}, fmt.Errorf("unknown mode %q", *mode)
	}
	p, err := scanProject(l.pool.QueryRow(ctx,
		`UPDATE projects SET cap_micros = COALESCE($2, cap_micros), mode = COALESCE($3, mode)
		 WHERE id = $1 RETURNING `+projectColumns, id, cap, mode))
	if err == nil {
		l.cache.clear() // cached keys carry the project's mode
	}
	return p, err
}

// ResetSpend starts a project over: spent goes to zero and its receipts are
// deleted. Holds of requests still running are kept and settle normally.
func (l *Ledger) ResetSpend(ctx context.Context, id int64) error {
	return pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
		// Keys before the project, in id order: the same lock order as
		// everything else, so a reset can't deadlock with live traffic.
		if _, err := tx.Exec(ctx, `UPDATE api_keys SET spent_micros = 0 WHERE id IN (
			SELECT id FROM api_keys WHERE project_id = $1 ORDER BY id FOR UPDATE)`, id); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE projects SET spent_micros = 0, requests = 0 WHERE id = $1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = tx.Exec(ctx, `DELETE FROM usage_events WHERE project_id = $1`, id)
		return err
	})
}

// KeyPrefix starts every Hardcap API key, so leaked keys are easy to spot
// (GitHub's secret scanning works the same way).
const KeyPrefix = "hc_"

// A key is "hc_" + 32 characters: 24 random bytes, base64url-encoded.
const keyLength = len(KeyPrefix) + 32

const keyColumns = `k.id, k.project_id, k.name, k.prefix, k.cap_micros, k.spent_micros, k.reserved_micros,
	k.rpm_limit, k.tpm_limit, k.revoked_at IS NOT NULL, k.created_at`

func scanKey(row pgx.Row, extra ...any) (Key, error) {
	var k Key
	dest := append([]any{&k.ID, &k.ProjectID, &k.Name, &k.Prefix, &k.Cap, &k.Spent, &k.Reserved,
		&k.RPM, &k.TPM, &k.Revoked, &k.CreatedAt}, extra...)
	err := row.Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, ErrNotFound
	}
	return k, err
}

// CreateKey adds an API key to a project and returns it with its secret.
// Only a SHA-256 hash of the secret is stored, so this is the one and only
// time anyone sees it.
//
// A plain SHA-256 is the right tool here, unlike for passwords: the secret
// is 192 random bits, so there's nothing to brute-force, and a fast hash
// keeps every request's key check cheap.
func (l *Ledger) CreateKey(ctx context.Context, projectID int64, opts KeyOptions) (Key, string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return Key{}, "", err
	}
	secret := KeyPrefix + base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(secret))

	k, err := scanKey(l.pool.QueryRow(ctx, `
		INSERT INTO api_keys AS k (project_id, name, prefix, secret_hash, cap_micros, rpm_limit, tpm_limit)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+keyColumns,
		projectID, opts.Name, secret[:len(KeyPrefix)+8], hash[:], opts.Cap, opts.RPM, opts.TPM))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign key violation: no such project
		return Key{}, "", ErrNotFound
	}
	if err != nil {
		return Key{}, "", err
	}
	return k, secret, nil
}

// Keys lists a project's keys, oldest first.
func (l *Ledger) Keys(ctx context.Context, projectID int64) ([]Key, error) {
	rows, err := l.pool.Query(ctx, `SELECT `+keyColumns+` FROM api_keys k WHERE k.project_id = $1 ORDER BY k.id`, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Key, error) { return scanKey(row) })
}

// RevokeKey switches a key off for good. Other gateway instances may accept
// it for up to 10 more seconds (their auth cache), but no billable request
// gets through: Reserve checks revocation in the database every time.
func (l *Ledger) RevokeKey(ctx context.Context, id int64) error {
	tag, err := l.pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return err
	}
	l.cache.clear()
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Authenticate looks up the key for a secret. Results are cached for 10
// seconds, so most requests skip this database round trip.
func (l *Ledger) Authenticate(ctx context.Context, secret string) (Key, error) {
	if len(secret) != keyLength || !strings.HasPrefix(secret, KeyPrefix) {
		return Key{}, ErrInvalidKey // not even shaped like a key: no need to ask the database
	}
	hash := sha256.Sum256([]byte(secret))
	if k, ok := l.cache.get(hash); ok {
		return k, nil
	}
	var mode Mode
	k, err := scanKey(l.pool.QueryRow(ctx, `
		SELECT `+keyColumns+`, p.mode
		  FROM api_keys k JOIN projects p ON p.id = k.project_id
		 WHERE k.secret_hash = $1 AND k.revoked_at IS NULL`, hash[:]), &mode)
	if errors.Is(err, ErrNotFound) {
		return Key{}, ErrInvalidKey
	}
	if err != nil {
		return Key{}, err
	}
	k.Mode = mode
	l.cache.put(hash, k)
	return k, nil
}

// RecentUsage returns a project's newest receipts, newest first.
func (l *Ledger) RecentUsage(ctx context.Context, projectID int64, limit int) ([]UsageEvent, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT id, request_id, key_id, model, provider, outcome, prompt_tokens, completion_tokens,
		       usage_known, held_micros, cost_micros, duration_ms, created_at
		  FROM usage_events WHERE project_id = $1 ORDER BY id DESC LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (UsageEvent, error) {
		var e UsageEvent
		err := row.Scan(&e.ID, &e.RequestID, &e.KeyID, &e.Model, &e.Provider, &e.Outcome, &e.PromptTokens,
			&e.CompletionTokens, &e.UsageKnown, &e.Held, &e.Cost, &e.DurationMS, &e.CreatedAt)
		return e, err
	})
}

// Audit recounts a project's money from its receipts and open holds.
func (l *Ledger) Audit(ctx context.Context, projectID int64) (Audit, error) {
	var a Audit
	err := pgx.BeginTxFunc(ctx, l.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			// One snapshot for all three reads, so they agree with each other.
			p, err := scanProject(tx.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = $1`, projectID))
			if err != nil {
				return err
			}
			a.Project = p
			if err := tx.QueryRow(ctx, `SELECT count(*), COALESCE(sum(cost_micros), 0) FROM usage_events WHERE project_id = $1`,
				projectID).Scan(&a.ReceiptCount, &a.ReceiptTotal); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*), COALESCE(sum(amount_micros), 0) FROM holds WHERE project_id = $1`,
				projectID).Scan(&a.OpenHolds, &a.HeldTotal)
		})
	return a, err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// keyCache remembers recently used keys for a few seconds.
type keyCache struct {
	ttl     time.Duration
	mu      sync.Mutex
	entries map[[32]byte]cachedKey
}

type cachedKey struct {
	key     Key
	expires time.Time
}

const maxCachedKeys = 10_000

func (c *keyCache) get(hash [32]byte) (Key, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[hash]
	if !ok || time.Now().After(e.expires) {
		return Key{}, false
	}
	return e.key, true
}

func (c *keyCache) put(hash [32]byte, k Key) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil || len(c.entries) >= maxCachedKeys {
		c.entries = map[[32]byte]cachedKey{} // simple bound: start over rather than grow forever
	}
	c.entries[hash] = cachedKey{key: k, expires: time.Now().Add(c.ttl)}
}

func (c *keyCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
}
