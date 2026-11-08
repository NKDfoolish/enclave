package app

import (
	"crypto/ed25519"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"enclave/api/internal/identity/domain"
)

const (
	jwtIssuer   = "enclave-api"
	jwtAudience = "enclave"

	tokenUseAccess  = "access"
	tokenUseRefresh = "refresh"

	// clockLeeway tolerates small clock differences when other services verify tokens.
	clockLeeway = 10 * time.Second
)

// tokenClaims is the JWT payload. Both token types share it; TokenUse tells them apart so a
// refresh token can never be used as an access token or the other way round.
type tokenClaims struct {
	jwt.RegisteredClaims
	Email    string `json:"email"`
	TokenUse string `json:"token_use"`
	// FamilyID links every token issued from one login. Revoking the family logs that login
	// out everywhere.
	FamilyID string `json:"fid"`
}

// TokenPair is what the client stores after login or refresh.
type TokenPair struct {
	AccessToken      string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
}

func (s *Service) issueTokenPair(userID uuid.UUID, email, familyID string) (TokenPair, error) {
	now := s.now()
	access, accessExpiresAt, err := s.signToken(userID, email, tokenUseAccess, familyID, now, s.accessTTL)
	if err != nil {
		return TokenPair{}, err
	}
	refresh, refreshExpiresAt, err := s.signToken(userID, email, tokenUseRefresh, familyID, now, s.refreshTTL)
	if err != nil {
		return TokenPair{}, err
	}
	return TokenPair{
		AccessToken:      access,
		AccessExpiresAt:  accessExpiresAt,
		RefreshToken:     refresh,
		RefreshExpiresAt: refreshExpiresAt,
	}, nil
}

func (s *Service) signToken(userID uuid.UUID, email, tokenUse, familyID string, now time.Time, ttl time.Duration) (string, time.Time, error) {
	expiresAt := now.Add(ttl)
	claims := tokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(),
			Subject:   userID.String(),
			Issuer:    jwtIssuer,
			Audience:  jwt.ClaimStrings{jwtAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
		Email:    email,
		TokenUse: tokenUse,
		FamilyID: familyID,
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(s.signingKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign %s token: %w", tokenUse, err)
	}
	return signed, expiresAt, nil
}

// parseToken verifies the signature, issuer, audience, expiry and token use. Any failure is
// reported as ErrUnauthenticated; the reason is not returned because the client cannot act on it.
func (s *Service) parseToken(raw, wantUse string) (tokenClaims, error) {
	var claims tokenClaims
	_, err := jwt.ParseWithClaims(raw, &claims, s.publicKeyFunc,
		// Accepting only EdDSA blocks "alg confusion" attacks such as alg=none or HS256
		// signed with the public key.
		jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
		jwt.WithIssuer(jwtIssuer),
		jwt.WithAudience(jwtAudience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(clockLeeway),
		jwt.WithTimeFunc(s.now),
	)
	if err != nil || claims.TokenUse != wantUse || claims.FamilyID == "" {
		return tokenClaims{}, domain.ErrUnauthenticated
	}
	return claims, nil
}

// familyOf returns the family of a token signed by us, even if it has expired. Logout uses
// it so that an expired access token can still end its login.
func (s *Service) familyOf(raw string) (string, bool) {
	var claims tokenClaims
	_, err := jwt.ParseWithClaims(raw, &claims, s.publicKeyFunc,
		jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
		jwt.WithoutClaimsValidation(),
	)
	if err != nil || claims.FamilyID == "" {
		return "", false
	}
	return claims.FamilyID, true
}

func (s *Service) publicKeyFunc(*jwt.Token) (any, error) {
	return s.signingKey.Public().(ed25519.PublicKey), nil
}
