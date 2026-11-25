package domain

import (
	"slices"
	"time"
)

// ExternalProvider is an AI provider that runs outside Enclave. Sending it a question or a
// document excerpt means the workspace's content leaves the system, so only an admin may
// allow it. Providers that run inside Enclave (a local Ollama) need no approval.
type ExternalProvider string

const (
	ProviderAnthropic ExternalProvider = "anthropic"
	ProviderNVIDIA    ExternalProvider = "nvidia"
	ProviderOpenAI    ExternalProvider = "openai"
)

// ExternalProviders lists every external provider an admin can allow, sorted by name.
func ExternalProviders() []ExternalProvider {
	return []ExternalProvider{ProviderAnthropic, ProviderNVIDIA, ProviderOpenAI}
}

func ParseExternalProvider(s string) (ExternalProvider, bool) {
	provider := ExternalProvider(s)
	return provider, slices.Contains(ExternalProviders(), provider)
}

// AISettings is a workspace's AI policy. The LLM Gateway reads it on every call: it never
// sends the workspace's content to an external provider that is not listed here.
type AISettings struct {
	// ExternalProviders is sorted and has no duplicates. Empty means local providers only.
	ExternalProviders []ExternalProvider
	// UpdatedAt is zero while the workspace still uses the default (no external providers).
	UpdatedAt time.Time
}

// CanManageAISettings: choosing where content may be sent is an admin decision.
func (r Role) CanManageAISettings() bool {
	return r == RoleAdmin
}

// CanViewAISettings: staff see the policy so they know which models their questions reach.
func (r Role) CanViewAISettings() bool {
	return r == RoleAdmin || r == RoleStaff
}
