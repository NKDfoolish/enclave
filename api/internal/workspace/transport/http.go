// Package transport exposes the workspace use cases over HTTP. Every route requires an
// access token; the caller comes from the authenticated principal in the request context.
package transport

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"enclave/api/internal/platform/authn"
	"enclave/api/internal/platform/httpserver"
	"enclave/api/internal/workspace/app"
	"enclave/api/internal/workspace/domain"
)

type Handler struct {
	service *app.Service
	logger  *slog.Logger
}

func NewHandler(service *app.Service, logger *slog.Logger) *Handler {
	return &Handler{service: service, logger: logger}
}

// RegisterRoutes adds the workspace routes, each wrapped in requireAuth.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, requireAuth httpserver.Middleware) {
	routes := map[string]http.HandlerFunc{
		"POST /v1/workspaces":                                            h.createWorkspace,
		"GET /v1/workspaces":                                             h.listWorkspaces,
		"GET /v1/workspaces/{workspaceID}":                               h.getWorkspace,
		"GET /v1/workspaces/{workspaceID}/members":                       h.listMembers,
		"PATCH /v1/workspaces/{workspaceID}/members/{userID}":            h.changeRole,
		"DELETE /v1/workspaces/{workspaceID}/members/{userID}":           h.removeMember,
		"POST /v1/workspaces/{workspaceID}/invitations":                  h.invite,
		"GET /v1/workspaces/{workspaceID}/invitations":                   h.listInvitations,
		"DELETE /v1/workspaces/{workspaceID}/invitations/{invitationID}": h.revokeInvitation,
		"POST /v1/invitations/accept":                                    h.acceptInvitation,
	}
	for pattern, handler := range routes {
		mux.Handle(pattern, requireAuth(handler))
	}
}

type workspaceResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

func newWorkspaceResponse(uw domain.UserWorkspace) workspaceResponse {
	return workspaceResponse{
		ID:        uw.Workspace.ID.String(),
		Name:      uw.Workspace.Name,
		Role:      string(uw.Role),
		CreatedAt: uw.Workspace.CreatedAt,
	}
}

type memberResponse struct {
	UserID      string    `json:"user_id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	JoinedAt    time.Time `json:"joined_at"`
}

type invitationResponse struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func newInvitationResponse(inv domain.Invitation) invitationResponse {
	return invitationResponse{
		ID:        inv.ID.String(),
		Email:     inv.Email,
		Role:      string(inv.Role),
		CreatedAt: inv.CreatedAt,
		ExpiresAt: inv.ExpiresAt,
	}
}

func (h *Handler) createWorkspace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := httpserver.ReadJSON(w, r, &body); err != nil {
		httpserver.WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	workspace, err := h.service.CreateWorkspace(r.Context(), caller(r), body.Name)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusCreated, newWorkspaceResponse(workspace))
}

func (h *Handler) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	workspaces, err := h.service.ListWorkspaces(r.Context(), caller(r))
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	response := make([]workspaceResponse, 0, len(workspaces))
	for _, uw := range workspaces {
		response = append(response, newWorkspaceResponse(uw))
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"workspaces": response})
}

func (h *Handler) getWorkspace(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := pathID(w, r, "workspaceID")
	if !ok {
		return
	}

	workspace, err := h.service.GetWorkspace(r.Context(), caller(r), workspaceID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, newWorkspaceResponse(workspace))
}

func (h *Handler) listMembers(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := pathID(w, r, "workspaceID")
	if !ok {
		return
	}

	members, err := h.service.ListMembers(r.Context(), caller(r), workspaceID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	response := make([]memberResponse, 0, len(members))
	for _, m := range members {
		response = append(response, memberResponse{
			UserID:      m.UserID.String(),
			Email:       m.Email,
			DisplayName: m.DisplayName,
			Role:        string(m.Role),
			JoinedAt:    m.JoinedAt,
		})
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"members": response})
}

func (h *Handler) changeRole(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := pathID(w, r, "workspaceID")
	if !ok {
		return
	}
	userID, ok := pathID(w, r, "userID")
	if !ok {
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if err := httpserver.ReadJSON(w, r, &body); err != nil {
		httpserver.WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	if err := h.service.ChangeRole(r.Context(), caller(r), workspaceID, userID, body.Role); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := pathID(w, r, "workspaceID")
	if !ok {
		return
	}
	userID, ok := pathID(w, r, "userID")
	if !ok {
		return
	}

	if err := h.service.RemoveMember(r.Context(), caller(r), workspaceID, userID); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) invite(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := pathID(w, r, "workspaceID")
	if !ok {
		return
	}
	var body struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := httpserver.ReadJSON(w, r, &body); err != nil {
		httpserver.WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	result, err := h.service.Invite(r.Context(), caller(r), workspaceID, body.Email, body.Role)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	// No email service yet, so the admin receives the token and shares the link.
	// The token sits in the URL fragment, which browsers never send to the server,
	// so it does not end up in access logs.
	httpserver.WriteJSON(w, http.StatusCreated, map[string]any{
		"invitation": newInvitationResponse(result.Invitation),
		"token":      result.Token,
		"accept_url": "/#accept=" + result.Token,
	})
}

func (h *Handler) listInvitations(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := pathID(w, r, "workspaceID")
	if !ok {
		return
	}

	invitations, err := h.service.ListInvitations(r.Context(), caller(r), workspaceID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	response := make([]invitationResponse, 0, len(invitations))
	for _, inv := range invitations {
		response = append(response, newInvitationResponse(inv))
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"invitations": response})
}

func (h *Handler) revokeInvitation(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := pathID(w, r, "workspaceID")
	if !ok {
		return
	}
	invitationID, ok := pathID(w, r, "invitationID")
	if !ok {
		return
	}

	if err := h.service.RevokeInvitation(r.Context(), caller(r), workspaceID, invitationID); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := httpserver.ReadJSON(w, r, &body); err != nil {
		httpserver.WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	workspace, err := h.service.AcceptInvitation(r.Context(), caller(r), body.Token)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, newWorkspaceResponse(workspace))
}

// caller returns the authenticated user. RegisterRoutes wraps every route in
// requireAuth, so the principal is always present here.
func caller(r *http.Request) authn.Principal {
	principal, _ := authn.PrincipalFrom(r.Context())
	return principal
}

// pathID parses a UUID path parameter. A malformed ID gets the same 404 as an unknown one,
// so probing IDs reveals nothing.
func pathID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		httpserver.WriteError(w, r, http.StatusNotFound, "not_found", "not found")
		return uuid.UUID{}, false
	}
	return id, true
}

// writeError maps workspace errors to HTTP responses. Unexpected errors are logged once here.
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidInput):
		httpserver.WriteError(w, r, http.StatusBadRequest, "invalid_input", err.Error())
	case errors.Is(err, domain.ErrNotFound):
		httpserver.WriteError(w, r, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, domain.ErrForbidden):
		httpserver.WriteError(w, r, http.StatusForbidden, "forbidden", "your role does not allow this action")
	case errors.Is(err, domain.ErrAlreadyMember):
		httpserver.WriteError(w, r, http.StatusConflict, "already_member", "user is already a member")
	case errors.Is(err, domain.ErrInvitationPending):
		httpserver.WriteError(w, r, http.StatusConflict, "invitation_pending", "an invitation for this email is already pending")
	case errors.Is(err, domain.ErrLastAdmin):
		httpserver.WriteError(w, r, http.StatusConflict, "last_admin", "workspace must keep at least one admin")
	case errors.Is(err, domain.ErrInvitationInvalid):
		httpserver.WriteError(w, r, http.StatusNotFound, "invitation_invalid", "invitation is invalid, expired or already used")
	case errors.Is(err, domain.ErrInvitationEmailMismatch):
		httpserver.WriteError(w, r, http.StatusForbidden, "invitation_email_mismatch", "log in with the email address the invitation was sent to")
	default:
		h.logger.ErrorContext(r.Context(), "workspace request failed",
			slog.String("request_id", httpserver.RequestIDFrom(r.Context())),
			slog.Any("err", err),
		)
		httpserver.WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}
