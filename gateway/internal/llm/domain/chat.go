// Package domain holds the gateway's provider-neutral model of a model call: chat requests
// and answer chunks, embeddings, the model catalog with routes and prices, a workspace's
// provider policy, and the errors every layer agrees on. Provider adapters translate their
// own wire formats into these types; nothing here knows any provider's format.
package domain

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Call identifies who is asking: the internal service that called the gateway and the
// workspace the request is made for. Policy, rate limits and accounting all key on it.
type Call struct {
	Caller      string
	WorkspaceID uuid.UUID
	RequestID   string
}

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type Message struct {
	Role    Role
	Content string
}

const (
	maxMessages  = 200
	maxMaxTokens = 128_000
)

// ChatRequest is a chat call in provider-neutral form. Model is a catalog alias such as
// "chat-fast", never a provider's own model name.
type ChatRequest struct {
	Model    string
	Messages []Message
	// MaxTokens caps the answer length; nil lets the provider (or adapter) choose.
	MaxTokens *int
	// Temperature is passed to providers that accept it; nil uses the provider default.
	Temperature *float64
}

// Validate checks what every provider would reject anyway, so a bad request fails fast with
// a clear message instead of failing once per fallback route.
func (r ChatRequest) Validate() error {
	if r.Model == "" {
		return invalid("model is required")
	}
	if len(r.Messages) == 0 || len(r.Messages) > maxMessages {
		return invalid(fmt.Sprintf("messages must contain 1-%d entries", maxMessages))
	}
	hasUserMessage := false
	for i, message := range r.Messages {
		switch message.Role {
		case RoleSystem, RoleAssistant:
		case RoleUser:
			hasUserMessage = true
		default:
			return invalid(fmt.Sprintf("messages[%d].role must be system, user or assistant", i))
		}
		if !utf8.ValidString(message.Content) {
			return invalid(fmt.Sprintf("messages[%d].content must be valid UTF-8", i))
		}
	}
	if !hasUserMessage {
		return invalid("messages must include at least one user message")
	}
	if r.MaxTokens != nil && (*r.MaxTokens < 1 || *r.MaxTokens > maxMaxTokens) {
		return invalid(fmt.Sprintf("max_tokens must be 1-%d", maxMaxTokens))
	}
	if r.Temperature != nil && (*r.Temperature < 0 || *r.Temperature > 2) {
		return invalid("temperature must be between 0 and 2")
	}
	return nil
}

// FinishReason uses OpenAI's vocabulary, which the gateway's API speaks.
type FinishReason string

const (
	FinishStop          FinishReason = "stop"
	FinishLength        FinishReason = "length"
	FinishContentFilter FinishReason = "content_filter"
)

// Chunk is one piece of a streamed answer. Providers report usage in different places (a
// final chunk, the first and last events, or not at all), so any chunk may carry it.
type Chunk struct {
	Content      string
	FinishReason FinishReason
	Usage        *Usage
}

type Usage struct {
	InputTokens  int
	OutputTokens int
}

var (
	ErrInvalidRequest = errors.New("invalid request")
	// ErrUnknownModel: the alias is not in the catalog, or not of the requested kind.
	ErrUnknownModel = errors.New("unknown model")
	// ErrWorkspaceNotFound: the workspace ID in the request does not exist.
	ErrWorkspaceNotFound = errors.New("workspace not found")
	// ErrNoAllowedProvider: every route of the alias goes to a provider that is either not
	// configured or not allowed by the workspace's AI settings.
	ErrNoAllowedProvider = errors.New("no provider allowed for this model")
	// ErrAllProvidersFailed: every allowed route was tried and failed before any output.
	ErrAllProvidersFailed = errors.New("all providers failed")
)

func invalid(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidRequest, message)
}
