package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"

	"enclave/gateway/internal/llm/domain"
)

// Provider kinds: which adapter talks to the provider.
const (
	KindOpenAI    = "openai"    // OpenAI's wire format: OpenAI, NVIDIA NIM, Ollama, vLLM
	KindAnthropic = "anthropic" // Anthropic's Messages API
)

// ProviderConfig is one provider entry in the catalog file. It holds no secret: APIKeyEnv
// names the environment variable that holds the key.
type ProviderConfig struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	BaseURL   string `json:"base_url"`
	APIKeyEnv string `json:"api_key_env"`
	// External providers run outside Enclave and need the workspace admin's approval.
	External      bool `json:"external"`
	SendInputType bool `json:"send_input_type"`
}

type routeConfig struct {
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	InputUSDPerMTok  float64 `json:"input_usd_per_mtok"`
	OutputUSDPerMTok float64 `json:"output_usd_per_mtok"`
}

type aliasConfig struct {
	Alias  string        `json:"alias"`
	Kind   string        `json:"kind"`
	Routes []routeConfig `json:"routes"`
}

type catalogFile struct {
	Providers []ProviderConfig `json:"providers"`
	Models    []aliasConfig    `json:"models"`
}

// Catalog is the parsed catalog file: how to reach each provider, and the aliases.
type Catalog struct {
	Providers []ProviderConfig
	Models    domain.Catalog
}

// LoadCatalog reads and validates the catalog file. Unknown fields are errors, so a typo in
// the file fails startup instead of being silently ignored.
func LoadCatalog(path string) (Catalog, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the path comes from the operator's configuration
	if err != nil {
		return Catalog{}, fmt.Errorf("read catalog: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var file catalogFile
	if err := decoder.Decode(&file); err != nil {
		return Catalog{}, fmt.Errorf("parse catalog %s: %w", path, err)
	}
	return newCatalog(file)
}

func newCatalog(file catalogFile) (Catalog, error) {
	var errs []error
	providers := map[string]domain.Provider{}
	for _, p := range file.Providers {
		switch {
		case p.Name == "":
			errs = append(errs, errors.New("a provider has no name"))
		case p.Kind != KindOpenAI && p.Kind != KindAnthropic:
			errs = append(errs, fmt.Errorf("provider %q: kind must be %s or %s", p.Name, KindOpenAI, KindAnthropic))
		case p.BaseURL == "":
			errs = append(errs, fmt.Errorf("provider %q: base_url is required", p.Name))
		}
		if _, duplicate := providers[p.Name]; duplicate {
			errs = append(errs, fmt.Errorf("provider %q is defined twice", p.Name))
		}
		providers[p.Name] = domain.Provider{Name: p.Name, External: p.External}
	}

	var aliases []domain.Alias
	for _, model := range file.Models {
		alias := domain.Alias{Name: model.Alias, Kind: domain.ModelKind(model.Kind)}
		for _, route := range model.Routes {
			provider, known := providers[route.Provider]
			if !known {
				errs = append(errs, fmt.Errorf("alias %q: unknown provider %q", model.Alias, route.Provider))
				continue
			}
			if model.Kind == string(domain.KindEmbedding) && !canEmbed(file.Providers, route.Provider) {
				errs = append(errs, fmt.Errorf("alias %q: provider %q has no embeddings API", model.Alias, route.Provider))
			}
			alias.Routes = append(alias.Routes, domain.Route{
				Provider: provider,
				Model:    route.Model,
				Price: domain.Price{
					InputMicroUSDPerMTok:  microUSD(route.InputUSDPerMTok),
					OutputMicroUSDPerMTok: microUSD(route.OutputUSDPerMTok),
				},
			})
		}
		aliases = append(aliases, alias)
	}

	models, err := domain.NewCatalog(aliases)
	if err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return Catalog{}, fmt.Errorf("invalid catalog: %w", err)
	}
	return Catalog{Providers: file.Providers, Models: models}, nil
}

// canEmbed: only the OpenAI-format adapter implements embeddings (Anthropic has none).
func canEmbed(providers []ProviderConfig, name string) bool {
	return slices.ContainsFunc(providers, func(p ProviderConfig) bool { return p.Name == name && p.Kind == KindOpenAI })
}

// microUSD converts US dollars to integer millionths of a dollar.
func microUSD(usd float64) int64 {
	return int64(math.Round(usd * 1_000_000))
}
