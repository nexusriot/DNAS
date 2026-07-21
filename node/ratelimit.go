package node

import "time"

// rateLimiter is a token bucket used to bound how fast a peer may send messages.
// It starts full (burst tokens) and refills at `rate` tokens per second up to
// `burst`; allow() consumes one token and returns false when the bucket is empty.
// Each peer read loop owns one, so no locking is needed.
type rateLimiter struct {
	tokens float64
	rate   float64
	burst  float64
	last   time.Time
}

func newRateLimiter(rate, burst float64) *rateLimiter {
	return &rateLimiter{tokens: burst, rate: rate, burst: burst}
}

// allow accounts for time since the last call, refills the bucket, and consumes a
// token. `now` is passed in so it is deterministically testable.
func (r *rateLimiter) allow(now time.Time) bool {
	if !r.last.IsZero() {
		r.tokens += now.Sub(r.last).Seconds() * r.rate
		if r.tokens > r.burst {
			r.tokens = r.burst
		}
	}
	r.last = now
	if r.tokens < 1 {
		return false
	}
	r.tokens--
	return true
}
