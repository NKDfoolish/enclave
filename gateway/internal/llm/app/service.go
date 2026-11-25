// Package app runs model calls: it resolves an alias to its routes, keeps only the routes
// the workspace allows, applies the rate limit, then tries the routes in order. It falls
// back to the next route only while nothing has reached the caller yet, and records the
// usage and cost of every attempt.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"enclave/gateway/internal/llm/domain"
)

// ChatStream is a provider's answer while it is being generated.
type ChatStream interface {
	// Next blocks until the next chunk, and returns io.EOF once the answer is complete.
	Next() (domain.Chunk, error)
	// Close releases the connection. Closing before io.EOF stops the generation.
	Close() error
}

// ChatProvider is a provider adapter for chat. StreamChat returns once the provider has
// accepted the request; failures before that are returned as *domain.ProviderError.
type ChatProvider interface {
	StreamChat(ctx context.Context, model string, req domain.ChatRequest) (ChatStream, error)
}

// Embedder is a provider adapter for embeddings.
type Embedder interface {
	Embed(ctx context.Context, model string, req domain.EmbeddingRequest) (domain.Embeddings, error)
}

// Policies reads a workspace's AI settings, or returns domain.ErrWorkspaceNotFound.
type Policies interface {
	Policy(ctx context.Context, workspaceID uuid.UUID) (domain.Policy, error)
}

// RateLimiter takes one request from the workspace's budget. When the budget is empty it
// returns allowed=false and how long until a request will be accepted again.
type RateLimiter interface {
	Allow(ctx context.Context, workspaceID uuid.UUID) (allowed bool, retryAfter time.Duration, err error)
}

// UsageRecorder stores one usage record per provider call.
type UsageRecorder interface {
	Record(ctx context.Context, record domain.UsageRecord) error
}

// ChatOutput receives the answer. Begin is called exactly once, before the first chunk, with
// the route that answers. From then on the gateway is committed to that route: an error can
// only be reported to the caller mid-answer, never retried on another provider.
type ChatOutput interface {
	Begin(route domain.Route) error
	Write(chunk domain.Chunk) error
}

type Options struct {
	Catalog       domain.Catalog
	ChatProviders map[string]ChatProvider
	Embedders     map[string]Embedder
	Policies      Policies
	Limiter       RateLimiter
	Usage         UsageRecorder
	Logger        *slog.Logger
	// FirstTokenTimeout bounds the wait for a provider's first output. It is short enough
	// that a stuck provider leaves time to fall back to the next route.
	FirstTokenTimeout time.Duration
	// RequestTimeout bounds a whole request, fallbacks included.
	RequestTimeout time.Duration
}

type Service struct {
	catalog           domain.Catalog
	chatProviders     map[string]ChatProvider
	embedders         map[string]Embedder
	policies          Policies
	limiter           RateLimiter
	usage             UsageRecorder
	logger            *slog.Logger
	firstTokenTimeout time.Duration
	requestTimeout    time.Duration
	now               func() time.Time
}

func NewService(opts Options) *Service {
	return &Service{
		catalog:           opts.Catalog,
		chatProviders:     opts.ChatProviders,
		embedders:         opts.Embedders,
		policies:          opts.Policies,
		limiter:           opts.Limiter,
		usage:             opts.Usage,
		logger:            opts.Logger,
		firstTokenTimeout: opts.FirstTokenTimeout,
		requestTimeout:    opts.RequestTimeout,
		now:               time.Now,
	}
}

// Catalog returns the aliases the gateway serves.
func (s *Service) Catalog() domain.Catalog {
	return s.catalog
}

// Chat answers req through the first route that works and writes the answer to out.
func (s *Service) Chat(ctx context.Context, call domain.Call, req domain.ChatRequest, out ChatOutput) error {
	if err := req.Validate(); err != nil {
		return err
	}
	alias, err := s.catalog.Alias(req.Model, domain.KindChat)
	if err != nil {
		return err
	}
	routes, err := s.allowedRoutes(ctx, call.WorkspaceID, alias, s.hasChatProvider)
	if err != nil {
		return err
	}
	if err := s.takeRateLimit(ctx, call.WorkspaceID); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, s.requestTimeout)
	defer cancel()

	var lastErr error
	for i, route := range routes {
		began, err := s.chatAttempt(ctx, call, alias, route, i+1, req, out)
		if err == nil {
			return nil
		}
		// After output began, or when the whole request is over (caller gone, request
		// timeout), another route cannot help.
		if began || ctx.Err() != nil || !domain.IsRetryable(err) {
			return err
		}
		lastErr = err
		if i+1 < len(routes) {
			s.logger.WarnContext(ctx, "falling back to next provider",
				slog.String("request_id", call.RequestID),
				slog.String("model", alias.Name),
				slog.String("failed_provider", route.Provider.Name),
				slog.String("next_provider", routes[i+1].Provider.Name),
				slog.Any("err", err),
			)
		}
	}
	return fmt.Errorf("%w: %w", domain.ErrAllProvidersFailed, lastErr)
}

// chatAttempt runs req on one route and reports whether any output reached the caller.
// It records exactly one usage record, however the attempt ends.
func (s *Service) chatAttempt(ctx context.Context, call domain.Call, alias domain.Alias, route domain.Route, attempt int, req domain.ChatRequest, out ChatOutput) (began bool, err error) {
	started := s.now()
	counter := newTokenCounter(req)
	var firstToken *time.Duration
	defer func() {
		usage, estimated := counter.usage(began)
		s.record(ctx, domain.UsageRecord{
			Call:         call,
			Kind:         domain.KindChat,
			Alias:        alias.Name,
			Route:        route,
			Attempt:      attempt,
			Status:       attemptStatus(ctx, began, err),
			ErrorCode:    errorCode(err),
			Usage:        usage,
			Estimated:    estimated,
			CostMicroUSD: route.Price.Cost(usage),
			Latency:      s.now().Sub(started),
			FirstToken:   firstToken,
		})
	}()

	// The first-token timer cancels the attempt (not the whole request) with a cause, so a
	// silent provider can be told apart from the caller hanging up.
	attemptCtx, cancelAttempt := context.WithCancelCause(ctx)
	defer cancelAttempt(nil)
	firstTokenTimer := time.AfterFunc(s.firstTokenTimeout, func() { cancelAttempt(domain.ErrFirstTokenTimeout) })
	defer firstTokenTimer.Stop()

	stream, err := s.chatProviders[route.Provider.Name].StreamChat(attemptCtx, route.Model, req)
	if err != nil {
		return false, explainCancel(attemptCtx, err)
	}
	defer func() { _ = stream.Close() }()

	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return began, explainCancel(attemptCtx, err)
		}
		if !began {
			firstTokenTimer.Stop()
			elapsed := s.now().Sub(started)
			firstToken = &elapsed
			began = true
			if err := out.Begin(route); err != nil {
				return true, err
			}
		}
		counter.add(chunk)
		if err := out.Write(chunk); err != nil {
			return true, err
		}
	}

	if !began { // the provider finished without a single chunk: an empty answer
		began = true
		if err := out.Begin(route); err != nil {
			return true, err
		}
	}
	return true, nil
}

// Embed embeds req.Inputs through the alias's single route.
func (s *Service) Embed(ctx context.Context, call domain.Call, req domain.EmbeddingRequest) (domain.Embeddings, domain.Route, error) {
	if err := req.Validate(); err != nil {
		return domain.Embeddings{}, domain.Route{}, err
	}
	alias, err := s.catalog.Alias(req.Model, domain.KindEmbedding)
	if err != nil {
		return domain.Embeddings{}, domain.Route{}, err
	}
	routes, err := s.allowedRoutes(ctx, call.WorkspaceID, alias, s.hasEmbedder)
	if err != nil {
		return domain.Embeddings{}, domain.Route{}, err
	}
	if err := s.takeRateLimit(ctx, call.WorkspaceID); err != nil {
		return domain.Embeddings{}, domain.Route{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, s.requestTimeout)
	defer cancel()

	route := routes[0] // embedding aliases have exactly one route (see domain.NewCatalog)
	started := s.now()
	result, err := s.embedders[route.Provider.Name].Embed(ctx, route.Model, req)
	if err == nil && len(result.Vectors) != len(req.Inputs) {
		err = fmt.Errorf("provider %s returned %d vectors for %d inputs", route.Provider.Name, len(result.Vectors), len(req.Inputs))
	}

	usage := domain.Usage{InputTokens: result.InputTokens}
	estimated := false
	if err == nil && usage.InputTokens == 0 { // answered without reporting usage
		usage.InputTokens = domain.EstimateTokens(totalRunes(req.Inputs))
		estimated = true
	}
	s.record(ctx, domain.UsageRecord{
		Call:         call,
		Kind:         domain.KindEmbedding,
		Alias:        alias.Name,
		Route:        route,
		Attempt:      1,
		Status:       attemptStatus(ctx, false, err),
		ErrorCode:    errorCode(err),
		Usage:        usage,
		Estimated:    estimated,
		CostMicroUSD: route.Price.Cost(usage),
		Latency:      s.now().Sub(started),
	})
	if err != nil {
		return domain.Embeddings{}, domain.Route{}, err
	}
	return result, route, nil
}

// allowedRoutes keeps the alias's routes whose provider is configured on this gateway and
// allowed by the workspace's AI settings, in catalog order.
func (s *Service) allowedRoutes(ctx context.Context, workspaceID uuid.UUID, alias domain.Alias, isConfigured func(string) bool) ([]domain.Route, error) {
	policy, err := s.policies.Policy(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("load ai policy: %w", err)
	}
	var routes []domain.Route
	for _, route := range alias.Routes {
		if isConfigured(route.Provider.Name) && policy.Allows(route.Provider) {
			routes = append(routes, route)
		}
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("%w: no route of %q is both configured and allowed by the workspace's AI settings", domain.ErrNoAllowedProvider, alias.Name)
	}
	return routes, nil
}

func (s *Service) takeRateLimit(ctx context.Context, workspaceID uuid.UUID) error {
	allowed, retryAfter, err := s.limiter.Allow(ctx, workspaceID)
	if err != nil {
		// Fail closed: without the limiter nothing stops one workspace from running up cost.
		return fmt.Errorf("check rate limit: %w", err)
	}
	if !allowed {
		return &domain.RateLimitError{RetryAfter: retryAfter}
	}
	return nil
}

func (s *Service) hasChatProvider(name string) bool {
	_, ok := s.chatProviders[name]
	return ok
}

func (s *Service) hasEmbedder(name string) bool {
	_, ok := s.embedders[name]
	return ok
}

// record stores a usage record even when the caller has gone away: the provider may already
// have charged for the tokens. It logs the attempt without any prompt or answer text.
func (s *Service) record(ctx context.Context, record domain.UsageRecord) {
	level := slog.LevelInfo
	if record.Status != domain.AttemptOK {
		level = slog.LevelWarn
	}
	s.logger.Log(ctx, level, "model call",
		slog.String("request_id", record.Call.RequestID),
		slog.String("caller", record.Call.Caller),
		slog.String("workspace_id", record.Call.WorkspaceID.String()),
		slog.String("model", record.Alias),
		slog.String("provider", record.Route.Provider.Name),
		slog.String("provider_model", record.Route.Model),
		slog.Int("attempt", record.Attempt),
		slog.String("status", string(record.Status)),
		slog.String("error_code", record.ErrorCode),
		slog.Int("input_tokens", record.Usage.InputTokens),
		slog.Int("output_tokens", record.Usage.OutputTokens),
		slog.Bool("tokens_estimated", record.Estimated),
		slog.Int64("latency_ms", record.Latency.Milliseconds()),
	)

	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.usage.Record(recordCtx, record); err != nil {
		s.logger.ErrorContext(ctx, "store usage record", slog.String("request_id", record.Call.RequestID), slog.Any("err", err))
	}
}
