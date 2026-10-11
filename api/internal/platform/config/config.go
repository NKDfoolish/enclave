// Package config loads Core API settings from environment variables and validates them at startup.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	// HTTPAddr is where the public HTTP API listens, e.g. ":8080".
	HTTPAddr string
	// ShutdownTimeout bounds how long in-flight requests may finish after SIGTERM.
	ShutdownTimeout time.Duration
	// RAGAddr is the RAG service gRPC address, e.g. "localhost:50051".
	RAGAddr string
	// EnableDebugRoutes exposes /v1/debug/* endpoints. Keep false outside local development.
	EnableDebugRoutes bool
}

// Load reads the environment, applies defaults and returns every problem found at once,
// so a misconfigured deployment fails at startup instead of on the first request.
func Load() (Config, error) {
	var errs []error

	cfg := Config{
		HTTPAddr:          getString("API_HTTP_ADDR", ":8080"),
		ShutdownTimeout:   getDuration("API_SHUTDOWN_TIMEOUT", 10*time.Second, &errs),
		RAGAddr:           getString("RAG_GRPC_ADDR", "localhost:50051"),
		EnableDebugRoutes: getBool("API_ENABLE_DEBUG_ROUTES", false, &errs),
	}

	if cfg.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("API_SHUTDOWN_TIMEOUT must be positive"))
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

func getString(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getDuration(key string, fallback time.Duration, errs *[]error) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %q is not a duration like 10s or 500ms", key, v))
		return fallback
	}
	return d
}

func getBool(key string, fallback bool, errs *[]error) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %q is not a boolean", key, v))
		return fallback
	}
	return b
}
