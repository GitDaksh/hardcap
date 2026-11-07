// Package ratelimit limits how fast each API key can send requests and
// tokens, shared across every gateway instance through Redis.
//
// Each key gets two token buckets: one for requests per minute (RPM) and one
// for tokens per minute (TPM). A bucket holds up to a minute's allowance and
// refills continuously, so a key can burst, then settles to its steady rate.
// Both buckets are checked and charged in one Lua script, which Redis runs
// atomically: no other request can sneak in between the check and the charge.
package ratelimit

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// The script checks both buckets and only charges them if BOTH have room,
// so a request refused for tokens doesn't use up a request slot too.
//
// It reads the clock from Redis (TIME), not from the gateway, so every
// gateway instance agrees on what time it is.
//
// KEYS: requests bucket, tokens bucket.
// ARGV: rpm limit, tpm limit, tokens this request needs. 0 = no limit.
// Returns: {allowed (1/0), retry after in ms, requests left, tokens left,
// reason: "" | "rpm" | "tpm" | "too_large"}.
var script = redis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)

-- Refill a bucket for the time that passed since it was last used.
local function level(key, limit)
  local b = redis.call('HMGET', key, 'level', 'at')
  local lvl, at = tonumber(b[1]), tonumber(b[2])
  if lvl == nil then return limit end -- never used: full
  return math.min(limit, lvl + (now - at) * limit / 60000)
end

local rpm, tpm, cost = tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3])
if tpm > 0 and cost > tpm then
  return {0, 0, 0, 0, 'too_large'} -- would never fit, so waiting won't help
end

local reqLevel, tokLevel = rpm, tpm
local wait, reason = 0, ''
if rpm > 0 then
  reqLevel = level(KEYS[1], rpm)
  if reqLevel < 1 then
    wait = math.ceil((1 - reqLevel) * 60000 / rpm)
    reason = 'rpm'
  end
end
if tpm > 0 then
  tokLevel = level(KEYS[2], tpm)
  if tokLevel < cost then
    local w = math.ceil((cost - tokLevel) * 60000 / tpm)
    if w > wait then wait, reason = w, 'tpm' end
  end
end
if wait > 0 then
  return {0, wait, math.floor(reqLevel), math.floor(tokLevel), reason}
end

-- Both have room: charge them. An idle bucket refills completely within a
-- minute, so after two minutes its key can simply expire.
if rpm > 0 then
  reqLevel = reqLevel - 1
  redis.call('HSET', KEYS[1], 'level', reqLevel, 'at', now)
  redis.call('PEXPIRE', KEYS[1], 120000)
end
if tpm > 0 then
  tokLevel = tokLevel - cost
  redis.call('HSET', KEYS[2], 'level', tokLevel, 'at', now)
  redis.call('PEXPIRE', KEYS[2], 120000)
end
return {1, 0, math.floor(reqLevel), math.floor(tokLevel), ''}
`)

// Decision is the result of one check.
type Decision struct {
	Allowed bool
	// Reason a request was refused: "rpm", "tpm", or "too_large" (it needs
	// more tokens than the key's whole per-minute limit, so it never fits).
	Reason string
	// RetryAfter is how long until the request would fit.
	RetryAfter time.Duration
	// What's left after this request (only meaningful when that limit is on).
	RemainingRequests int64
	RemainingTokens   int64
}

// Limiter checks rate limits in Redis.
type Limiter struct {
	rdb     redis.UniversalClient
	prefix  string
	timeout time.Duration
}

// New returns a Limiter. prefix namespaces its Redis keys ("hardcap:" in
// production; tests use a unique prefix each).
func New(rdb redis.UniversalClient, prefix string) *Limiter {
	return &Limiter{rdb: rdb, prefix: prefix, timeout: 100 * time.Millisecond}
}

// Allow checks and charges one request needing the given tokens. rpm and
// tpm of 0 mean "no limit".
//
// An error means Redis didn't answer in time. The gateway then lets the
// request through ("fails open"): rate limits protect availability, so a
// Redis outage shouldn't become a full outage. Budgets are different: they
// protect money and live in Postgres, so they always fail closed.
func (l *Limiter) Allow(ctx context.Context, keyID int64, rpm, tpm int, tokens int64) (Decision, error) {
	if rpm <= 0 && tpm <= 0 {
		return Decision{Allowed: true}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()

	// The {braces} are a Redis Cluster hash tag: both keys land on the same
	// node, which a multi-key script needs.
	id := strconv.FormatInt(keyID, 10)
	keys := []string{l.prefix + "rl:{" + id + "}:req", l.prefix + "rl:{" + id + "}:tok"}
	res, err := script.Run(ctx, l.rdb, keys, max(rpm, 0), max(tpm, 0), tokens).Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("rate limit: %w", err)
	}
	if len(res) != 5 {
		return Decision{}, fmt.Errorf("rate limit: unexpected reply %v", res)
	}
	allowed, _ := res[0].(int64)
	waitMS, _ := res[1].(int64)
	reqLeft, _ := res[2].(int64)
	tokLeft, _ := res[3].(int64)
	reason, _ := res[4].(string)
	return Decision{
		Allowed:           allowed == 1,
		Reason:            reason,
		RetryAfter:        time.Duration(waitMS) * time.Millisecond,
		RemainingRequests: reqLeft,
		RemainingTokens:   tokLeft,
	}, nil
}
