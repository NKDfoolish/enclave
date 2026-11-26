package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"enclave/gateway/internal/llm/domain"
)

// messagesStream is a Messages API stream in Anthropic's event format: typed events, usage at
// message_start (input) and message_delta (output), a ping and a thinking block in between.
const messagesStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-opus-5-5","stop_reason":null,"usage":{"input_tokens":21,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Xin chào"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":" bạn!"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

`

func question() domain.ChatRequest {
	temperature := 0.2
	return domain.ChatRequest{
		Model: "chat-smart",
		Messages: []domain.Message{
			{Role: domain.RoleSystem, Content: "Answer briefly."},
			{Role: domain.RoleUser, Content: "Say hello in Vietnamese."},
		},
		Temperature: &temperature,
	}
}

func newTestClient(server *httptest.Server) *Client {
	return New(Options{Provider: "anthropic", BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client()})
}

func TestStreamChatTranslatesEvents(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This SDK version sends the API key as a bearer token.
		if r.URL.Path != "/v1/messages" || r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("Anthropic-Version") == "" {
			t.Errorf("request %s with headers %v", r.URL.Path, r.Header)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, messagesStream)
	}))
	defer server.Close()

	stream, err := newTestClient(server).StreamChat(context.Background(), "claude-opus-5-5", question())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()

	var (
		text   strings.Builder
		usage  *domain.Usage
		finish domain.FinishReason
		events int
	)
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		events++
		text.WriteString(chunk.Content)
		if chunk.Usage != nil {
			usage = chunk.Usage
			finish = chunk.FinishReason
		}
	}

	if text.String() != "Xin chào bạn!" {
		t.Fatalf("text = %q", text.String())
	}
	if usage == nil || usage.InputTokens != 21 || usage.OutputTokens != 42 || finish != domain.FinishStop {
		t.Fatalf("usage = %+v, finish = %q", usage, finish)
	}
	if events < 4 {
		t.Fatalf("got %d chunks; events without text must still come through as liveness", events)
	}

	// The request is translated, not forwarded: system is its own field, max_tokens is set,
	// and temperature is dropped because current Claude models reject it.
	if body["model"] != "claude-opus-5-5" || body["max_tokens"] != float64(defaultMaxTokens) {
		t.Fatalf("body = %v", body)
	}
	if _, sent := body["temperature"]; sent {
		t.Fatal("temperature must not be sent to Anthropic")
	}
	system, _ := json.Marshal(body["system"])
	if !strings.Contains(string(system), "Answer briefly.") {
		t.Fatalf("system = %s", system)
	}
	if messages := body["messages"].([]any); len(messages) != 1 {
		t.Fatalf("messages = %v; the system message must not be sent as a message", messages)
	}
}

func TestErrorsAreClassifiedOnFirstNext(t *testing.T) {
	cases := []struct {
		status        int
		wantRetryable bool
	}{
		{529, true}, // overloaded
		{http.StatusTooManyRequests, true},
		{http.StatusBadRequest, false},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			}))
			defer server.Close()

			stream, err := newTestClient(server).StreamChat(context.Background(), "claude-opus-5-5", question())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stream.Close() }()
			_, err = stream.Next()

			var providerErr *domain.ProviderError
			if !errors.As(err, &providerErr) || providerErr.StatusCode != tc.status || providerErr.Retryable != tc.wantRetryable {
				t.Fatalf("err = %#v", err)
			}
		})
	}
}

func TestErrorEventMidStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.SplitAfter(messagesStream, "\n\n")[0]+
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
	}))
	defer server.Close()

	stream, err := newTestClient(server).StreamChat(context.Background(), "claude-opus-5-5", question())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if _, err := stream.Next(); err != nil {
		t.Fatalf("message_start: %v", err)
	}
	if _, err := stream.Next(); err == nil {
		t.Fatal("an error event must end the stream with an error")
	}
}

func TestJSONOutputIsAskedForInTheSystemPrompt(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, messagesStream)
	}))
	defer server.Close()
	req := question()
	req.JSONOutput = true

	stream, err := newTestClient(server).StreamChat(context.Background(), "claude-opus-5-5", req)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()

	system, _ := json.Marshal(body["system"])
	if !strings.Contains(string(system), "single JSON object") || body["response_format"] != nil {
		t.Fatalf("system = %s", system)
	}
}
