package main

// End-to-end tests: the real routes, service, adapters, Postgres and Redis. Only the model
// providers are fakes (httptest servers speaking the OpenAI streaming format), so every
// failure can be produced on demand. Run with `make test`.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"enclave/gateway/internal/config"
	"enclave/gateway/internal/llm/app"
	"enclave/gateway/internal/llm/domain"
	"enclave/gateway/internal/llm/infra"
	"enclave/internal/platform/testenv"
)

const serviceToken = "test-service-token-0123456789"

// fakeProvider is an OpenAI-format provider whose behaviour each test sets.
type fakeProvider struct {
	server *httptest.Server

	mu      sync.Mutex
	handler http.HandlerFunc
	calls   int
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	p := &fakeProvider{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.calls++
		handler := p.handler
		p.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *fakeProvider) set(handler http.HandlerFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handler = handler
	p.calls = 0
}

func (p *fakeProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// streamWords answers like Ollama: one chunk per word, a finish chunk, a usage chunk, [DONE].
func streamWords(words ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, word := range words {
			writeEvent(w, fmt.Sprintf(`{"choices":[{"delta":{"content":%q},"finish_reason":null}]}`, word))
		}
		writeEvent(w, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		writeEvent(w, `{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":34}}`)
		writeEvent(w, "[DONE]")
	}
}

func writeEvent(w http.ResponseWriter, data string) {
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	w.(http.Flusher).Flush()
}

type testEnv struct {
	server *httptest.Server
	pool   *pgxpool.Pool
	local  *fakeProvider // runs inside Enclave
	cloud  *fakeProvider // external: needs the workspace admin's approval
}

func newTestEnv(t *testing.T, burst int) *testEnv {
	t.Helper()
	pool := testenv.Postgres(t)
	redisClient := testenv.Redis(t)
	local, cloud := newFakeProvider(t), newFakeProvider(t)

	providers := []config.ProviderConfig{
		{Name: "local", Kind: config.KindOpenAI, BaseURL: local.server.URL},
		{Name: "cloud", Kind: config.KindOpenAI, BaseURL: cloud.server.URL, External: true},
	}
	localRoute := domain.Route{Provider: domain.Provider{Name: "local"}, Model: "local-model"}
	cloudRoute := domain.Route{Provider: domain.Provider{Name: "cloud", External: true}, Model: "cloud-model"}
	catalog, err := domain.NewCatalog([]domain.Alias{
		{Name: "chat-local-first", Kind: domain.KindChat, Routes: []domain.Route{localRoute, cloudRoute}},
		{Name: "chat-cloud-first", Kind: domain.KindChat, Routes: []domain.Route{cloudRoute, localRoute}},
		{Name: "embed", Kind: domain.KindEmbedding, Routes: []domain.Route{localRoute}},
	})
	if err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	chatProviders, embedders := buildProviders(providers, newProviderHTTPClient(), logger)
	service := app.NewService(app.Options{
		Catalog:           catalog,
		ChatProviders:     chatProviders,
		Embedders:         embedders,
		Policies:          infra.NewPolicies(pool),
		Limiter:           infra.NewRateLimiter(redisClient, 60, burst),
		Usage:             infra.NewUsageRecorder(pool),
		Logger:            logger,
		FirstTokenTimeout: 2 * time.Second,
		RequestTimeout:    10 * time.Second,
	})
	cfg := config.Config{ServiceTokens: map[string]string{serviceToken: "test"}}
	server := httptest.NewServer(routes(cfg, logger, service))
	t.Cleanup(server.Close)
	return &testEnv{server: server, pool: pool, local: local, cloud: cloud}
}

// newWorkspace creates a workspace and, optionally, allows external providers for it.
func (env *testEnv) newWorkspace(t *testing.T, externalProviders ...string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := env.pool.QueryRow(ctx, `INSERT INTO workspaces (name) VALUES ('gateway e2e') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = env.pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id = $1`, id) })
	if len(externalProviders) > 0 {
		if _, err := env.pool.Exec(ctx, `INSERT INTO workspace_ai_settings (workspace_id, external_providers) VALUES ($1, $2)`, id, externalProviders); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

type request struct {
	path      string
	body      any
	workspace uuid.UUID
	token     string
	requestID string
}

func (env *testEnv) send(t *testing.T, ctx context.Context, req request) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(req.body)
	if err != nil {
		t.Fatal(err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, env.server.URL+req.path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.token)
	}
	if req.workspace != uuid.Nil {
		httpReq.Header.Set("X-Enclave-Workspace-ID", req.workspace.String())
	}
	if req.requestID != "" {
		httpReq.Header.Set("X-Request-ID", req.requestID)
	}
	resp, err := env.server.Client().Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func chatBody(model string, stream bool) map[string]any {
	return map[string]any{
		"model":          model,
		"stream":         stream,
		"stream_options": map[string]bool{"include_usage": true},
		"messages":       []map[string]string{{"role": "user", "content": "Xin chào"}},
	}
}

func newRequestID() string {
	return "req" + rand.Text()[:16]
}

// usageRows returns "provider:status" for every attempt recorded for requestID.
func (env *testEnv) usageRows(t *testing.T, requestID string) []string {
	t.Helper()
	rows, err := env.pool.Query(context.Background(),
		`SELECT provider || ':' || status FROM llm_usage WHERE request_id = $1 ORDER BY attempt`, requestID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		result = append(result, row)
	}
	return result
}

// readSSE returns the data payloads of a streamed response.
func readSSE(t *testing.T, body io.Reader) []string {
	t.Helper()
	var events []string
	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		if data, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
			events = append(events, data)
		}
	}
	return events
}

// waitFor polls condition for a few seconds: some effects (the usage row of a cancelled
// stream) happen on the server just after the client has stopped reading.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sendJSON sends req and decodes the JSON answer; the body is closed before it returns.
func (env *testEnv) sendJSON(t *testing.T, req request) (int, http.Header, map[string]any) {
	t.Helper()
	resp := env.send(t, context.Background(), req)
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, body
}

func TestServiceAuthentication(t *testing.T) {
	env := newTestEnv(t, 20)
	workspace := env.newWorkspace(t)
	cases := map[string]struct {
		token      string
		workspace  uuid.UUID
		wantStatus int
	}{
		"no token":           {"", workspace, http.StatusUnauthorized},
		"wrong token":        {"wrong-token-0123456789abcdef", workspace, http.StatusUnauthorized},
		"no workspace":       {serviceToken, uuid.Nil, http.StatusBadRequest},
		"unknown workspace":  {serviceToken, uuid.New(), http.StatusNotFound},
		"valid token and ID": {serviceToken, workspace, http.StatusOK},
	}
	env.local.set(streamWords("ok"))
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resp := env.send(t, context.Background(), request{path: "/v1/chat/completions", body: chatBody("chat-local-first", false), workspace: tc.workspace, token: tc.token})
			_ = resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
		})
	}
}

func TestChatCompletion(t *testing.T) {
	env := newTestEnv(t, 20)
	workspace := env.newWorkspace(t)
	env.local.set(streamWords("Xin", " chào", "!"))

	t.Run("whole answer at once", func(t *testing.T) {
		status, header, body := env.sendJSON(t, request{path: "/v1/chat/completions", body: chatBody("chat-local-first", false), workspace: workspace, token: serviceToken})
		if status != http.StatusOK || header.Get("X-Enclave-Provider") != "local" || header.Get("X-Enclave-Provider-Model") != "local-model" {
			t.Fatalf("status %d, headers %v", status, header)
		}
		choice := body["choices"].([]any)[0].(map[string]any)
		if choice["message"].(map[string]any)["content"] != "Xin chào!" || choice["finish_reason"] != "stop" {
			t.Fatalf("choice = %v", choice)
		}
		if body["model"] != "chat-local-first" || body["usage"].(map[string]any)["total_tokens"] != float64(46) {
			t.Fatalf("body = %v", body)
		}
	})

	t.Run("streamed in OpenAI's SSE format", func(t *testing.T) {
		resp := env.send(t, context.Background(), request{path: "/v1/chat/completions", body: chatBody("chat-local-first", true), workspace: workspace, token: serviceToken})
		defer func() { _ = resp.Body.Close() }()
		if resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("Content-Type = %q", resp.Header.Get("Content-Type"))
		}
		events := readSSE(t, resp.Body)
		// 3 text chunks, the finish chunk, the usage chunk, [DONE].
		if len(events) != 6 || events[len(events)-1] != "[DONE]" {
			t.Fatalf("events = %v", events)
		}
		var first, usage struct {
			Choices []struct {
				Delta struct{ Role, Content string } `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				TotalTokens int `json:"total_tokens"`
			} `json:"usage"`
		}
		_ = json.Unmarshal([]byte(events[0]), &first)
		_ = json.Unmarshal([]byte(events[4]), &usage)
		if first.Choices[0].Delta.Role != "assistant" || first.Choices[0].Delta.Content != "Xin" {
			t.Fatalf("first chunk = %s", events[0])
		}
		if usage.Usage == nil || usage.Usage.TotalTokens != 46 || len(usage.Choices) != 0 {
			t.Fatalf("usage chunk = %s", events[4])
		}
	})

	t.Run("json_object response format is accepted, other formats are not", func(t *testing.T) {
		body := chatBody("chat-local-first", false)
		body["response_format"] = map[string]string{"type": "json_object"}
		status, _, _ := env.sendJSON(t, request{path: "/v1/chat/completions", body: body, workspace: workspace, token: serviceToken})
		if status != http.StatusOK {
			t.Fatalf("json_object: status %d", status)
		}
		body["response_format"] = map[string]string{"type": "json_schema"}
		status, _, _ = env.sendJSON(t, request{path: "/v1/chat/completions", body: body, workspace: workspace, token: serviceToken})
		if status != http.StatusBadRequest {
			t.Fatalf("json_schema: status %d", status)
		}
	})

	t.Run("unknown model", func(t *testing.T) {
		status, _, body := env.sendJSON(t, request{path: "/v1/chat/completions", body: chatBody("gpt-9", false), workspace: workspace, token: serviceToken})
		if status != http.StatusNotFound || body["error"].(map[string]any)["code"] != "model_not_found" {
			t.Fatalf("status %d body %v", status, body)
		}
	})
}

func TestFallbackAndAccounting(t *testing.T) {
	env := newTestEnv(t, 20)
	workspace := env.newWorkspace(t, "cloud")
	env.local.set(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"model is loading"}}`, http.StatusServiceUnavailable)
	})
	env.cloud.set(streamWords("from", " cloud"))
	requestID := newRequestID()

	status, header, body := env.sendJSON(t, request{path: "/v1/chat/completions", body: chatBody("chat-local-first", false), workspace: workspace, token: serviceToken, requestID: requestID})
	if status != http.StatusOK || header.Get("X-Enclave-Provider") != "cloud" {
		t.Fatalf("status %d via %q: %v", status, header.Get("X-Enclave-Provider"), body)
	}
	if got := env.usageRows(t, requestID); strings.Join(got, ",") != "local:failed,cloud:ok" {
		t.Fatalf("usage rows = %v; every attempt must be accounted for", got)
	}
}

func TestWorkspaceAISettingsDecideWhereContentGoes(t *testing.T) {
	env := newTestEnv(t, 20)
	env.local.set(streamWords("local answer"))
	env.cloud.set(streamWords("cloud answer"))

	t.Run("without approval the external provider is never called", func(t *testing.T) {
		workspace := env.newWorkspace(t)
		resp := env.send(t, context.Background(), request{path: "/v1/chat/completions", body: chatBody("chat-cloud-first", false), workspace: workspace, token: serviceToken})
		_ = resp.Body.Close()
		if resp.Header.Get("X-Enclave-Provider") != "local" || env.cloud.callCount() != 0 {
			t.Fatalf("answered by %q, cloud called %d times", resp.Header.Get("X-Enclave-Provider"), env.cloud.callCount())
		}
	})

	t.Run("with approval it is used first", func(t *testing.T) {
		workspace := env.newWorkspace(t, "cloud")
		resp := env.send(t, context.Background(), request{path: "/v1/chat/completions", body: chatBody("chat-cloud-first", false), workspace: workspace, token: serviceToken})
		_ = resp.Body.Close()
		if resp.Header.Get("X-Enclave-Provider") != "cloud" {
			t.Fatalf("answered by %q", resp.Header.Get("X-Enclave-Provider"))
		}
	})
}

func TestClientCancellationStopsTheProvider(t *testing.T) {
	env := newTestEnv(t, 20)
	workspace := env.newWorkspace(t)
	providerStopped := make(chan struct{})
	env.local.set(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeEvent(w, `{"choices":[{"delta":{"content":"first words"}}]}`)
		<-r.Context().Done() // a long answer still being generated
		close(providerStopped)
	})
	requestID := newRequestID()

	ctx, cancel := context.WithCancel(context.Background())
	resp := env.send(t, ctx, request{path: "/v1/chat/completions", body: chatBody("chat-local-first", true), workspace: workspace, token: serviceToken, requestID: requestID})
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.Contains(line, "first words") {
		t.Fatalf("first event = %q, %v", line, err)
	}
	cancel() // the caller hangs up mid-answer
	_ = resp.Body.Close()

	select {
	case <-providerStopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the gateway kept the provider generating after the caller left")
	}
	waitFor(t, func() bool { return strings.Join(env.usageRows(t, requestID), ",") == "local:cancelled" })
}

func TestFailureAfterOutputBegan(t *testing.T) {
	env := newTestEnv(t, 20)
	workspace := env.newWorkspace(t, "cloud")
	env.local.set(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeEvent(w, `{"choices":[{"delta":{"content":"half an ans"}}]}`)
		// The connection drops without finish_reason or [DONE].
	})
	env.cloud.set(streamWords("unused"))
	requestID := newRequestID()

	resp := env.send(t, context.Background(), request{path: "/v1/chat/completions", body: chatBody("chat-local-first", true), workspace: workspace, token: serviceToken, requestID: requestID})
	events := readSSE(t, resp.Body)
	_ = resp.Body.Close()

	last := events[len(events)-1]
	if !strings.Contains(last, `"error"`) || strings.Contains(strings.Join(events, ""), "[DONE]") {
		t.Fatalf("events = %v; a broken answer must end with an error event and no [DONE]", events)
	}
	if env.cloud.callCount() != 0 {
		t.Fatal("no fallback once part of the answer was sent")
	}
	if got := env.usageRows(t, requestID); strings.Join(got, ",") != "local:interrupted" {
		t.Fatalf("usage rows = %v", got)
	}
}

func TestRateLimit(t *testing.T) {
	env := newTestEnv(t, 2)
	workspace := env.newWorkspace(t)
	env.local.set(streamWords("ok"))

	for i := range 2 {
		resp := env.send(t, context.Background(), request{path: "/v1/chat/completions", body: chatBody("chat-local-first", false), workspace: workspace, token: serviceToken})
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status %d", i+1, resp.StatusCode)
		}
	}
	resp := env.send(t, context.Background(), request{path: "/v1/chat/completions", body: chatBody("chat-local-first", false), workspace: workspace, token: serviceToken})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if env.local.callCount() != 2 {
		t.Fatalf("provider called %d times; a rejected request must not reach it", env.local.callCount())
	}
}

func TestEmbeddings(t *testing.T) {
	env := newTestEnv(t, 20)
	workspace := env.newWorkspace(t)
	env.local.set(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[0.5,-1.25]}],"usage":{"prompt_tokens":3}}`)
	})

	t.Run("floats", func(t *testing.T) {
		status, _, body := env.sendJSON(t, request{path: "/v1/embeddings", body: map[string]any{"model": "embed", "input": "xin chào"}, workspace: workspace, token: serviceToken})
		vector := body["data"].([]any)[0].(map[string]any)["embedding"].([]any)
		if status != http.StatusOK || vector[0] != 0.5 || vector[1] != -1.25 {
			t.Fatalf("status %d body %v", status, body)
		}
	})

	t.Run("base64, as OpenAI's Python client requests by default", func(t *testing.T) {
		_, _, body := env.sendJSON(t, request{path: "/v1/embeddings", body: map[string]any{"model": "embed", "input": []string{"xin chào"}, "encoding_format": "base64"}, workspace: workspace, token: serviceToken})
		raw, err := base64.StdEncoding.DecodeString(body["data"].([]any)[0].(map[string]any)["embedding"].(string))
		if err != nil || len(raw) != 8 {
			t.Fatalf("embedding = %v, %v", body, err)
		}
		if got := math.Float32frombits(binary.LittleEndian.Uint32(raw[4:])); got != -1.25 {
			t.Fatalf("second value = %v", got)
		}
	})
}
