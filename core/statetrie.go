package core

import (
	"container/list"
	"sort"
	"sync"
)

// Incremental state commitment.
//
// The trie made absence provable (trie.go). It did not make the root cheap: a
// root was still built by inserting EVERY account into a throwaway store, so
// the cost of committing a block grew with the size of the ledger rather than
// with the size of the block. Measured on this machine, a 100k-account ledger
// cost 2.25s per root — paid once per block validated, once per block mined,
// and once per /stateproof served, which made an unauthenticated read endpoint
// the most expensive thing a node does.
//
// A content-addressed trie does not need any of that. A node is written once
// and named by its own hash, so an update rewrites only the path from the
// changed leaf to the root — O(depth) nodes — and every root that ever existed
// stays readable from the same store. The undo log already records exactly
// which accounts a block touched, so the set of keys to update is in hand
// before the root is asked for.
//
// Two properties make this safe to bolt onto the existing path:
//
//   - The account map remains the authoritative state. The trie is a
//     commitment over it, never the source of it. (Reading state THROUGH the
//     trie, and so letting it outgrow memory, is the separate ROADMAP §1 item.)
//   - The node store is a CACHE. Nodes are immutable and content-addressed, so
//     a store that has been evicted, or that has never been populated at all
//     (a freshly loaded chain), can only cause a miss — never a wrong answer. A
//     miss falls back to the full rebuild, which repopulates the store on its
//     way through. Correctness does not depend on the cache being warm; only
//     speed does.

// stateCommitter computes state roots incrementally against a shared node
// store. The zero value is not usable; see Blockchain.committerAt.
type stateCommitter struct {
	store NodeStore
	root  string
}

// rootAfter returns the state root for `state` given that `undo` lists every
// account the block changed. It updates only those keys.
//
// On any store miss it falls back to building the trie from the whole account
// set, which is the old behaviour and always correct — the fallback is what
// lets the store be a bounded cache rather than a second source of truth.
func (c *stateCommitter) rootAfter(state map[string]Account, undo []undoEntry) string {
	touched := touchedAddrs(undo)
	t := NewTrie(c.store, c.root)
	for _, addr := range touched {
		value := ""
		if acc, ok := state[addr]; ok {
			value = stateLeaf(addr, acc)
		}
		if _, err := t.Update(trieKeyFor(addr), value); err != nil {
			// A missing node (an evicted or never-populated cache) is the only
			// reachable error here; a key collision would be a sha256 collision.
			// Either way the whole-set rebuild is authoritative.
			return stateRootInto(c.store, state)
		}
	}
	c.root = t.Root()
	return c.root
}

// commitCtx carries the incremental commitments a block's validation needs:
// the account trie's position, and the history accumulator over the block
// hashes below it. Both are maintained as the chain advances rather than
// rebuilt per block, which is the difference between a cost that tracks the
// block and one that tracks the chain.
//
// A nil commitCtx — or a nil field within one — means "no incremental state
// available": the caller gets the from-scratch computation, which is always
// correct and is what the tests and any chainless caller run on.
type commitCtx struct {
	state   *stateCommitter
	history *MMR
}

// commitStateRoot is the root of `state` after a block whose changes `undo`
// records.
func commitStateRoot(cc *commitCtx, state map[string]Account, undo []undoEntry) string {
	if cc == nil || cc.state == nil {
		return stateRoot(state)
	}
	return cc.state.rootAfter(state, undo)
}

// expectedHistoryRoot is the MMR root a block at this position must commit,
// or "" when the accumulator is unavailable and the check must be skipped.
func (cc *commitCtx) expectedHistoryRoot() string {
	if cc == nil || cc.history == nil {
		return ""
	}
	return cc.history.Root()
}

// advance moves the context onto a block that has just validated, so the next
// block in a suffix is checked against the right position. The state
// committer advanced itself when it produced the root; only the append-only
// accumulator is left.
func (cc *commitCtx) advance(block Block) {
	if cc == nil || cc.history == nil {
		return
	}
	if IsUpgradeActive(UpgradeHistoryRoot, block.Index) {
		cc.history.Append(block.Hash)
	}
}

// touchedAddrs is the deduplicated set of accounts a block's undo log mentions,
// in a deterministic order. The order does not affect the root (the trie is
// canonical) but it keeps the nodes written to the store reproducible.
func touchedAddrs(undo []undoEntry) []string {
	seen := make(map[string]struct{}, len(undo))
	out := make([]string, 0, len(undo))
	for _, u := range undo {
		if _, ok := seen[u.addr]; ok {
			continue
		}
		seen[u.addr] = struct{}{}
		out = append(out, u.addr)
	}
	sort.Strings(out)
	return out
}

// commitCtxAt returns the commitment context for building on the block at
// index `at`: the account trie positioned at that block's committed state
// root, and a private copy of the history accumulator as of that block.
//
// Any root the chain has ever committed is valid for the trie, which is what
// makes a reorg cost nothing there: rolling back to the fork point is just
// naming its root. The accumulator is append-only and so cannot be rewound the
// same way — a fork point below the tip gets a rebuild (see historyUpTo).
//
// Callers hold bc.mu.
func (bc *Blockchain) commitCtxAt(at int) *commitCtx {
	return &commitCtx{
		state:   &stateCommitter{store: bc.nodes, root: bc.blocks[at].StateRoot},
		history: bc.historyUpTo(at),
	}
}

// historyUpTo returns an accumulator over the block hashes at and below index
// `at`. At the tip that is the maintained range (cloned, so a speculative
// append cannot disturb it); below it the range is rebuilt, which only happens
// on a reorg and is bounded by the chain's length.
func (bc *Blockchain) historyUpTo(at int) *MMR {
	if at == len(bc.blocks)-1 && bc.history != nil {
		return bc.history.Clone()
	}
	m := &MMR{}
	for i := 0; i <= at; i++ {
		if IsUpgradeActive(UpgradeHistoryRoot, bc.blocks[i].Index) {
			m.Append(bc.blocks[i].Hash)
		}
	}
	return m
}

// boundedNodeStore is a node store with a cap on how many nodes it retains,
// evicting least-recently-used entries past it.
//
// The cap exists because the store accumulates every version of every node a
// chain has ever had: without eviction it is a memory leak shaped like a
// history. Eviction is safe precisely because the store is a cache (see the
// file comment) — the cost of evicting a node that is still referenced is one
// full rebuild, not a wrong root.
type boundedNodeStore struct {
	mu    sync.Mutex
	max   int
	nodes map[string]*list.Element
	order *list.List // front = most recently used
}

type storedNode struct {
	hash string
	data []byte
}

// DefaultTrieCacheNodes is how many trie nodes a chain retains by default.
// A node is ~100 bytes, so this is a budget of roughly 25 MB — enough to hold
// the live trie of a sizeable ledger plus the recent history a reorg can reach
// back through.
const DefaultTrieCacheNodes = 250_000

// NewBoundedNodeStore returns a node store retaining at most max nodes
// (max <= 0 means unbounded).
func NewBoundedNodeStore(max int) NodeStore {
	return &boundedNodeStore{max: max, nodes: map[string]*list.Element{}, order: list.New()}
}

func (s *boundedNodeStore) GetNode(hash string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.nodes[hash]
	if !ok {
		return nil, false
	}
	s.order.MoveToFront(el)
	return el.Value.(*storedNode).data, true
}

func (s *boundedNodeStore) PutNode(hash string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.nodes[hash]; ok {
		s.order.MoveToFront(el)
		return nil
	}
	s.nodes[hash] = s.order.PushFront(&storedNode{hash: hash, data: append([]byte(nil), data...)})
	if s.max > 0 {
		for s.order.Len() > s.max {
			back := s.order.Back()
			s.order.Remove(back)
			delete(s.nodes, back.Value.(*storedNode).hash)
		}
	}
	return nil
}

// Len reports how many nodes are retained. Used by the tests and by /metrics.
func (s *boundedNodeStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.order.Len()
}

// TrieCacheSize reports how many trie nodes the chain currently retains, for
// operators watching the memory the commitment cache is allowed to use.
func (bc *Blockchain) TrieCacheSize() int {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if s, ok := bc.nodes.(*boundedNodeStore); ok {
		return s.Len()
	}
	return 0
}
