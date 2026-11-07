// Command hardcap runs the Hardcap gateway: an OpenAI-compatible proxy that
// enforces hard spending caps, plus its admin API and live dashboard.
//
// Every setting is an environment variable with a sensible local default;
// the README lists them all.
package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/GitDaksh/hardcap/internal/admin"
	"github.com/GitDaksh/hardcap/internal/config"
	"github.com/GitDaksh/hardcap/internal/db"
	"github.com/GitDaksh/hardcap/internal/env"
	"github.com/GitDaksh/hardcap/internal/gateway"
	"github.com/GitDaksh/hardcap/internal/ledger"
	"github.com/GitDaksh/hardcap/internal/metrics"
	"github.com/GitDaksh/hardcap/internal/ratelimit"
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
	var e env.Loader
	// localhost by default: reachable only from this machine. Set
	// HARDCAP_ADDR=:8080 to listen on every interface (e.g. in a container).
	addr := e.String("HARDCAP_ADDR", "localhost:8080")
	configPath := e.String("HARDCAP_CONFIG", "hardcap.yaml")
	dbURL := e.String("HARDCAP_DATABASE_URL", "postgres://localhost:5432/hardcap?sslmode=disable")
	dbConns := e.Int("HARDCAP_DB_MAX_CONNS", 20)
	redisURL := e.String("HARDCAP_REDIS_URL", "")
	adminToken := e.String("HARDCAP_ADMIN_TOKEN", "")
	demo := e.Bool("HARDCAP_DEMO", false)
	requestTimeout := e.Duration("HARDCAP_REQUEST_TIMEOUT", 5*time.Minute)
	holdGrace := e.Duration("HARDCAP_HOLD_GRACE", time.Minute)
	sweepEvery := e.Duration("HARDCAP_SWEEP_INTERVAL", 15*time.Second)
	shutdownGrace := e.Duration("HARDCAP_SHUTDOWN_GRACE", 25*time.Second)
	if err := e.Err(); err != nil {
		return err
	}

	models, warnings, err := config.Load(configPath, os.Getenv)
	for _, w := range warnings {
		logger.Warn(w)
	}
	if err != nil {
		return err
	}

	// ctx ends on Ctrl+C or SIGTERM, which starts a graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, dbURL, int32(dbConns))
	if err != nil {
		return err
	}
	defer pool.Close() // runs last, after every in-flight request has settled
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	books := ledger.New(pool)
	reg := metrics.NewRegistry()

	var limiter *ratelimit.Limiter
	if redisURL == "" {
		logger.Warn("HARDCAP_REDIS_URL isn't set: rate limiting is off")
	} else {
		opts, err := redis.ParseURL(redisURL)
		if err != nil {
			return err
		}
		opts.DialTimeout = time.Second
		opts.ReadTimeout = 100 * time.Millisecond
		opts.WriteTimeout = 100 * time.Millisecond
		opts.MaxRetries = -1 // fail fast: a slow limiter must not slow every request
		rdb := redis.NewClient(opts)
		defer rdb.Close()
		if err := rdb.Ping(ctx).Err(); err != nil {
			logger.Warn("Redis isn't answering; requests go through without rate limits until it's back", "error", err)
		}
		limiter = ratelimit.New(rdb, "hardcap:")
	}

	gw, err := gateway.New(gateway.Config{
		Models: models, Ledger: books, Limiter: limiter, Logger: logger, Metrics: reg,
		RequestTimeout: requestTimeout, HoldGrace: holdGrace,
	})
	if err != nil {
		return err
	}
	reg.GaugeFunc("hardcap_db_connections_in_use", "Postgres connections in use right now.",
		func() float64 { return float64(pool.Stat().AcquiredConns()) })

	mux := http.NewServeMux()
	mux.Handle("/v1/", gw.Handler())
	mux.Handle("GET /metrics", reg)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		// Ready means "can check budgets". Without Postgres every chat
		// request would be refused, so a load balancer should skip us.
		pingCtx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "application/json")
		if err := pool.Ping(pingCtx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"budget store unavailable"}` + "\n"))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ready"}` + "\n"))
	})

	if adminToken == "" {
		logger.Warn("HARDCAP_ADMIN_TOKEN isn't set: the admin API and dashboard are off")
	} else {
		var runner *admin.Demo
		if demo {
			runner = admin.NewDemo(selfURL(addr), books, logger)
		}
		adm, err := admin.New(admin.Config{Ledger: books, Token: adminToken, Models: models.ModelNames(), Logger: logger, Demo: runner})
		if err != nil {
			return err
		}
		mux.Handle("/", adm.Handler())
	}

	go sweep(ctx, books, sweepEvery, reg, logger)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second, // blocks slow-header (Slowloris) attacks
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout on purpose: streamed replies can run for minutes.
	}
	logger.Info("hardcap listening", "addr", addr, "models", models.ModelNames(),
		"dashboard", adminToken != "", "demo", demo, "rate_limits", limiter != nil)
	return serve.Run(ctx, srv, logger, shutdownGrace)
}

// sweep charges expired holds on a timer. A hold expires only when the
// gateway that made it died mid-request, so each one is logged loudly.
func sweep(ctx context.Context, books *ledger.Ledger, every time.Duration, reg *metrics.Registry, logger *slog.Logger) {
	charged := reg.Counter("hardcap_holds_charged_on_expiry_total",
		"Expired holds charged in full by the sweeper. Each one means a gateway died mid-request.")
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		n, err := books.SweepExpired(ctx, 1000)
		switch {
		case err != nil && ctx.Err() == nil:
			logger.Error("sweep failed", "error", err)
		case n > 0:
			charged.With().Add(float64(n))
			logger.Warn("charged expired holds in full (a gateway died mid-request)", "holds", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// selfURL turns a listen address into a URL this process can call itself on.
func selfURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}
