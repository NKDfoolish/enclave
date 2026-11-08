// Package redis opens the Redis client used for short-lived shared state such as the
// token revocation list.
package redis

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

const pingTimeout = 5 * time.Second

// Connect parses redisURL (e.g. redis://localhost:6380/0) and checks that Redis answers,
// so a wrong REDIS_URL fails at startup rather than on the first request.
func Connect(ctx context.Context, redisURL string) (*goredis.Client, error) {
	options, err := goredis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	client := goredis.NewClient(options)

	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return client, nil
}
