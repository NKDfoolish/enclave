// Package anthropic is the adapter for Anthropic's Messages API, using the official Go SDK.
// It is the gateway's example of a provider whose format really differs from OpenAI's:
//
//   - system instructions are a separate field, not a message;
//   - max_tokens is required;
//   - sampling parameters (temperature, top_p) are rejected by current models, so they are
//     not forwarded;
//   - there is no JSON response format, so JSON output is asked for in the system prompt;
//   - the stream is a sequence of typed events (message_start, content_block_delta,
//     message_delta, ...), and token usage arrives at the start and the end of it.
package anthropic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/anthropics/anthropic-sdk-go/v2/packages/ssestream"

	"enclave/gateway/internal/llm/app"
	"enclave/gateway/internal/llm/domain"
)

// defaultMaxTokens is used when the caller does not set max_tokens, which Anthropic requires.
// It is a ceiling, not a target: a short answer still costs only what it uses.
const defaultMaxTokens = 16_000

// jsonInstruction stands in for OpenAI's json_object response format.
const jsonInstruction = "Respond with a single JSON object and nothing else: no prose, no code fences."

type Options struct {
	Provider   string
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

type Client struct {
	provider string
	client   sdk.Client
}

func New(opts Options) *Client {
	client := sdk.NewClient(
		// Configuration comes from the gateway's catalog only, never from ANTHROPIC_*
		// environment variables that happen to be set on the machine.
		option.WithoutEnvironmentDefaults(),
		option.WithAPIKey(opts.APIKey),
		option.WithBaseURL(opts.BaseURL),
		option.WithHTTPClient(opts.HTTPClient),
		// The gateway decides about retries: it falls back to another route instead.
		option.WithMaxRetries(0),
	)
	return &Client{provider: opts.Provider, client: client}
}

func (c *Client) StreamChat(ctx context.Context, model string, req domain.ChatRequest) (app.ChatStream, error) {
	params := sdk.MessageNewParams{
		Model:     model,
		MaxTokens: defaultMaxTokens,
	}
	if req.MaxTokens != nil {
		params.MaxTokens = int64(*req.MaxTokens)
	}

	var system []string
	for _, message := range req.Messages {
		switch message.Role {
		case domain.RoleSystem:
			system = append(system, message.Content)
		case domain.RoleUser:
			params.Messages = append(params.Messages, sdk.NewUserMessage(sdk.NewTextBlock(message.Content)))
		case domain.RoleAssistant:
			params.Messages = append(params.Messages, sdk.NewAssistantMessage(sdk.NewTextBlock(message.Content)))
		}
	}
	if req.JSONOutput {
		system = append(system, jsonInstruction)
	}
	if len(system) > 0 {
		params.System = []sdk.TextBlockParam{{Text: strings.Join(system, "\n\n")}}
	}

	// The SDK sends the request here; an HTTP error surfaces on the first Next, which the
	// gateway treats like any failure before output (so it can still fall back).
	stream := c.client.Messages.NewStreaming(ctx, params)
	return &chatStream{provider: c.provider, stream: stream}, nil
}

type chatStream struct {
	provider    string
	stream      *ssestream.Stream[sdk.MessageStreamEventUnion]
	inputTokens int
}

// Next turns Anthropic's typed events into chunks. Events without text (message_start,
// thinking deltas, pings) still return an empty chunk: they show the provider is alive.
func (s *chatStream) Next() (domain.Chunk, error) {
	for s.stream.Next() {
		switch event := s.stream.Current().AsAny().(type) {
		case sdk.MessageStartEvent:
			usage := event.Message.Usage
			s.inputTokens = int(usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens)
			return domain.Chunk{}, nil
		case sdk.ContentBlockDeltaEvent:
			if text, ok := event.Delta.AsAny().(sdk.TextDelta); ok {
				return domain.Chunk{Content: text.Text}, nil
			}
			return domain.Chunk{}, nil // thinking or other non-text deltas
		case sdk.MessageDeltaEvent:
			// Usage here is cumulative. Input tokens were counted at message_start.
			inputTokens := int(event.Usage.InputTokens + event.Usage.CacheCreationInputTokens + event.Usage.CacheReadInputTokens)
			return domain.Chunk{
				FinishReason: finishReason(event.Delta.StopReason),
				Usage:        &domain.Usage{InputTokens: max(inputTokens, s.inputTokens), OutputTokens: int(event.Usage.OutputTokens)},
			}, nil
		}
	}
	if err := s.stream.Err(); err != nil {
		return domain.Chunk{}, classify(s.provider, err)
	}
	return domain.Chunk{}, io.EOF
}

func (s *chatStream) Close() error {
	return s.stream.Close()
}

// finishReason maps Anthropic's stop reasons onto the OpenAI vocabulary the gateway speaks.
func finishReason(reason sdk.StopReason) domain.FinishReason {
	switch reason {
	case sdk.StopReasonMaxTokens, sdk.StopReasonModelContextWindowExceeded:
		return domain.FinishLength
	case sdk.StopReasonRefusal:
		// The model declined; callers see it as OpenAI's content_filter.
		return domain.FinishContentFilter
	default:
		return domain.FinishStop
	}
}

// classify turns SDK errors into gateway provider errors. The SDK reports a non-success
// status as *sdk.Error; anything else means no answer arrived at all.
func classify(provider string, err error) error {
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode != 0 {
		return domain.NewStatusError(provider, apiErr.StatusCode, fmt.Errorf("%s (request id %s)", http.StatusText(apiErr.StatusCode), apiErr.RequestID))
	}
	return domain.NewTransportError(provider, err)
}
