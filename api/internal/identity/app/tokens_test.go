package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"enclave/api/internal/identity/domain"
)

// fakeRevocations is an in-memory RevocationStore.
type fakeRevocations struct {
	mu              sync.Mutex
	revokedFamilies map[string]bool
	usedRefresh     map[string]time.Time
	err             error // when set, every call fails (Redis down)
}

func newFakeRevocations() *fakeRevocations {
	return &fakeRevocations{revokedFamilies: map[string]bool{}, usedRefresh: map[string]time.Time{}}
}

func (f *fakeRevocations) RevokeFamily(_ context.Context, familyID string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokedFamilies[familyID] = true
	return f.err
}

func (f *fakeRevocations) IsFamilyRevoked(_ context.Context, familyID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revokedFamilies[familyID], f.err
}

func (f *fakeRevocations) MarkRefreshUsed(_ context.Context, tokenID string, usedAt time.Time, _ time.Duration) (bool, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, time.Time{}, f.err
	}
	if first, ok := f.usedRefresh[tokenID]; ok {
		return false, first, nil
	}
	f.usedRefresh[tokenID] = usedAt
	return true, usedAt, nil
}

// testService returns a Service with a fake revocation store and a clock the test controls.
func testService(t *testing.T) (*Service, *fakeRevocations, *time.Time) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	revocations := newFakeRevocations()
	s := NewService(nil, revocations, TokenConfig{SigningKey: key, AccessTTL: 15 * time.Minute, RefreshTTL: 7 * 24 * time.Hour})

	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	return s, revocations, &clock
}

func login(t *testing.T, s *Service) (uuid.UUID, TokenPair) {
	t.Helper()
	userID := uuid.New()
	tokens, err := s.issueTokenPair(userID, "alice@example.com", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	return userID, tokens
}

func TestAuthenticateAcceptsValidAccessToken(t *testing.T) {
	s, _, _ := testService(t)
	userID, tokens := login(t, s)

	principal, err := s.Authenticate(context.Background(), tokens.AccessToken)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal.UserID != userID || principal.Email != "alice@example.com" {
		t.Fatalf("principal = %+v", principal)
	}
}

func TestAuthenticateRejectsInvalidTokens(t *testing.T) {
	s, _, clock := testService(t)
	_, tokens := login(t, s)

	other, _, _ := testService(t)
	_, foreign := login(t, other)

	parts := strings.Split(tokens.AccessToken, ".")
	tamperedPayload := parts[0] + "." + parts[1][:len(parts[1])-2] + "AA." + parts[2]

	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"sub": uuid.NewString(), "email": "x@example.com", "token_use": "access", "fid": "f",
		"iss": jwtIssuer, "aud": jwtAudience, "iat": clock.Unix(), "exp": clock.Add(time.Hour).Unix(),
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"garbage", "not-a-jwt"},
		{"refresh token used as access token", tokens.RefreshToken},
		{"signed by another key", foreign.AccessToken},
		{"payload tampered", tamperedPayload},
		{"alg none", unsigned},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Authenticate(context.Background(), tc.token); !errors.Is(err, domain.ErrUnauthenticated) {
				t.Fatalf("err = %v, want ErrUnauthenticated", err)
			}
		})
	}

	t.Run("expired", func(t *testing.T) {
		*clock = clock.Add(15*time.Minute + clockLeeway + time.Second)
		if _, err := s.Authenticate(context.Background(), tokens.AccessToken); !errors.Is(err, domain.ErrUnauthenticated) {
			t.Fatalf("err = %v, want ErrUnauthenticated", err)
		}
	})
}

func TestRefreshRotatesAndDetectsReuse(t *testing.T) {
	s, _, clock := testService(t)
	_, first := login(t, s)

	second, err := s.Refresh(context.Background(), first.RefreshToken)
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if second.RefreshToken == first.RefreshToken || second.AccessToken == first.AccessToken {
		t.Fatal("refresh must issue new tokens")
	}

	// Another tab presenting the same old token a moment later is a race, not theft.
	if _, err := s.Refresh(context.Background(), first.RefreshToken); !errors.Is(err, domain.ErrRefreshConflict) {
		t.Fatalf("reuse within grace: err = %v, want ErrRefreshConflict", err)
	}
	if _, err := s.Authenticate(context.Background(), second.AccessToken); err != nil {
		t.Fatalf("a race must not log the user out: %v", err)
	}

	// The old token showing up long after rotation means someone copied it.
	*clock = clock.Add(refreshReuseGrace + time.Second)
	if _, err := s.Refresh(context.Background(), first.RefreshToken); !errors.Is(err, domain.ErrRefreshReused) {
		t.Fatalf("reuse after grace: err = %v, want ErrRefreshReused", err)
	}
	if _, err := s.Authenticate(context.Background(), second.AccessToken); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatal("reuse must revoke every token of the login")
	}
	if _, err := s.Refresh(context.Background(), second.RefreshToken); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatal("reuse must revoke the newest refresh token too")
	}
}

func TestLogoutRevokesTheLogin(t *testing.T) {
	s, _, clock := testService(t)
	_, tokens := login(t, s)
	_, otherLogin := login(t, s)

	// Even with an expired access token and no refresh token, logout still ends the login.
	*clock = clock.Add(time.Hour)
	if err := s.Logout(context.Background(), "", tokens.AccessToken); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	*clock = clock.Add(-time.Hour)

	if _, err := s.Authenticate(context.Background(), tokens.AccessToken); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatal("access token must be rejected after logout")
	}
	if _, err := s.Refresh(context.Background(), tokens.RefreshToken); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatal("refresh token must be rejected after logout")
	}
	if _, err := s.Authenticate(context.Background(), otherLogin.AccessToken); err != nil {
		t.Fatalf("logout must not affect other logins: %v", err)
	}
	if err := s.Logout(context.Background(), "garbage", ""); err != nil {
		t.Fatalf("logout without a valid token must succeed: %v", err)
	}
}

func TestAuthenticateFailsClosedWhenRevocationListIsDown(t *testing.T) {
	s, revocations, _ := testService(t)
	_, tokens := login(t, s)
	revocations.err = errors.New("redis: connection refused")

	if _, err := s.Authenticate(context.Background(), tokens.AccessToken); err == nil {
		t.Fatal("a valid token must not be accepted when revocations cannot be checked")
	}
}
