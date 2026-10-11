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

	"enclave/api/internal/platform/config"
	"enclave/api/internal/platform/httpserver"
	"enclave/api/internal/platform/ragclient"
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

	rag, err := ragclient.New(cfg.RAGAddr)
	if err != nil {
		return err
	}
	defer func() {
		if err := rag.Close(); err != nil {
			logger.Warn("close rag client", "err", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, restore default signal handling so a second Ctrl+C
	// kills the process immediately instead of waiting for graceful shutdown.
	go func() {
		<-ctx.Done()
		stop()
	}()

	handler := routes(cfg, logger, rag)
	return httpserver.Run(ctx, logger, cfg.HTTPAddr, handler, cfg.ShutdownTimeout)
}
