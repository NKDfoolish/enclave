// Package infra holds the gateway's storage: workspace AI policies and usage records in
// Postgres, and the per-workspace rate limit in Redis. Provider adapters live in the
// openai and anthropic subpackages.
package infra

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"enclave/gateway/internal/llm/domain"
)

// Policies reads workspace_ai_settings, which the Core API owns and writes. The gateway only
// reads it, on every call, so an admin's change applies to the very next model call.
type Policies struct {
	pool *pgxpool.Pool
}

func NewPolicies(pool *pgxpool.Pool) *Policies {
	return &Policies{pool: pool}
}

func (p *Policies) Policy(ctx context.Context, workspaceID uuid.UUID) (domain.Policy, error) {
	// LEFT JOIN: a workspace without settings exists but allows no external provider; a
	// missing workspace returns no row at all.
	var external []string
	err := p.pool.QueryRow(ctx, `
		SELECT coalesce(s.external_providers, '{}')
		FROM workspaces w
		LEFT JOIN workspace_ai_settings s ON s.workspace_id = w.id
		WHERE w.id = $1`, workspaceID,
	).Scan(&external)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Policy{}, domain.ErrWorkspaceNotFound
	}
	if err != nil {
		return domain.Policy{}, fmt.Errorf("select ai settings: %w", err)
	}
	return domain.NewPolicy(external), nil
}

// UsageRecorder writes llm_usage rows.
type UsageRecorder struct {
	pool *pgxpool.Pool
}

func NewUsageRecorder(pool *pgxpool.Pool) *UsageRecorder {
	return &UsageRecorder{pool: pool}
}

func (u *UsageRecorder) Record(ctx context.Context, record domain.UsageRecord) error {
	var firstTokenMS *int64
	if record.FirstToken != nil {
		ms := record.FirstToken.Milliseconds()
		firstTokenMS = &ms
	}
	var errorCode *string
	if record.ErrorCode != "" {
		errorCode = &record.ErrorCode
	}

	_, err := u.pool.Exec(ctx, `
		INSERT INTO llm_usage (
			workspace_id, caller, request_id, kind, model_alias, provider, provider_model,
			attempt, status, error_code, input_tokens, output_tokens, tokens_estimated,
			cost_microusd, latency_ms, first_token_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		record.Call.WorkspaceID, record.Call.Caller, record.Call.RequestID, record.Kind,
		record.Alias, record.Route.Provider.Name, record.Route.Model,
		record.Attempt, record.Status, errorCode, record.Usage.InputTokens, record.Usage.OutputTokens,
		record.Estimated, record.CostMicroUSD, record.Latency.Milliseconds(), firstTokenMS,
	)
	if err != nil {
		return fmt.Errorf("insert usage record: %w", err)
	}
	return nil
}
