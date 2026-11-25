package app

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"enclave/api/internal/authn"
	"enclave/api/internal/workspace/domain"
)

// AISettings returns the workspace's AI policy to admins and staff.
func (s *Service) AISettings(ctx context.Context, caller authn.Principal, workspaceID uuid.UUID) (domain.AISettings, error) {
	membership, err := s.membership(ctx, caller, workspaceID)
	if err != nil {
		return domain.AISettings{}, err
	}
	if !membership.Role.CanViewAISettings() {
		return domain.AISettings{}, domain.ErrForbidden
	}

	settings, err := s.repo.AISettings(ctx, workspaceID)
	if err != nil {
		return domain.AISettings{}, fmt.Errorf("load ai settings of workspace %s: %w", workspaceID, err)
	}
	return settings, nil
}

// UpdateAISettings replaces the list of external providers allowed to receive the workspace's
// content. An empty list sends everything back to local providers only. The gateway reads the
// policy on every call, so a removal takes effect on the next model call.
func (s *Service) UpdateAISettings(ctx context.Context, caller authn.Principal, workspaceID uuid.UUID, rawProviders []string) (domain.AISettings, error) {
	membership, err := s.membership(ctx, caller, workspaceID)
	if err != nil {
		return domain.AISettings{}, err
	}
	if !membership.Role.CanManageAISettings() {
		return domain.AISettings{}, domain.ErrForbidden
	}

	providers, err := parseExternalProviders(rawProviders)
	if err != nil {
		return domain.AISettings{}, err
	}
	settings, err := s.repo.SaveAISettings(ctx, workspaceID, providers, caller.UserID)
	if err != nil {
		return domain.AISettings{}, fmt.Errorf("save ai settings of workspace %s: %w", workspaceID, err)
	}
	return settings, nil
}

// parseExternalProviders validates every name and returns them sorted without duplicates.
func parseExternalProviders(rawProviders []string) ([]domain.ExternalProvider, error) {
	providers := make([]domain.ExternalProvider, 0, len(rawProviders))
	for _, raw := range rawProviders {
		provider, ok := domain.ParseExternalProvider(raw)
		if !ok {
			return nil, invalidInput(fmt.Sprintf("unknown provider %q; known providers: %s", raw, knownProviderNames()))
		}
		providers = append(providers, provider)
	}
	slices.Sort(providers)
	return slices.Compact(providers), nil
}

func knownProviderNames() string {
	var names []string
	for _, provider := range domain.ExternalProviders() {
		names = append(names, string(provider))
	}
	return strings.Join(names, ", ")
}
