package node

import (
	"sync"
	"time"

	"github.com/nexusriot/DNAS/core"
)

// Announcement-based transaction relay.
//
// A transaction used to be pushed in full to every peer the moment it arrived,
// so a node with eight peers sent the same body eight times — seven of them to
// peers that, on a well-connected network, already had it. Blocks have been
// announced and pulled since the beginning (MsgInv/MsgGetData); this gives
// transactions the same treatment.
//
// The shape is Bitcoin's: announce the id, let a peer ask for what it lacks,
// send only that. Two details make it safe rather than merely smaller.
//
// An announcement is NOT recorded in the seen set. "Seen" means "we have it and
// have processed it"; marking a transaction on its announcement would mean that
// if the announcing peer never delivered — it disconnected, it was lying, the
// request was dropped — the transaction would be permanently unfetchable from
// anyone else, because every later announcement would look like a duplicate.
// That is the failure this design has to avoid, and it is why requests are
// tracked separately.
//
// So the in-flight set below records what has been ASKED FOR and from whom, with
// an expiry. One outstanding request per transaction stops eight peers announcing
// the same id from producing eight identical downloads; the expiry stops a peer
// that never answers from blocking the transaction forever.
//
// Dandelion++ is deliberately untouched. Its stem phase forwards a transaction to
// exactly one successor, where the bandwidth argument does not apply and the
// extra round trip would only widen the timing signal the stem exists to hide —
// so the stem still pushes the body, and only the fluff phase announces.

// txRequestTimeout is how long a request for a transaction body stays
// outstanding before another peer's announcement may be acted on instead.
const txRequestTimeout = 30 * time.Second

// maxTxInFlight bounds the in-flight table. It is a DoS bound, not a tuning
// knob: without it, a peer announcing endless fabricated ids would grow the map
// without limit for the cost of sending hashes.
const maxTxInFlight = 8192

// txRequests tracks which transaction bodies have been requested and when, so
// one announcement is acted on rather than all of them.
type txRequests struct {
	mu  sync.Mutex
	at  map[string]time.Time
	ord []string // insertion order, for bounded eviction
}

func newTxRequests() *txRequests {
	return &txRequests{at: make(map[string]time.Time)}
}

// claim reports whether the caller should request h now, recording the request
// if so. A hash already requested within txRequestTimeout belongs to someone
// else's in-flight download.
func (r *txRequests) claim(h string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if at, ok := r.at[h]; ok && now.Sub(at) < txRequestTimeout {
		return false
	}
	if _, ok := r.at[h]; !ok {
		if len(r.ord) >= maxTxInFlight {
			oldest := r.ord[0]
			r.ord = r.ord[1:]
			delete(r.at, oldest)
		}
		r.ord = append(r.ord, h)
	}
	r.at[h] = now
	return true
}

// done forgets a request, because the body arrived (or the transaction turned up
// some other way). Leaving it would make a re-broadcast of the same transaction
// wait out the timeout for no reason.
func (r *txRequests) done(h string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.at[h]; !ok {
		return
	}
	delete(r.at, h)
	for i, v := range r.ord {
		if v == h {
			r.ord = append(r.ord[:i], r.ord[i+1:]...)
			break
		}
	}
}

func (r *txRequests) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.at)
}

// announceTx tells peers a transaction exists without sending it. Peers that do
// not speak CapTxInv get the body, exactly as they always did, so this rolls out
// against an older network with no flag day and no loss of propagation.
func (n *Node) announceTx(tx core.Transaction, except *peer) {
	h := tx.Hash()
	inv := Message{Type: MsgTxInv, Hashes: []string{h}}
	full := Message{Type: MsgTx, Tx: &tx}

	n.peersMu.Lock()
	targets := make([]*peer, 0, len(n.peers))
	for p := range n.peers {
		if p != except {
			targets = append(targets, p)
		}
	}
	n.peersMu.Unlock()

	for _, p := range targets {
		if p.supports(CapTxInv) {
			p.send(inv)
		} else {
			p.send(full)
		}
	}
}

// onTxInv answers an announcement: request the ids we do not already hold and
// have not already asked someone else for.
func (n *Node) onTxInv(p *peer, hashes []string) {
	if len(hashes) > maxTxInvBatch {
		hashes = hashes[:maxTxInvBatch]
	}
	now := time.Now()
	want := make([]string, 0, len(hashes))
	for _, h := range hashes {
		if !looksLikeTxid(h) {
			continue
		}
		if n.seenTx.has(h) {
			continue
		}
		if _, ok := n.mempool.Get(h); ok {
			continue
		}
		if !n.txReq.claim(h, now) {
			continue
		}
		want = append(want, h)
	}
	if len(want) > 0 {
		p.send(Message{Type: MsgGetTx, Hashes: want})
	}
}

// onGetTx serves the bodies a peer asked for. Unknown ids are simply omitted:
// the pool is a moving target and a transaction can be mined out of it between
// the announcement and the request, which is normal rather than misbehaviour.
func (n *Node) onGetTx(p *peer, hashes []string) {
	if len(hashes) > maxTxInvBatch {
		hashes = hashes[:maxTxInvBatch]
	}
	txs := make([]core.Transaction, 0, len(hashes))
	for _, h := range hashes {
		if tx, ok := n.mempool.Get(h); ok {
			txs = append(txs, tx)
			if len(txs) >= maxMempoolBatch {
				break
			}
		}
	}
	if len(txs) > 0 {
		p.send(Message{Type: MsgTxs, Txs: txs})
	}
}

// looksLikeTxid screens an announced id before it is put in a request. A peer
// supplies these, so they are attacker-controlled: without the check, a peer
// could have us echo arbitrary strings back at it and hold them in the in-flight
// table. The bodies themselves are validated on arrival regardless.
func looksLikeTxid(h string) bool {
	if len(h) != 64 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
