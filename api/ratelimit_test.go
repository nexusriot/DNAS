package api_test

import (
	"net/http"
	"testing"
	"time"

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
		{"[2001:db8::1]:443", "2001:db8::/64"}, // keyed by /64: one subscriber, one bucket
		{"[2001:db8::ffff:2]:443", "2001:db8::/64"},
		{"[2001:db8:0:1::1]:443", "2001:db8:0:1::/64"},
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

// Expensive endpoints debit more than cheap ones, so the budget tracks the work a
// client can demand rather than its request count.
func TestRateLimitIsCostWeighted(t *testing.T) {
	cost := func(path string) float64 {
		r, err := http.NewRequest("GET", path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return api.RouteCostForTest(r)
	}
	if c := cost("/info"); c != 1 {
		t.Fatalf("/info costs %v, want 1", c)
	}
	if !(cost("/snapshot/0") > cost("/richlist") && cost("/richlist") > cost("/stateproof/x") && cost("/stateproof/x") > cost("/info")) {
		t.Fatal("route costs are not ordered by the work they do")
	}
	if cost("/chain?limit=2000") <= cost("/chain?limit=10") {
		t.Fatal("a bigger page of /chain must cost more")
	}

	// With a burst of 60, two whole-ledger snapshots exhaust it where sixty /info
	// reads would not.
	srv, _, _ := testServerWith(t, func(s *api.Server) { s.SetRateLimit(0.001, 60) })
	if code := statusOf(t, srv.URL+"/snapshot/0"); code == http.StatusTooManyRequests {
		t.Fatal("the first snapshot was refused")
	}
	if code := statusOf(t, srv.URL+"/snapshot/0"); code == http.StatusTooManyRequests {
		t.Fatal("the second snapshot was refused")
	}
	if code := statusOf(t, srv.URL+"/snapshot/0"); code != http.StatusTooManyRequests {
		t.Fatalf("a third snapshot answered %d, want 429: snapshots are not weighted", code)
	}
}

// /events is exempt from the request budget, so it must be bounded by its own
// stream count: one client cannot hold more than MaxEventStreamsPerClient.
func TestEventStreamsAreCappedPerClient(t *testing.T) {
	srv, _, _ := testServer(t)
	var open []*http.Response
	defer func() {
		for _, r := range open {
			r.Body.Close()
		}
	}()
	for i := 0; i < api.MaxEventStreamsPerClient; i++ {
		resp, err := http.Get(srv.URL + "/events")
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d answered %d", i+1, resp.StatusCode)
		}
	}
	resp, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("stream past the per-client cap answered %d, want 429", resp.StatusCode)
	}
	// Closing one frees its slot.
	open[0].Body.Close()
	open = open[1:]
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := http.Get(srv.URL + "/events")
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == http.StatusOK {
			open = append(open, resp)
			break
		}
		resp.Body.Close()
		if time.Now().After(deadline) {
			t.Fatal("a closed stream never released its slot")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
