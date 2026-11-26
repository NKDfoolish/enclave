// Package openai is the adapter for providers that speak OpenAI's chat-completions and
// embeddings wire format: OpenAI itself, NVIDIA NIM, Ollama, vLLM and others. It uses plain
// net/http. The providers differ in small ways (extra fields, where usage is reported, extra
// embedding parameters); those differences are handled here and nowhere else.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"enclave/gateway/internal/llm/app"
	"enclave/gateway/internal/llm/domain"
)

// maxErrorBody bounds how much of an error response is read into an error message.
const maxErrorBody = 4 << 10

type Options struct {
	// Provider is the catalog name ("ollama", "nvidia"), used in errors and logs.
	Provider string
	// BaseURL ends before the endpoint, e.g. "https://integrate.api.nvidia.com/v1".
	BaseURL string
	// APIKey is sent as a bearer token; empty for providers without auth (local Ollama).
	APIKey     string
	HTTPClient *http.Client
	// SendInputType adds "input_type" (query or passage) to embedding requests. NVIDIA's
	// asymmetric embedding models need it; OpenAI rejects unknown parameters.
	SendInputType bool
}

type Client struct {
	provider      string
	baseURL       string
	apiKey        string
	httpClient    *http.Client
	sendInputType bool
}

func New(opts Options) *Client {
	return &Client{
		provider:      opts.Provider,
		baseURL:       strings.TrimSuffix(opts.BaseURL, "/"),
		apiKey:        opts.APIKey,
		httpClient:    opts.HTTPClient,
		sendInputType: opts.SendInputType,
	}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	// IncludeUsage asks for a final chunk with token counts. Without it, OpenAI does not
	// report usage at all when streaming.
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	MaxTokens      *int            `json:"max_tokens,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	TopP           *float64        `json:"top_p,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type responseFormat struct {
	Type string `json:"type"`
}

// StreamChat always streams, even when the caller wants the whole answer at once: one code
// path, and the gateway can stop the provider as soon as the caller goes away.
func (c *Client) StreamChat(ctx context.Context, model string, req domain.ChatRequest) (app.ChatStream, error) {
	body := chatRequest{
		Model:       model,
		Messages:    make([]chatMessage, len(req.Messages)),
		Stream:      true,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}
	if req.JSONOutput {
		body.ResponseFormat = &responseFormat{Type: "json_object"}
	}
	body.StreamOptions.IncludeUsage = true
	for i, message := range req.Messages {
		body.Messages[i] = chatMessage{Role: string(message.Role), Content: message.Content}
	}

	resp, err := c.post(ctx, "/chat/completions", body, "text/event-stream") //nolint:bodyclose // the stream owns the body; Close closes it
	if err != nil {
		return nil, err
	}
	return newChatStream(c.provider, resp.Body), nil
}

type embeddingRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
	InputType      string   `json:"input_type,omitempty"`
}

type embeddingResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
	} `json:"usage"`
}

func (c *Client) Embed(ctx context.Context, model string, req domain.EmbeddingRequest) (domain.Embeddings, error) {
	body := embeddingRequest{Model: model, Input: req.Inputs, EncodingFormat: "float"}
	if c.sendInputType {
		body.InputType = string(req.InputType)
		if body.InputType == "" {
			body.InputType = string(domain.InputPassage)
		}
	}

	resp, err := c.post(ctx, "/embeddings", body, "application/json")
	if err != nil {
		return domain.Embeddings{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	var decoded embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return domain.Embeddings{}, domain.NewTransportError(c.provider, fmt.Errorf("decode embeddings: %w", err))
	}
	// Providers may return the vectors in any order; index says which input each belongs to.
	vectors := make([][]float32, len(decoded.Data))
	for _, item := range decoded.Data {
		if item.Index < 0 || item.Index >= len(vectors) || vectors[item.Index] != nil {
			return domain.Embeddings{}, domain.NewTransportError(c.provider, fmt.Errorf("embedding index %d out of range or repeated", item.Index))
		}
		vectors[item.Index] = item.Embedding
	}
	return domain.Embeddings{Vectors: vectors, InputTokens: decoded.Usage.PromptTokens}, nil
}

// post sends body as JSON and returns the response if the provider accepted it. Any other
// outcome becomes a classified *domain.ProviderError.
func (c *Client) post(ctx context.Context, path string, body any, accept string) (*http.Response, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", path, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", path, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", accept)
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, domain.NewTransportError(c.provider, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer func() { _ = resp.Body.Close() }()
		return nil, domain.NewStatusError(c.provider, resp.StatusCode, errors.New(errorMessage(resp.Body)))
	}
	return resp, nil
}

// errorMessage extracts {"error": {"message": ...}} (or {"error": "..."}, as Ollama sends)
// from an error body, falling back to the start of the raw body.
func errorMessage(body io.Reader) string {
	raw, _ := io.ReadAll(io.LimitReader(body, maxErrorBody))
	var structured struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &structured) == nil && len(structured.Error) > 0 {
		var detail struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(structured.Error, &detail) == nil && detail.Message != "" {
			return detail.Message
		}
		var text string
		if json.Unmarshal(structured.Error, &text) == nil && text != "" {
			return text
		}
	}
	message := strings.TrimSpace(string(raw))
	if len(message) > 200 {
		message = message[:200] + "..."
	}
	if message == "" {
		return "empty error response"
	}
	return message
}
