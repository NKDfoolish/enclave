package transport

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"enclave/internal/platform/httpserver"
)

type callerKey struct{}

// ServiceAuth admits only internal services that present one of the configured bearer tokens
// and records which service called. The gateway is never exposed to browsers: its callers
// (RAG, agent) have already checked the user's identity and workspace access, and pass the
// workspace in a header.
//
// tokens maps a token to the caller's name. Every token is compared in constant time, so the
// response time does not reveal how much of a guess was right.
func ServiceAuth(tokens map[string]string) httpserver.Middleware {
	type entry struct {
		hash   [32]byte
		caller string
	}
	entries := make([]entry, 0, len(tokens))
	for token, caller := range tokens {
		entries = append(entries, entry{hash: sha256.Sum256([]byte(token)), caller: caller})
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || presented == "" {
				httpserver.WriteError(w, r, http.StatusUnauthorized, "unauthenticated", "a service token is required")
				return
			}
			// Hashing first gives equal-length inputs; ConstantTimeCompare is only
			// constant-time for inputs of the same length.
			presentedHash := sha256.Sum256([]byte(presented))
			caller := ""
			for _, e := range entries {
				if subtle.ConstantTimeCompare(presentedHash[:], e.hash[:]) == 1 {
					caller = e.caller
				}
			}
			if caller == "" {
				httpserver.WriteError(w, r, http.StatusUnauthorized, "unauthenticated", "invalid service token")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, caller)))
		})
	}
}

func callerFrom(ctx context.Context) string {
	caller, _ := ctx.Value(callerKey{}).(string)
	return caller
}
