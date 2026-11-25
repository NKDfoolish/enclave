// Package config loads Core API settings from environment variables and validates them at startup.
package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"enclave/internal/platform/env"
	"enclave/internal/platform/objectstore"
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
	// DatabaseURL is the Postgres connection string.
	DatabaseURL string
	// RedisURL is the Redis connection string; Redis holds the token revocation list.
	RedisURL string
	// JWTSigningKey signs access and refresh tokens (Ed25519). Only the Core API holds it;
	// other services verify tokens with its public key.
	JWTSigningKey ed25519.PrivateKey
	// AccessTokenTTL is how long an access token is accepted; RefreshTokenTTL how long a
	// login can be kept alive without entering the password again.
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	// SecureCookies marks token cookies HTTPS-only. Only local development over plain
	// HTTP may turn it off.
	SecureCookies bool
	// ObjectStore is the S3-compatible bucket holding original document files.
	ObjectStore objectstore.Config
	// MaxDocumentBytes is the largest file a user may upload.
	MaxDocumentBytes int64
}

// Load reads the environment, applies defaults and returns every problem found at once,
// so a misconfigured deployment fails at startup instead of on the first request.
func Load() (Config, error) {
	var r env.Reader

	cfg := Config{
		HTTPAddr:          r.String("API_HTTP_ADDR", ":8080"),
		ShutdownTimeout:   r.Duration("API_SHUTDOWN_TIMEOUT", 10*time.Second),
		RAGAddr:           r.String("RAG_GRPC_ADDR", "localhost:50051"),
		EnableDebugRoutes: r.Bool("API_ENABLE_DEBUG_ROUTES", false),
		DatabaseURL:       r.Required("DATABASE_URL"),
		RedisURL:          r.Required("REDIS_URL"),
		JWTSigningKey:     signingKey(&r, "API_JWT_SIGNING_KEY"),
		AccessTokenTTL:    r.Duration("API_ACCESS_TOKEN_TTL", 15*time.Minute),
		RefreshTokenTTL:   r.Duration("API_REFRESH_TOKEN_TTL", 7*24*time.Hour),
		SecureCookies:     r.Bool("API_SECURE_COOKIES", true),
		ObjectStore: objectstore.Config{
			Endpoint:       r.Required("OBJECTSTORE_ENDPOINT"),
			PublicEndpoint: r.String("OBJECTSTORE_PUBLIC_ENDPOINT", r.String("OBJECTSTORE_ENDPOINT", "")),
			Region:         r.String("OBJECTSTORE_REGION", "us-east-1"),
			Bucket:         r.String("OBJECTSTORE_BUCKET", "enclave-documents"),
			AccessKey:      r.Required("OBJECTSTORE_ACCESS_KEY"),
			SecretKey:      r.Required("OBJECTSTORE_SECRET_KEY"),
		},
		MaxDocumentBytes: r.Int64("API_MAX_DOCUMENT_BYTES", 25<<20),
	}

	if cfg.ShutdownTimeout <= 0 {
		r.Fail(errors.New("API_SHUTDOWN_TIMEOUT must be positive"))
	}
	if _, err := url.ParseRequestURI(cfg.ObjectStore.PublicEndpoint); cfg.ObjectStore.PublicEndpoint != "" && err != nil {
		r.Fail(errors.New("OBJECTSTORE_PUBLIC_ENDPOINT must be an absolute URL"))
	}
	if cfg.MaxDocumentBytes <= 0 {
		r.Fail(errors.New("API_MAX_DOCUMENT_BYTES must be positive"))
	}
	if cfg.AccessTokenTTL <= 0 || cfg.RefreshTokenTTL <= cfg.AccessTokenTTL {
		r.Fail(errors.New("API_ACCESS_TOKEN_TTL must be positive and shorter than API_REFRESH_TOKEN_TTL"))
	}

	if err := r.Err(); err != nil {
		return Config{}, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

// signingKey reads a base64-encoded 32-byte Ed25519 seed. There is deliberately no
// default: a missing key must stop the server instead of signing with a known value.
// Generate one with: openssl rand -base64 32
func signingKey(r *env.Reader, key string) ed25519.PrivateKey {
	v := os.Getenv(key)
	if v == "" {
		r.Fail(fmt.Errorf("%s is required (generate with: openssl rand -base64 32)", key))
		return nil
	}
	seed, err := base64.StdEncoding.DecodeString(v)
	if err != nil || len(seed) != ed25519.SeedSize {
		r.Fail(fmt.Errorf("%s must be %d bytes encoded as base64", key, ed25519.SeedSize))
		return nil
	}
	return ed25519.NewKeyFromSeed(seed)
}
