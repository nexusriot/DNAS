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
type orphanPool struct {
	mu     sync.Mutex
	byPrev map[string][]core.Block // parent hash -> blocks waiting for it
	have   map[string]struct{}     // block hashes held, to ignore duplicates
	order  []string                // insertion order, for bounded FIFO eviction
	max    int
}

func newOrphanPool(max int) *orphanPool {
	if max <= 0 {
		max = 1
	}
	return &orphanPool{
		byPrev: map[string][]core.Block{},
		have:   map[string]struct{}{},
		max:    max,
	}
}

// add buffers a block to wait for its parent, and reports whether it was newly
// stored. The caller is responsible for having checked core.Block.SelfValid.
func (p *orphanPool) add(b core.Block) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, dup := p.have[b.Hash]; dup {
		return false
	}
	if len(p.order) >= p.max {
		p.evictOldestLocked()
	}
	p.byPrev[b.PrevHash] = append(p.byPrev[b.PrevHash], b)
	p.have[b.Hash] = struct{}{}
	p.order = append(p.order, b.Hash)
	return true
}

// evictOldestLocked drops the longest-held block. p.mu held.
func (p *orphanPool) evictOldestLocked() {
	oldest := p.order[0]
	p.order = p.order[1:]
	delete(p.have, oldest)
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
