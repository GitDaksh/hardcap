package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GitDaksh/hardcap/internal/ledger"
	"github.com/GitDaksh/hardcap/internal/pricing"
	"github.com/GitDaksh/hardcap/internal/testenv"
)

const dollar = pricing.PerDollar

// setup returns a ledger on a fresh database with one project and one key.
func setup(t *testing.T, cap pricing.Micros, mode ledger.Mode) (*ledger.Ledger, ledger.Project, ledger.Key) {
	t.Helper()
	l := ledger.New(testenv.Postgres(t))
	ctx := context.Background()
	p, err := l.CreateProject(ctx, "test", cap, mode)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := l.CreateKey(ctx, p.ID, ledger.KeyOptions{Name: "k"})
	if err != nil {
		t.Fatal(err)
	}
	k, err := l.Authenticate(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	return l, p, k
}

var nextID atomic.Int64

func reqID() string { return fmt.Sprintf("req_%d", nextID.Add(1)) }

func audit(t *testing.T, l *ledger.Ledger, projectID int64) ledger.Audit {
	t.Helper()
	a, err := l.Audit(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestReserveThenSettle(t *testing.T) {
	l, p, k := setup(t, dollar, ledger.ModeHardcap)
	ctx := context.Background()

	h, err := l.Reserve(ctx, k, reqID(), "m", 300_000, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if a := audit(t, l, p.ID); a.Project.Reserved != 300_000 || a.OpenHolds != 1 {
		t.Fatalf("after reserve: %+v", a)
	}

	// The request really cost less than the worst case.
	if err := l.Settle(ctx, h, ledger.Settlement{Outcome: "ok", UsageKnown: true, Cost: 120_000}); err != nil {
		t.Fatal(err)
	}
	a := audit(t, l, p.ID)
	if a.Project.Spent != 120_000 || a.Project.Reserved != 0 || a.OpenHolds != 0 || a.Project.Requests != 1 {
		t.Fatalf("after settle: %+v", a)
	}
	if !a.Balanced() {
		t.Fatalf("books don't balance: %+v", a)
	}
}

func TestRefusesWhatDoesntFit(t *testing.T) {
	l, _, k := setup(t, dollar, ledger.ModeHardcap)
	ctx := context.Background()

	if _, err := l.Reserve(ctx, k, reqID(), "m", 600_000, time.Minute); err != nil {
		t.Fatal(err)
	}
	// $0.60 is held, so $0.50 more doesn't fit, even though nothing is spent yet.
	_, err := l.Reserve(ctx, k, reqID(), "m", 500_000, time.Minute)
	var be *ledger.BudgetError
	if !errors.As(err, &be) || be.Scope != "project" || be.Free() != 400_000 {
		t.Fatalf("err = %v, want a project BudgetError with $0.40 free", err)
	}
	// $0.40 still fits exactly.
	if _, err := l.Reserve(ctx, k, reqID(), "m", 400_000, time.Minute); err != nil {
		t.Fatalf("an exact fit should be allowed: %v", err)
	}
}

func TestKeyCapIsCheckedToo(t *testing.T) {
	l, p, _ := setup(t, 10*dollar, ledger.ModeHardcap)
	ctx := context.Background()
	keyCap := pricing.Micros(200_000)
	_, secret, err := l.CreateKey(ctx, p.ID, ledger.KeyOptions{Name: "intern", Cap: &keyCap})
	if err != nil {
		t.Fatal(err)
	}
	k, err := l.Authenticate(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}

	_, err = l.Reserve(ctx, k, reqID(), "m", 250_000, time.Minute)
	var be *ledger.BudgetError
	if !errors.As(err, &be) || be.Scope != "key" {
		t.Fatalf("err = %v, want a key BudgetError", err)
	}
	// A refused reserve must not leave anything behind.
	if a := audit(t, l, p.ID); a.Project.Reserved != 0 || a.OpenHolds != 0 {
		t.Fatalf("refused reserve left state behind: %+v", a)
	}
}

func TestRevokedKeysAreRefused(t *testing.T) {
	l, p, k := setup(t, dollar, ledger.ModeHardcap)
	ctx := context.Background()
	if err := l.RevokeKey(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve(ctx, k, reqID(), "m", 1, time.Minute); !errors.Is(err, ledger.ErrInvalidKey) {
		t.Errorf("Reserve with a revoked key: err = %v", err)
	}
	if err := l.RevokeKey(ctx, k.ID); !errors.Is(err, ledger.ErrNotFound) {
		t.Errorf("revoking twice: err = %v, want ErrNotFound", err)
	}
	keys, err := l.Keys(ctx, p.ID)
	if err != nil || len(keys) != 1 || !keys[0].Revoked {
		t.Errorf("Keys = %+v, %v", keys, err)
	}
}

func TestAuthenticate(t *testing.T) {
	l, p, _ := setup(t, dollar, ledger.ModeNaive)
	ctx := context.Background()
	key, secret, err := l.CreateKey(ctx, p.ID, ledger.KeyOptions{Name: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if len(secret) != 35 || secret[:3] != "hc_" || key.Prefix != secret[:11] {
		t.Errorf("secret %q / prefix %q look wrong", secret, key.Prefix)
	}

	k, err := l.Authenticate(ctx, secret)
	if err != nil || k.ID != key.ID || k.Mode != ledger.ModeNaive {
		t.Fatalf("Authenticate = %+v, %v", k, err)
	}
	flipped := secret[:34] + "x"
	if secret[34] == 'x' {
		flipped = secret[:34] + "y"
	}
	for _, bad := range []string{"", "hc_short", "sk_" + secret[3:], flipped} {
		if _, err := l.Authenticate(ctx, bad); !errors.Is(err, ledger.ErrInvalidKey) {
			t.Errorf("Authenticate(%q) err = %v, want ErrInvalidKey", bad, err)
		}
	}
}

func TestSettleTwiceChargesOnce(t *testing.T) {
	l, p, k := setup(t, dollar, ledger.ModeHardcap)
	ctx := context.Background()
	h, err := l.Reserve(ctx, k, reqID(), "m", 100_000, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	s := ledger.Settlement{Outcome: "ok", UsageKnown: true, Cost: 70_000}
	for range 3 {
		if err := l.Settle(ctx, h, s); err != nil {
			t.Fatal(err)
		}
	}
	if a := audit(t, l, p.ID); a.Project.Spent != 70_000 || a.ReceiptCount != 1 || !a.Balanced() {
		t.Fatalf("after 3 settles: %+v", a)
	}
}

// A gateway that dies mid-request never settles. Its hold expires, and the
// sweeper charges it in full: the provider may have used every token.
func TestExpiredHoldsAreChargedInFull(t *testing.T) {
	l, p, k := setup(t, dollar, ledger.ModeHardcap)
	ctx := context.Background()

	orphan, err := l.Reserve(ctx, k, reqID(), "m", 250_000, -time.Second) // already expired
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve(ctx, k, reqID(), "m", 100_000, time.Hour); err != nil { // still running
		t.Fatal(err)
	}

	n, err := l.SweepExpired(ctx, 100)
	if err != nil || n != 1 {
		t.Fatalf("SweepExpired = %d, %v; want 1", n, err)
	}
	a := audit(t, l, p.ID)
	if a.Project.Spent != 250_000 || a.Project.Reserved != 100_000 || a.OpenHolds != 1 || !a.Balanced() {
		t.Fatalf("after sweep: %+v", a)
	}

	// If the "dead" gateway was only slow and settles late, nothing changes:
	// the receipt is already written, so this is a harmless duplicate.
	if err := l.Settle(ctx, orphan, ledger.Settlement{Outcome: "ok", UsageKnown: true, Cost: 10}); err != nil {
		t.Fatal(err)
	}
	if a := audit(t, l, p.ID); a.Project.Spent != 250_000 || !a.Balanced() {
		t.Fatalf("a late settle changed the books: %+v", a)
	}
}

// THE test. 1,000 requests arrive at the same moment against a $1 cap, each
// needing $0.003 held. Exactly 333 fit ($0.999), and all 1,000 are decided
// without the cap ever being crossed.
func TestThousandConcurrentReservesNeverCrossTheCap(t *testing.T) {
	l, p, k := setup(t, dollar, ledger.ModeHardcap)
	ctx := context.Background()

	const workers, hold = 1000, 3_000
	var accepted atomic.Int64
	holds := make(chan ledger.Hold, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			<-start // release everyone at once
			h, err := l.Reserve(ctx, k, reqID(), "m", hold, time.Minute)
			var be *ledger.BudgetError
			switch {
			case err == nil:
				accepted.Add(1)
				holds <- h
			case errors.As(err, &be):
			default:
				t.Error(err)
			}
		})
	}
	close(start)
	wg.Wait()
	close(holds)

	if got := accepted.Load(); got != 333 {
		t.Fatalf("accepted %d requests, want exactly 333", got)
	}
	// Every accepted request finishes, costing anywhere up to its hold.
	for h := range holds {
		if err := l.Settle(ctx, h, ledger.Settlement{Outcome: "ok", UsageKnown: true, Cost: pricing.Micros(rand.IntN(hold + 1))}); err != nil {
			t.Fatal(err)
		}
	}
	a := audit(t, l, p.ID)
	if a.Project.Spent > dollar || a.Project.Reserved != 0 || !a.Balanced() {
		t.Fatalf("after settling: %+v", a)
	}
}

// Sustained load: 200 workers loop reserve → "call the AI" → settle until
// the budget runs dry. Spending climbs close to the cap and never past it.
func TestSustainedLoadFillsTheBudgetWithoutCrossingIt(t *testing.T) {
	l, p, k := setup(t, dollar, ledger.ModeHardcap)
	ctx := context.Background()

	const hold = 5_000
	var wg sync.WaitGroup
	for range 200 {
		wg.Go(func() {
			for refused := 0; refused < 20; {
				h, err := l.Reserve(ctx, k, reqID(), "m", hold, time.Minute)
				var be *ledger.BudgetError
				if errors.As(err, &be) {
					refused++
					time.Sleep(time.Millisecond)
					continue
				}
				if err != nil {
					t.Error(err)
					return
				}
				time.Sleep(time.Duration(rand.IntN(3)) * time.Millisecond) // the AI "thinks"
				cost := pricing.Micros(hold/2 + rand.IntN(hold/2+1))
				if err := l.Settle(ctx, h, ledger.Settlement{Outcome: "ok", UsageKnown: true, Cost: cost}); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()

	a := audit(t, l, p.ID)
	if a.Project.Spent > dollar {
		t.Fatalf("CAP CROSSED: spent %s of %s", a.Project.Spent, pricing.Micros(dollar))
	}
	if a.Project.Spent < dollar-hold {
		t.Errorf("spent %s: the budget should fill to within one hold of the cap", a.Project.Spent)
	}
	if a.Project.Reserved != 0 || !a.Balanced() {
		t.Fatalf("books don't balance: %+v", a)
	}
	t.Logf("spent %s of %s over %d requests", a.Project.Spent, pricing.Micros(dollar), a.ReceiptCount)
}

// The same burst against both modes: 100 requests, each costing $0.05,
// against a $1 cap. Naive mode lets all 100 through ($5.00 spent).
// Hardcap lets exactly 20 through ($1.00).
func TestNaiveModeOverspendsAndHardcapDoesnt(t *testing.T) {
	for _, mode := range []ledger.Mode{ledger.ModeNaive, ledger.ModeHardcap} {
		t.Run(string(mode), func(t *testing.T) {
			l, p, k := setup(t, dollar, mode)
			ctx := context.Background()

			const workers, cost = 100, 50_000
			var checked, done sync.WaitGroup
			checked.Add(workers)
			var accepted atomic.Int64
			for range workers {
				done.Go(func() {
					h, err := l.Reserve(ctx, k, reqID(), "m", cost, time.Minute)
					checked.Done()
					if err != nil {
						return
					}
					accepted.Add(1)
					checked.Wait() // every request has checked the balance before any finishes
					if err := l.Settle(ctx, h, ledger.Settlement{Outcome: "ok", UsageKnown: true, Cost: cost}); err != nil {
						t.Error(err)
					}
				})
			}
			done.Wait()

			a := audit(t, l, p.ID)
			t.Logf("%s: %d accepted, spent %s of a %s cap", mode, accepted.Load(), a.Project.Spent, a.Project.Cap)
			switch mode {
			case ledger.ModeNaive:
				if a.Project.Spent != workers*cost {
					t.Errorf("naive spent %s, expected it to overspend to $5.00", a.Project.Spent)
				}
			case ledger.ModeHardcap:
				if accepted.Load() != 20 || a.Project.Spent != dollar {
					t.Errorf("hardcap accepted %d and spent %s, want 20 and $1.00", accepted.Load(), a.Project.Spent)
				}
			}
			if !a.Balanced() {
				t.Errorf("books don't balance: %+v", a)
			}
		})
	}
}

func TestResetSpend(t *testing.T) {
	l, p, k := setup(t, dollar, ledger.ModeHardcap)
	ctx := context.Background()
	h1, _ := l.Reserve(ctx, k, reqID(), "m", 100_000, time.Minute)
	if err := l.Settle(ctx, h1, ledger.Settlement{Outcome: "ok", Cost: 90_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve(ctx, k, reqID(), "m", 50_000, time.Minute); err != nil { // still running
		t.Fatal(err)
	}

	if err := l.ResetSpend(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	a := audit(t, l, p.ID)
	if a.Project.Spent != 0 || a.ReceiptCount != 0 || a.Project.Reserved != 50_000 || !a.Balanced() {
		t.Fatalf("after reset: %+v", a)
	}
	if err := l.ResetSpend(ctx, 999_999); !errors.Is(err, ledger.ErrNotFound) {
		t.Errorf("reset of a missing project: err = %v", err)
	}
}

func TestProjectsAndKeysCRUD(t *testing.T) {
	l, p, _ := setup(t, dollar, ledger.ModeHardcap)
	ctx := context.Background()

	if _, err := l.CreateProject(ctx, "test", dollar, ledger.ModeHardcap); !errors.Is(err, ledger.ErrNameTaken) {
		t.Errorf("duplicate name: err = %v", err)
	}
	if _, err := l.CreateProject(ctx, "x", dollar, "yolo"); err == nil {
		t.Error("an unknown mode should be refused")
	}
	if _, _, err := l.CreateKey(ctx, 999_999, ledger.KeyOptions{Name: "k"}); !errors.Is(err, ledger.ErrNotFound) {
		t.Errorf("key for a missing project: err = %v", err)
	}

	newCap, naive := pricing.Micros(5*dollar), ledger.ModeNaive
	got, err := l.UpdateProject(ctx, p.ID, &newCap, &naive)
	if err != nil || got.Cap != newCap || got.Mode != naive {
		t.Fatalf("UpdateProject = %+v, %v", got, err)
	}
	got, err = l.UpdateProject(ctx, p.ID, nil, nil) // nothing to change
	if err != nil || got.Cap != newCap {
		t.Fatalf("UpdateProject(nil, nil) = %+v, %v", got, err)
	}
	if _, err := l.Project(ctx, 999_999); !errors.Is(err, ledger.ErrNotFound) {
		t.Errorf("missing project: err = %v", err)
	}
	all, err := l.Projects(ctx)
	if err != nil || len(all) != 1 {
		t.Errorf("Projects = %+v, %v", all, err)
	}
}

func TestRecentUsage(t *testing.T) {
	l, p, k := setup(t, dollar, ledger.ModeHardcap)
	ctx := context.Background()
	for i := range 3 {
		h, err := l.Reserve(ctx, k, reqID(), "mock-1", 10_000, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Settle(ctx, h, ledger.Settlement{Provider: "mock", Outcome: "ok", PromptTokens: 5, CompletionTokens: i, UsageKnown: true, Cost: 100}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := l.RecentUsage(ctx, p.ID, 2)
	if err != nil || len(events) != 2 || events[0].CompletionTokens != 2 || events[0].Provider != "mock" || events[0].Held != 10_000 {
		t.Fatalf("RecentUsage = %+v, %v", events, err)
	}
}
