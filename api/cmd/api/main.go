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
	"sync"
	"syscall"
	"time"

	documentapp "enclave/api/internal/document/app"
	documentinfra "enclave/api/internal/document/infra"
	identityapp "enclave/api/internal/identity/app"
	identityinfra "enclave/api/internal/identity/infra"
	"enclave/api/internal/platform/config"
	"enclave/api/internal/platform/httpserver"
	"enclave/api/internal/platform/objectstore"
	"enclave/api/internal/platform/outbox"
	"enclave/api/internal/platform/postgres"
	"enclave/api/internal/platform/ragclient"
	"enclave/api/internal/platform/redis"
	workspaceapp "enclave/api/internal/workspace/app"
	workspaceinfra "enclave/api/internal/workspace/infra"
)

const abandonedUploadSweepInterval = 10 * time.Minute

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

	store := objectstore.New(cfg.ObjectStore)
	if err := store.CheckBucket(context.Background()); err != nil {
		return err
	}

	rag, err := ragclient.New(cfg.RAGAddr)
	if err != nil {
		return err
	}
	defer func() {
		if err := rag.Close(); err != nil {
			logger.Warn("close rag client", "err", err)
		}
	}()

	workspaces := workspaceapp.NewService(workspaceinfra.NewRepository(pool))
	documents := documentapp.NewService(workspaces, documentinfra.NewRepository(pool), store, cfg.MaxDocumentBytes)
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
		workspace: workspaces,
		document:  documents,
		rag:       rag,
	}

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, restore default signal handling so a second Ctrl+C
	// kills the process immediately instead of waiting for graceful shutdown.
	go func() {
		<-signalCtx.Done()
		stop()
	}()

	// Background work stops when ctx is cancelled: on a signal, or when the HTTP server
	// exits with an error. Deferred calls run last-in-first-out, so cancel() runs before
	// background.Wait(), and both run before the pool is closed.
	ctx, cancel := context.WithCancel(signalCtx)
	var background sync.WaitGroup
	defer background.Wait()
	defer cancel()

	worker := outbox.NewWorker(pool, logger)
	worker.Handle(documentapp.JobValidateUpload, documents.ValidateUpload)
	worker.Handle(documentapp.JobDeleteObject, documents.DeleteObject)
	background.Go(func() { worker.Run(ctx) })
	background.Go(func() {
		every(ctx, abandonedUploadSweepInterval, func() {
			removed, err := documents.CleanupAbandonedUploads(ctx)
			if err != nil {
				logger.Error("clean up abandoned uploads", "err", err)
			}
			if removed > 0 {
				logger.Info("cleaned up abandoned uploads", "removed", removed)
			}
		})
	})

	handler := routes(cfg, logger, svc)
	return httpserver.Run(ctx, logger, cfg.HTTPAddr, handler, cfg.ShutdownTimeout)
}

// every calls fn once per interval until ctx is cancelled.
func every(ctx context.Context, interval time.Duration, fn func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fn()
		}
	}
}
