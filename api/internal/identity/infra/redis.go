package infra

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// RevocationStore is the token blacklist in Redis. Every key has a TTL equal to the lifetime
// of the tokens it blocks, so the list cleans itself up and never grows without bound.
type RevocationStore struct {
	client *goredis.Client
}

func NewRevocationStore(client *goredis.Client) *RevocationStore {
	return &RevocationStore{client: client}
}

func revokedFamilyKey(familyID string) string { return "auth:revoked-family:" + familyID }
func usedRefreshKey(tokenID string) string    { return "auth:used-refresh:" + tokenID }

func (s *RevocationStore) RevokeFamily(ctx context.Context, familyID string, ttl time.Duration) error {
	if err := s.client.Set(ctx, revokedFamilyKey(familyID), "1", ttl).Err(); err != nil {
		return fmt.Errorf("set revoked family: %w", err)
	}
	return nil
}

func (s *RevocationStore) IsFamilyRevoked(ctx context.Context, familyID string) (bool, error) {
	count, err := s.client.Exists(ctx, revokedFamilyKey(familyID)).Result()
	if err != nil {
		return false, fmt.Errorf("check revoked family: %w", err)
	}
	return count > 0, nil
}

// MarkRefreshUsed uses SET NX GET, a single atomic command: it stores the use time only if
// the key is absent and returns the previous value otherwise. Two concurrent refreshes with
// the same token therefore cannot both be "first".
func (s *RevocationStore) MarkRefreshUsed(ctx context.Context, tokenID string, usedAt time.Time, ttl time.Duration) (bool, time.Time, error) {
	previous, err := s.client.SetArgs(ctx, usedRefreshKey(tokenID), usedAt.UnixMilli(), goredis.SetArgs{
		Mode: "NX",
		TTL:  ttl,
		Get:  true,
	}).Result()
	if errors.Is(err, goredis.Nil) {
		return true, usedAt, nil // key was absent: this is the first use
	}
	if err != nil {
		return false, time.Time{}, fmt.Errorf("mark refresh token used: %w", err)
	}

	firstUsedMillis, err := strconv.ParseInt(previous, 10, 64)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("parse first use time %q: %w", previous, err)
	}
	return false, time.UnixMilli(firstUsedMillis), nil
}
