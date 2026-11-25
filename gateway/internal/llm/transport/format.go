package transport

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"enclave/gateway/internal/llm/domain"
)

// This file holds the OpenAI wire format the gateway answers in.

type usageJSON struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func newUsageJSON(usage *domain.Usage) *usageJSON {
	if usage == nil {
		return nil
	}
	return &usageJSON{
		PromptTokens:     usage.InputTokens,
		CompletionTokens: usage.OutputTokens,
		TotalTokens:      usage.InputTokens + usage.OutputTokens,
	}
}

// completion carries the fields shared by every chunk and response of one answer. The
// model field echoes the alias the caller asked for; the provider model is in a header.
type completion struct {
	id      string
	created int64
	model   string
}

func newCompletion(requestID, model string) completion {
	return completion{id: "chatcmpl-" + requestID, created: time.Now().Unix(), model: model}
}

type messageJSON struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type completionChoice struct {
	Index        int         `json:"index"`
	Message      messageJSON `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type completionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   *usageJSON         `json:"usage,omitempty"`
}

func (c completion) message(text []byte, finish domain.FinishReason, usage *domain.Usage) completionResponse {
	if finish == "" {
		finish = domain.FinishStop
	}
	return completionResponse{
		ID:      c.id,
		Object:  "chat.completion",
		Created: c.created,
		Model:   c.model,
		Choices: []completionChoice{{Message: messageJSON{Role: "assistant", Content: string(text)}, FinishReason: string(finish)}},
		Usage:   newUsageJSON(usage),
	}
}

type chunkDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chunkResponse struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *usageJSON    `json:"usage,omitempty"`
}

func (c completion) chunk(choices []chunkChoice, usage *domain.Usage) chunkResponse {
	return chunkResponse{ID: c.id, Object: "chat.completion.chunk", Created: c.created, Model: c.model, Choices: choices, Usage: newUsageJSON(usage)}
}

// sseOutput streams the answer as Server-Sent Events, flushing after every event so the
// caller sees each piece as soon as the provider produces it.
type sseOutput struct {
	w            http.ResponseWriter
	controller   *http.ResponseController
	completion   completion
	includeUsage bool

	began        bool
	sentRole     bool
	finishReason domain.FinishReason
	usage        *domain.Usage
}

func (o *sseOutput) Begin(route domain.Route) error {
	header := o.w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	// Tells nginx-style proxies not to buffer the stream.
	header.Set("X-Accel-Buffering", "no")
	setProviderHeaders(o.w, route)
	o.w.WriteHeader(http.StatusOK)
	o.began = true
	return o.controller.Flush()
}

// Write forwards text right away. Finish reasons and usage are held back and sent once, at
// the end, because providers report them in different places.
func (o *sseOutput) Write(chunk domain.Chunk) error {
	if chunk.FinishReason != "" {
		o.finishReason = chunk.FinishReason
	}
	if chunk.Usage != nil {
		o.usage = chunk.Usage
	}
	if chunk.Content == "" {
		return nil
	}
	delta := chunkDelta{Content: chunk.Content}
	if !o.sentRole {
		delta.Role = "assistant"
		o.sentRole = true
	}
	return o.send(o.completion.chunk([]chunkChoice{{Delta: delta}}, nil))
}

// finish ends a complete answer: the finish reason, the usage if asked for, then [DONE].
func (o *sseOutput) finish() {
	reason := string(o.finishReason)
	if reason == "" {
		reason = string(domain.FinishStop)
	}
	delta := chunkDelta{}
	if !o.sentRole {
		delta.Role = "assistant"
	}
	_ = o.send(o.completion.chunk([]chunkChoice{{Delta: delta, FinishReason: &reason}}, nil))
	if o.includeUsage && o.usage != nil {
		_ = o.send(o.completion.chunk([]chunkChoice{}, o.usage))
	}
	_ = o.event("[DONE]")
}

// fail reports an error after the answer has started. There is no [DONE] afterwards.
func (o *sseOutput) fail(err error) {
	_ = o.send(map[string]any{"error": map[string]string{"code": "provider_error", "message": providerMessage(err)}})
}

func (o *sseOutput) send(v any) error {
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	return o.event(string(encoded))
}

func (o *sseOutput) event(data string) error {
	if _, err := fmt.Fprintf(o.w, "data: %s\n\n", data); err != nil {
		return err
	}
	return o.controller.Flush()
}

// stringOrSlice accepts "input": "text" as well as "input": ["a", "b"].
type stringOrSlice []string

func (s *stringOrSlice) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*s = []string{single}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return errors.New("input must be a string or an array of strings")
	}
	*s = many
	return nil
}

type embeddingJSON struct {
	Object string `json:"object"`
	Index  int    `json:"index"`
	// Embedding is a list of floats, or a base64 string of little-endian float32 values.
	Embedding any `json:"embedding"`
}

type embeddingsResponse struct {
	Object string          `json:"object"`
	Model  string          `json:"model"`
	Data   []embeddingJSON `json:"data"`
	Usage  usageJSON       `json:"usage"`
}

func newEmbeddingsResponse(model string, result domain.Embeddings, asBase64 bool) embeddingsResponse {
	data := make([]embeddingJSON, len(result.Vectors))
	for i, vector := range result.Vectors {
		data[i] = embeddingJSON{Object: "embedding", Index: i, Embedding: vector}
		if asBase64 {
			data[i].Embedding = encodeFloat32s(vector)
		}
	}
	return embeddingsResponse{
		Object: "list",
		Model:  model,
		Data:   data,
		Usage:  usageJSON{PromptTokens: result.InputTokens, TotalTokens: result.InputTokens},
	}
}

func encodeFloat32s(vector []float32) string {
	raw := make([]byte, 4*len(vector))
	for i, value := range vector {
		binary.LittleEndian.PutUint32(raw[4*i:], math.Float32bits(value))
	}
	return base64.StdEncoding.EncodeToString(raw)
}
