// Command mockllm runs a fake, OpenAI-compatible AI provider.
//
// Hardcap's development and every load test run against it, so they cost
// nothing. Tune it with environment variables (see the README).
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/GitDaksh/hardcap/internal/env"
	"github.com/GitDaksh/hardcap/internal/mock"
	"github.com/GitDaksh/hardcap/internal/serve"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("mockllm stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	var cfg env.Loader
	addr := cfg.String("MOCK_ADDR", "localhost:8090") // this machine only
	mockCfg := mock.Config{
		TokenDelay:  cfg.Duration("MOCK_TOKEN_DELAY", 30*time.Millisecond),
		FailRate:    cfg.Float("MOCK_FAIL_RATE", 0),
		ReplyTokens: cfg.Int("MOCK_REPLY_TOKENS", 24),
	}
	if err := cfg.Err(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              addr,
		Handler:           mock.NewHandler(mockCfg),
		ReadHeaderTimeout: 10 * time.Second,
	}
	logger.Info("mock provider listening",
		"addr", addr,
		"token_delay", mockCfg.TokenDelay,
		"fail_rate", mockCfg.FailRate,
		"reply_tokens", mockCfg.ReplyTokens)
	return serve.Run(ctx, srv, logger, 10*time.Second)
}
