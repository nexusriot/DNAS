package api_test

import (
	"net/http"
	"testing"

	"github.com/nexusriot/DNAS/api"
)

// The HTTP API had no limit at all, while the peer protocol has had a token
// bucket per peer from early on — and the API is the cheaper target: no
// handshake, no protocol, and the expensive endpoints are plain GETs.
func TestAPIRateLimitRefusesAFlood(t *testing.T) {
	srv, _, _ := testServerWith(t, func(s *api.Server) { s.SetRateLimit(1, 3) })

	// The burst is spent first, and those requests must all succeed: a limit that
	// rejected the first few would break the explorer, which fires several
	// requests to draw one page.
	for i := 0; i < 3; i++ {
		if code := statusOf(t, srv.URL+"/info"); code != http.StatusOK {
			t.Fatalf("request %d in the burst was refused with %d", i+1, code)
		}
	}
	// Past the burst, at a sustained rate of one per second, the next one is
	// refused rather than served.
	code := statusOf(t, srv.URL+"/info")
	if code != http.StatusTooManyRequests {
		t.Fatalf("the request past the burst returned %d, want 429", code)
	}
	// A refusal has to tell a well-behaved client to wait, or it retries at once
	// and makes the problem worse.
	resp, err := http.Get(srv.URL + "/info")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("a 429 carried no Retry-After")
	}
}

// A limit of zero is the documented way to turn it off, for a node behind
// something that does this properly.
func TestAPIRateLimitCanBeDisabled(t *testing.T) {
	srv, _, _ := testServerWith(t, func(s *api.Server) { s.SetRateLimit(0, 0) })
	for i := 0; i < 200; i++ {
		if code := statusOf(t, srv.URL+"/info"); code != http.StatusOK {
			t.Fatalf("request %d was refused with %d despite no limit", i+1, code)
		}
	}
}

// The default has to be generous enough that ordinary use never sees it: a light
// wallet paging headers, or an explorer redrawing, makes a burst of requests.
func TestDefaultAPIRateLimitAllowsOrdinaryUse(t *testing.T) {
	srv, _, _ := testServer(t)
	for i := 0; i < int(api.DefaultAPIBurst); i++ {
		if code := statusOf(t, srv.URL+"/info"); code != http.StatusOK {
			t.Fatalf("request %d of a default-sized burst was refused with %d", i+1, code)
		}
	}
}

// The limiter keys on the client's IP, not on host:port — a client uses a new
// source port per request, so keying on the full address would hand out a fresh
// bucket every time and limit nothing.
func TestRateLimitKeysOnTheClientAddress(t *testing.T) {
	for _, tc := range []struct{ remote, want string }{
		{"192.0.2.7:54321", "192.0.2.7"},
		{"192.0.2.7:1", "192.0.2.7"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"nonsense", "nonsense"},
	} {
		r, err := http.NewRequest("GET", "/info", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.RemoteAddr = tc.remote
		// A header the client sets must NOT change its identity: trusting it would
		// let anyone claim a new bucket per request, which is worse than no limit —
		// the limiter becomes a memory allocator for the attacker.
		r.Header.Set("X-Forwarded-For", "203.0.113.99")
		if got := api.ClientKeyForTest(r); got != tc.want {
			t.Errorf("clientKey(%q) = %q, want %q", tc.remote, got, tc.want)
		}
	}
}

// The event stream is one long request that then sends many messages, so
// counting it against a burst of ordinary reads would drop a client's live feed
// for reasons unrelated to it.
func TestRateLimitExemptsTheEventStream(t *testing.T) {
	srv, _, _ := testServerWith(t, func(s *api.Server) { s.SetRateLimit(1, 1) })
	if code := statusOf(t, srv.URL+"/info"); code != http.StatusOK {
		t.Fatalf("the first request was refused with %d", code)
	}
	if code := statusOf(t, srv.URL+"/info"); code != http.StatusTooManyRequests {
		t.Fatalf("the second request returned %d, want 429", code)
	}
	// /events must still be reachable with the bucket empty. The stream is
	// long-lived, so this only checks that the request is accepted.
	req, err := http.NewRequest("GET", srv.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		t.Fatal("the event stream was rate-limited")
	}
}
