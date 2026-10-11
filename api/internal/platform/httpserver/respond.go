package httpserver

import (
	"encoding/json"
	"net/http"
)

// ErrorBody is the JSON shape of every error response:
//
//	{"error": {"code": "rag_unavailable", "message": "rag service unavailable", "request_id": "..."}}
//
// Code is stable for clients to branch on; Message is for humans and must not leak internals.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// WriteJSON writes v as a JSON response. Headers must be set before WriteHeader is called.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes a standard error body that includes the request ID for support and log lookup.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	WriteJSON(w, status, ErrorBody{Error: ErrorDetail{
		Code:      code,
		Message:   message,
		RequestID: RequestIDFrom(r.Context()),
	}})
}
