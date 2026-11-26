package domain

import (
	"errors"
	"fmt"
	"slices"
)

// Provider describes where a route sends its requests.
type Provider struct {
	Name string
	// External providers run outside Enclave; content reaches them only if the workspace's
	// AI settings allow it.
	External bool
}

// Price is what a provider charges, in millionths of a US dollar per million tokens.
// Integers keep cost sums exact.
type Price struct {
	InputMicroUSDPerMTok  int64
	OutputMicroUSDPerMTok int64
}

// Cost returns the cost of usage in millionths of a US dollar.
func (p Price) Cost(usage Usage) int64 {
	input := int64(usage.InputTokens) * p.InputMicroUSDPerMTok
	output := int64(usage.OutputTokens) * p.OutputMicroUSDPerMTok
	return (input + output) / 1_000_000
}

// Route is one way to serve an alias: a provider and that provider's own model name.
type Route struct {
	Provider Provider
	Model    string
	Price    Price
	// InputPrefixes are prepended to embedding inputs by input type. Some open embedding models
	// (nomic-embed-text: "search_query: ", "search_document: ") are trained to expect them and
	// retrieve worse without; providers do not add them. Empty for models that need none.
	InputPrefixes map[InputType]string
}

// PrefixInputs returns inputs with the route's prefix for inputType, or inputs unchanged.
func (r Route) PrefixInputs(inputs []string, inputType InputType) []string {
	prefix := r.InputPrefixes[inputType]
	if prefix == "" {
		return inputs
	}
	prefixed := make([]string, len(inputs))
	for i, input := range inputs {
		prefixed[i] = prefix + input
	}
	return prefixed
}

type ModelKind string

const (
	KindChat      ModelKind = "chat"
	KindEmbedding ModelKind = "embedding"
)

// Alias is the stable name callers use ("chat-fast"). Its routes are tried in order; later
// routes are fallbacks. Callers never name provider models directly, so a provider retiring
// a model is a one-line catalog change instead of a change in every service.
type Alias struct {
	Name   string
	Kind   ModelKind
	Routes []Route
}

// Catalog is the set of aliases the gateway serves.
type Catalog struct {
	aliases []Alias
}

// NewCatalog validates the aliases. An embedding alias must have exactly one route: vectors
// from two different models live in different spaces and cannot be compared, so falling
// back to another embedding model would silently corrupt search.
func NewCatalog(aliases []Alias) (Catalog, error) {
	var errs []error
	seen := map[string]bool{}
	for _, alias := range aliases {
		if alias.Name == "" {
			errs = append(errs, errors.New("alias with an empty name"))
			continue
		}
		if seen[alias.Name] {
			errs = append(errs, fmt.Errorf("alias %q is defined twice", alias.Name))
		}
		seen[alias.Name] = true
		if alias.Kind != KindChat && alias.Kind != KindEmbedding {
			errs = append(errs, fmt.Errorf("alias %q: kind must be chat or embedding", alias.Name))
		}
		if len(alias.Routes) == 0 {
			errs = append(errs, fmt.Errorf("alias %q has no routes", alias.Name))
		}
		if alias.Kind == KindEmbedding && len(alias.Routes) > 1 {
			errs = append(errs, fmt.Errorf("alias %q: an embedding alias must have exactly one route", alias.Name))
		}
		for _, route := range alias.Routes {
			if route.Provider.Name == "" || route.Model == "" {
				errs = append(errs, fmt.Errorf("alias %q: every route needs a provider and a model", alias.Name))
			}
			if route.Price.InputMicroUSDPerMTok < 0 || route.Price.OutputMicroUSDPerMTok < 0 {
				errs = append(errs, fmt.Errorf("alias %q: prices cannot be negative", alias.Name))
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Catalog{}, err
	}
	return Catalog{aliases: slices.Clone(aliases)}, nil
}

// Alias returns the alias with this name and kind, or ErrUnknownModel.
func (c Catalog) Alias(name string, kind ModelKind) (Alias, error) {
	for _, alias := range c.aliases {
		if alias.Name == name && alias.Kind == kind {
			return alias, nil
		}
	}
	return Alias{}, fmt.Errorf("%w: %q is not a %s model", ErrUnknownModel, name, kind)
}

// Aliases returns every alias in catalog order.
func (c Catalog) Aliases() []Alias {
	return slices.Clone(c.aliases)
}

// Policy is a workspace's AI settings as the gateway enforces them.
type Policy struct {
	allowedExternal []string
}

func NewPolicy(allowedExternal []string) Policy {
	return Policy{allowedExternal: slices.Clone(allowedExternal)}
}

// Allows reports whether content may be sent to provider. Providers inside Enclave are
// always allowed; external ones only when the workspace admin listed them.
func (p Policy) Allows(provider Provider) bool {
	return !provider.External || slices.Contains(p.allowedExternal, provider.Name)
}
