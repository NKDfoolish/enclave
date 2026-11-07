// Package domain holds the identity model: users and the errors of logging in and staying
// logged in.
package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

type User struct {
	ID          uuid.UUID
	Email       string
	DisplayName string
	// PasswordHash is an argon2id hash in PHC string format, never the password itself.
	PasswordHash string
	CreatedAt    time.Time
}

var (
	ErrInvalidInput       = errors.New("invalid input")
	ErrEmailTaken         = errors.New("email is already registered")
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrUnauthenticated    = errors.New("not authenticated")
	ErrNotFound           = errors.New("not found")
	// ErrRefreshConflict: the refresh token was rotated a moment ago, typically by another
	// browser tab. The new cookies are already set, so the client should simply retry.
	ErrRefreshConflict = errors.New("refresh token was just rotated")
	// ErrRefreshReused: a refresh token that was rotated long ago came back. Only a copy can
	// do that, so the whole login is revoked as a precaution.
	ErrRefreshReused = errors.New("refresh token reuse detected")
)
