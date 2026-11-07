// Package transport exposes the identity use cases over HTTP and provides the middleware
// that turns an access-token cookie into an authenticated principal.
package transport

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"enclave/api/internal/identity/app"
	"enclave/api/internal/identity/domain"
	"enclave/api/internal/platform/authn"
	"enclave/api/internal/platform/httpserver"
)

const (
	accessCookieName  = "enclave_access"
	refreshCookieName = "enclave_refresh"
	// The refresh token is only ever sent to the auth endpoints, never with ordinary API
	// calls, which keeps the long-lived credential off most requests.
	refreshCookiePath = "/v1/auth"
)

type Handler struct {
	service       *app.Service
	logger        *slog.Logger
	secureCookies bool
}

// NewHandler builds the identity HTTP handlers. secureCookies must be true whenever the API
// is served over HTTPS; it is false only for local development over plain HTTP.
func NewHandler(service *app.Service, logger *slog.Logger, secureCookies bool) *Handler {
	return &Handler{service: service, logger: logger, secureCookies: secureCookies}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/auth/register", h.register)
	mux.HandleFunc("POST /v1/auth/login", h.login)
	mux.HandleFunc("POST /v1/auth/refresh", h.refresh)
	mux.HandleFunc("POST /v1/auth/logout", h.logout)
	mux.Handle("GET /v1/me", h.RequireAuth(http.HandlerFunc(h.me)))
}

// RequireAuth rejects requests without a valid access token and stores the authenticated
// user in the request context for the handlers behind it.
func (h *Handler) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, err := h.service.Authenticate(r.Context(), cookieValue(r, accessCookieName))
		if err != nil {
			h.writeError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(authn.WithPrincipal(r.Context(), principal)))
	})
}

type userResponse struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
}

func newUserResponse(u domain.User) userResponse {
	return userResponse{ID: u.ID.String(), Email: u.Email, DisplayName: u.DisplayName, CreatedAt: u.CreatedAt}
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
	}
	if err := httpserver.ReadJSON(w, r, &body); err != nil {
		httpserver.WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	user, err := h.service.Register(r.Context(), app.RegisterInput{
		Email:       body.Email,
		DisplayName: body.DisplayName,
		Password:    body.Password,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusCreated, newUserResponse(user))
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := httpserver.ReadJSON(w, r, &body); err != nil {
		httpserver.WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	user, tokens, err := h.service.Login(r.Context(), body.Email, body.Password)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.setTokenCookies(w, tokens)
	httpserver.WriteJSON(w, http.StatusOK, newUserResponse(user))
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	tokens, err := h.service.Refresh(r.Context(), cookieValue(r, refreshCookieName))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.setTokenCookies(w, tokens)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	err := h.service.Logout(r.Context(), cookieValue(r, refreshCookieName), cookieValue(r, accessCookieName))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.clearTokenCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	principal, _ := authn.PrincipalFrom(r.Context())
	httpserver.WriteJSON(w, http.StatusOK, map[string]string{
		"id":    principal.UserID.String(),
		"email": principal.Email,
	})
}

func (h *Handler) setTokenCookies(w http.ResponseWriter, tokens app.TokenPair) {
	http.SetCookie(w, h.cookie(accessCookieName, "/", tokens.AccessToken, tokens.AccessExpiresAt))
	http.SetCookie(w, h.cookie(refreshCookieName, refreshCookiePath, tokens.RefreshToken, tokens.RefreshExpiresAt))
}

func (h *Handler) clearTokenCookies(w http.ResponseWriter) {
	for _, c := range []*http.Cookie{
		h.cookie(accessCookieName, "/", "", time.Time{}),
		h.cookie(refreshCookieName, refreshCookiePath, "", time.Time{}),
	} {
		c.MaxAge = -1 // tells the browser to delete the cookie now
		http.SetCookie(w, c)
	}
}

func (h *Handler) cookie(name, path, value string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		Expires:  expires,
		HttpOnly: true, // not readable from JavaScript, so XSS cannot steal the token
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode, // not sent on cross-site POST/PATCH/DELETE (CSRF)
	}
}

func cookieValue(r *http.Request, name string) string {
	cookie, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return cookie.Value
}

// writeError maps identity errors to HTTP responses. Unexpected errors are logged here,
// once, and the client only sees a generic message.
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidInput):
		httpserver.WriteError(w, r, http.StatusBadRequest, "invalid_input", err.Error())
	case errors.Is(err, domain.ErrEmailTaken):
		httpserver.WriteError(w, r, http.StatusConflict, "email_taken", "email is already registered")
	case errors.Is(err, domain.ErrInvalidCredentials):
		httpserver.WriteError(w, r, http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
	case errors.Is(err, domain.ErrUnauthenticated):
		httpserver.WriteError(w, r, http.StatusUnauthorized, "unauthenticated", "login required")
	case errors.Is(err, domain.ErrRefreshConflict):
		httpserver.WriteError(w, r, http.StatusConflict, "refresh_conflict", "tokens were just refreshed; retry the request")
	case errors.Is(err, domain.ErrRefreshReused):
		h.logger.WarnContext(r.Context(), "refresh token reuse detected; login revoked",
			slog.String("request_id", httpserver.RequestIDFrom(r.Context())),
		)
		h.clearTokenCookies(w)
		httpserver.WriteError(w, r, http.StatusUnauthorized, "unauthenticated", "login required")
	default:
		h.logger.ErrorContext(r.Context(), "identity request failed",
			slog.String("request_id", httpserver.RequestIDFrom(r.Context())),
			slog.Any("err", err),
		)
		httpserver.WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}
