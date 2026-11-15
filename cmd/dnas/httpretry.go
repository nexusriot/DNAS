package main

import (
	"io"
	"net/http"
	"strconv"
	"time"
)

// retryTransport retries an idempotent request the node refused with 429,
// waiting as long as its Retry-After says.
//
// The node's API is rate limited per client, and weighted by cost: a paged read
// of 2000 headers is 22 tokens of a 60-token burst, a snapshot 30. One `dnas
// fastsync` or a light wallet's first sync makes several of those back to back,
// so against a node on its DEFAULT settings the CLI's own commands were refused
// part way through — fastsync finished with its filter headers unverified. A
// 429 with Retry-After is the node saying "not yet", not "no", and every CLI
// read goes through this client, so this is the one place to honour it.
//
// Only GET and HEAD are retried: a POST may already have taken effect. The
// retries stay inside the request's own deadline (http.Client.Timeout covers the
// whole exchange), and a wait that would overrun it, or exceed maxRetryWait,
// returns the 429 as it is rather than sleeping into a timeout.
type retryTransport struct {
	base     http.RoundTripper
	attempts int                 // retries after the first try
	sleep    func(time.Duration) // injectable for tests
}

const (
	maxRetryAttempts = 4
	maxRetryWait     = 5 * time.Second
)

func newRetryTransport(base http.RoundTripper) *retryTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &retryTransport{base: base, attempts: maxRetryAttempts, sleep: time.Sleep}
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for try := 0; ; try++ {
		resp, err := t.base.RoundTrip(req)
		if err != nil || resp.StatusCode != http.StatusTooManyRequests ||
			(req.Method != http.MethodGet && req.Method != http.MethodHead) || try >= t.attempts {
			return resp, err
		}
		wait := retryAfter(resp.Header.Get("Retry-After"))
		if wait > maxRetryWait {
			return resp, nil
		}
		if dl, ok := req.Context().Deadline(); ok && time.Until(dl) < wait {
			return resp, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		t.sleep(wait)
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
	}
}

// retryAfter reads a Retry-After given in seconds. An absent or unreadable
// value (the HTTP-date form included) means one second.
func retryAfter(v string) time.Duration {
	s, err := strconv.Atoi(v)
	if err != nil || s < 1 {
		return time.Second
	}
	return time.Duration(s) * time.Second
}
