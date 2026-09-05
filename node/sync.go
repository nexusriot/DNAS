package node

import (
	"sync/atomic"
	"time"

	"github.com/nexusriot/DNAS/core"
)

// Catching up used to be entirely reactive: a peer's announcement triggered a
// headers request, the headers triggered a block request, and the blocks were
// applied. Nothing watched whether the answers ever came. A peer that replies to
// pings — so the idle timeout never fires — but silently stops serving bodies
// would stall a node's sync indefinitely, and a mining node in that state keeps
// building on its stale tip, forking itself off the network.
//
// This file adds the missing half: what we asked for, from whom, when, and what
// to do when the answer does not arrive. It also downloads several ranges at once
// from different peers, with out-of-order arrivals parked in the orphan pool
// until their parents land.
const (
	// syncTickInterval is how often the sync state machine re-examines itself.
	syncTickInterval = 2 * time.Second
	// blockRequestTimeout is how long a peer has to answer a ranged block request
	// before we give up on it. Generous next to a batch download, but far below the
	// forever the node used to wait.
	blockRequestTimeout = 20 * time.Second
	// maxSyncPeers is how many ranged block requests may be outstanding at once,
	// each with a different peer, so catch-up is not limited by one peer's upload.
	maxSyncPeers = 3
	// orphanPoolCapacity bounds how many parentless blocks are buffered, and
	// orphanPoolBytes how much they may weigh in total — a count alone does not
	// bound memory, since 512 full blocks is ~512 MB (see orphan.go).
	orphanPoolCapacity = 512
	orphanPoolBytes    = 64 << 20
)

// blockRequest is one outstanding ranged block download.
type blockRequest struct {
	from, to uint64
	sent     time.Time
}

// noteBestHeight records the highest block height any peer has told us about, so
// the sync loop knows whether it is behind and how far. Announcements are only
// hints — nothing is trusted until the blocks themselves validate — so a peer
// exaggerating only costs it a request that times out.
func (n *Node) noteBestHeight(h uint64) {
	for {
		cur := atomic.LoadInt64(&n.bestHeight)
		if int64(h) <= cur {
			return
		}
		if atomic.CompareAndSwapInt64(&n.bestHeight, cur, int64(h)) {
			return
		}
	}
}

// bestKnownHeight is the highest height a peer has announced.
func (n *Node) bestKnownHeight() uint64 {
	h := atomic.LoadInt64(&n.bestHeight)
	if h < 0 {
		return 0
	}
	return uint64(h)
}

// BlocksBehind is how many blocks the best height any peer has announced is
// ahead of our tip — 0 when we are caught up (or when nobody has told us about
// anything higher). Announcements are hints, not proof, so this is a status
// indicator and not a consensus input.
func (n *Node) BlocksBehind() uint64 {
	tip := n.chain.Height()
	if best := n.bestKnownHeight(); best > tip {
		return best - tip
	}
	return 0
}

// trackRequest records that we asked p for blocks [from, to].
func (n *Node) trackRequest(p *peer, from, to uint64) {
	n.syncMu.Lock()
	defer n.syncMu.Unlock()
	n.inflight[p] = &blockRequest{from: from, to: to, sent: time.Now()}
}

// clearRequest forgets any outstanding request to p (it answered, or it is gone).
func (n *Node) clearRequest(p *peer) {
	n.syncMu.Lock()
	defer n.syncMu.Unlock()
	delete(n.inflight, p)
}

// syncLoop drives catch-up: it times out unanswered requests and keeps enough
// ranges in flight to make progress. It runs until the node shuts down.
func (n *Node) syncLoop() {
	t := time.NewTicker(syncTickInterval)
	defer t.Stop()
	for {
		select {
		case <-n.quit:
			return
		case <-t.C:
			n.syncTick()
		}
	}
}

// syncTick is one pass of the sync state machine, exported to the package so
// tests can drive it deterministically instead of waiting on the ticker.
func (n *Node) syncTick() {
	n.dropStalledPeers()
	n.requestMoreBlocks()
	n.reconcileMempoolWithPeers()
}

// reconcileMempoolWithPeers asks each peer, once per connection, for the
// transactions it has pending (see MsgGetMempool).
//
// It waits until we are CAUGHT UP before asking, which is not a nicety: mempool
// admission is checked against confirmed state, so a node still downloading the
// chain would reject every transaction it was told about — the sender's coin
// does not exist yet as far as it knows. Asking after the blocks have landed is
// the difference between reconciliation working and silently doing nothing.
func (n *Node) reconcileMempoolWithPeers() {
	if n.chain.Height() < n.bestKnownHeight() {
		return
	}
	n.peersMu.Lock()
	var ask []*peer
	for p := range n.peers {
		if !p.askedMempool && p.supports(CapMempool) {
			p.askedMempool = true
			ask = append(ask, p)
		}
	}
	n.peersMu.Unlock()
	for _, p := range ask {
		p.send(Message{Type: MsgGetMempool})
	}
}

// dropStalledPeers disconnects peers that accepted a ranged block request and
// never answered it. Staying connected to them is worse than useless: the request
// slot is held, and while it is, nothing re-asks anyone else.
func (n *Node) dropStalledPeers() {
	n.syncMu.Lock()
	var stalled []*peer
	for p, req := range n.inflight {
		if time.Since(req.sent) > blockRequestTimeout {
			stalled = append(stalled, p)
			delete(n.inflight, p)
		}
	}
	n.syncMu.Unlock()
	for _, p := range stalled {
		Warnf("peer dropped", "peer", short(p.id), "reason", "no answer to a block request", "after", blockRequestTimeout.String())
		n.bans.add(p.id, banStalling)
		_ = p.conn.Close() // the read loop's defer removes it from the peer set
	}
}

// requestMoreBlocks keeps catch-up moving. With nothing outstanding it restarts
// the headers-first pipeline (a locator finds the fork point, and the headers are
// proof-of-work-checked before any body is downloaded). With a request already in
// flight and a long way still to go, it asks *other* peers for the windows beyond
// it, so several ranges download at once.
func (n *Node) requestMoreBlocks() {
	tip := n.chain.Height()
	best := n.bestKnownHeight()
	if best <= tip {
		return
	}

	n.syncMu.Lock()
	busy := make(map[*peer]bool, len(n.inflight))
	nextWindow := tip + 1
	for p, req := range n.inflight {
		busy[p] = true
		if req.to >= nextWindow {
			nextWindow = req.to + 1
		}
	}
	slots := maxSyncPeers - len(n.inflight)
	n.syncMu.Unlock()

	if slots <= 0 {
		return
	}
	idle := n.idlePeers(busy)
	if len(idle) == 0 {
		return
	}
	if nextWindow == tip+1 {
		// Nothing outstanding: go through headers, so bodies are only fetched for a
		// chain whose proof of work we have already checked.
		idle[0].send(n.getHeadersMsg())
		return
	}
	for _, p := range idle {
		if slots <= 0 || nextWindow > best {
			return
		}
		to := nextWindow + uint64(maxBlocksBatch) - 1
		if to > best {
			to = best
		}
		p.send(Message{Type: MsgGetBlocks, From: nextWindow, To: to})
		n.trackRequest(p, nextWindow, to)
		nextWindow = to + 1
		slots--
	}
}

// idlePeers returns connected peers with no outstanding block request.
func (n *Node) idlePeers(busy map[*peer]bool) []*peer {
	n.peersMu.Lock()
	defer n.peersMu.Unlock()
	out := make([]*peer, 0, len(n.peers))
	for p := range n.peers {
		if !busy[p] {
			out = append(out, p)
		}
	}
	return out
}

// bufferOrphan parks a block whose parent we do not have yet, so it can be
// connected the moment the parent arrives instead of being fetched again. Only
// blocks that are valid on their own terms are buffered, so the pool cannot be
// filled with junk.
func (n *Node) bufferOrphan(b core.Block) bool {
	if b.Index <= n.chain.Height() {
		return false // not ahead of us; it is a fork, not an orphan
	}
	if b.SelfValid() != nil {
		return false
	}
	return n.orphans.add(b)
}

// connectOrphans applies every buffered block that now descends from the tip,
// repeating as each one moves the tip forward. It returns how many connected.
func (n *Node) connectOrphans() int {
	connected := 0
	for {
		children := n.orphans.takeChildren(n.chain.Tip().Hash)
		if len(children) == 0 {
			return connected
		}
		progress := false
		for _, b := range children {
			if err := n.chain.AddBlock(b); err != nil {
				continue // a losing sibling on a fork; it will be refetched if it wins
			}
			n.markSeenBlock(b.Hash)
			connected++
			progress = true
		}
		if !progress {
			return connected
		}
	}
}
