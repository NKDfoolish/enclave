package diagnostics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ragv1 "enclave/gen/go/enclave/rag/v1"
)

type fakePinger struct {
	resp *ragv1.PingResponse
	err  error
}

func (f fakePinger) Ping(context.Context, string) (*ragv1.PingResponse, error) {
	return f.resp, f.err
}

func TestRAGPing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	tests := []struct {
		name       string
		rag        fakePinger
		wantStatus int
		wantBody   string
	}{
		{
			name:       "rag answers",
			rag:        fakePinger{resp: &ragv1.PingResponse{Message: "ping from api", Service: "rag"}},
			wantStatus: http.StatusOK,
			wantBody:   `"service":"rag"`,
		},
		{
			name:       "rag down",
			rag:        fakePinger{err: errors.New("dial tcp 10.0.0.5:50051: connection refused")},
			wantStatus: http.StatusBadGateway,
			wantBody:   `"code":"rag_unavailable"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			RAGPing(logger, tc.rag).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/debug/rag-ping", nil))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("body %s should contain %s", rec.Body.String(), tc.wantBody)
			}
			if strings.Contains(rec.Body.String(), "10.0.0.5") {
				t.Fatal("internal address must not leak to the client")
			}
		})
	}
}
