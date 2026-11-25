package main

import (
	"log/slog"
	"net/http"

	"enclave/gateway/internal/config"
	"enclave/gateway/internal/llm/app"
	"enclave/gateway/internal/llm/transport"
	"enclave/internal/platform/httpserver"
)

// routes is the single place to see every endpoint the gateway exposes.
func routes(cfg config.Config, logger *slog.Logger, service *app.Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpserver.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	transport.NewHandler(service, logger).RegisterRoutes(mux, transport.ServiceAuth(cfg.ServiceTokens))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpserver.WriteError(w, r, http.StatusNotFound, "not_found", "not found")
	})

	// The access log records method, path, status and timing only: never prompts or answers.
	return httpserver.Chain(mux,
		httpserver.RequestID,
		httpserver.AccessLog(logger),
		httpserver.Recover(logger),
	)
}
