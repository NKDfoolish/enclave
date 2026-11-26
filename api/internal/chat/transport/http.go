// Package transport exposes conversations over HTTP. Asking a question answers with
// Server-Sent Events so the browser shows the answer while it is written:
//
//	event: sources   {"sources": [{number, document_id, title, page, section, snippet}]}
//	event: delta     {"text": "..."}            (many)
//	event: done      {"message": {...}}         the stored answer with its citations
//	event: error     {"error": {"code", "message"}}  if the answer fails after it started
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"enclave/api/internal/authn"
	"enclave/api/internal/chat/app"
	"enclave/api/internal/chat/domain"
	"enclave/internal/platform/httpserver"
)

type Handler struct {
	service *app.Service
	logger  *slog.Logger
}

func NewHandler(service *app.Service, logger *slog.Logger) *Handler {
	return &Handler{service: service, logger: logger}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, requireAuth httpserver.Middleware) {
	const base = "/v1/workspaces/{workspaceID}/conversations"
	routes := map[string]http.HandlerFunc{
		"POST " + base:                                h.start,
		"GET " + base:                                 h.list,
		"GET " + base + "/{conversationID}":           h.get,
		"DELETE " + base + "/{conversationID}":        h.delete,
		"POST " + base + "/{conversationID}/messages": h.ask,
	}
	for pattern, handler := range routes {
		mux.Handle(pattern, requireAuth(handler))
	}
}

type conversationResponse struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func newConversationResponse(c domain.Conversation) conversationResponse {
	return conversationResponse{ID: c.ID.String(), Title: c.Title, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

type citationResponse struct {
	Number     int    `json:"number"`
	DocumentID string `json:"document_id"`
	Title      string `json:"title"`
	Page       int    `json:"page,omitempty"`
	Section    string `json:"section,omitempty"`
	Snippet    string `json:"snippet,omitempty"`
}

func newCitationResponses(citations []domain.Citation) []citationResponse {
	responses := make([]citationResponse, 0, len(citations))
	for _, c := range citations {
		responses = append(responses, citationResponse{
			Number: c.Number, DocumentID: c.DocumentID.String(), Title: c.Title,
			Page: c.Page, Section: c.Section, Snippet: c.Snippet,
		})
	}
	return responses
}

// hiddenAnswer replaces the text of an answer whose sources the reader can no longer read.
const hiddenAnswer = "This answer is hidden because one of its sources is no longer available to you."

type messageResponse struct {
	ID        int64              `json:"id"`
	Role      string             `json:"role"`
	Content   string             `json:"content"`
	Citations []citationResponse `json:"citations"`
	Grounded  bool               `json:"grounded"`
	Hidden    bool               `json:"hidden"`
	CreatedAt time.Time          `json:"created_at"`
}

func newMessageResponse(m domain.Message) messageResponse {
	response := messageResponse{
		ID: m.ID, Role: string(m.Role), Content: m.Content, Citations: newCitationResponses(m.Citations),
		Grounded: m.Grounded, Hidden: m.Hidden, CreatedAt: m.CreatedAt,
	}
	if m.Hidden {
		response.Content = hiddenAnswer
		response.Citations = []citationResponse{}
	}
	return response
}

func (h *Handler) start(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := pathID(w, r, "workspaceID")
	if !ok {
		return
	}
	conversation, err := h.service.StartConversation(r.Context(), caller(r), workspaceID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusCreated, newConversationResponse(conversation))
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := pathID(w, r, "workspaceID")
	if !ok {
		return
	}
	conversations, err := h.service.ListConversations(r.Context(), caller(r), workspaceID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	response := make([]conversationResponse, 0, len(conversations))
	for _, c := range conversations {
		response = append(response, newConversationResponse(c))
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"conversations": response})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	workspaceID, conversationID, ok := pathIDs(w, r)
	if !ok {
		return
	}
	conversation, messages, err := h.service.GetConversation(r.Context(), caller(r), workspaceID, conversationID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	response := make([]messageResponse, 0, len(messages))
	for _, m := range messages {
		response = append(response, newMessageResponse(m))
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{
		"conversation": newConversationResponse(conversation),
		"messages":     response,
	})
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	workspaceID, conversationID, ok := pathIDs(w, r)
	if !ok {
		return
	}
	if err := h.service.DeleteConversation(r.Context(), caller(r), workspaceID, conversationID); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) ask(w http.ResponseWriter, r *http.Request) {
	workspaceID, conversationID, ok := pathIDs(w, r)
	if !ok {
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := httpserver.ReadJSON(w, r, &body); err != nil {
		httpserver.WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	out := &sseOutput{w: w, controller: http.NewResponseController(w)}
	answer, err := h.service.Ask(r.Context(), caller(r), workspaceID, conversationID, body.Content,
		httpserver.RequestIDFrom(r.Context()), out)
	switch {
	case err == nil:
		_ = out.event("done", map[string]any{"message": newMessageResponse(answer)})
	case !out.started:
		h.writeError(w, r, err)
	case r.Context().Err() != nil:
		// The browser left; nobody is reading.
	default:
		code, message := errorCode(err)
		h.logError(r, err)
		_ = out.event("error", map[string]any{"error": map[string]string{"code": code, "message": message}})
	}
}

// sseOutput writes the answer as Server-Sent Events, flushing each one.
type sseOutput struct {
	w          http.ResponseWriter
	controller *http.ResponseController
	started    bool
}

func (o *sseOutput) Sources(sources []domain.Citation) error {
	return o.event("sources", map[string]any{"sources": newCitationResponses(sources)})
}

func (o *sseOutput) Delta(text string) error {
	return o.event("delta", map[string]string{"text": text})
}

func (o *sseOutput) event(name string, data any) error {
	if !o.started {
		header := o.w.Header()
		header.Set("Content-Type", "text/event-stream")
		header.Set("Cache-Control", "no-cache")
		header.Set("X-Accel-Buffering", "no")
		o.w.WriteHeader(http.StatusOK)
		o.started = true
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", name, err)
	}
	if _, err := fmt.Fprintf(o.w, "event: %s\ndata: %s\n\n", name, encoded); err != nil {
		return err
	}
	return o.controller.Flush()
}

// errorCode maps errors to a stable code and a message safe to show.
func errorCode(err error) (code, message string) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return "not_found", "not found"
	case errors.Is(err, domain.ErrInvalidQuestion):
		return "invalid_question", domain.ErrInvalidQuestion.Error()
	case errors.Is(err, domain.ErrNoModelAllowed):
		return "no_model_allowed", domain.ErrNoModelAllowed.Error()
	case errors.Is(err, domain.ErrAnswerUnavailable), errors.Is(err, context.DeadlineExceeded):
		return "answer_unavailable", domain.ErrAnswerUnavailable.Error()
	default:
		return "internal_error", "internal server error"
	}
}

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return // the caller left
	}
	code, message := errorCode(err)
	status := map[string]int{
		"not_found":          http.StatusNotFound,
		"invalid_question":   http.StatusBadRequest,
		"no_model_allowed":   http.StatusConflict,
		"answer_unavailable": http.StatusServiceUnavailable,
		"internal_error":     http.StatusInternalServerError,
	}[code]
	if status >= http.StatusInternalServerError {
		h.logError(r, err)
	}
	httpserver.WriteError(w, r, status, code, message)
}

func (h *Handler) logError(r *http.Request, err error) {
	h.logger.ErrorContext(r.Context(), "chat request failed",
		slog.String("request_id", httpserver.RequestIDFrom(r.Context())),
		slog.Any("err", err),
	)
}

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

func pathIDs(w http.ResponseWriter, r *http.Request) (workspaceID, conversationID uuid.UUID, ok bool) {
	if workspaceID, ok = pathID(w, r, "workspaceID"); !ok {
		return
	}
	conversationID, ok = pathID(w, r, "conversationID")
	return
}
