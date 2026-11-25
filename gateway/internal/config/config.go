// Package config loads the LLM Gateway's settings from environment variables and its model
// catalog from a JSON file, and validates both at startup.
package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"enclave/internal/platform/env"
)

// minServiceTokenLength rejects tokens short enough to guess.
const minServiceTokenLength = 16

type Config struct {
	// HTTPAddr is where the gateway listens, e.g. ":8090". Only internal services reach it.
	HTTPAddr        string
	ShutdownTimeout time.Duration
	DatabaseURL     string
	RedisURL        string
	// CatalogFile is the JSON file with providers and model aliases.
	CatalogFile string
	// ServiceTokens maps each accepted bearer token to the name of the service using it.
	ServiceTokens map[string]string
	// RateLimitPerMinute and RateLimitBurst size each workspace's token bucket.
	RateLimitPerMinute int
	RateLimitBurst     int
	// FirstTokenTimeout bounds the wait for a provider's first output before falling back.
	FirstTokenTimeout time.Duration
	// RequestTimeout bounds a whole request, fallbacks included.
	RequestTimeout time.Duration
}

func Load() (Config, error) {
	var r env.Reader

	cfg := Config{
		HTTPAddr:           r.String("GATEWAY_HTTP_ADDR", ":8090"),
		ShutdownTimeout:    r.Duration("GATEWAY_SHUTDOWN_TIMEOUT", 10*time.Second),
		DatabaseURL:        r.Required("DATABASE_URL"),
		RedisURL:           r.Required("REDIS_URL"),
		CatalogFile:        r.String("GATEWAY_CATALOG_FILE", "gateway/catalog.json"),
		ServiceTokens:      serviceTokens(&r, "GATEWAY_SERVICE_TOKENS"),
		RateLimitPerMinute: r.Int("GATEWAY_RATE_LIMIT_PER_MINUTE", 60),
		RateLimitBurst:     r.Int("GATEWAY_RATE_LIMIT_BURST", 20),
		FirstTokenTimeout:  r.Duration("GATEWAY_FIRST_TOKEN_TIMEOUT", 30*time.Second),
		RequestTimeout:     r.Duration("GATEWAY_REQUEST_TIMEOUT", 5*time.Minute),
	}

	if cfg.ShutdownTimeout <= 0 {
		r.Fail(errors.New("GATEWAY_SHUTDOWN_TIMEOUT must be positive"))
	}
	if cfg.RateLimitPerMinute <= 0 || cfg.RateLimitBurst <= 0 {
		r.Fail(errors.New("GATEWAY_RATE_LIMIT_PER_MINUTE and GATEWAY_RATE_LIMIT_BURST must be positive"))
	}
	if cfg.FirstTokenTimeout <= 0 || cfg.RequestTimeout <= cfg.FirstTokenTimeout {
		r.Fail(errors.New("GATEWAY_FIRST_TOKEN_TIMEOUT must be positive and shorter than GATEWAY_REQUEST_TIMEOUT"))
	}

	if err := r.Err(); err != nil {
		return Config{}, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

// serviceTokens parses "rag=<token>,agent=<token>". Each internal service gets its own token,
// so usage is attributed to it and one service's token can be rotated alone.
func serviceTokens(r *env.Reader, key string) map[string]string {
	raw := r.Required(key)
	tokens := map[string]string{}
	for entry := range strings.SplitSeq(raw, ",") {
		caller, token, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok || caller == "" {
			r.Fail(fmt.Errorf("%s must look like name=token,name=token", key))
			return nil
		}
		if len(token) < minServiceTokenLength {
			r.Fail(fmt.Errorf("%s: the token for %q must be at least %d characters", key, caller, minServiceTokenLength))
			return nil
		}
		if _, duplicate := tokens[token]; duplicate {
			r.Fail(fmt.Errorf("%s: two services share a token", key))
			return nil
		}
		tokens[token] = caller
	}
	return tokens
}
