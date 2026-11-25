package domain

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ProviderError is a failed provider call, already classified. Adapters create it so the
// app layer can decide on fallback without knowing any provider's status codes.
type ProviderError struct {
	Provider string
	// StatusCode is the provider's HTTP status, or 0 when the request never got an answer
	// (connection refused, DNS, timeout).
	StatusCode int
	// Retryable: another provider may well succeed. False when the request itself is at
	// fault, which no fallback can fix.
	Retryable bool
	Err       error
}

func (e *ProviderError) Error() string {
	if e.StatusCode == 0 {
		return fmt.Sprintf("provider %s: %v", e.Provider, e.Err)
	}
	return fmt.Sprintf("provider %s: status %d: %v", e.Provider, e.StatusCode, e.Err)
}

func (e *ProviderError) Unwrap() error { return e.Err }

// NewStatusError classifies a non-success HTTP status from a provider.
//
// A provider answering 401/403/404 is a problem with that provider (an expired key, a model it
// has retired), not with the caller's request, so another provider may still answer. Only
// statuses that blame the request itself (400, 413, 422) are not worth a fallback.
func NewStatusError(provider string, statusCode int, err error) *ProviderError {
	retryable := true
	switch statusCode {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		retryable = false
	}
	return &ProviderError{Provider: provider, StatusCode: statusCode, Retryable: retryable, Err: err}
}

// NewTransportError wraps a failure to get any answer from the provider at all.
func NewTransportError(provider string, err error) *ProviderError {
	return &ProviderError{Provider: provider, Retryable: true, Err: err}
}

// IsRetryable reports whether err is worth trying on the next route.
func IsRetryable(err error) bool {
	if errors.Is(err, ErrFirstTokenTimeout) {
		return true
	}
	var providerErr *ProviderError
	return errors.As(err, &providerErr) && providerErr.Retryable
}

// ErrFirstTokenTimeout: a provider accepted the request but sent nothing in time. The gateway
// cancels it and may try the next route.
var ErrFirstTokenTimeout = errors.New("provider sent no output before the first-token timeout")

// RateLimitError: the workspace has used up its request budget for now.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limit exceeded; retry after %s", e.RetryAfter)
}
