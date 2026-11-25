package transport

import (
	"net/http"
	"time"

	"enclave/api/internal/workspace/domain"
	"enclave/internal/platform/httpserver"
)

type aiSettingsResponse struct {
	ExternalProviders []string `json:"external_providers"`
	// AvailableExternalProviders lets the UI offer every provider an admin could allow.
	AvailableExternalProviders []string `json:"available_external_providers"`
	// UpdatedAt is null while the workspace still uses the default.
	UpdatedAt *time.Time `json:"updated_at"`
}

func newAISettingsResponse(settings domain.AISettings) aiSettingsResponse {
	response := aiSettingsResponse{
		ExternalProviders:          providerNames(settings.ExternalProviders),
		AvailableExternalProviders: providerNames(domain.ExternalProviders()),
	}
	if !settings.UpdatedAt.IsZero() {
		response.UpdatedAt = &settings.UpdatedAt
	}
	return response
}

func providerNames(providers []domain.ExternalProvider) []string {
	names := make([]string, len(providers))
	for i, provider := range providers {
		names[i] = string(provider)
	}
	return names
}

func (h *Handler) getAISettings(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := pathID(w, r, "workspaceID")
	if !ok {
		return
	}

	settings, err := h.service.AISettings(r.Context(), caller(r), workspaceID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, newAISettingsResponse(settings))
}

func (h *Handler) updateAISettings(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := pathID(w, r, "workspaceID")
	if !ok {
		return
	}
	var body struct {
		ExternalProviders []string `json:"external_providers"`
	}
	if err := httpserver.ReadJSON(w, r, &body); err != nil {
		httpserver.WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	settings, err := h.service.UpdateAISettings(r.Context(), caller(r), workspaceID, body.ExternalProviders)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, newAISettingsResponse(settings))
}
