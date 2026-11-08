package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://localhost/enclave")
	t.Setenv("REDIS_URL", "redis://localhost:6380/0")
	t.Setenv("API_JWT_SIGNING_KEY", base64.StdEncoding.EncodeToString(make([]byte, ed25519.SeedSize)))
}

func TestLoadDefaults(t *testing.T) {
	for _, key := range []string{
		"API_HTTP_ADDR", "API_SHUTDOWN_TIMEOUT", "RAG_GRPC_ADDR", "API_ENABLE_DEBUG_ROUTES",
		"API_ACCESS_TOKEN_TTL", "API_REFRESH_TOKEN_TTL", "API_SECURE_COOKIES",
	} {
		t.Setenv(key, "")
	}
	setRequired(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.ShutdownTimeout != 10*time.Second || cfg.RAGAddr != "localhost:50051" {
		t.Errorf("unexpected server defaults: %+v", cfg)
	}
	if cfg.EnableDebugRoutes || !cfg.SecureCookies {
		t.Errorf("defaults must be the safe ones: debug=%v secureCookies=%v", cfg.EnableDebugRoutes, cfg.SecureCookies)
	}
	if cfg.AccessTokenTTL != 15*time.Minute || cfg.RefreshTokenTTL != 7*24*time.Hour {
		t.Errorf("token TTLs = %v / %v", cfg.AccessTokenTTL, cfg.RefreshTokenTTL)
	}
	if len(cfg.JWTSigningKey) != ed25519.PrivateKeySize {
		t.Errorf("signing key length = %d", len(cfg.JWTSigningKey))
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	t.Setenv("API_SHUTDOWN_TIMEOUT", "ten seconds")
	t.Setenv("API_ENABLE_DEBUG_ROUTES", "maybe")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("REDIS_URL", "")
	t.Setenv("API_JWT_SIGNING_KEY", "too-short")

	_, err := Load()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	for _, key := range []string{
		"API_SHUTDOWN_TIMEOUT", "API_ENABLE_DEBUG_ROUTES", "DATABASE_URL", "REDIS_URL", "API_JWT_SIGNING_KEY",
	} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error should mention %s: %v", key, err)
		}
	}
}

func TestLoadRejectsAccessTTLNotShorterThanRefresh(t *testing.T) {
	setRequired(t)
	t.Setenv("API_ACCESS_TOKEN_TTL", "2h")
	t.Setenv("API_REFRESH_TOKEN_TTL", "1h")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "API_ACCESS_TOKEN_TTL") {
		t.Fatalf("want TTL error, got %v", err)
	}
}
