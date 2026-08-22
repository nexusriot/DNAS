package node

import (
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// chainOfBlocks mines n blocks onto a throwaway chain and returns them in order,
// so tests can feed a node bodies out of order.
func chainOfBlocks(t *testing.T, n int) []core.Block {
	t.Helper()
	w, _ := wallet.New()
	src := core.NewBlockchain()
	mp := core.NewMempool()
	node := New(Config{ListenAddr: "127.0.0.1:0"}, src, mp, w)
	if _, err := node.Generate(n); err != nil {
		t.Fatalf("generate: %v", err)
	}
	all := src.Blocks()
	return all[1:] // drop genesis
}

// The orphan pool exists so an out-of-order block is not thrown away. Feeding a
// node a batch back-to-front must still leave it fully synced.
func TestOutOfOrderBlocksStillConnect(t *testing.T) {
	blocks := chainOfBlocks(t, 6)
	n, _, _ := testNode(t)
	p, _ := recordingPeer(t)

	// Deliver everything except the first block: none can connect yet.
	n.onBlocks(p, blocks[1:])
	if got := n.chain.Height(); got != 0 {
		t.Fatalf("height = %d before the parent arrived, want 0", got)
	}
	if n.orphans.len() != len(blocks)-1 {
		t.Fatalf("buffered %d blocks, want %d", n.orphans.len(), len(blocks)-1)
	}

	// The missing parent unlocks the whole run in one go.
	n.onBlocks(p, blocks[:1])
	if got := n.chain.Height(); got != uint64(len(blocks)) {
		t.Fatalf("height = %d after the parent arrived, want %d", got, len(blocks))
	}
	if n.orphans.len() != 0 {
		t.Fatalf("orphan pool still holds %d blocks", n.orphans.len())
	}
}

// The pool is bounded and refuses blocks that are invalid on their own terms, so
// a peer cannot use it to spend a node's memory cheaply.
func TestOrphanPoolIsBoundedAndPicky(t *testing.T) {
	pool := newOrphanPool(3)
	blocks := chainOfBlocks(t, 5)
	for _, b := range blocks {
		pool.add(b)
	}
	if pool.len() != 3 {
		t.Fatalf("pool holds %d blocks, want its cap of 3", pool.len())
	}

	n, _, _ := testNode(t)
	p, _ := recordingPeer(t)
	junk := blocks[2]
	junk.Nonce++ // breaks the proof of work
	if n.bufferOrphan(junk) {
		t.Fatal("buffered a block with invalid proof of work")
	}
	// A block at or below our tip is a fork, not an orphan, and is not buffered.
	n.onBlocks(p, blocks[:1])
	if n.bufferOrphan(blocks[0]) {
		t.Fatal("buffered a block we already have")
	}
}

// A peer that accepts a ranged block request and never answers used to stall sync
// forever: the idle timeout never fires because it still answers pings, and
// nothing re-asked anyone else. It must now be given up on.
func TestStalledPeerIsDroppedAndItsSlotFreed(t *testing.T) {
	n, _, _ := testNode(t)
	p := &peer{id: "stalling-peer"}

	n.trackRequest(p, 1, 10)
	n.syncMu.Lock()
	n.inflight[p].sent = time.Now().Add(-blockRequestTimeout - time.Second)
	n.syncMu.Unlock()

	n.dropStalledPeers()

	n.syncMu.Lock()
	left := len(n.inflight)
	n.syncMu.Unlock()
	if left != 0 {
		t.Fatalf("%d requests still outstanding after the timeout", left)
	}
	if n.bans.scoreOf(p.id) == 0 {
		t.Fatal("a stalling peer earned no ban score")
	}
	// A peer that answers keeps its good standing and frees its slot.
	good := &peer{id: "good-peer"}
	n.trackRequest(good, 11, 20)
	n.onBlocks(good, nil)
	n.syncMu.Lock()
	left = len(n.inflight)
	n.syncMu.Unlock()
	if left != 0 {
		t.Fatalf("answering peer's slot not released: %d outstanding", left)
	}
	if n.bans.scoreOf(good.id) != 0 {
		t.Fatal("a peer that answered was penalised")
	}
}

// Announced heights drive the sync loop: without them it cannot tell that it is
// behind, and never asks for anything.
func TestBestHeightTracksAnnouncements(t *testing.T) {
	n, _, _ := testNode(t)
	if got := n.bestKnownHeight(); got != 0 {
		t.Fatalf("initial best height = %d, want 0", got)
	}
	n.noteBestHeight(42)
	n.noteBestHeight(7) // a lower announcement must not lower it
	if got := n.bestKnownHeight(); got != 42 {
		t.Fatalf("best height = %d, want 42", got)
	}
	// An inv announcement feeds it too.
	p, _ := recordingPeer(t)
	n.handleMessage(p, Message{Type: MsgInv, Index: 100, Hash: "abc"})
	if got := n.bestKnownHeight(); got != 100 {
		t.Fatalf("best height after inv = %d, want 100", got)
	}
}

// With one range already in flight and a long way still to go, other peers are
// asked for the windows beyond it, so catch-up is not limited to one peer's
// upload speed.
func TestParallelRangesRequestedFromIdlePeers(t *testing.T) {
	n, _, _ := testNode(t)
	n.noteBestHeight(5000)

	busy := &peer{id: "busy"}
	n.trackRequest(busy, 1, uint64(maxBlocksBatch))

	// Two idle peers, each backed by a pipe we can read the request off.
	idle := make([]*peer, 2)
	msgs := make([]chan Message, 2)
	for i := range idle {
		idle[i], msgs[i] = recordingPeer(t)
		n.addPeer(idle[i])
	}
	n.addPeer(busy)

	n.requestMoreBlocks()

	seen := map[uint64]uint64{} // from -> to
	for i := range idle {
		select {
		case m := <-msgs[i]:
			if m.Type != MsgGetBlocks {
				t.Fatalf("idle peer got %s, want getblocks", m.Type)
			}
			seen[m.From] = m.To
		case <-time.After(time.Second):
			t.Fatalf("idle peer %d was not asked for a range", i)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("expected two distinct ranges, got %v", seen)
	}
	// The windows must not overlap each other or the one already in flight.
	for from, to := range seen {
		if from <= uint64(maxBlocksBatch) {
			t.Fatalf("range %d-%d overlaps the request already in flight", from, to)
		}
		for otherFrom, otherTo := range seen {
			if otherFrom != from && otherFrom <= to && from <= otherTo {
				t.Fatalf("ranges %d-%d and %d-%d overlap", from, to, otherFrom, otherTo)
			}
		}
	}
	n.syncMu.Lock()
	outstanding := len(n.inflight)
	n.syncMu.Unlock()
	if outstanding != maxSyncPeers {
		t.Fatalf("%d requests outstanding, want the cap of %d", outstanding, maxSyncPeers)
	}
}

// A node already at the best known height asks for nothing.
func TestNoRequestsWhenCaughtUp(t *testing.T) {
	n, _, _ := testNode(t)
	p, msgs := recordingPeer(t)
	n.addPeer(p)
	n.noteBestHeight(0)
	n.requestMoreBlocks()
	select {
	case m := <-msgs:
		t.Fatalf("a caught-up node sent %s", m.Type)
	case <-time.After(200 * time.Millisecond):
	}
}

// recordingPeer returns a peer whose sent messages can be read off the returned
// channel, so a test can assert what the node asked it for.
func recordingPeer(t *testing.T) (*peer, chan Message) {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	p := &peer{enc: json.NewEncoder(pw), caps: map[string]bool{}}
	out := make(chan Message, 16)
	go func() {
		dec := json.NewDecoder(pr)
		for {
			var m Message
			if err := dec.Decode(&m); err != nil {
				return
			}
			out <- m
		}
	}()
	return p, out
}
