// Package transport exposes the document library over HTTP. Every route requires an access
// token; files themselves never pass through these handlers (presigned URLs, ADR 0003).
package transport

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"enclave/api/internal/document/app"
	"enclave/api/internal/document/domain"
	"enclave/api/internal/platform/authn"
	"enclave/api/internal/platform/httpserver"
)

type Handler struct {
	service *app.Service
	logger  *slog.Logger
}

func NewHandler(service *app.Service, logger *slog.Logger) *Handler {
	return &Handler{service: service, logger: logger}
}

// RegisterRoutes adds the document routes, each wrapped in requireAuth.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, requireAuth httpserver.Middleware) {
	const base = "/v1/workspaces/{workspaceID}/documents"
	routes := map[string]http.HandlerFunc{
		"POST " + base:                            h.createUpload,
		"GET " + base:                             h.list,
		"GET " + base + "/{documentID}":           h.get,
		"PATCH " + base + "/{documentID}":         h.update,
		"DELETE " + base + "/{documentID}":        h.delete,
		"POST " + base + "/{documentID}/complete": h.completeUpload,
		"GET " + base + "/{documentID}/download":  h.download,
	}
	for pattern, handler := range routes {
		mux.Handle(pattern, requireAuth(handler))
	}
}

type documentResponse struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	Filename      string    `json:"filename"`
	ContentType   string    `json:"content_type"`
	SizeBytes     int64     `json:"size_bytes"`
	Visibility    string    `json:"visibility"`
	Status        string    `json:"status"`
	FailureReason string    `json:"failure_reason,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func newDocumentResponse(d domain.Document) documentResponse {
	return documentResponse{
		ID:            d.ID.String(),
		Title:         d.Title,
		Filename:      d.Filename,
		ContentType:   string(d.ContentType),
		SizeBytes:     d.SizeBytes,
		Visibility:    string(d.Visibility),
		Status:        string(d.Status),
		FailureReason: d.FailureReason,
		CreatedAt:     d.CreatedAt,
		UpdatedAt:     d.UpdatedAt,
	}
}

type uploadResponse struct {
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt time.Time         `json:"expires_at"`
}

func (h *Handler) createUpload(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := pathID(w, r, "workspaceID")
	if !ok {
		return
	}
	var body struct {
		Title     string `json:"title"`
		Filename  string `json:"filename"`
		SizeBytes int64  `json:"size_bytes"`
	}
	if err := httpserver.ReadJSON(w, r, &body); err != nil {
		httpserver.WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	ticket, err := h.service.CreateUpload(r.Context(), caller(r), workspaceID, app.CreateUploadInput{
		Title:     body.Title,
		Filename:  body.Filename,
		SizeBytes: body.SizeBytes,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusCreated, map[string]any{
		"document": newDocumentResponse(ticket.Document),
		"upload": uploadResponse{
			URL:       ticket.Upload.URL,
			Method:    ticket.Upload.Method,
			Headers:   ticket.Upload.Headers,
			ExpiresAt: ticket.Upload.ExpiresAt,
		},
	})
}

func (h *Handler) completeUpload(w http.ResponseWriter, r *http.Request) {
	workspaceID, documentID, ok := pathIDs(w, r)
	if !ok {
		return
	}
	doc, err := h.service.CompleteUpload(r.Context(), caller(r), workspaceID, documentID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	// 202: accepted, the content check runs in the background.
	httpserver.WriteJSON(w, http.StatusAccepted, newDocumentResponse(doc))
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := pathID(w, r, "workspaceID")
	if !ok {
		return
	}
	limit, err := optionalInt(r.URL.Query().Get("limit"))
	if err != nil {
		httpserver.WriteError(w, r, http.StatusBadRequest, "invalid_input", "limit must be a number")
		return
	}

	page, err := h.service.List(r.Context(), caller(r), workspaceID, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	documents := make([]documentResponse, 0, len(page.Documents))
	for _, d := range page.Documents {
		documents = append(documents, newDocumentResponse(d))
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{
		"documents":   documents,
		"next_cursor": page.NextCursor,
	})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	workspaceID, documentID, ok := pathIDs(w, r)
	if !ok {
		return
	}
	doc, err := h.service.Get(r.Context(), caller(r), workspaceID, documentID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, newDocumentResponse(doc))
}

// download checks access, then redirects the browser to a 60-second presigned URL. The file
// is served from the storage origin, never from the app's origin.
func (h *Handler) download(w http.ResponseWriter, r *http.Request) {
	workspaceID, documentID, ok := pathIDs(w, r)
	if !ok {
		return
	}
	url, err := h.service.DownloadURL(r.Context(), caller(r), workspaceID, documentID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store") // never reuse a redirect after access changes
	http.Redirect(w, r, url, http.StatusFound)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	workspaceID, documentID, ok := pathIDs(w, r)
	if !ok {
		return
	}
	var body struct {
		Title      *string `json:"title"`
		Visibility *string `json:"visibility"`
	}
	if err := httpserver.ReadJSON(w, r, &body); err != nil {
		httpserver.WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	doc, err := h.service.Update(r.Context(), caller(r), workspaceID, documentID, app.UpdateInput{
		Title:      body.Title,
		Visibility: body.Visibility,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, newDocumentResponse(doc))
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	workspaceID, documentID, ok := pathIDs(w, r)
	if !ok {
		return
	}
	if err := h.service.Delete(r.Context(), caller(r), workspaceID, documentID); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// caller returns the authenticated user; RegisterRoutes wraps every route in requireAuth.
func caller(r *http.Request) authn.Principal {
	principal, _ := authn.PrincipalFrom(r.Context())
	return principal
}

// pathID parses a UUID path parameter; malformed IDs get the same 404 as unknown ones.
func pathID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		httpserver.WriteError(w, r, http.StatusNotFound, "not_found", "not found")
		return uuid.UUID{}, false
	}
	return id, true
}

func pathIDs(w http.ResponseWriter, r *http.Request) (workspaceID, documentID uuid.UUID, ok bool) {
	if workspaceID, ok = pathID(w, r, "workspaceID"); !ok {
		return
	}
	documentID, ok = pathID(w, r, "documentID")
	return
}

func optionalInt(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.Atoi(s)
}

// writeError maps document errors to HTTP responses. Unexpected errors are logged once here.
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidInput):
		httpserver.WriteError(w, r, http.StatusBadRequest, "invalid_input", err.Error())
	case errors.Is(err, domain.ErrNotFound):
		httpserver.WriteError(w, r, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, domain.ErrForbidden):
		httpserver.WriteError(w, r, http.StatusForbidden, "forbidden", "your role does not allow this action")
	case errors.Is(err, domain.ErrUnsupportedType):
		httpserver.WriteError(w, r, http.StatusUnsupportedMediaType, "unsupported_type", "only PDF, Markdown and DOCX files are accepted")
	case errors.Is(err, domain.ErrTooLarge):
		httpserver.WriteError(w, r, http.StatusRequestEntityTooLarge, "too_large", "file is larger than the allowed size")
	case errors.Is(err, domain.ErrNotReady):
		httpserver.WriteError(w, r, http.StatusConflict, "not_ready", "document is still being processed")
	case errors.Is(err, domain.ErrUploadIncomplete):
		httpserver.WriteError(w, r, http.StatusConflict, "upload_incomplete", "the file has not been fully uploaded")
	case errors.Is(err, domain.ErrWrongStatus):
		httpserver.WriteError(w, r, http.StatusConflict, "wrong_status", "this action does not fit the document's current status")
	default:
		h.logger.ErrorContext(r.Context(), "document request failed",
			slog.String("request_id", httpserver.RequestIDFrom(r.Context())),
			slog.Any("err", err),
		)
		httpserver.WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}
