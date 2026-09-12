package node

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/nexusriot/DNAS/core"
)

// Compact block relay (the shape of BIP152).
//
// A block was announced as a hash and then pulled in full, so every peer
// downloaded every transaction a second time — once when it was relayed into the
// mempool, once inside the block. On a busy chain that is most of a block's bytes
// arriving twice, at exactly the moment latency matters most: the longer a block
// takes to propagate, the more work the network wastes mining on a stale tip.
//
// A compact block replaces the bodies with 8-byte SHORT IDS. A peer resolves them
// against the transactions it already holds and reconstructs the block locally,
// asking only for the few it is missing. What crosses the wire for a full block
// of a thousand payments is a header, a coinbase and eight kilobytes of ids.
//
// Two things make this safe rather than merely clever.
//
// The short ids are KEYED BY THE BLOCK. An id is the first 8 bytes of
// sha256(block hash || txid), so an attacker cannot precompute a transaction that
// collides with one in a block that has not been mined yet — the block hash is
// not known until the proof of work is found. Without the key, colliding ids
// could be prepared in advance and used to make peers reconstruct blocks that
// differ from the miner's.
//
// And reconstruction is VERIFIED before it is believed: the assembled body must
// produce the merkle root the header commits to. An honest short-id collision
// (two pool transactions sharing 8 bytes) resolves the wrong transaction, the
// root does not match, and the node falls back to requesting the full block — so
// a collision costs a round trip, never a wrong chain.

// CapCompact advertises that a peer understands compact block relay.
const CapCompact = "cmpct"

// shortIDLen is how many bytes of the keyed hash identify a transaction.
const shortIDLen = 8

// maxPendingBlocks bounds how many partially-reconstructed blocks are held at
// once, and pendingBlockTTL how long one may wait for its missing transactions.
// A peer that sends compact blocks it never completes would otherwise pin memory.
const (
	maxPendingBlocks = 16
	pendingBlockTTL  = 60 * time.Second
)

// CompactBlock is a block with its non-coinbase transactions replaced by short
// ids. The coinbase is always sent in full: it is the one transaction no peer can
// have in its mempool, because it does not exist until the block is mined.
type CompactBlock struct {
	Header   core.Header      `json:"header"`
	Coinbase core.Transaction `json:"coinbase"`
	ShortIDs []string         `json:"short_ids"` // hex, in block order
}

// shortID is the block-keyed identifier of a transaction.
func shortID(blockHash, txid string) string {
	h := sha256.Sum256([]byte(blockHash + "|" + txid))
	return hex.EncodeToString(h[:shortIDLen])
}

// NewCompactBlock summarizes a block for relay.
func NewCompactBlock(b core.Block) *CompactBlock {
	cb := &CompactBlock{Header: b.Header()}
	if len(b.Transactions) > 0 {
		cb.Coinbase = b.Transactions[0]
	}
	cb.ShortIDs = make([]string, 0, len(b.Transactions))
	for _, tx := range b.Transactions[1:] {
		cb.ShortIDs = append(cb.ShortIDs, shortID(b.Hash, tx.Hash()))
	}
	return cb
}

// pendingBlock is a reconstruction waiting on transactions a peer must supply.
type pendingBlock struct {
	header   core.Header
	txs      []core.Transaction // block order; holes are zero-valued
	missing  []uint32           // positions still unfilled (1-based block positions)
	from     *peer
	deadline time.Time
}

// blockAssembly holds reconstructions in progress, keyed by block hash.
type blockAssembly struct {
	mu      sync.Mutex
	pending map[string]*pendingBlock
}

func newBlockAssembly() *blockAssembly {
	return &blockAssembly{pending: make(map[string]*pendingBlock)}
}

// put records a partial reconstruction, evicting the oldest when full and
// dropping anything that has timed out.
func (a *blockAssembly) put(hash string, pb *pendingBlock, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for h, p := range a.pending {
		if now.After(p.deadline) {
			delete(a.pending, h)
		}
	}
	for len(a.pending) >= maxPendingBlocks {
		var oldest string
		var at time.Time
		for h, p := range a.pending {
			if oldest == "" || p.deadline.Before(at) {
				oldest, at = h, p.deadline
			}
		}
		delete(a.pending, oldest)
	}
	a.pending[hash] = pb
}

// take removes and returns a reconstruction, if it is still waiting.
func (a *blockAssembly) take(hash string, now time.Time) (*pendingBlock, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pb, ok := a.pending[hash]
	if !ok {
		return nil, false
	}
	delete(a.pending, hash)
	if now.After(pb.deadline) {
		return nil, false
	}
	return pb, true
}

func (a *blockAssembly) len() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending)
}

// announceBlock tells peers about a new block: a compact block to those that can
// reconstruct one, a bare hash to the rest. Both paths end with the peer holding
// the block, so a peer that speaks neither loses nothing.
func (n *Node) announceBlock(b core.Block, except *peer) {
	inv := Message{Type: MsgInv, Index: b.Index, Hash: b.Hash}
	var cmpct Message
	// A placeholder (a pruned body) has nothing to summarize, and an empty block
	// is already smaller as an inv than as a compact block.
	if !b.IsPlaceholder() && len(b.Transactions) > 1 {
		cmpct = Message{Type: MsgCmpctBlock, Cmpct: NewCompactBlock(b)}
	}

	n.peersMu.Lock()
	targets := make([]*peer, 0, len(n.peers))
	for p := range n.peers {
		if p != except {
			targets = append(targets, p)
		}
	}
	n.peersMu.Unlock()

	for _, p := range targets {
		if cmpct.Type != "" && p.supports(CapCompact) {
			p.send(cmpct)
		} else {
			p.send(inv)
		}
	}
}

// onCompactBlock reconstructs an announced block from the transactions this node
// already holds, requesting only what it lacks.
func (n *Node) onCompactBlock(p *peer, cb *CompactBlock) {
	if cb == nil {
		return
	}
	hash := cb.Header.Hash
	if hash == "" || len(cb.ShortIDs) > core.MaxBlockTxs {
		return
	}
	if n.seenBlk.has(hash) {
		return
	}
	// Nothing below this point trusts the peer: the header's own proof of work is
	// checked first, so reconstructing a block costs an attacker real hashing
	// rather than a cheap message.
	if !cb.Header.HasValidPoW() {
		if n.bans.add(p.id, banInvalidBlock) {
			Warnf("peer banned", "peer", short(p.id), "reason", "compact block with invalid proof of work")
		}
		return
	}

	// Index what we hold by this block's keyed short id.
	byID := make(map[string]core.Transaction, n.mempool.Size())
	for _, tx := range n.mempool.All() {
		byID[shortID(hash, tx.Hash())] = tx
	}

	txs := make([]core.Transaction, len(cb.ShortIDs)+1)
	txs[0] = cb.Coinbase
	var missing []uint32
	for i, id := range cb.ShortIDs {
		if tx, ok := byID[id]; ok {
			txs[i+1] = tx
			continue
		}
		missing = append(missing, uint32(i+1))
	}

	if len(missing) == 0 {
		n.completeBlock(p, cb.Header, txs)
		return
	}
	n.assembly.put(hash, &pendingBlock{
		header:   cb.Header,
		txs:      txs,
		missing:  missing,
		from:     p,
		deadline: time.Now().Add(pendingBlockTTL),
	}, time.Now())
	Debugf("compact block needs transactions", "hash", short(hash),
		"missing", len(missing), "of", len(cb.ShortIDs))
	p.send(Message{Type: MsgGetBlockTxn, Index: cb.Header.Index, Hash: hash, Indexes: missing})
}

// onGetBlockTxn serves the transactions a peer could not resolve.
func (n *Node) onGetBlockTxn(p *peer, index uint64, hash string, indexes []uint32) {
	b, ok := n.chain.BlockAt(index)
	// The hash is checked as well as the height: a reorg may have replaced the
	// block at that height since the request was sent, and serving the new one's
	// transactions under the old one's hash would corrupt the reconstruction.
	if !ok || b.Hash != hash || b.IsPlaceholder() {
		return
	}
	if len(indexes) > core.MaxBlockTxs {
		indexes = indexes[:core.MaxBlockTxs]
	}
	txs := make([]core.Transaction, 0, len(indexes))
	for _, i := range indexes {
		if int(i) < len(b.Transactions) {
			txs = append(txs, b.Transactions[i])
		}
	}
	p.send(Message{Type: MsgBlockTxn, Hash: hash, Txs: txs})
}

// onBlockTxn fills the holes in a reconstruction and finishes it.
func (n *Node) onBlockTxn(p *peer, hash string, txs []core.Transaction) {
	pb, ok := n.assembly.take(hash, time.Now())
	if !ok {
		return
	}
	if len(txs) != len(pb.missing) {
		// The peer did not send what was asked for; fall back rather than assemble
		// a block from a list whose positions no longer line up.
		p.send(Message{Type: MsgGetData, Index: pb.header.Index})
		return
	}
	for i, pos := range pb.missing {
		if int(pos) >= len(pb.txs) {
			return
		}
		pb.txs[pos] = txs[i]
	}
	n.completeBlock(p, pb.header, pb.txs)
}

// completeBlock rebuilds the block, checks it against the header's commitment,
// and hands it to the ordinary block path. A merkle mismatch means the
// reconstruction resolved a different transaction than the miner used — an
// honest short-id collision or a lying peer — and is answered by asking for the
// block in full, which is always correct if slower.
func (n *Node) completeBlock(p *peer, h core.Header, txs []core.Transaction) {
	b := core.Block{
		Version:      h.Version,
		Index:        h.Index,
		Timestamp:    h.Timestamp,
		Transactions: txs,
		PrevHash:     h.PrevHash,
		MerkleRoot:   h.MerkleRoot,
		StateRoot:    h.StateRoot,
		BaseFee:      h.BaseFee,
		Bits:         h.Bits,
		Nonce:        h.Nonce,
		Hash:         h.Hash,
	}
	if core.MerkleRoot(b.Transactions) != h.MerkleRoot {
		Debugf("compact block did not reconstruct; fetching in full",
			"hash", short(h.Hash), "height", h.Index)
		n.compactMiss.Add(1)
		p.send(Message{Type: MsgGetData, Index: h.Index})
		return
	}
	n.compactHit.Add(1)
	n.onBlockReceived(p, b)
}
