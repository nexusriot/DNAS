package api

import (
	"net"
	"net/http"
	"strconv"
	"strings"
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
	// maxAPIClients caps the bucket map. Past it, the least recently seen
	// client's bucket is dropped to make room, so the limiter's memory is bounded
	// by a constant rather than by how many source addresses an attacker has.
	maxAPIClients = 65536
	// MaxEventStreams bounds concurrent /events subscribers node-wide, and
	// MaxEventStreamsPerClient per client. A stream is exempt from the request
	// budget (it is one request that then sends many messages), so without these
	// an unauthenticated client could hold unlimited goroutines and subscriber
	// slots — and every event is fanned out to every subscriber on the relay path.
	MaxEventStreams          = 256
	MaxEventStreamsPerClient = 8
)

// routeCost is what a request debits from its client's bucket, in units of one
// cheap read. The bucket used to charge one token per request whatever the
// path, which priced /info and /snapshot identically — while a snapshot clones
// the whole ledger and replays undo records from the tip down to the requested
// height, under the chain's read lock. The nominal limit and the CPU a client
// could actually demand were not the same number. These weights are rough
// orders of magnitude, not measurements; they only have to put the expensive
// endpoints in a different class from the cheap ones.
//
// A cost is never more than the bucket's burst (see allowN), so every endpoint
// stays reachable at the default settings.
func routeCost(r *http.Request) float64 {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/snapshot/"), p == "/audit":
		return 30 // whole-ledger work
	case p == "/richlist", p == "/series", p == "/chainstats", p == "/supply":
		// A pass over every account or a window of blocks. Kept low enough that
		// the explorer's dashboard, which loads all four at once alongside a
		// dozen cheap reads, still fits comfortably in one default burst.
		return 5
	case p == "/chain", p == "/headers", p == "/cfheaders", p == "/cfilters":
		// Paged: the cost scales with how much was asked for.
		return 2 + pagedCost(r)
	case strings.HasPrefix(p, "/stateproof/"), strings.HasPrefix(p, "/nameproof/"),
		strings.HasPrefix(p, "/proof/"), p == "/ancestry", strings.HasPrefix(p, "/cfilter/"):
		return 3 // a proof built per call
	case p == "/metrics", strings.HasPrefix(p, "/address"):
		return 3
	}
	return 1
}

// pagedCost charges one extra token per hundred items requested.
func pagedCost(r *http.Request) float64 {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		n, err = strconv.Atoi(r.URL.Query().Get("max"))
		if err != nil || n <= 0 {
			return 0
		}
	}
	return float64(n / 100)
}

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

// allow consumes one token for key, reporting whether the request may proceed.
func (l *limiter) allow(key string) bool { return l.allowN(key, 1) }

// allowN consumes cost tokens for key. A cost above the burst is charged as the
// burst, so a client with a full bucket can always make any single request.
func (l *limiter) allowN(key string, cost float64) bool {
	if cost < 1 {
		cost = 1
	}
	if cost > l.burst {
		cost = l.burst
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.clients[key]
	if !ok {
		// A brand-new client starts with a full bucket minus this request, so the
		// first request from anyone is never delayed.
		l.sweepLocked(now)
		l.clients[key] = &apiBucket{tokens: l.burst - cost, last: now}
		return true
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < cost {
		return false
	}
	b.tokens -= cost
	return true
}

// sweepLocked drops buckets nobody has used for a while, and makes room when the
// map is at its cap. It runs on the arrival of a new client, which is the only
// moment the map can grow.
func (l *limiter) sweepLocked(now time.Time) {
	if len(l.clients) < 64 {
		return // not worth walking the map yet
	}
	for key, b := range l.clients {
		if now.Sub(b.last) > apiClientTTL {
			delete(l.clients, key)
		}
	}
	for len(l.clients) >= maxAPIClients {
		var oldest string
		var at time.Time
		for key, b := range l.clients {
			if oldest == "" || b.last.Before(at) {
				oldest, at = key, b.last
			}
		}
		delete(l.clients, oldest)
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
//
// An IPv6 client is keyed by its /64. A single subscriber is routinely handed a
// whole /64, so keying on the full address would give one machine 2^64 fresh
// buckets to rotate through.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil && len(ip) == net.IPv6len {
		return (&net.IPNet{IP: ip.Mask(net.CIDRMask(64, 128)), Mask: net.CIDRMask(64, 128)}).String()
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
		// count instead (see acquireStream).
		if r.URL.Path == "/events" {
			h.ServeHTTP(w, r)
			return
		}
		if !s.limiter.allowN(clientKey(r), routeCost(r)) {
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

// streamGate bounds concurrent long-lived streams, overall and per client.
type streamGate struct {
	mu       sync.Mutex
	total    int
	byClient map[string]int
}

// acquireStream reserves a stream slot for key, returning a release func, or
// false when the node or the client is at its cap.
func (s *Server) acquireStream(key string) (func(), bool) {
	g := &s.streams
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.byClient == nil {
		g.byClient = map[string]int{}
	}
	if g.total >= MaxEventStreams || g.byClient[key] >= MaxEventStreamsPerClient {
		return nil, false
	}
	g.total++
	g.byClient[key]++
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.total--
		if g.byClient[key]--; g.byClient[key] <= 0 {
			delete(g.byClient, key)
		}
	}, true
}

// RouteCostForTest exposes routeCost to the package's external tests.
func RouteCostForTest(r *http.Request) float64 { return routeCost(r) }
