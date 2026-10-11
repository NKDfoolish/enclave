// Package httpserver holds HTTP plumbing shared by every bounded context:
// server lifecycle, middleware and JSON responses. It contains no business rules.
package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Run serves handler on addr until ctx is cancelled (e.g. SIGTERM), then shuts down gracefully:
// it stops accepting connections and waits up to shutdownTimeout for in-flight requests.
func Run(ctx context.Context, logger *slog.Logger, addr string, handler http.Handler, shutdownTimeout time.Duration) error {
	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
		// Guards against slow clients holding connections open (Slowloris).
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No ReadTimeout/WriteTimeout: uploads (Part 2) and SSE streams (Part 3) are long-lived.
		// Per-route deadlines will be added where needed.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	logger.Info("http server shutting down", "timeout", shutdownTimeout.String())
	// ctx is already cancelled, so shutdown needs a fresh deadline of its own.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
