// Command api runs the Enclave Core API.
//
// This package is the composition root: it loads config, builds dependencies and wires them
// together. Business rules live in the bounded contexts under internal/, never here.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	identityapp "enclave/api/internal/identity/app"
	identityinfra "enclave/api/internal/identity/infra"
	"enclave/api/internal/platform/config"
	"enclave/api/internal/platform/httpserver"
	"enclave/api/internal/platform/postgres"
	"enclave/api/internal/platform/ragclient"
	"enclave/api/internal/platform/redis"
	workspaceapp "enclave/api/internal/workspace/app"
	workspaceinfra "enclave/api/internal/workspace/infra"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("api exited", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	pool, err := postgres.Connect(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	redisClient, err := redis.Connect(context.Background(), cfg.RedisURL)
	if err != nil {
		return err
	}
	defer func() {
		if err := redisClient.Close(); err != nil {
			logger.Warn("close redis client", "err", err)
		}
	}()

	rag, err := ragclient.New(cfg.RAGAddr)
	if err != nil {
		return err
	}
	defer func() {
		if err := rag.Close(); err != nil {
			logger.Warn("close rag client", "err", err)
		}
	}()

	svc := services{
		identity: identityapp.NewService(
			identityinfra.NewRepository(pool),
			identityinfra.NewRevocationStore(redisClient),
			identityapp.TokenConfig{
				SigningKey: cfg.JWTSigningKey,
				AccessTTL:  cfg.AccessTokenTTL,
				RefreshTTL: cfg.RefreshTokenTTL,
			},
		),
		workspace: workspaceapp.NewService(workspaceinfra.NewRepository(pool)),
		rag:       rag,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, restore default signal handling so a second Ctrl+C
	// kills the process immediately instead of waiting for graceful shutdown.
	go func() {
		<-ctx.Done()
		stop()
	}()

	handler := routes(cfg, logger, svc)
	return httpserver.Run(ctx, logger, cfg.HTTPAddr, handler, cfg.ShutdownTimeout)
}
