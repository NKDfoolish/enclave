// Command gateway runs the Enclave LLM Gateway: the one service that talks to model
// providers. Other services (RAG, agent) send it OpenAI-style requests with a model alias;
// it applies the workspace's AI settings and rate limit, routes to a provider with
// fallback, streams the answer back and records the cost.
//
// This package is the composition root: it loads config, builds the provider adapters and
// wires everything together.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"enclave/gateway/internal/config"
	"enclave/gateway/internal/llm/app"
	"enclave/gateway/internal/llm/infra"
	"enclave/gateway/internal/llm/infra/anthropic"
	"enclave/gateway/internal/llm/infra/openai"
	"enclave/internal/platform/httpserver"
	"enclave/internal/platform/postgres"
	"enclave/internal/platform/redis"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("gateway exited", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	catalog, err := config.LoadCatalog(cfg.CatalogFile)
	if err != nil {
		return err
	}

	pool, err := postgres.Connect(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	redisClient, err := redis.Connect(context.Background(), cfg.RedisURL)
	if err != nil {
		return err
	}
	defer func() {
		if err := redisClient.Close(); err != nil {
			logger.Warn("close redis client", "err", err)
		}
	}()

	chatProviders, embedders := buildProviders(catalog.Providers, newProviderHTTPClient(), logger)
	service := app.NewService(app.Options{
		Catalog:           catalog.Models,
		ChatProviders:     chatProviders,
		Embedders:         embedders,
		Policies:          infra.NewPolicies(pool),
		Limiter:           infra.NewRateLimiter(redisClient, cfg.RateLimitPerMinute, cfg.RateLimitBurst),
		Usage:             infra.NewUsageRecorder(pool),
		Logger:            logger,
		FirstTokenTimeout: cfg.FirstTokenTimeout,
		RequestTimeout:    cfg.RequestTimeout,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, a second Ctrl+C kills the process instead of waiting.
	go func() {
		<-ctx.Done()
		stop()
	}()

	return httpserver.Run(ctx, logger, cfg.HTTPAddr, routes(cfg, logger, service), cfg.ShutdownTimeout)
}

// buildProviders creates one adapter per configured provider. A provider whose API key is
// not set is left out with a log line: its routes are skipped, and aliases fall back to the
// providers that are available.
func buildProviders(providers []config.ProviderConfig, httpClient *http.Client, logger *slog.Logger) (map[string]app.ChatProvider, map[string]app.Embedder) {
	chatProviders := map[string]app.ChatProvider{}
	embedders := map[string]app.Embedder{}
	for _, p := range providers {
		apiKey := ""
		if p.APIKeyEnv != "" {
			apiKey = os.Getenv(p.APIKeyEnv)
			if apiKey == "" {
				logger.Warn("provider disabled: API key not set", "provider", p.Name, "env", p.APIKeyEnv)
				continue
			}
		}

		switch p.Kind {
		case config.KindOpenAI:
			client := openai.New(openai.Options{
				Provider: p.Name, BaseURL: p.BaseURL, APIKey: apiKey,
				HTTPClient: httpClient, SendInputType: p.SendInputType,
			})
			chatProviders[p.Name] = client
			embedders[p.Name] = client
		case config.KindAnthropic:
			chatProviders[p.Name] = anthropic.New(anthropic.Options{
				Provider: p.Name, BaseURL: p.BaseURL, APIKey: apiKey, HTTPClient: httpClient,
			})
		}
		logger.Info("provider enabled", "provider", p.Name, "kind", p.Kind, "external", p.External)
	}
	return chatProviders, embedders
}

// newProviderHTTPClient is shared by every adapter. It has no overall timeout because
// answers stream for as long as they need; contexts bound each call instead. It keeps more
// idle connections per provider than the default (2), since the gateway sends many
// concurrent requests to the same few hosts.
func newProviderHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 64
	return &http.Client{Transport: transport}
}
