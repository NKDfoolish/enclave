package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"enclave/gateway/internal/llm/domain"
)

// --- Fakes ---

// step is one thing a fake stream does when Next is called.
type step struct {
	chunk domain.Chunk
	err   error
	// hang blocks until the attempt is cancelled, like a provider that stops sending.
	hang bool
}

type fakeProvider struct {
	name    string
	openErr error // returned by StreamChat
	steps   []step

	mu       sync.Mutex
	calls    int
	lastCtx  context.Context
	lastReq  domain.ChatRequest
	embedErr error
	vectors  int // how many vectors Embed returns; -1 means one per input
}

func (p *fakeProvider) StreamChat(ctx context.Context, _ string, req domain.ChatRequest) (ChatStream, error) {
	p.mu.Lock()
	p.calls++
	p.lastCtx = ctx
	p.lastReq = req
	p.mu.Unlock()
	if p.openErr != nil {
		return nil, p.openErr
	}
	return &fakeStream{ctx: ctx, steps: p.steps}, nil
}

func (p *fakeProvider) Embed(_ context.Context, _ string, req domain.EmbeddingRequest) (domain.Embeddings, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if p.embedErr != nil {
		return domain.Embeddings{}, p.embedErr
	}
	count := len(req.Inputs)
	if p.vectors >= 0 {
		count = p.vectors
	}
	vectors := make([][]float32, count)
	for i := range vectors {
		vectors[i] = []float32{0.1, 0.2}
	}
	return domain.Embeddings{Vectors: vectors, InputTokens: 7}, nil
}

func (p *fakeProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type fakeStream struct {
	ctx   context.Context
	steps []step
	next  int
}

func (s *fakeStream) Next() (domain.Chunk, error) {
	if s.next >= len(s.steps) {
		return domain.Chunk{}, io.EOF
	}
	current := s.steps[s.next]
	s.next++
	if current.hang {
		<-s.ctx.Done()
		return domain.Chunk{}, s.ctx.Err()
	}
	return current.chunk, current.err
}

func (s *fakeStream) Close() error { return nil }

type fakePolicies struct {
	policy domain.Policy
	err    error
}

func (f fakePolicies) Policy(context.Context, uuid.UUID) (domain.Policy, error) {
	return f.policy, f.err
}

type fakeLimiter struct {
	allowed    bool
	retryAfter time.Duration
}

func (f fakeLimiter) Allow(context.Context, uuid.UUID) (bool, time.Duration, error) {
	return f.allowed, f.retryAfter, nil
}

type fakeUsage struct {
	mu      sync.Mutex
	records []domain.UsageRecord
}

func (f *fakeUsage) Record(_ context.Context, record domain.UsageRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, record)
	return nil
}

func (f *fakeUsage) all() []domain.UsageRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.UsageRecord(nil), f.records...)
}

// recordingOutput collects what the caller would receive.
type recordingOutput struct {
	began   *domain.Route
	text    string
	onWrite func()
}

func (o *recordingOutput) Begin(route domain.Route) error {
	if o.began != nil {
		panic("Begin called twice")
	}
	o.began = &route
	return nil
}

func (o *recordingOutput) Write(chunk domain.Chunk) error {
	if o.began == nil {
		panic("Write before Begin")
	}
	o.text += chunk.Content
	if o.onWrite != nil {
		o.onWrite()
	}
	return nil
}

// --- Helpers ---

var (
	local    = domain.Provider{Name: "local"}
	backup   = domain.Provider{Name: "backup"}
	external = domain.Provider{Name: "external", External: true}
)

type harness struct {
	service   *Service
	usage     *fakeUsage
	providers map[string]*fakeProvider
}

type harnessOptions struct {
	aliases  []domain.Alias
	policy   domain.Policy
	limiter  fakeLimiter
	timeout  time.Duration
	provider map[string]*fakeProvider
}

func newHarness(t *testing.T, opts harnessOptions) *harness {
	t.Helper()
	catalog, err := domain.NewCatalog(opts.aliases)
	if err != nil {
		t.Fatal(err)
	}
	chat := map[string]ChatProvider{}
	embedders := map[string]Embedder{}
	for name, provider := range opts.provider {
		provider.name = name
		chat[name] = provider
		embedders[name] = provider
	}
	if opts.timeout == 0 {
		opts.timeout = time.Second
	}
	usage := &fakeUsage{}
	service := NewService(Options{
		Catalog:           catalog,
		ChatProviders:     chat,
		Embedders:         embedders,
		Policies:          fakePolicies{policy: opts.policy},
		Limiter:           opts.limiter,
		Usage:             usage,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		FirstTokenTimeout: opts.timeout,
		RequestTimeout:    10 * time.Second,
	})
	return &harness{service: service, usage: usage, providers: opts.provider}
}

func chatAlias(routes ...domain.Provider) domain.Alias {
	alias := domain.Alias{Name: "chat-test", Kind: domain.KindChat}
	for _, provider := range routes {
		alias.Routes = append(alias.Routes, domain.Route{
			Provider: provider,
			Model:    provider.Name + "-model",
			Price:    domain.Price{InputMicroUSDPerMTok: 1_000_000, OutputMicroUSDPerMTok: 2_000_000},
		})
	}
	return alias
}

func answer(text string, usage *domain.Usage) []step {
	return []step{
		{chunk: domain.Chunk{Content: text}},
		{chunk: domain.Chunk{FinishReason: domain.FinishStop, Usage: usage}},
	}
}

func question() domain.ChatRequest {
	return domain.ChatRequest{Model: "chat-test", Messages: []domain.Message{{Role: domain.RoleUser, Content: "hello there"}}}
}

var testCall = domain.Call{Caller: "test", WorkspaceID: uuid.New(), RequestID: "req-1"}

func allowed() fakeLimiter { return fakeLimiter{allowed: true} }

// --- Chat ---

func TestChatAnswersFromFirstRouteAndRecordsUsage(t *testing.T) {
	h := newHarness(t, harnessOptions{
		aliases: []domain.Alias{chatAlias(local, backup)},
		limiter: allowed(),
		provider: map[string]*fakeProvider{
			"local":  {steps: answer("Hi!", &domain.Usage{InputTokens: 10, OutputTokens: 20})},
			"backup": {},
		},
	})
	out := &recordingOutput{}

	if err := h.service.Chat(context.Background(), testCall, question(), out); err != nil {
		t.Fatal(err)
	}
	if out.began == nil || out.began.Provider.Name != "local" || out.text != "Hi!" {
		t.Fatalf("output = %+v %q", out.began, out.text)
	}
	if h.providers["backup"].callCount() != 0 {
		t.Fatal("the fallback route must not be called when the first one works")
	}
	records := h.usage.all()
	if len(records) != 1 {
		t.Fatalf("got %d usage records, want 1", len(records))
	}
	got := records[0]
	if got.Status != domain.AttemptOK || got.Estimated || got.Usage.InputTokens != 10 || got.Usage.OutputTokens != 20 {
		t.Fatalf("record = %+v", got)
	}
	// 10 tokens at $1/Mtok + 20 tokens at $2/Mtok = 50 micro-dollars.
	if got.CostMicroUSD != 50 {
		t.Fatalf("cost = %d micro-USD, want 50", got.CostMicroUSD)
	}
	if got.FirstToken == nil {
		t.Fatal("time to first token must be recorded")
	}
}

func TestChatFallsBackBeforeOutputBegins(t *testing.T) {
	h := newHarness(t, harnessOptions{
		aliases: []domain.Alias{chatAlias(local, backup)},
		limiter: allowed(),
		provider: map[string]*fakeProvider{
			"local":  {openErr: domain.NewStatusError("local", http.StatusServiceUnavailable, errors.New("overloaded"))},
			"backup": {steps: answer("from backup", nil)},
		},
	})
	out := &recordingOutput{}

	if err := h.service.Chat(context.Background(), testCall, question(), out); err != nil {
		t.Fatal(err)
	}
	if out.began.Provider.Name != "backup" || out.text != "from backup" {
		t.Fatalf("answered by %s: %q", out.began.Provider.Name, out.text)
	}
	records := h.usage.all()
	if len(records) != 2 {
		t.Fatalf("got %d usage records, want one per attempt (2)", len(records))
	}
	if records[0].Status != domain.AttemptFailed || records[0].ErrorCode != "provider_status_503" || records[0].Attempt != 1 {
		t.Fatalf("first attempt = %+v", records[0])
	}
	if records[0].Usage != (domain.Usage{}) || records[0].Estimated {
		t.Fatalf("first attempt usage = %+v; a provider that produced nothing billed nothing", records[0].Usage)
	}
	if records[1].Status != domain.AttemptOK || records[1].Attempt != 2 || !records[1].Estimated {
		t.Fatalf("second attempt = %+v (no usage reported, so it must be estimated)", records[1])
	}
}

func TestChatDoesNotFallBackWhenTheRequestIsAtFault(t *testing.T) {
	h := newHarness(t, harnessOptions{
		aliases: []domain.Alias{chatAlias(local, backup)},
		limiter: allowed(),
		provider: map[string]*fakeProvider{
			"local":  {openErr: domain.NewStatusError("local", http.StatusBadRequest, errors.New("bad request"))},
			"backup": {steps: answer("unused", nil)},
		},
	})

	err := h.service.Chat(context.Background(), testCall, question(), &recordingOutput{})
	if domain.IsRetryable(err) || err == nil {
		t.Fatalf("err = %v, want the non-retryable provider error", err)
	}
	if h.providers["backup"].callCount() != 0 {
		t.Fatal("a 400 would fail on every provider; the gateway must not fall back")
	}
}

func TestChatDoesNotFallBackOnceOutputBegan(t *testing.T) {
	h := newHarness(t, harnessOptions{
		aliases: []domain.Alias{chatAlias(local, backup)},
		limiter: allowed(),
		provider: map[string]*fakeProvider{
			"local": {steps: []step{
				{chunk: domain.Chunk{Content: "partial answer"}},
				{err: domain.NewTransportError("local", errors.New("connection reset"))},
			}},
			"backup": {steps: answer("unused", nil)},
		},
	})
	out := &recordingOutput{}

	err := h.service.Chat(context.Background(), testCall, question(), out)
	if err == nil {
		t.Fatal("want the mid-stream error")
	}
	if out.text != "partial answer" || h.providers["backup"].callCount() != 0 {
		t.Fatalf("text = %q, backup calls = %d; a second provider would repeat the answer from the start",
			out.text, h.providers["backup"].callCount())
	}
	records := h.usage.all()
	if len(records) != 1 || records[0].Status != domain.AttemptInterrupted || !records[0].Estimated || records[0].Usage.OutputTokens == 0 {
		t.Fatalf("records = %+v; an interrupted stream must still be charged (estimated)", records)
	}
}

func TestChatFallsBackWhenFirstTokenTimesOut(t *testing.T) {
	h := newHarness(t, harnessOptions{
		aliases: []domain.Alias{chatAlias(local, backup)},
		limiter: allowed(),
		timeout: 50 * time.Millisecond,
		provider: map[string]*fakeProvider{
			"local":  {steps: []step{{hang: true}}},
			"backup": {steps: answer("fast enough", nil)},
		},
	})
	out := &recordingOutput{}

	start := time.Now()
	if err := h.service.Chat(context.Background(), testCall, question(), out); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("took %v; the silent provider must be cut off by the first-token timeout", elapsed)
	}
	if out.began.Provider.Name != "backup" {
		t.Fatalf("answered by %s", out.began.Provider.Name)
	}
	if got := h.usage.all()[0]; got.ErrorCode != "first_token_timeout" || got.Status != domain.AttemptFailed {
		t.Fatalf("first attempt = %+v", got)
	}
}

func TestChatStopsTheProviderWhenTheCallerCancels(t *testing.T) {
	h := newHarness(t, harnessOptions{
		aliases: []domain.Alias{chatAlias(local, backup)},
		limiter: allowed(),
		provider: map[string]*fakeProvider{
			"local":  {steps: []step{{chunk: domain.Chunk{Content: "first words"}}, {hang: true}}},
			"backup": {steps: answer("unused", nil)},
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &recordingOutput{onWrite: cancel} // the caller hangs up after the first chunk

	err := h.service.Chat(ctx, testCall, question(), out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if providerCtx := h.providers["local"].lastCtx; providerCtx.Err() == nil {
		t.Fatal("the provider call must be cancelled so it stops generating (and billing)")
	}
	if h.providers["backup"].callCount() != 0 {
		t.Fatal("no fallback after the caller left")
	}
	if got := h.usage.all(); len(got) != 1 || got[0].Status != domain.AttemptCancelled {
		t.Fatalf("records = %+v", got)
	}
}

func TestChatReportsWhenEveryRouteFails(t *testing.T) {
	h := newHarness(t, harnessOptions{
		aliases: []domain.Alias{chatAlias(local, backup)},
		limiter: allowed(),
		provider: map[string]*fakeProvider{
			"local":  {openErr: domain.NewTransportError("local", errors.New("connection refused"))},
			"backup": {openErr: domain.NewStatusError("backup", http.StatusTooManyRequests, errors.New("slow down"))},
		},
	})

	err := h.service.Chat(context.Background(), testCall, question(), &recordingOutput{})
	if !errors.Is(err, domain.ErrAllProvidersFailed) {
		t.Fatalf("err = %v, want ErrAllProvidersFailed", err)
	}
	if got := len(h.usage.all()); got != 2 {
		t.Fatalf("got %d records, want 2", got)
	}
}

func TestChatOnlyUsesProvidersTheWorkspaceAllows(t *testing.T) {
	providers := func() map[string]*fakeProvider {
		return map[string]*fakeProvider{
			"external": {steps: answer("from outside", nil)},
			"local":    {steps: answer("from inside", nil)},
		}
	}

	t.Run("external provider skipped without approval", func(t *testing.T) {
		h := newHarness(t, harnessOptions{
			aliases:  []domain.Alias{chatAlias(external, local)},
			limiter:  allowed(),
			policy:   domain.NewPolicy(nil),
			provider: providers(),
		})
		out := &recordingOutput{}
		if err := h.service.Chat(context.Background(), testCall, question(), out); err != nil {
			t.Fatal(err)
		}
		if out.began.Provider.Name != "local" || h.providers["external"].callCount() != 0 {
			t.Fatal("content was sent to an external provider the workspace did not allow")
		}
	})

	t.Run("external provider used once allowed", func(t *testing.T) {
		h := newHarness(t, harnessOptions{
			aliases:  []domain.Alias{chatAlias(external, local)},
			limiter:  allowed(),
			policy:   domain.NewPolicy([]string{"external"}),
			provider: providers(),
		})
		out := &recordingOutput{}
		if err := h.service.Chat(context.Background(), testCall, question(), out); err != nil {
			t.Fatal(err)
		}
		if out.began.Provider.Name != "external" {
			t.Fatalf("answered by %s, want the allowed external provider first", out.began.Provider.Name)
		}
	})

	t.Run("no route left", func(t *testing.T) {
		h := newHarness(t, harnessOptions{
			aliases:  []domain.Alias{chatAlias(external)},
			limiter:  allowed(),
			policy:   domain.NewPolicy(nil),
			provider: providers(),
		})
		err := h.service.Chat(context.Background(), testCall, question(), &recordingOutput{})
		if !errors.Is(err, domain.ErrNoAllowedProvider) {
			t.Fatalf("err = %v, want ErrNoAllowedProvider", err)
		}
	})

	t.Run("routes to unconfigured providers are skipped", func(t *testing.T) {
		h := newHarness(t, harnessOptions{
			aliases:  []domain.Alias{chatAlias(backup, local)}, // backup has no adapter configured
			limiter:  allowed(),
			provider: map[string]*fakeProvider{"local": {steps: answer("ok", nil)}},
		})
		out := &recordingOutput{}
		if err := h.service.Chat(context.Background(), testCall, question(), out); err != nil {
			t.Fatal(err)
		}
		if out.began.Provider.Name != "local" {
			t.Fatalf("answered by %s", out.began.Provider.Name)
		}
	})
}

func TestChatRejectsBeforeCallingAnyProvider(t *testing.T) {
	cases := map[string]struct {
		limiter fakeLimiter
		req     domain.ChatRequest
		want    func(error) bool
	}{
		"rate limited": {
			limiter: fakeLimiter{allowed: false, retryAfter: 3 * time.Second},
			req:     question(),
			want: func(err error) bool {
				var limited *domain.RateLimitError
				return errors.As(err, &limited) && limited.RetryAfter == 3*time.Second
			},
		},
		"unknown model": {
			limiter: allowed(),
			req:     domain.ChatRequest{Model: "nope", Messages: question().Messages},
			want:    func(err error) bool { return errors.Is(err, domain.ErrUnknownModel) },
		},
		"no user message": {
			limiter: allowed(),
			req:     domain.ChatRequest{Model: "chat-test", Messages: []domain.Message{{Role: domain.RoleSystem, Content: "x"}}},
			want:    func(err error) bool { return errors.Is(err, domain.ErrInvalidRequest) },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{
				aliases:  []domain.Alias{chatAlias(local)},
				limiter:  tc.limiter,
				provider: map[string]*fakeProvider{"local": {steps: answer("unused", nil)}},
			})
			err := h.service.Chat(context.Background(), testCall, tc.req, &recordingOutput{})
			if !tc.want(err) {
				t.Fatalf("err = %v", err)
			}
			if h.providers["local"].callCount() != 0 || len(h.usage.all()) != 0 {
				t.Fatal("no provider may be called and nothing charged")
			}
		})
	}
}

// --- Embeddings ---

func embedAlias(provider domain.Provider) domain.Alias {
	return domain.Alias{Name: "embed-test", Kind: domain.KindEmbedding, Routes: []domain.Route{{Provider: provider, Model: "embed-model"}}}
}

func TestEmbed(t *testing.T) {
	req := domain.EmbeddingRequest{Model: "embed-test", Inputs: []string{"a", "b"}}

	t.Run("returns one vector per input and records usage", func(t *testing.T) {
		h := newHarness(t, harnessOptions{
			aliases:  []domain.Alias{embedAlias(local)},
			limiter:  allowed(),
			provider: map[string]*fakeProvider{"local": {vectors: -1}},
		})
		result, route, err := h.service.Embed(context.Background(), testCall, req)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Vectors) != 2 || route.Provider.Name != "local" {
			t.Fatalf("result = %+v via %s", result, route.Provider.Name)
		}
		if got := h.usage.all(); len(got) != 1 || got[0].Kind != domain.KindEmbedding || got[0].Usage.InputTokens != 7 {
			t.Fatalf("records = %+v", got)
		}
	})

	t.Run("a wrong number of vectors is an error", func(t *testing.T) {
		h := newHarness(t, harnessOptions{
			aliases:  []domain.Alias{embedAlias(local)},
			limiter:  allowed(),
			provider: map[string]*fakeProvider{"local": {vectors: 1}},
		})
		if _, _, err := h.service.Embed(context.Background(), testCall, req); err == nil {
			t.Fatal("vectors must line up with inputs, or search would attach them to the wrong chunks")
		}
	})

	t.Run("chat aliases are not embedding models", func(t *testing.T) {
		h := newHarness(t, harnessOptions{
			aliases:  []domain.Alias{chatAlias(local)},
			limiter:  allowed(),
			provider: map[string]*fakeProvider{"local": {vectors: -1}},
		})
		_, _, err := h.service.Embed(context.Background(), testCall, domain.EmbeddingRequest{Model: "chat-test", Inputs: []string{"a"}})
		if !errors.Is(err, domain.ErrUnknownModel) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestCatalogRejectsEmbeddingFallback(t *testing.T) {
	alias := embedAlias(local)
	alias.Routes = append(alias.Routes, domain.Route{Provider: backup, Model: "other-embed"})
	if _, err := domain.NewCatalog([]domain.Alias{alias}); err == nil {
		t.Fatal("two embedding models produce incomparable vectors; the catalog must refuse a fallback")
	}
}
