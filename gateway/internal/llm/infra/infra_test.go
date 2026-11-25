package infra

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"enclave/gateway/internal/llm/domain"
	"enclave/internal/platform/testenv"
)

func TestRateLimiterAllowsBurstThenRefills(t *testing.T) {
	limiter := NewRateLimiter(testenv.Redis(t), 600, 3) // 10 per second, burst 3
	workspace := uuid.New()
	ctx := context.Background()

	for i := range 3 {
		if allowed, _, err := limiter.Allow(ctx, workspace); err != nil || !allowed {
			t.Fatalf("request %d: allowed=%v err=%v; the burst must be allowed", i+1, allowed, err)
		}
	}
	allowed, retryAfter, err := limiter.Allow(ctx, workspace)
	if err != nil || allowed {
		t.Fatalf("4th request: allowed=%v err=%v; the bucket should be empty", allowed, err)
	}
	if retryAfter <= 0 || retryAfter > 150*time.Millisecond {
		t.Fatalf("retryAfter = %v, want about 100ms at 10 requests per second", retryAfter)
	}

	time.Sleep(retryAfter + 20*time.Millisecond)
	if allowed, _, _ := limiter.Allow(ctx, workspace); !allowed {
		t.Fatal("a token must be available again after retryAfter")
	}

	if allowed, _, _ := limiter.Allow(ctx, uuid.New()); !allowed {
		t.Fatal("each workspace has its own bucket")
	}
}

func TestRateLimiterIsAtomicUnderConcurrency(t *testing.T) {
	limiter := NewRateLimiter(testenv.Redis(t), 1, 10) // burst 10, practically no refill
	workspace := uuid.New()

	var allowedCount atomic.Int32
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			allowed, _, err := limiter.Allow(context.Background(), workspace)
			if err != nil {
				t.Error(err)
			}
			if allowed {
				allowedCount.Add(1)
			}
		})
	}
	wg.Wait()

	if got := allowedCount.Load(); got != 10 {
		t.Fatalf("%d of 50 concurrent requests were allowed with a burst of 10; read-refill-take must be atomic", got)
	}
}

func createWorkspace(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `INSERT INTO workspaces (name) VALUES ('gateway test') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id = $1`, id) })
	return id
}

func TestPolicies(t *testing.T) {
	pool := testenv.Postgres(t)
	policies := NewPolicies(pool)
	ctx := context.Background()
	nvidia := domain.Provider{Name: "nvidia", External: true}
	ollama := domain.Provider{Name: "ollama"}

	t.Run("unknown workspace", func(t *testing.T) {
		if _, err := policies.Policy(ctx, uuid.New()); !errors.Is(err, domain.ErrWorkspaceNotFound) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("workspace without settings allows only local providers", func(t *testing.T) {
		policy, err := policies.Policy(ctx, createWorkspace(t, pool))
		if err != nil {
			t.Fatal(err)
		}
		if !policy.Allows(ollama) || policy.Allows(nvidia) {
			t.Fatal("default must be local providers only")
		}
	})

	t.Run("admin-approved provider is allowed", func(t *testing.T) {
		workspace := createWorkspace(t, pool)
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_ai_settings (workspace_id, external_providers) VALUES ($1, '{nvidia}')`, workspace); err != nil {
			t.Fatal(err)
		}
		policy, err := policies.Policy(ctx, workspace)
		if err != nil {
			t.Fatal(err)
		}
		if !policy.Allows(nvidia) || policy.Allows(domain.Provider{Name: "anthropic", External: true}) {
			t.Fatal("only the listed external provider may be allowed")
		}
	})
}

func TestUsageRecorder(t *testing.T) {
	pool := testenv.Postgres(t)
	workspace := createWorkspace(t, pool)
	firstToken := 120 * time.Millisecond

	err := NewUsageRecorder(pool).Record(context.Background(), domain.UsageRecord{
		Call:         domain.Call{Caller: "rag", WorkspaceID: workspace, RequestID: "req-usage"},
		Kind:         domain.KindChat,
		Alias:        "chat-fast",
		Route:        domain.Route{Provider: domain.Provider{Name: "ollama"}, Model: "llama3.2:3b"},
		Attempt:      2,
		Status:       domain.AttemptInterrupted,
		ErrorCode:    "provider_unreachable",
		Usage:        domain.Usage{InputTokens: 11, OutputTokens: 22},
		Estimated:    true,
		CostMicroUSD: 33,
		Latency:      2 * time.Second,
		FirstToken:   &firstToken,
	})
	if err != nil {
		t.Fatal(err)
	}

	var (
		status, errorCode     string
		input, output         int
		estimated             bool
		latency, firstTokenMS int
	)
	err = pool.QueryRow(context.Background(), `
		SELECT status, error_code, input_tokens, output_tokens, tokens_estimated, latency_ms, first_token_ms
		FROM llm_usage WHERE request_id = 'req-usage'`,
	).Scan(&status, &errorCode, &input, &output, &estimated, &latency, &firstTokenMS)
	if err != nil {
		t.Fatal(err)
	}
	if status != "interrupted" || errorCode != "provider_unreachable" || input != 11 || output != 22 || !estimated || latency != 2000 || firstTokenMS != 120 {
		t.Fatalf("row = %s %s %d %d %v %d %d", status, errorCode, input, output, estimated, latency, firstTokenMS)
	}
}
