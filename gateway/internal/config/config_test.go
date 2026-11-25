package config

import (
	"strings"
	"testing"
)

func TestRealCatalogLoads(t *testing.T) {
	catalog, err := LoadCatalog("../../catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models.Aliases()) == 0 || len(catalog.Providers) == 0 {
		t.Fatal("catalog is empty")
	}
}

func TestCatalogValidation(t *testing.T) {
	ollama := ProviderConfig{Name: "ollama", Kind: KindOpenAI, BaseURL: "http://localhost:11434/v1"}
	claude := ProviderConfig{Name: "anthropic", Kind: KindAnthropic, BaseURL: "https://api.anthropic.com", External: true}

	cases := map[string]struct {
		file    catalogFile
		wantErr string
	}{
		"unknown provider in a route": {
			file:    catalogFile{Providers: []ProviderConfig{ollama}, Models: []aliasConfig{{Alias: "a", Kind: "chat", Routes: []routeConfig{{Provider: "nope", Model: "m"}}}}},
			wantErr: `unknown provider "nope"`,
		},
		"embedding through a provider without an embeddings API": {
			file:    catalogFile{Providers: []ProviderConfig{claude}, Models: []aliasConfig{{Alias: "e", Kind: "embedding", Routes: []routeConfig{{Provider: "anthropic", Model: "m"}}}}},
			wantErr: "has no embeddings API",
		},
		"embedding with a fallback": {
			file: catalogFile{Providers: []ProviderConfig{ollama}, Models: []aliasConfig{{Alias: "e", Kind: "embedding", Routes: []routeConfig{
				{Provider: "ollama", Model: "a"}, {Provider: "ollama", Model: "b"},
			}}}},
			wantErr: "exactly one route",
		},
		"unknown provider kind": {
			file:    catalogFile{Providers: []ProviderConfig{{Name: "x", Kind: "grpc", BaseURL: "http://x"}}},
			wantErr: "kind must be",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := newCatalog(tc.file)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestPricesBecomeIntegerMicroDollars(t *testing.T) {
	file := catalogFile{
		Providers: []ProviderConfig{{Name: "anthropic", Kind: KindAnthropic, BaseURL: "https://api.anthropic.com"}},
		Models: []aliasConfig{{Alias: "c", Kind: "chat", Routes: []routeConfig{
			{Provider: "anthropic", Model: "m", InputUSDPerMTok: 4, OutputUSDPerMTok: 0.1},
		}}},
	}
	catalog, err := newCatalog(file)
	if err != nil {
		t.Fatal(err)
	}
	price := catalog.Models.Aliases()[0].Routes[0].Price
	if price.InputMicroUSDPerMTok != 4_000_000 || price.OutputMicroUSDPerMTok != 100_000 {
		t.Fatalf("price = %+v", price)
	}
}

func TestServiceTokens(t *testing.T) {
	setRequired := func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://x")
		t.Setenv("REDIS_URL", "redis://x")
	}

	t.Run("each service gets its own token", func(t *testing.T) {
		setRequired(t)
		t.Setenv("GATEWAY_SERVICE_TOKENS", "rag=rag-token-0123456789, agent=agent-token-0123456789")
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ServiceTokens["rag-token-0123456789"] != "rag" || cfg.ServiceTokens["agent-token-0123456789"] != "agent" {
			t.Fatalf("tokens = %v", cfg.ServiceTokens)
		}
	})

	for name, value := range map[string]string{
		"missing":      "",
		"too short":    "rag=short",
		"no name":      "=rag-token-0123456789",
		"shared token": "rag=same-token-0123456789,agent=same-token-0123456789",
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			setRequired(t)
			t.Setenv("GATEWAY_SERVICE_TOKENS", value)
			if _, err := Load(); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}
