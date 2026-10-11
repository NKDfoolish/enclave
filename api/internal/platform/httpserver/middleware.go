package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// Middleware wraps a handler with behaviour that runs before and/or after it.
type Middleware func(http.Handler) http.Handler

// Chain applies middlewares so the first one listed is the outermost:
// Chain(h, A, B) handles a request as A -> B -> h -> B -> A.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// --- Request ID ---

const RequestIDHeader = "X-Request-ID"

// ctxKey is unexported so no other package can read or overwrite our context values by accident.
type ctxKey struct{}

// RequestID gives every request an ID, stores it in the context and echoes it in the response,
// so a client error report can be matched with server logs. A safe incoming ID from a trusted
// proxy is kept; anything else is replaced.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	})
}

// RequestIDFrom returns the request ID stored by the RequestID middleware, or "" if absent.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand never returns an error on supported platforms.
	return hex.EncodeToString(b)
}

// validRequestID accepts short IDs made of [A-Za-z0-9_-] so client input cannot inject
// newlines or huge values into logs and headers.
func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}

// --- Access log ---

// statusRecorder captures the status code and body size a handler writes.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK // net/http sends 200 implicitly on the first Write.
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the real writer, e.g. to Flush SSE streams later.
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

// AccessLog logs one line per request. It logs the path only, never the query string or body,
// because those may contain document text, questions or tokens.
func AccessLog(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)

			status := rec.status
			if status == 0 {
				status = http.StatusOK // handler wrote nothing
			}
			logger.LogAttrs(r.Context(), levelFor(status), "http request",
				slog.String("request_id", RequestIDFrom(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int("bytes", rec.bytes),
				slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
			)
		})
	}
}

func levelFor(status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// --- Recover ---

// Recover turns a panic in a handler into a 500 response and an error log with the stack trace,
// instead of dropping the connection. It must sit inside AccessLog so the 500 gets logged.
func Recover(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				// http.ErrAbortHandler is net/http's deliberate way to abort a response; let it through.
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				logger.ErrorContext(r.Context(), "panic in handler",
					slog.String("request_id", RequestIDFrom(r.Context())),
					slog.Any("panic", v),
					slog.String("stack", string(debug.Stack())),
				)
				WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
			}()
			next.ServeHTTP(w, r)
		})
	}
}
