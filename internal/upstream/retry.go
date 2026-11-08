package upstream

import (
	"errors"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Verdict is what a provider's answer means for retrying.
type Verdict int

const (
	// Success: a 2xx reply. Relay it to the client.
	Success Verdict = iota
	// ClientError: a 4xx other than 429. The request itself is bad, so
	// retrying won't help and the provider is healthy. Pass the error on.
	ClientError
	// Retryable: a 429 or 5xx reply, or the connection was never made. In
	// each case the provider generated nothing, so trying again (or trying
	// another provider) can't double-spend.
	Retryable
	// Ambiguous: the request may have reached the provider and started
	// generating (a timeout, a dropped connection). Retrying could pay for
	// the same work twice and blow past the hold, so Hardcap doesn't retry,
	// and charges the full hold.
	Ambiguous
)

func (v Verdict) String() string {
	switch v {
	case Success:
		return "success"
	case ClientError:
		return "client_error"
	case Retryable:
		return "retryable"
	default:
		return "ambiguous"
	}
}

// Classify decides what a provider's reply (or the error from trying to get
// one) means.
func Classify(resp *http.Response, err error) Verdict {
	if err != nil {
		if neverSent(err) {
			return Retryable
		}
		return Ambiguous
	}
	switch code := resp.StatusCode; {
	case code >= 200 && code < 300:
		return Success
	case code == http.StatusTooManyRequests || code >= 500:
		return Retryable
	default:
		return ClientError
	}
}

// neverSent reports whether err proves the request never reached the
// provider: the name didn't resolve, or the connection was refused.
func neverSent(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// Backoff returns how long to wait before retry number attempt (1 for the
// first retry): an exponentially growing ceiling with "full jitter", a
// random wait between 0 and the ceiling. The randomness stops a crowd of
// clients that failed together from all retrying together.
func Backoff(attempt int, base, ceiling time.Duration) time.Duration {
	limit := base
	for i := 1; i < attempt && limit < ceiling; i++ {
		limit *= 2
	}
	limit = min(limit, ceiling)
	if limit <= 0 {
		return 0
	}
	return rand.N(limit) + 1
}

// RetryAfter reads a Retry-After header, given in seconds or as an HTTP date.
func RetryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	v := h.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 {
		return time.Duration(secs * float64(time.Second)), true
	}
	if at, err := http.ParseTime(v); err == nil {
		return max(at.Sub(now), 0), true
	}
	return 0, false
}
