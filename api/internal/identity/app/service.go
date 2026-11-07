// Package app implements the identity use cases: register, log in, refresh tokens, log out
// and authenticate a request.
//
// Login state is a pair of signed JWTs (ADR 0002): a short-lived access token checked on
// every request without touching Postgres, and a longer-lived refresh token that is rotated
// on each use. Redis holds the revocation list: logged-out login families and refresh
// tokens that were already rotated.
package app

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"enclave/api/internal/identity/domain"
	"enclave/api/internal/platform/authn"
	"enclave/api/internal/platform/email"
)

const (
	minPasswordLength    = 10
	maxPasswordLength    = 128
	maxDisplayNameLength = 100

	// refreshReuseGrace: a rotated refresh token seen again within this window is treated as
	// a race between browser tabs, not theft.
	refreshReuseGrace = 10 * time.Second
)

// Repository stores users. Declared next to its consumer so this package does not depend on
// Postgres (ADR 0001).
type Repository interface {
	CreateUser(ctx context.Context, user domain.User) (domain.User, error)
	UserByEmail(ctx context.Context, email string) (domain.User, error)
}

// RevocationStore is the token blacklist.
type RevocationStore interface {
	RevokeFamily(ctx context.Context, familyID string, ttl time.Duration) error
	IsFamilyRevoked(ctx context.Context, familyID string) (bool, error)
	// MarkRefreshUsed atomically records the first use of a refresh token. It reports whether
	// this call was the first use and, if not, when the first use happened.
	MarkRefreshUsed(ctx context.Context, tokenID string, usedAt time.Time, ttl time.Duration) (firstUse bool, firstUsedAt time.Time, err error)
}

type TokenConfig struct {
	SigningKey ed25519.PrivateKey
	AccessTTL  time.Duration
	RefreshTTL time.Duration
}

type Service struct {
	users       Repository
	revocations RevocationStore
	signingKey  ed25519.PrivateKey
	accessTTL   time.Duration
	refreshTTL  time.Duration
	now         func() time.Time
}

func NewService(users Repository, revocations RevocationStore, tokens TokenConfig) *Service {
	return &Service{
		users:       users,
		revocations: revocations,
		signingKey:  tokens.SigningKey,
		accessTTL:   tokens.AccessTTL,
		refreshTTL:  tokens.RefreshTTL,
		now:         time.Now,
	}
}

type RegisterInput struct {
	Email       string
	DisplayName string
	Password    string
}

func (s *Service) Register(ctx context.Context, in RegisterInput) (domain.User, error) {
	address, ok := email.Normalize(in.Email)
	if !ok {
		return domain.User{}, invalidInput("email is not a valid address")
	}
	displayName := strings.TrimSpace(in.DisplayName)
	if displayName == "" || utf8.RuneCountInString(displayName) > maxDisplayNameLength {
		return domain.User{}, invalidInput(fmt.Sprintf("display name must be 1-%d characters", maxDisplayNameLength))
	}
	passwordLength := utf8.RuneCountInString(in.Password)
	if passwordLength < minPasswordLength || passwordLength > maxPasswordLength {
		return domain.User{}, invalidInput(fmt.Sprintf("password must be %d-%d characters", minPasswordLength, maxPasswordLength))
	}

	user, err := s.users.CreateUser(ctx, domain.User{
		Email:        address,
		DisplayName:  displayName,
		PasswordHash: hashPassword(in.Password),
	})
	if err != nil {
		return domain.User{}, fmt.Errorf("create user: %w", err)
	}
	return user, nil
}

// dummyPasswordHash is checked when the email is unknown, so a failed login takes about as
// long whether or not the account exists and timing does not reveal registered emails.
var dummyPasswordHash = sync.OnceValue(func() string {
	return hashPassword("dummy password used only to equalise login timing")
})

// Login checks the password and starts a new login family.
func (s *Service) Login(ctx context.Context, rawEmail, password string) (domain.User, TokenPair, error) {
	address, ok := email.Normalize(rawEmail)
	if !ok {
		return domain.User{}, TokenPair{}, domain.ErrInvalidCredentials
	}

	user, err := s.users.UserByEmail(ctx, address)
	if errors.Is(err, domain.ErrNotFound) {
		_, _ = verifyPassword(password, dummyPasswordHash())
		return domain.User{}, TokenPair{}, domain.ErrInvalidCredentials
	}
	if err != nil {
		return domain.User{}, TokenPair{}, fmt.Errorf("load user: %w", err)
	}

	matches, err := verifyPassword(password, user.PasswordHash)
	if err != nil {
		return domain.User{}, TokenPair{}, fmt.Errorf("verify password for user %s: %w", user.ID, err)
	}
	if !matches {
		return domain.User{}, TokenPair{}, domain.ErrInvalidCredentials
	}

	tokens, err := s.issueTokenPair(user.ID, user.Email, uuid.NewString())
	if err != nil {
		return domain.User{}, TokenPair{}, err
	}
	return user, tokens, nil
}

// Authenticate verifies an access token. It needs no database query: the signature proves
// who the user is, and Redis only says whether the login was revoked.
func (s *Service) Authenticate(ctx context.Context, accessToken string) (authn.Principal, error) {
	claims, err := s.parseToken(accessToken, tokenUseAccess)
	if err != nil {
		return authn.Principal{}, err
	}
	if err := s.ensureFamilyActive(ctx, claims.FamilyID); err != nil {
		return authn.Principal{}, err
	}

	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return authn.Principal{}, domain.ErrUnauthenticated
	}
	return authn.Principal{UserID: userID, Email: claims.Email}, nil
}

// Refresh exchanges a refresh token for a new token pair and retires the old refresh token
// (rotation). Seeing a retired token again after the grace period means it was copied, so
// the whole login family is revoked.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (TokenPair, error) {
	claims, err := s.parseToken(refreshToken, tokenUseRefresh)
	if err != nil {
		return TokenPair{}, err
	}
	if err := s.ensureFamilyActive(ctx, claims.FamilyID); err != nil {
		return TokenPair{}, err
	}

	now := s.now()
	firstUse, firstUsedAt, err := s.revocations.MarkRefreshUsed(ctx, claims.ID, now, remainingLifetime(claims, now))
	if err != nil {
		return TokenPair{}, fmt.Errorf("mark refresh token used: %w", err)
	}
	if !firstUse {
		if now.Sub(firstUsedAt) < refreshReuseGrace {
			return TokenPair{}, domain.ErrRefreshConflict
		}
		if err := s.revocations.RevokeFamily(ctx, claims.FamilyID, s.refreshTTL); err != nil {
			return TokenPair{}, fmt.Errorf("revoke family after refresh reuse: %w", err)
		}
		return TokenPair{}, domain.ErrRefreshReused
	}

	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return TokenPair{}, domain.ErrUnauthenticated
	}
	return s.issueTokenPair(userID, claims.Email, claims.FamilyID)
}

// Logout revokes the login family of whichever token the client still has. Logging out
// without a valid token is not an error.
func (s *Service) Logout(ctx context.Context, refreshToken, accessToken string) error {
	familyID, ok := s.familyOf(refreshToken)
	if !ok {
		familyID, ok = s.familyOf(accessToken)
	}
	if !ok {
		return nil
	}
	// Every token of the family expires within refreshTTL, so the entry can expire then too.
	if err := s.revocations.RevokeFamily(ctx, familyID, s.refreshTTL); err != nil {
		return fmt.Errorf("revoke family: %w", err)
	}
	return nil
}

// ensureFamilyActive fails closed: if the revocation list cannot be read, the request is
// rejected rather than trusted.
func (s *Service) ensureFamilyActive(ctx context.Context, familyID string) error {
	revoked, err := s.revocations.IsFamilyRevoked(ctx, familyID)
	if err != nil {
		return fmt.Errorf("check revoked family: %w", err)
	}
	if revoked {
		return domain.ErrUnauthenticated
	}
	return nil
}

// remainingLifetime is how long a blacklist entry for this token must live: until the token
// would have expired anyway.
func remainingLifetime(claims tokenClaims, now time.Time) time.Duration {
	remaining := claims.ExpiresAt.Sub(now) + clockLeeway
	if remaining < time.Second {
		return time.Second
	}
	return remaining
}

func invalidInput(message string) error {
	return fmt.Errorf("%w: %s", domain.ErrInvalidInput, message)
}
