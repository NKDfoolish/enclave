// Package infra stores users in Postgres and the token revocation list in Redis.
package infra

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"enclave/api/internal/identity/domain"
	"enclave/api/internal/platform/postgres"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) CreateUser(ctx context.Context, user domain.User) (domain.User, error) {
	const query = `
		INSERT INTO users (email, display_name, password_hash)
		VALUES ($1, $2, $3)
		RETURNING id, created_at`

	err := r.pool.QueryRow(ctx, query, user.Email, user.DisplayName, user.PasswordHash).
		Scan(&user.ID, &user.CreatedAt)
	if postgres.IsUniqueViolation(err, "users_email_key") {
		return domain.User{}, domain.ErrEmailTaken
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("insert user: %w", err)
	}
	return user, nil
}

func (r *Repository) UserByEmail(ctx context.Context, email string) (domain.User, error) {
	const query = `
		SELECT id, email, display_name, password_hash, created_at
		FROM users
		WHERE email = $1`

	var user domain.User
	err := r.pool.QueryRow(ctx, query, email).
		Scan(&user.ID, &user.Email, &user.DisplayName, &user.PasswordHash, &user.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.User{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("select user by email: %w", err)
	}
	return user, nil
}
