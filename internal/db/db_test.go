package db_test

import (
	"context"
	"sync"
	"testing"

	"github.com/GitDaksh/hardcap/internal/db"
	"github.com/GitDaksh/hardcap/internal/testenv"
)

// testenv.Postgres has already migrated the schema once. Running it again,
// from several "gateways" at the same moment, must be a harmless no-op.
func TestMigrateIsSafeToRunAgainConcurrently(t *testing.T) {
	pool := testenv.Postgres(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if err := db.Migrate(ctx, pool); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	var applied int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	var functions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_proc WHERE proname LIKE 'hardcap_%'
		AND pronamespace = current_schema()::regnamespace`).Scan(&functions); err != nil {
		t.Fatal(err)
	}
	if applied != 1 || functions != 3 {
		t.Fatalf("migrations applied = %d, functions = %d; want 1 and 3", applied, functions)
	}
}

func TestConnectReportsAnUnreachableDatabase(t *testing.T) {
	if _, err := db.Connect(context.Background(), "postgres://127.0.0.1:1/x?connect_timeout=1", 0); err == nil {
		t.Fatal("want an error")
	}
	if _, err := db.Connect(context.Background(), "::not a url::", 0); err == nil {
		t.Fatal("want an error for a bad URL")
	}
}
