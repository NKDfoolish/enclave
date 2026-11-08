package main

import (
	"log/slog"
	"net/http"
	"net/url"

	documentapp "enclave/api/internal/document/app"
	documenttransport "enclave/api/internal/document/transport"
	identityapp "enclave/api/internal/identity/app"
	identitytransport "enclave/api/internal/identity/transport"
	"enclave/api/internal/platform/config"
	"enclave/api/internal/platform/diagnostics"
	"enclave/api/internal/platform/httpserver"
	workspaceapp "enclave/api/internal/workspace/app"
	workspacetransport "enclave/api/internal/workspace/transport"
	"enclave/web"
)

// services are the dependencies the HTTP layer exposes, built once in run().
type services struct {
	identity  *identityapp.Service
	workspace *workspaceapp.Service
	document  *documentapp.Service
	rag       diagnostics.RAGPinger
}

// routes is the single place to see every endpoint the Core API exposes.
func routes(cfg config.Config, logger *slog.Logger, svc services) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", diagnostics.Health)
	if cfg.EnableDebugRoutes {
		mux.Handle("GET /v1/debug/rag-ping", diagnostics.RAGPing(logger, svc.rag))
	}

	identity := identitytransport.NewHandler(svc.identity, logger, cfg.SecureCookies)
	identity.RegisterRoutes(mux)
	workspacetransport.NewHandler(svc.workspace, logger).RegisterRoutes(mux, identity.RequireAuth)
	documenttransport.NewHandler(svc.document, logger).RegisterRoutes(mux, identity.RequireAuth)

	// Unknown /v1 paths get a JSON 404 instead of falling through to the web UI.
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		httpserver.WriteError(w, r, http.StatusNotFound, "not_found", "not found")
	})
	// Not "GET /": it would conflict with "/v1/" under Go 1.22 pattern rules.
	// web.Handler rejects methods other than GET and HEAD itself.
	mux.Handle("/", web.Handler(origin(cfg.ObjectStore.PublicEndpoint)))

	// Outermost first: every request gets an ID, is logged, and panics become logged 500s.
	return httpserver.Chain(mux,
		httpserver.RequestID,
		httpserver.AccessLog(logger),
		httpserver.Recover(logger),
	)
}

// origin returns scheme://host of endpoint: the web UI may send presigned uploads there.
func origin(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
