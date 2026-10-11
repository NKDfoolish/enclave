package main

import (
	"log/slog"
	"net/http"

	"enclave/api/internal/platform/config"
	"enclave/api/internal/platform/diagnostics"
	"enclave/api/internal/platform/httpserver"
)

// routes is the single place to see every endpoint the Core API exposes.
// Each bounded context will register its own routes here, e.g. workspace.RegisterRoutes(mux, ...).
func routes(cfg config.Config, logger *slog.Logger, rag diagnostics.RAGPinger) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", diagnostics.Health)
	if cfg.EnableDebugRoutes {
		mux.Handle("GET /v1/debug/rag-ping", diagnostics.RAGPing(logger, rag))
	}

	// Outermost first: every request gets an ID, is logged, and panics become logged 500s.
	return httpserver.Chain(mux,
		httpserver.RequestID,
		httpserver.AccessLog(logger),
		httpserver.Recover(logger),
	)
}
