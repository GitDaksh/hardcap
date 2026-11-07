// Package testenv gives tests a real Postgres and Redis.
//
// Tests that need them call Postgres(t) or Redis(t). When the matching
// environment variable isn't set, the test is skipped with a note on how to
// run it, so `go test ./...` still works on a machine without either. Set
// HARDCAP_REQUIRE_SERVICES=1 (CI and `make test` do) to turn those skips
// into failures, so a missing database can't make a broken build look green.
package testenv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/GitDaksh/hardcap/internal/db"
)

// Postgres returns a connection pool to a brand-new, fully migrated schema.
// Every test gets its own schema, so tests can run in parallel without
// seeing each other's data. The schema is dropped when the test ends.
func Postgres(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := need(t, "HARDCAP_TEST_DATABASE_URL", "postgres://localhost:5432/hardcap_test?sslmode=disable")
	ctx := context.Background()

	schema := "test_" + randomHex(6)
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("testenv: can't reach Postgres: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("testenv: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("testenv: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 40
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("testenv: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(dropCtx, "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close(dropCtx)
	})

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("testenv: migrate: %v", err)
	}
	return pool
}

// Redis returns a client plus a key prefix unique to this test. Keys under
// the prefix are deleted when the test ends.
func Redis(t testing.TB) (*redis.Client, string) {
	t.Helper()
	url := need(t, "HARDCAP_TEST_REDIS_URL", "redis://localhost:6379/0")
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("testenv: %v", err)
	}
	client := redis.NewClient(opts)
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("testenv: can't reach Redis: %v", err)
	}

	prefix := "hardcap-test:" + randomHex(6) + ":"
	t.Cleanup(func() {
		iter := client.Scan(ctx, 0, prefix+"*", 100).Iterator()
		for iter.Next(ctx) {
			client.Del(ctx, iter.Val())
		}
		_ = client.Close()
	})
	return client, prefix
}

func need(t testing.TB, envVar, example string) string {
	t.Helper()
	v := os.Getenv(envVar)
	if v == "" {
		msg := "set " + envVar + " (e.g. " + example + ") to run this test"
		if os.Getenv("HARDCAP_REQUIRE_SERVICES") != "" {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}
	return v
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
