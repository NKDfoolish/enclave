package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"enclave/gateway/internal/llm/app"
	"enclave/gateway/internal/llm/domain"
)

// The testdata streams are real responses captured from Ollama (llama3.2:3b) and NVIDIA NIM
// (nemotron-3.5-bolt-e4b) for the prompt "Say hello in Vietnamese in 5 words."

func readAll(t *testing.T, stream app.ChatStream) (string, *domain.Usage, domain.FinishReason) {
	t.Helper()
	var (
		text   strings.Builder
		usage  *domain.Usage
		finish domain.FinishReason
	)
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			return text.String(), usage, finish
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		text.WriteString(chunk.Content)
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if chunk.FinishReason != "" {
			finish = chunk.FinishReason
		}
	}
}

func replayServer(t *testing.T, fixture string, check func(*http.Request, chatRequest)) *httptest.Server {
	t.Helper()
	recorded, err := os.ReadFile("testdata/" + fixture) //nolint:gosec // fixture names are constants in this file
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body chatRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if check != nil {
			check(r, body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(recorded)
	}))
	t.Cleanup(server.Close)
	return server
}

func newClient(server *httptest.Server, apiKey string) *Client {
	return New(Options{Provider: "test", BaseURL: server.URL + "/v1", APIKey: apiKey, HTTPClient: server.Client()})
}

func question() domain.ChatRequest {
	maxTokens := 60
	return domain.ChatRequest{
		Model:     "chat-fast",
		Messages:  []domain.Message{{Role: domain.RoleUser, Content: "Say hello in Vietnamese in 5 words."}},
		MaxTokens: &maxTokens,
	}
}

func TestStreamChatReadsRealProviderStreams(t *testing.T) {
	cases := []struct {
		fixture   string
		wantText  string
		wantUsage domain.Usage
	}{
		// Ollama sends usage in a chunk with an empty choices list after the finish chunk.
		{"ollama_chat_stream.txt", `Xin chào, bạn là ai? (pronounced "sin chow, buu lah ai")`, domain.Usage{InputTokens: 34, OutputTokens: 23}},
		// NVIDIA adds logprobs and reasoning token details, which must not confuse the parser.
		{"nvidia_chat_stream.txt", "Xin chào bạn khỏe không", domain.Usage{InputTokens: 18, OutputTokens: 6}},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			server := replayServer(t, tc.fixture, func(r *http.Request, body chatRequest) {
				if r.URL.Path != "/v1/chat/completions" {
					t.Errorf("path = %s", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer secret-key" {
					t.Errorf("Authorization = %q", got)
				}
				if body.Model != "provider-model" || !body.Stream || !body.StreamOptions.IncludeUsage || *body.MaxTokens != 60 {
					t.Errorf("request body = %+v", body)
				}
			})

			stream, err := newClient(server, "secret-key").StreamChat(context.Background(), "provider-model", question())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stream.Close() }()

			text, usage, finish := readAll(t, stream)
			if text != tc.wantText {
				t.Fatalf("text = %q, want %q", text, tc.wantText)
			}
			if usage == nil || *usage != tc.wantUsage {
				t.Fatalf("usage = %+v, want %+v", usage, tc.wantUsage)
			}
			if finish != domain.FinishStop {
				t.Fatalf("finish = %q", finish)
			}
		})
	}
}

func TestStreamChatWithoutAPIKeySendsNoAuthorization(t *testing.T) {
	server := replayServer(t, "ollama_chat_stream.txt", func(r *http.Request, _ chatRequest) {
		if r.Header.Get("Authorization") != "" {
			t.Error("a provider without a key must not get an Authorization header")
		}
	})
	stream, err := newClient(server, "").StreamChat(context.Background(), "m", question())
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
}

func TestProviderErrorsAreClassified(t *testing.T) {
	cases := []struct {
		status        int
		body          string
		wantRetryable bool
		wantMessage   string
	}{
		{http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`, true, "rate limited"},
		{http.StatusServiceUnavailable, `upstream overloaded`, true, "upstream overloaded"},
		{http.StatusNotFound, `{"error":"model \"x\" not found"}`, true, `model "x" not found`}, // Ollama's shape
		{http.StatusBadRequest, `{"error":{"message":"context too long"}}`, false, "context too long"},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, tc.body, tc.status)
			}))
			defer server.Close()

			_, err := newClient(server, "").StreamChat(context.Background(), "m", question())
			var providerErr *domain.ProviderError
			if !errors.As(err, &providerErr) {
				t.Fatalf("err = %v, want *domain.ProviderError", err)
			}
			if providerErr.StatusCode != tc.status || providerErr.Retryable != tc.wantRetryable {
				t.Fatalf("got status %d retryable %v", providerErr.StatusCode, providerErr.Retryable)
			}
			if !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("error %q does not include the provider's message %q", err, tc.wantMessage)
			}
		})
	}
}

func TestStreamCutShortIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// One chunk, then the connection closes without a finish_reason or [DONE].
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
	}))
	defer server.Close()

	stream, err := newClient(server, "").StreamChat(context.Background(), "m", question())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if chunk, err := stream.Next(); err != nil || chunk.Content != "Hel" {
		t.Fatalf("first chunk = %+v, %v", chunk, err)
	}
	if _, err := stream.Next(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v; a truncated answer must not look complete", err)
	}
}

func TestErrorEventInsideStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, ": keep-alive comment\n\ndata: {\"error\":{\"message\":\"worker crashed\"}}\n\n")
	}))
	defer server.Close()

	stream, err := newClient(server, "").StreamChat(context.Background(), "m", question())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if _, err := stream.Next(); err == nil || !strings.Contains(err.Error(), "worker crashed") {
		t.Fatalf("err = %v", err)
	}
}

func TestEmbed(t *testing.T) {
	var got embeddingRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		// Out of order on purpose: index decides which input a vector belongs to.
		_, _ = io.WriteString(w, `{"data":[{"index":1,"embedding":[0.3,0.4]},{"index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":5}}`)
	}))
	defer server.Close()
	req := domain.EmbeddingRequest{Model: "embed", Inputs: []string{"first", "second"}, InputType: domain.InputQuery}

	t.Run("orders vectors by index", func(t *testing.T) {
		result, err := newClient(server, "").Embed(context.Background(), "embed-model", req)
		if err != nil {
			t.Fatal(err)
		}
		if result.Vectors[0][0] != 0.1 || result.Vectors[1][0] != 0.3 || result.InputTokens != 5 {
			t.Fatalf("result = %+v", result)
		}
		if got.InputType != "" {
			t.Fatalf("input_type = %q; only providers that need it may receive it", got.InputType)
		}
	})

	t.Run("sends input_type when the provider needs it", func(t *testing.T) {
		client := New(Options{Provider: "nvidia", BaseURL: server.URL, HTTPClient: server.Client(), SendInputType: true})
		if _, err := client.Embed(context.Background(), "embed-model", req); err != nil {
			t.Fatal(err)
		}
		if got.InputType != "query" {
			t.Fatalf("input_type = %q, want query", got.InputType)
		}
	})
}

func TestJSONOutputAndTopPAreForwarded(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	req := question()
	topP := 0.9
	req.TopP = &topP
	req.JSONOutput = true

	stream, err := newClient(server, "").StreamChat(context.Background(), "m", req)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()

	format, _ := got["response_format"].(map[string]any)
	if format["type"] != "json_object" || got["top_p"] != 0.9 {
		t.Fatalf("request = %v", got)
	}
}
