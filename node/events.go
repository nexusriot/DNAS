package node

import "sync"

// Event is a real-time notification a node emits as its state changes, so clients
// can react (refresh a UI, update a wallet) without polling. It is a small,
// self-describing JSON envelope; only the fields relevant to Type are set.
type Event struct {
	Type   string `json:"type"` // "block", "reorg", or "tx"
	Height uint64 `json:"height,omitempty"`
	Hash   string `json:"hash,omitempty"`
	Txs    int    `json:"txs,omitempty"` // transactions in the block (block/reorg)
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Amount uint64 `json:"amount,omitempty"`
	Fee    uint64 `json:"fee,omitempty"`
}

// eventBus is a minimal in-process publish/subscribe hub. Each subscriber gets a
// buffered channel; a subscriber that cannot keep up simply drops events (the bus
// never blocks the node's hot paths on a slow client).
type eventBus struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func newEventBus() *eventBus { return &eventBus{subs: map[chan Event]struct{}{}} }

// subscribe registers a new subscriber and returns its channel plus a function
// that unsubscribes and closes the channel. The channel is buffered so a burst of
// events doesn't immediately drop under a briefly-busy consumer.
func (b *eventBus) subscribe() (chan Event, func()) {
	ch := make(chan Event, 64)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	unsub := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, ch)
			close(ch)
			b.mu.Unlock()
		})
	}
	return ch, unsub
}

// publish delivers e to every subscriber, skipping any whose buffer is full so a
// slow consumer can never stall block/tx processing.
func (b *eventBus) publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default: // subscriber is behind; drop rather than block
		}
	}
}
