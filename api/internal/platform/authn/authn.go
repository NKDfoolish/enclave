// Package authn carries the authenticated caller through a request context. The identity
// context stores the principal after validating the session; other contexts only read it,
// so they never depend on how authentication works.
package authn

import (
	"context"

	"github.com/google/uuid"
)

// Principal is the authenticated user making the request.
type Principal struct {
	UserID uuid.UUID
	Email  string
}

type ctxKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFrom returns the authenticated user, or false if the request is anonymous.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}
