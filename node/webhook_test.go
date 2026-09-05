package node

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// A webhook is the delivery mechanism for a service that cannot hold an SSE
// connection open — a shop backend, a bot, a cron job.
func TestWebhookDeliversEvents(t *testing.T) {
	var mu sync.Mutex
	var got []WebhookDelivery
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var d WebhookDelivery
		if err := json.Unmarshal(body, &d); err != nil {
			t.Errorf("delivery body is not a WebhookDelivery: %v", err)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content type = %q", ct)
		}
		mu.Lock()
		got = append(got, d)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	quit := make(chan struct{})
	defer close(quit)
	s := newWebhookSender([]string{srv.URL}, quit, func() string { return "regtest" }, func() uint64 { return 42 })
	go s.run()

	s.notify(Event{Type: "block", Height: 42, Hash: "abc", Txs: 1})
	s.notify(Event{Type: "tx", Hash: "def", From: "dnasa", To: "dnasb", Amount: 5})

	if !waitFor(5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 2
	}) {
		t.Fatal("both deliveries did not arrive")
	}
	mu.Lock()
	defer mu.Unlock()
	// The network and height travel with every delivery: a receiver has to be able
	// to tell a regtest event from a mainnet one, and to notice a gap.
	for _, d := range got {
		if d.Network != "regtest" || d.Height != 42 {
			t.Fatalf("delivery lost its context: %+v", d)
		}
	}
	if got[0].Event.Type != "block" || got[1].Event.Type != "tx" {
		t.Fatalf("events arrived as %q, %q", got[0].Event.Type, got[1].Event.Type)
	}
	if st := s.stats(); st.Sent != 2 || st.Failed != 0 || st.Dropped != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

// A receiver that is down must not lose the event to the first failure, and must
// not be retried forever either.
func TestWebhookRetriesThenGivesUp(t *testing.T) {
	var attempts int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A 5xx is the receiver saying "not now", which is worth retrying.
		if atomic.AddInt64(&attempts, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	quit := make(chan struct{})
	defer close(quit)
	s := newWebhookSender([]string{srv.URL}, quit, func() string { return "regtest" }, func() uint64 { return 1 })
	s.retryBase = time.Millisecond // the schedule's shape, not its seconds
	s.deliver(srv.URL, []byte(`{}`))
	if n := atomic.LoadInt64(&attempts); n != 3 {
		t.Fatalf("made %d attempts, want 3 (two failures then a success)", n)
	}
	if st := s.stats(); st.Sent != 1 {
		t.Fatalf("the eventual success was not counted: %+v", st)
	}
}

// A 4xx means the request itself is wrong, and no number of retries fixes that:
// retrying is a loop neither side benefits from.
func TestWebhookDoesNotRetryAClientError(t *testing.T) {
	var attempts int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	quit := make(chan struct{})
	defer close(quit)
	s := newWebhookSender([]string{srv.URL}, quit, func() string { return "regtest" }, func() uint64 { return 1 })
	s.deliver(srv.URL, []byte(`{}`))
	if n := atomic.LoadInt64(&attempts); n != 1 {
		t.Fatalf("a 400 was retried %d times", n-1)
	}
	if st := s.stats(); st.Failed != 1 || st.Sent != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

// The rule that keeps a webhook from becoming a liability: notifying must never
// block the node, whatever the receiver is doing.
func TestWebhookNeverBlocksTheNode(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)
	// No run() goroutine, so nothing drains the queue — the worst case.
	s := newWebhookSender([]string{"http://127.0.0.1:1"}, quit,
		func() string { return "regtest" }, func() uint64 { return 1 })

	done := make(chan struct{})
	go func() {
		for i := 0; i < webhookQueue*4; i++ {
			s.notify(Event{Type: "tx", Height: uint64(i)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("notify blocked on a full queue")
	}
	st := s.stats()
	if st.Dropped == 0 {
		t.Fatal("a full queue dropped nothing, so it must have grown without bound")
	}
	if st.Queued > webhookQueue {
		t.Fatalf("queue holds %d, over its %d bound", st.Queued, webhookQueue)
	}
}

// No URLs configured must cost nothing at all — the node's hot path calls this
// on every block and every transaction.
func TestWebhookSenderIsNilWithoutURLs(t *testing.T) {
	for _, urls := range [][]string{nil, {}, {""}, {"  ", ""}} {
		if s := newWebhookSender(urls, nil, nil, nil); s != nil {
			t.Fatalf("newWebhookSender(%v) returned a sender", urls)
		}
	}
	// And a nil sender's methods are safe to call, which is what lets the node
	// publish unconditionally.
	var s *webhookSender
	s.notify(Event{Type: "block"})
	s.run()
	if st := s.stats(); st.URLs != 0 || st.Sent != 0 {
		t.Fatalf("a nil sender reported %+v", st)
	}
}

// Events published by the node must reach the webhook, not just the SSE bus:
// one path, so the two cannot come to carry different events.
func TestNodeEventsReachWebhooks(t *testing.T) {
	var delivered int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&delivered, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	n := New(Config{ListenAddr: "127.0.0.1:0", Webhooks: []string{srv.URL}},
		core.NewBlockchain(), core.NewMempool(), w)
	defer n.Shutdown()
	if !n.WebhooksEnabled() {
		t.Fatal("webhooks were configured and are not enabled")
	}
	go n.webhooks.run()

	// Also to the SSE subscribers, at the same time.
	sub, unsub := n.Subscribe()
	defer unsub()
	n.publishBlock(false)

	select {
	case e := <-sub:
		if e.Type != "block" {
			t.Fatalf("subscriber got %q", e.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the in-process subscriber got nothing")
	}
	if !waitFor(5*time.Second, func() bool { return atomic.LoadInt64(&delivered) > 0 }) {
		t.Fatal("the webhook received nothing")
	}
}
