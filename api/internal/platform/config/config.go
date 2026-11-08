// Package config loads Core API settings from environment variables and validates them at startup.
package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"

	"enclave/api/internal/platform/objectstore"
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
	var errs []error

	cfg := Config{
		HTTPAddr:          getString("API_HTTP_ADDR", ":8080"),
		ShutdownTimeout:   getDuration("API_SHUTDOWN_TIMEOUT", 10*time.Second, &errs),
		RAGAddr:           getString("RAG_GRPC_ADDR", "localhost:50051"),
		EnableDebugRoutes: getBool("API_ENABLE_DEBUG_ROUTES", false, &errs),
		DatabaseURL:       getString("DATABASE_URL", ""),
		RedisURL:          getString("REDIS_URL", ""),
		JWTSigningKey:     getSigningKey("API_JWT_SIGNING_KEY", &errs),
		AccessTokenTTL:    getDuration("API_ACCESS_TOKEN_TTL", 15*time.Minute, &errs),
		RefreshTokenTTL:   getDuration("API_REFRESH_TOKEN_TTL", 7*24*time.Hour, &errs),
		SecureCookies:     getBool("API_SECURE_COOKIES", true, &errs),
		ObjectStore: objectstore.Config{
			Endpoint:       getString("OBJECTSTORE_ENDPOINT", ""),
			PublicEndpoint: getString("OBJECTSTORE_PUBLIC_ENDPOINT", getString("OBJECTSTORE_ENDPOINT", "")),
			Region:         getString("OBJECTSTORE_REGION", "us-east-1"),
			Bucket:         getString("OBJECTSTORE_BUCKET", "enclave-documents"),
			AccessKey:      getString("OBJECTSTORE_ACCESS_KEY", ""),
			SecretKey:      getString("OBJECTSTORE_SECRET_KEY", ""),
		},
		MaxDocumentBytes: getInt64("API_MAX_DOCUMENT_BYTES", 25<<20, &errs),
	}

	if cfg.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("API_SHUTDOWN_TIMEOUT must be positive"))
	}
	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if cfg.RedisURL == "" {
		errs = append(errs, errors.New("REDIS_URL is required"))
	}
	for key, value := range map[string]string{
		"OBJECTSTORE_ENDPOINT":   cfg.ObjectStore.Endpoint,
		"OBJECTSTORE_ACCESS_KEY": cfg.ObjectStore.AccessKey,
		"OBJECTSTORE_SECRET_KEY": cfg.ObjectStore.SecretKey,
	} {
		if value == "" {
			errs = append(errs, fmt.Errorf("%s is required", key))
		}
	}
	if _, err := url.ParseRequestURI(cfg.ObjectStore.PublicEndpoint); cfg.ObjectStore.PublicEndpoint != "" && err != nil {
		errs = append(errs, errors.New("OBJECTSTORE_PUBLIC_ENDPOINT must be an absolute URL"))
	}
	if cfg.MaxDocumentBytes <= 0 {
		errs = append(errs, errors.New("API_MAX_DOCUMENT_BYTES must be positive"))
	}
	if cfg.AccessTokenTTL <= 0 || cfg.RefreshTokenTTL <= cfg.AccessTokenTTL {
		errs = append(errs, errors.New("API_ACCESS_TOKEN_TTL must be positive and shorter than API_REFRESH_TOKEN_TTL"))
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

func getInt64(key string, fallback int64, errs *[]error) int64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %q is not an integer", key, v))
		return fallback
	}
	return n
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

// getSigningKey reads a base64-encoded 32-byte Ed25519 seed. There is deliberately no
// default: a missing key must stop the server instead of signing with a known value.
// Generate one with: openssl rand -base64 32
func getSigningKey(key string, errs *[]error) ed25519.PrivateKey {
	v := os.Getenv(key)
	if v == "" {
		*errs = append(*errs, fmt.Errorf("%s is required (generate with: openssl rand -base64 32)", key))
		return nil
	}
	seed, err := base64.StdEncoding.DecodeString(v)
	if err != nil || len(seed) != ed25519.SeedSize {
		*errs = append(*errs, fmt.Errorf("%s must be %d bytes encoded as base64", key, ed25519.SeedSize))
		return nil
	}
	return ed25519.NewKeyFromSeed(seed)
}
