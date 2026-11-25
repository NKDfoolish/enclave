package infra

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
)

// tokenBucket refills a workspace's bucket for the time since its last request, then takes
// one token if there is one. Running it as a Lua script makes read-refill-take one atomic
// step, so concurrent requests through several gateway instances cannot both take the last
// token. It reads the clock from Redis (TIME) so instances with skewed clocks agree.
//
// KEYS[1] bucket key. ARGV[1] capacity (burst), ARGV[2] refill rate in tokens per second.
// Returns {allowed (1/0), milliseconds until a token is available}.
var tokenBucket = goredis.NewScript(`
local capacity = tonumber(ARGV[1])
local rate = tonumber(ARGV[2]) / 1000 -- tokens per millisecond
local time = redis.call('TIME')
local now = tonumber(time[1]) * 1000 + math.floor(tonumber(time[2]) / 1000)

local state = redis.call('HMGET', KEYS[1], 'tokens', 'at')
local tokens = tonumber(state[1])
local at = tonumber(state[2])
if tokens == nil then
  tokens = capacity
  at = now
end
tokens = math.min(capacity, tokens + math.max(0, now - at) * rate)

local allowed = 0
local wait = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
else
  wait = math.ceil((1 - tokens) / rate)
end

redis.call('HSET', KEYS[1], 'tokens', tostring(tokens), 'at', now)
-- An idle bucket refills completely; after that its state is the default and can expire.
redis.call('PEXPIRE', KEYS[1], math.ceil(capacity / rate) + 1000)
return {allowed, wait}
`)

// RateLimiter is a token bucket per workspace: up to Burst requests at once, refilled at
// PerMinute requests per minute.
type RateLimiter struct {
	client    *goredis.Client
	burst     int
	perMinute int
}

func NewRateLimiter(client *goredis.Client, perMinute, burst int) *RateLimiter {
	return &RateLimiter{client: client, burst: burst, perMinute: perMinute}
}

func (l *RateLimiter) Allow(ctx context.Context, workspaceID uuid.UUID) (bool, time.Duration, error) {
	key := "gateway:ratelimit:" + workspaceID.String()
	perSecond := float64(l.perMinute) / 60
	result, err := tokenBucket.Run(ctx, l.client, []string{key}, l.burst, perSecond).Int64Slice()
	if err != nil {
		return false, 0, fmt.Errorf("run token bucket: %w", err)
	}
	return result[0] == 1, time.Duration(result[1]) * time.Millisecond, nil
}
