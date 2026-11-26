// Package transport exposes the gateway over HTTP in OpenAI's API format, so Python services
// can use the standard openai client library by pointing it at the gateway:
//
//	POST /v1/chat/completions   chat, streamed as Server-Sent Events or answered at once
//	POST /v1/embeddings         embeddings, as float lists or base64
//	GET  /v1/models             the aliases the gateway serves
//
// Every request needs a service token (Authorization: Bearer ...) and the workspace it is
// made for (X-Enclave-Workspace-ID).
package transport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"enclave/gateway/internal/llm/app"
	"enclave/gateway/internal/llm/domain"
	"enclave/internal/platform/httpserver"
)

const WorkspaceHeader = "X-Enclave-Workspace-ID"

// Headers that tell the caller which provider and provider model actually answered.
const (
	providerHeader      = "X-Enclave-Provider"
	providerModelHeader = "X-Enclave-Provider-Model"
)

type Handler struct {
	service *app.Service
	logger  *slog.Logger
}

func NewHandler(service *app.Service, logger *slog.Logger) *Handler {
	return &Handler{service: service, logger: logger}
}

// RegisterRoutes adds the API routes, each wrapped in requireService.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, requireService httpserver.Middleware) {
	mux.Handle("POST /v1/chat/completions", requireService(http.HandlerFunc(h.chatCompletions)))
	mux.Handle("POST /v1/embeddings", requireService(http.HandlerFunc(h.embeddings)))
	mux.Handle("GET /v1/models", requireService(http.HandlerFunc(h.models)))
}

// call reads who is asking and for which workspace.
func call(w http.ResponseWriter, r *http.Request) (domain.Call, bool) {
	workspaceID, err := uuid.Parse(r.Header.Get(WorkspaceHeader))
	if err != nil {
		httpserver.WriteError(w, r, http.StatusBadRequest, "workspace_required", WorkspaceHeader+" must be a workspace ID")
		return domain.Call{}, false
	}
	return domain.Call{
		Caller:      callerFrom(r.Context()),
		WorkspaceID: workspaceID,
		RequestID:   httpserver.RequestIDFrom(r.Context()),
	}, true
}

type chatCompletionRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Stream        bool `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	MaxTokens *int `json:"max_tokens"`
	// MaxCompletionTokens is the newer name OpenAI's clients send; it wins when both are set.
	MaxCompletionTokens *int     `json:"max_completion_tokens"`
	Temperature         *float64 `json:"temperature"`
	TopP                *float64 `json:"top_p"`
	// ResponseFormat {"type": "json_object"} asks for JSON output; "text" is the default.
	ResponseFormat *struct {
		Type string `json:"type"`
	} `json:"response_format"`
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	call, ok := call(w, r)
	if !ok {
		return
	}
	var body chatCompletionRequest
	if err := httpserver.ReadJSON(w, r, &body); err != nil {
		httpserver.WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	req := domain.ChatRequest{Model: body.Model, MaxTokens: body.MaxTokens, Temperature: body.Temperature, TopP: body.TopP}
	if body.ResponseFormat != nil {
		switch body.ResponseFormat.Type {
		case "text":
		case "json_object":
			req.JSONOutput = true
		default:
			httpserver.WriteError(w, r, http.StatusBadRequest, "invalid_request", "response_format.type must be text or json_object")
			return
		}
	}
	if body.MaxCompletionTokens != nil {
		req.MaxTokens = body.MaxCompletionTokens
	}
	for _, message := range body.Messages {
		req.Messages = append(req.Messages, domain.Message{Role: domain.Role(message.Role), Content: message.Content})
	}
	completion := newCompletion(call.RequestID, body.Model)

	if !body.Stream {
		out := &collectOutput{}
		if err := h.service.Chat(r.Context(), call, req, out); err != nil {
			h.writeError(w, r, err)
			return
		}
		setProviderHeaders(w, out.route)
		httpserver.WriteJSON(w, http.StatusOK, completion.message(out.text, out.finish, out.usage))
		return
	}

	out := &sseOutput{w: w, controller: http.NewResponseController(w), completion: completion}
	out.includeUsage = body.StreamOptions != nil && body.StreamOptions.IncludeUsage
	err := h.service.Chat(r.Context(), call, req, out)
	switch {
	case err == nil:
		out.finish()
	case !out.began:
		h.writeError(w, r, err) // nothing sent yet: a normal JSON error with a status code
	case r.Context().Err() != nil:
		// The caller hung up; there is nobody to tell.
	default:
		// Headers and part of the answer are already out, so the status code can no longer
		// change. Report the failure inside the stream and leave out [DONE], so the client
		// knows the answer is incomplete.
		out.fail(err)
	}
}

// collectOutput gathers a whole answer for a non-streaming response.
type collectOutput struct {
	route  domain.Route
	text   []byte
	finish domain.FinishReason
	usage  *domain.Usage
}

func (o *collectOutput) Begin(route domain.Route) error {
	o.route = route
	return nil
}

func (o *collectOutput) Write(chunk domain.Chunk) error {
	o.text = append(o.text, chunk.Content...)
	if chunk.FinishReason != "" {
		o.finish = chunk.FinishReason
	}
	if chunk.Usage != nil {
		o.usage = chunk.Usage
	}
	return nil
}

type embeddingsRequest struct {
	Model     string        `json:"model"`
	Input     stringOrSlice `json:"input"`
	InputType string        `json:"input_type"`
	// EncodingFormat is "float" or "base64". OpenAI's Python client asks for base64 by
	// default (smaller responses) and decodes it itself.
	EncodingFormat string `json:"encoding_format"`
}

func (h *Handler) embeddings(w http.ResponseWriter, r *http.Request) {
	call, ok := call(w, r)
	if !ok {
		return
	}
	var body embeddingsRequest
	if err := httpserver.ReadJSON(w, r, &body); err != nil {
		httpserver.WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if body.EncodingFormat != "" && body.EncodingFormat != "float" && body.EncodingFormat != "base64" {
		httpserver.WriteError(w, r, http.StatusBadRequest, "invalid_request", "encoding_format must be float or base64")
		return
	}

	req := domain.EmbeddingRequest{Model: body.Model, Inputs: body.Input, InputType: domain.InputType(body.InputType)}
	result, route, err := h.service.Embed(r.Context(), call, req)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	setProviderHeaders(w, route)
	httpserver.WriteJSON(w, http.StatusOK, newEmbeddingsResponse(body.Model, result, body.EncodingFormat == "base64"))
}

func (h *Handler) models(w http.ResponseWriter, _ *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
		Kind    string `json:"kind"`
	}
	var data []model
	for _, alias := range h.service.Catalog().Aliases() {
		data = append(data, model{ID: alias.Name, Object: "model", OwnedBy: "enclave", Kind: string(alias.Kind)})
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func setProviderHeaders(w http.ResponseWriter, route domain.Route) {
	w.Header().Set(providerHeader, route.Provider.Name)
	w.Header().Set(providerModelHeader, route.Model)
}

// writeError maps domain errors to status codes. Provider error details stay in the logs;
// callers get a stable code.
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		rateLimited *domain.RateLimitError
		providerErr *domain.ProviderError
	)
	switch {
	// Ask the request's context whether the caller left. The error alone cannot tell: a
	// first-token timeout also wraps context.Canceled, because the timer cancels the attempt.
	case r.Context().Err() != nil:
		return
	case errors.Is(err, domain.ErrInvalidRequest):
		httpserver.WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, domain.ErrUnknownModel):
		httpserver.WriteError(w, r, http.StatusNotFound, "model_not_found", err.Error())
	case errors.Is(err, domain.ErrWorkspaceNotFound):
		httpserver.WriteError(w, r, http.StatusNotFound, "workspace_not_found", "workspace not found")
	case errors.Is(err, domain.ErrNoAllowedProvider):
		httpserver.WriteError(w, r, http.StatusForbidden, "provider_not_allowed", err.Error())
	case errors.As(err, &rateLimited):
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(max(rateLimited.RetryAfter, time.Second).Seconds()))))
		httpserver.WriteError(w, r, http.StatusTooManyRequests, "rate_limited", "workspace rate limit exceeded")
	case errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, domain.ErrAllProvidersFailed):
		httpserver.WriteError(w, r, http.StatusGatewayTimeout, "timeout", "the model did not answer in time")
	case errors.As(err, &providerErr) || errors.Is(err, domain.ErrAllProvidersFailed):
		h.logger.WarnContext(r.Context(), "model request failed",
			slog.String("request_id", httpserver.RequestIDFrom(r.Context())), slog.Any("err", err))
		httpserver.WriteError(w, r, http.StatusBadGateway, "provider_error", providerMessage(err))
	default:
		h.logger.ErrorContext(r.Context(), "gateway request failed",
			slog.String("request_id", httpserver.RequestIDFrom(r.Context())), slog.Any("err", err))
		httpserver.WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}

// providerMessage says which provider failed and how, without its raw error text.
func providerMessage(err error) string {
	var providerErr *domain.ProviderError
	switch {
	case errors.Is(err, domain.ErrAllProvidersFailed):
		return "every allowed provider failed"
	case errors.As(err, &providerErr) && providerErr.StatusCode != 0:
		return fmt.Sprintf("provider %s answered with status %d", providerErr.Provider, providerErr.StatusCode)
	case errors.As(err, &providerErr):
		return fmt.Sprintf("provider %s could not be reached", providerErr.Provider)
	default:
		return "provider error"
	}
}
