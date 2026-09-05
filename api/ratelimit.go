package api

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Rate limiting the HTTP API.
//
// The peer protocol has had a per-peer token bucket from early on
// (node/ratelimit.go). The HTTP API had nothing, and it is the cheaper target of
// the two: no handshake, no protocol, just a URL. The expensive endpoints are
// worse than the cheap ones — /chain?limit=2000 serializes two thousand blocks,
// /snapshot walks the whole account state, /stateproof builds a merkle proof per
// call — so an unbounded API is a way to spend a node's CPU from a laptop.
//
// A token bucket per client address, checked before the handler runs. The
// defaults are generous enough that a person clicking around the explorer, or a
// light wallet paging the chain, never sees a 429; they only bind on a loop.
const (
	// DefaultAPIRate is the sustained requests per second allowed per client.
	DefaultAPIRate = 20.0
	// DefaultAPIBurst is how many may arrive back to back. The explorer's front
	// page alone makes several requests, and a light wallet's sync makes one per
	// page of headers, so the burst has to comfortably exceed both.
	DefaultAPIBurst = 60.0
	// apiClientTTL is how long an idle client's bucket is kept. Without eviction
	// the limiter is itself a memory leak: one entry per source address, held for
	// the node's uptime, which is exactly the resource it is meant to protect.
	apiClientTTL = 10 * time.Minute
)

// limiter is a keyed set of token buckets with expiry.
type limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	clients map[string]*apiBucket
	now     func() time.Time // injectable so the tests need not sleep
}

type apiBucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(rate, burst float64) *limiter {
	return &limiter{rate: rate, burst: burst, clients: map[string]*apiBucket{}, now: time.Now}
}

// allow consumes a token for key, reporting whether the request may proceed.
func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.clients[key]
	if !ok {
		// A brand-new client starts with a full bucket minus this request, so the
		// first request from anyone is never delayed.
		l.clients[key] = &apiBucket{tokens: l.burst - 1, last: now}
		l.sweepLocked(now)
		return true
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweepLocked drops buckets nobody has used for a while. It runs on the arrival
// of a new client, which is the only moment the map can grow.
func (l *limiter) sweepLocked(now time.Time) {
	if len(l.clients) < 64 {
		return // not worth walking the map yet
	}
	for key, b := range l.clients {
		if now.Sub(b.last) > apiClientTTL {
			delete(l.clients, key)
		}
	}
}

// clientKey is the identity a limit applies to: the peer's IP, not its
// host:port, since a client making many requests uses a new source port for
// each one and would otherwise get a fresh bucket every time.
//
// It deliberately ignores X-Forwarded-For. Trusting a header that the client
// sets would let anyone claim a new identity per request, which is worse than no
// limit at all — it turns the limiter into a memory allocator for the attacker.
// A node behind a real proxy needs the limit at the proxy.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RateLimit wraps a handler in a per-client token bucket. A refused request gets
// 429 with a Retry-After, which is what a well-behaved client needs to back off
// rather than retry immediately and make it worse.
func (s *Server) RateLimit(h http.Handler) http.Handler {
	if s.limiter == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The event stream is one long-lived request that then sends many
		// messages, so counting it once is right and rejecting it on a burst of
		// unrelated reads is not. It is exempt, and bounded by its own connection
		// count instead.
		if r.URL.Path == "/events" {
			h.ServeHTTP(w, r)
			return
		}
		if !s.limiter.allow(clientKey(r)) {
			w.Header().Set("Retry-After", "1")
			writeErr(w, http.StatusTooManyRequests, "rate limit exceeded; slow down")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// ClientKeyForTest exposes clientKey to the package's external tests, which is
// where the "identity is the IP, and never a client-set header" rule is checked.
func ClientKeyForTest(r *http.Request) string { return clientKey(r) }
