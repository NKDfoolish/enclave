package infra

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"enclave/api/internal/workspace/domain"
)

// AISettings returns the workspace's AI policy. A workspace without a row uses the default:
// no external providers.
func (r *Repository) AISettings(ctx context.Context, workspaceID uuid.UUID) (domain.AISettings, error) {
	var (
		providers []string
		updatedAt time.Time
	)
	err := r.pool.QueryRow(ctx,
		`SELECT external_providers, updated_at FROM workspace_ai_settings WHERE workspace_id = $1`, workspaceID,
	).Scan(&providers, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.AISettings{ExternalProviders: []domain.ExternalProvider{}}, nil
	}
	if err != nil {
		return domain.AISettings{}, fmt.Errorf("select ai settings: %w", err)
	}
	return newAISettings(providers, updatedAt), nil
}

// SaveAISettings creates or replaces the workspace's AI policy.
func (r *Repository) SaveAISettings(ctx context.Context, workspaceID uuid.UUID, providers []domain.ExternalProvider, updatedBy uuid.UUID) (domain.AISettings, error) {
	names := make([]string, len(providers))
	for i, provider := range providers {
		names[i] = string(provider)
	}

	var (
		saved     []string
		updatedAt time.Time
	)
	err := r.pool.QueryRow(ctx, `
		INSERT INTO workspace_ai_settings (workspace_id, external_providers, updated_by)
		VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id) DO UPDATE
		SET external_providers = EXCLUDED.external_providers,
		    updated_by = EXCLUDED.updated_by,
		    updated_at = now()
		RETURNING external_providers, updated_at`,
		workspaceID, names, updatedBy,
	).Scan(&saved, &updatedAt)
	if err != nil {
		return domain.AISettings{}, fmt.Errorf("upsert ai settings: %w", err)
	}
	return newAISettings(saved, updatedAt), nil
}

func newAISettings(names []string, updatedAt time.Time) domain.AISettings {
	providers := make([]domain.ExternalProvider, len(names))
	for i, name := range names {
		providers[i] = domain.ExternalProvider(name)
	}
	return domain.AISettings{ExternalProviders: providers, UpdatedAt: updatedAt}
}
