package node

import (
	"math/rand"
	"sync"
	"time"
)

// Dandelion++ transaction relay hides which node originated a transaction. Rather
// than immediately broadcasting a new transaction to every peer (which lets a
// network observer pinpoint the source), a node first relays it along a *stem*:
// it forwards the transaction to a single, epoch-stable successor peer. At each
// hop the transaction either continues down the stem or, with a small
// probability, transitions to the *fluff* phase — a normal broadcast to all
// peers. Because a relaying node and the originating node behave identically on
// the stem, an observer can't tell them apart.
//
// Two safety nets keep transactions from getting stuck on the stem: if a node has
// no Dandelion-capable successor it fluffs immediately, and every stem-forward
// starts an embargo timer that fluffs the transaction if it isn't seen fluffing
// on the network first (defeating a peer that black-holes stem transactions).
const (
	// dandFluffDenom: at each stem hop the transaction fluffs with probability
	// 1/dandFluffDenom. Kept modest so on a small devnet transactions fluff (and
	// so propagate widely) within a few hops.
	dandFluffDenom = 3
	// dandEpochDuration is how long a node keeps the same stem successor. Reusing
	// one successor per epoch is what makes relayed and originated transactions
	// indistinguishable.
	dandEpochDuration = 2 * time.Minute
)

// dandEmbargo is how long a stem-forwarded transaction may go unseen on the
// network before this node fluffs it itself (a var so tests can shorten it).
var dandEmbargo = 5 * time.Second

// dandelion holds a node's Dandelion++ state: the current epoch's stem successor
// and the embargo timers for transactions it has forwarded on the stem.
type dandelion struct {
	mu         sync.Mutex
	stemID     string    // identity of the current epoch's stem successor
	epochUntil time.Time // when the current epoch (and stem successor) expires
	embargoes  map[string]*time.Timer
}

func newDandelion() *dandelion { return &dandelion{embargoes: map[string]*time.Timer{}} }

// pick returns the epoch-stable stem successor from the candidate peers,
// re-choosing (and resetting the epoch) when the epoch has expired or the current
// successor is no longer connected. Returns nil if there are no candidates.
func (d *dandelion) pick(cands []*peer, now time.Time) *peer {
	d.mu.Lock()
	defer d.mu.Unlock()
	var cur *peer
	for _, p := range cands {
		if p.id == d.stemID {
			cur = p
			break
		}
	}
	if cur != nil && now.Before(d.epochUntil) {
		return cur
	}
	if len(cands) == 0 {
		d.stemID = ""
		return nil
	}
	p := cands[rand.Intn(len(cands))]
	d.stemID = p.id
	d.epochUntil = now.Add(dandEpochDuration)
	return p
}

// rollFluff reports whether a stem hop should transition to the fluff phase.
func (d *dandelion) rollFluff() bool { return rand.Intn(dandFluffDenom) == 0 }

// startEmbargo arms a one-shot timer that calls fluff if the transaction isn't
// cancelled (seen fluffing) first. A no-op if one is already armed for the hash.
func (d *dandelion) startEmbargo(hash string, fluff func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.embargoes[hash]; ok {
		return
	}
	d.embargoes[hash] = time.AfterFunc(dandEmbargo, func() {
		d.mu.Lock()
		delete(d.embargoes, hash)
		d.mu.Unlock()
		fluff()
	})
}

// cancelEmbargo stops and forgets the embargo timer for a hash, if any.
func (d *dandelion) cancelEmbargo(hash string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.embargoes[hash]; ok {
		t.Stop()
		delete(d.embargoes, hash)
	}
}

// stopAll cancels every armed embargo timer (called on shutdown).
func (d *dandelion) stopAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for h, t := range d.embargoes {
		t.Stop()
		delete(d.embargoes, h)
	}
}
