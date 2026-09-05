package node

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Webhooks: getting told about a payment without holding a connection open.
//
// /events already streams blocks and transactions over SSE, and that works for
// anything with a process running and a socket open — an explorer page, a TUI, a
// watching wallet. It does not work for the thing most people actually want to
// build, which is a service that gets a request when a payment arrives: a shop
// backend, a bot, a script. Those need the node to call THEM.
//
//	dnas node -webhook https://shop.example/dnas-hook
//
// The delivery rules are the ones that make a webhook usable rather than a
// source of surprises:
//
//   - it never blocks the node. Deliveries run on their own goroutine off a
//     bounded queue, and a full queue drops events rather than stalling block
//     processing — the same rule the event bus already follows for slow SSE
//     clients.
//   - a failed POST is retried with a growing delay, a few times, then dropped.
//     A receiver that is down for a minute does not lose the event; one that is
//     down for an hour is not worth an unbounded backlog.
//   - every delivery carries the network and the node's height, so a receiver
//     can tell a regtest event from a mainnet one and can spot a gap.
const (
	// webhookQueue is how many undelivered events are held. Small on purpose: a
	// receiver that is far enough behind to fill this has lost the events either
	// way, and a large queue only delays that discovery while holding memory.
	webhookQueue = 256
	// webhookTimeout bounds one delivery attempt.
	webhookTimeout = 10 * time.Second
	// webhookRetries is how many times one event is re-POSTed before it is dropped.
	webhookRetries = 3
	// webhookRetryBase is the first retry delay; it doubles per attempt.
	webhookRetryBase = 2 * time.Second
)

// WebhookDelivery is the JSON body POSTed to a webhook URL.
type WebhookDelivery struct {
	Network string `json:"network"`
	Height  uint64 `json:"height"` // the node's height at delivery, so gaps are visible
	Event   Event  `json:"event"`
}

// webhookSender delivers events to a set of URLs.
type webhookSender struct {
	urls    []string
	queue   chan Event
	client  *http.Client
	quit    <-chan struct{}
	network func() string
	height  func() uint64
	// retryBase is the first retry delay, doubling per attempt. A field rather
	// than the constant so a test can exercise the schedule without waiting it out.
	retryBase time.Duration

	mu      sync.Mutex
	sent    int
	dropped int
	failed  int
}

// newWebhookSender builds a sender for the given URLs. It returns nil when there
// are none, so the caller's hot path is a nil check rather than a channel send.
func newWebhookSender(urls []string, quit <-chan struct{}, network func() string, height func() uint64) *webhookSender {
	var clean []string
	for _, u := range urls {
		if u = strings.TrimSpace(u); u != "" {
			clean = append(clean, u)
		}
	}
	if len(clean) == 0 {
		return nil
	}
	return &webhookSender{
		urls:      clean,
		queue:     make(chan Event, webhookQueue),
		client:    &http.Client{Timeout: webhookTimeout},
		quit:      quit,
		network:   network,
		height:    height,
		retryBase: webhookRetryBase,
	}
}

// notify enqueues an event. It never blocks: a full queue means the receiver is
// too far behind for the events to be useful, and stalling the node to wait for
// somebody's shop backend would be the wrong trade in every direction.
func (s *webhookSender) notify(e Event) {
	if s == nil {
		return
	}
	select {
	case s.queue <- e:
	default:
		s.mu.Lock()
		s.dropped++
		s.mu.Unlock()
	}
}

// run delivers queued events until the node stops.
func (s *webhookSender) run() {
	if s == nil {
		return
	}
	for {
		select {
		case <-s.quit:
			return
		case e := <-s.queue:
			body := WebhookDelivery{Network: s.network(), Height: s.height(), Event: e}
			data, err := json.Marshal(body)
			if err != nil {
				continue
			}
			for _, url := range s.urls {
				s.deliver(url, data)
			}
		}
	}
}

// deliver POSTs one event to one URL, retrying a few times. It returns as soon
// as the node is shutting down, so a receiver that is not answering cannot hold
// shutdown open for the whole retry schedule.
func (s *webhookSender) deliver(url string, data []byte) {
	delay := s.retryBase
	if delay <= 0 {
		delay = webhookRetryBase
	}
	for attempt := 0; attempt <= webhookRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-s.quit:
				return
			case <-time.After(delay):
			}
			delay *= 2
		}
		resp, err := s.client.Post(url, "application/json", bytes.NewReader(data))
		if err == nil {
			code := resp.StatusCode
			resp.Body.Close()
			// Any 2xx is a delivery. A 4xx is the receiver saying the request itself
			// is wrong, which a retry cannot fix — retrying it would be a loop
			// neither side benefits from — so only 5xx and transport errors retry.
			if code >= 200 && code < 300 {
				s.mu.Lock()
				s.sent++
				s.mu.Unlock()
				return
			}
			if code >= 400 && code < 500 {
				Warnf("webhook rejected the delivery", "url", url, "status", code)
				s.mu.Lock()
				s.failed++
				s.mu.Unlock()
				return
			}
		}
	}
	Warnf("webhook delivery failed", "url", url, "attempts", webhookRetries+1)
	s.mu.Lock()
	s.failed++
	s.mu.Unlock()
}

// WebhookStats reports what a node's webhooks have done, for /info and for
// anyone wondering whether their receiver is actually being called.
type WebhookStats struct {
	URLs    int `json:"urls"`
	Sent    int `json:"sent"`
	Failed  int `json:"failed"`
	Dropped int `json:"dropped"` // events discarded because the queue was full
	Queued  int `json:"queued"`
}

func (s *webhookSender) stats() WebhookStats {
	if s == nil {
		return WebhookStats{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return WebhookStats{URLs: len(s.urls), Sent: s.sent, Failed: s.failed,
		Dropped: s.dropped, Queued: len(s.queue)}
}

// WebhookStats is the node's view of the same.
func (n *Node) WebhookStats() WebhookStats { return n.webhooks.stats() }

// WebhooksEnabled reports whether this node pushes events anywhere.
func (n *Node) WebhooksEnabled() bool { return n.webhooks != nil }
