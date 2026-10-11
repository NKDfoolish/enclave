// Package diagnostics exposes operational endpoints: liveness and, in development, a check
// that the Core API can reach the RAG service.
package diagnostics

import (
	"context"
	"log/slog"
	"net/http"

	"enclave/api/internal/platform/httpserver"
	ragv1 "enclave/gen/go/enclave/rag/v1"
)

// RAGPinger is the one capability the debug handler needs. *ragclient.Client satisfies it,
// and tests can pass a small fake instead of starting a gRPC server.
type RAGPinger interface {
	Ping(ctx context.Context, message string) (*ragv1.PingResponse, error)
}

// Health reports that the process is up. Load balancers and ECS call it; it must stay cheap.
func Health(w http.ResponseWriter, _ *http.Request) {
	httpserver.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// RAGPing calls the RAG service and reports the result. Development only.
func RAGPing(logger *slog.Logger, rag RAGPinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp, err := rag.Ping(r.Context(), "ping from api")
		if err != nil {
			logger.ErrorContext(r.Context(), "rag ping failed",
				slog.String("request_id", httpserver.RequestIDFrom(r.Context())),
				slog.Any("err", err),
			)
			httpserver.WriteError(w, r, http.StatusBadGateway, "rag_unavailable", "rag service unavailable")
			return
		}
		httpserver.WriteJSON(w, http.StatusOK, map[string]string{
			"service": resp.GetService(),
			"message": resp.GetMessage(),
		})
	}
}
