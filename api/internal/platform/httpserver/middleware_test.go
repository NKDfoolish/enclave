package httpserver

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

func TestChainOrder(t *testing.T) {
	var order []string
	mark := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name+" in")
				next.ServeHTTP(w, r)
				order = append(order, name+" out")
			})
		}
	}
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	}), mark("A"), mark("B"))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := "A in,B in,handler,B out,A out"
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
}

func TestRequestID(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}))

	tests := []struct {
		name     string
		incoming string
		keep     bool
	}{
		{"generates when missing", "", false},
		{"keeps a safe incoming id", "abc-123_XYZ", true},
		{"replaces unsafe incoming id", "bad\nid", false},
		{"replaces oversized incoming id", strings.Repeat("a", 65), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.incoming != "" {
				req.Header.Set(RequestIDHeader, tc.incoming)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if seen == "" || rec.Header().Get(RequestIDHeader) != seen {
				t.Fatalf("context id %q and header %q must match and be non-empty", seen, rec.Header().Get(RequestIDHeader))
			}
			if (seen == tc.incoming) != tc.keep {
				t.Fatalf("incoming %q, got %q, keep=%v", tc.incoming, seen, tc.keep)
			}
		})
	}
}

func TestRecoverReturns500AndAccessLogRecordsIt(t *testing.T) {
	logger, logs := newTestLogger()
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}), RequestID, AccessLog(logger), Recover(logger))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/explode", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code != "internal_error" || body.Error.RequestID == "" {
		t.Fatalf("unexpected body: %+v", body)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Fatal("panic value must not leak to the client")
	}
	if !strings.Contains(logs.String(), `"msg":"panic in handler"`) || !strings.Contains(logs.String(), `"status":500`) {
		t.Fatalf("expected panic and access log lines, got:\n%s", logs.String())
	}
}

func TestAccessLogDefaultsTo200(t *testing.T) {
	logger, logs := newTestLogger()
	h := AccessLog(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/quiet?token=secret", nil))

	if !strings.Contains(logs.String(), `"status":200`) {
		t.Fatalf("want status 200 in log, got:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "secret") {
		t.Fatal("query string must not be logged")
	}
}
