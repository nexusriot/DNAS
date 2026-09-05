package node

import (
	"sync"

	"github.com/nexusriot/DNAS/core"
)

// A block whose parent we do not have yet cannot be applied — but throwing it
// away is wasteful and, during sync, self-defeating. It happens constantly for
// two reasons:
//
//   - gossip races: a peer announces block N+1 while block N is still in flight;
//   - parallel sync: ranges are downloaded from several peers at once, so a later
//     window can land before the one before it.
//
// orphanPool holds those blocks, indexed by the parent they are waiting for, and
// the node drains them as soon as that parent connects. Without it every
// out-of-order block costs a full headers-then-bodies round trip to fetch again.
//
// It is bounded and only accepts blocks that are valid on their own terms
// (proof-of-work, merkle root, leading coinbase), so a peer cannot fill a node's
// memory with cheap junk.
//
// The bound is on BYTES as well as blocks. A count alone does not bound memory:
// 512 blocks at core.MaxBlockBytes each is ~512 MB, and every one of them is a
// legitimately-mined block a syncing node has every reason to hold. Blocks are
// wildly uneven in size, so the two limits catch different things — many small
// out-of-order blocks during a fast sync, and a handful of full ones.
type orphanPool struct {
	mu       sync.Mutex
	byPrev   map[string][]core.Block // parent hash -> blocks waiting for it
	have     map[string]struct{}     // block hashes held, to ignore duplicates
	sizes    map[string]int          // block hash -> its buffered byte size
	order    []string                // insertion order, for bounded FIFO eviction
	bytes    int                     // running total of buffered sizes
	max      int
	maxBytes int
}

func newOrphanPool(max, maxBytes int) *orphanPool {
	if max <= 0 {
		max = 1
	}
	if maxBytes <= 0 {
		maxBytes = orphanPoolBytes
	}
	return &orphanPool{
		byPrev:   map[string][]core.Block{},
		have:     map[string]struct{}{},
		sizes:    map[string]int{},
		max:      max,
		maxBytes: maxBytes,
	}
}

// blockBytes is a block's buffered size: the serialized size of its
// transactions, which is what dominates it.
func blockBytes(b core.Block) int {
	n := 0
	for _, tx := range b.Transactions {
		n += tx.Size()
	}
	return n
}

// add buffers a block to wait for its parent, and reports whether it was newly
// stored. The caller is responsible for having checked core.Block.SelfValid.
func (p *orphanPool) add(b core.Block) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, dup := p.have[b.Hash]; dup {
		return false
	}
	size := blockBytes(b)
	// A single block larger than the whole budget is refused rather than allowed
	// to evict everything and then not fit.
	if size > p.maxBytes {
		return false
	}
	for len(p.order) > 0 && (len(p.order) >= p.max || p.bytes+size > p.maxBytes) {
		p.evictOldestLocked()
	}
	p.byPrev[b.PrevHash] = append(p.byPrev[b.PrevHash], b)
	p.have[b.Hash] = struct{}{}
	p.sizes[b.Hash] = size
	p.bytes += size
	p.order = append(p.order, b.Hash)
	return true
}

// Bytes is the total size of the buffered blocks, for reporting.
func (p *orphanPool) byteLen() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bytes
}

// evictOldestLocked drops the longest-held block. p.mu held.
func (p *orphanPool) evictOldestLocked() {
	oldest := p.order[0]
	p.order = p.order[1:]
	delete(p.have, oldest)
	p.releaseLocked(oldest)
	for prev, blocks := range p.byPrev {
		for i, b := range blocks {
			if b.Hash != oldest {
				continue
			}
			p.byPrev[prev] = append(blocks[:i], blocks[i+1:]...)
			if len(p.byPrev[prev]) == 0 {
				delete(p.byPrev, prev)
			}
			return
		}
	}
}

// releaseLocked gives back the byte budget a block was holding. p.mu held.
func (p *orphanPool) releaseLocked(hash string) {
	if p.bytes -= p.sizes[hash]; p.bytes < 0 {
		p.bytes = 0
	}
	delete(p.sizes, hash)
}

// takeChildren removes and returns the blocks waiting for the given parent hash.
func (p *orphanPool) takeChildren(parentHash string) []core.Block {
	p.mu.Lock()
	defer p.mu.Unlock()
	children := p.byPrev[parentHash]
	if len(children) == 0 {
		return nil
	}
	delete(p.byPrev, parentHash)
	for _, b := range children {
		delete(p.have, b.Hash)
		// The byte budget has to be released here as well as on eviction: this is
		// the SUCCESS path, the one every buffered block leaves by when its parent
		// turns up, so a leak here would ratchet the counter up over a node's whole
		// uptime until the pool refused everything and sync stalled on every gap.
		p.releaseLocked(b.Hash)
		for i, h := range p.order {
			if h == b.Hash {
				p.order = append(p.order[:i], p.order[i+1:]...)
				break
			}
		}
	}
	return children
}

// len reports how many blocks are buffered.
func (p *orphanPool) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.have)
}
