package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("API_HTTP_ADDR", "")
	t.Setenv("API_SHUTDOWN_TIMEOUT", "")
	t.Setenv("RAG_GRPC_ADDR", "")
	t.Setenv("API_ENABLE_DEBUG_ROUTES", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.RAGAddr != "localhost:50051" || cfg.ShutdownTimeout != 10*time.Second || cfg.EnableDebugRoutes {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	t.Setenv("API_SHUTDOWN_TIMEOUT", "ten seconds")
	t.Setenv("API_ENABLE_DEBUG_ROUTES", "maybe")

	_, err := Load()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	for _, key := range []string{"API_SHUTDOWN_TIMEOUT", "API_ENABLE_DEBUG_ROUTES"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error should mention %s: %v", key, err)
		}
	}
}
