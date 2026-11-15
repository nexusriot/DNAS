package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// limitedServer refuses the first `refuse` requests with 429 and the given
// Retry-After, then answers 200.
func limitedServer(t *testing.T, refuse int32, retryAfter string) (*httptest.Server, *atomic.Int32) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= refuse {
			w.Header().Set("Retry-After", retryAfter)
			http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func retryClient(slept *[]time.Duration) *http.Client {
	rt := newRetryTransport(nil)
	rt.sleep = func(d time.Duration) { *slept = append(*slept, d) }
	return &http.Client{Timeout: 30 * time.Second, Transport: rt}
}

func TestRetryTransportWaitsOutA429(t *testing.T) {
	srv, hits := limitedServer(t, 2, "2")
	var slept []time.Duration
	var v struct{ OK bool }
	if err := getJSONVia(retryClient(&slept), srv.URL+"/headers", &v); err != nil {
		t.Fatal(err)
	}
	if !v.OK || hits.Load() != 3 {
		t.Fatalf("ok=%v after %d requests", v.OK, hits.Load())
	}
	if len(slept) != 2 || slept[0] != 2*time.Second {
		t.Fatalf("slept %v, want two waits of the server's 2s", slept)
	}
}

func TestRetryTransportGivesUp(t *testing.T) {
	// A node that never relents: the client stops after its attempts and reports
	// the refusal, rather than looping.
	srv, hits := limitedServer(t, 100, "1")
	var slept []time.Duration
	err := getJSONVia(retryClient(&slept), srv.URL+"/info", &struct{}{})
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v, want the 429", err)
	}
	if got := hits.Load(); got != maxRetryAttempts+1 {
		t.Fatalf("%d requests, want %d", got, maxRetryAttempts+1)
	}

	// A wait longer than the client is willing to sleep is not slept at all.
	srv2, hits2 := limitedServer(t, 100, "3600")
	slept = nil
	if err := getJSONVia(retryClient(&slept), srv2.URL+"/info", &struct{}{}); err == nil {
		t.Fatal("a 429 with an hour's Retry-After was not reported")
	}
	if hits2.Load() != 1 || len(slept) != 0 {
		t.Fatalf("%d requests, slept %v: an over-long wait was waited out", hits2.Load(), slept)
	}
}

func TestRetryTransportNeverRetriesAPost(t *testing.T) {
	// A POST the node refused may still be one it would have acted on next time;
	// replaying it is the caller's decision, not the transport's.
	srv, hits := limitedServer(t, 1, "1")
	var slept []time.Duration
	resp, err := retryClient(&slept).Post(srv.URL+"/tx", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || hits.Load() != 1 {
		t.Fatalf("status %d after %d requests: the POST was retried", resp.StatusCode, hits.Load())
	}
}

func TestRetryAfterParsing(t *testing.T) {
	for v, want := range map[string]time.Duration{
		"": time.Second, "0": time.Second, "3": 3 * time.Second, "junk": time.Second,
		"Wed, 21 Oct 2015 07:28:00 GMT": time.Second,
	} {
		if got := retryAfter(v); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", v, got, want)
		}
	}
}
