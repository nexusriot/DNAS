package core

import "sort"

// Where the account ledger lives.
//
// Consensus application read and wrote a bare `map[string]Account`. That is the
// right data structure and the wrong INTERFACE: it fixes the ledger's home as
// process memory, so the chain can only hold as many accounts as the node has
// RAM, and no amount of work on the commitment layer changes that. The trie
// made the state root cheap (statetrie.go); it did not and could not make the
// state itself bounded, because nothing in the application path ever asked for
// an account — it indexed a map.
//
// StateDB is that question made explicit. Every read and write on the
// application path goes through it, so the ledger's storage becomes a choice
// rather than an assumption. memState is the map, unchanged in behaviour and
// still the default; a disk-backed implementation is the same interface with a
// different backing, and nothing in applyBlock has to know which it got.
//
// Two properties the application path depends on, which any implementation must
// preserve:
//
//   - A missing account reads as the ZERO Account, not an error. A new address
//     is an account with nothing in it, and consensus has always treated the two
//     as the same thing — "has this address been seen" is not a question any
//     rule asks.
//   - Range is DETERMINISTIC in the order it yields. A map is not, which is why
//     every whole-ledger consumer sorted afterwards; folding that into the
//     interface means a future backend cannot quietly introduce an
//     order-dependence into a state root.
type StateDB interface {
	// Get returns the account at addr, or the zero Account if there is none.
	Get(addr string) Account
	// Lookup is Get plus whether the account exists. The undo log needs the
	// distinction: restoring an account that did not exist means deleting it,
	// not writing a zero one.
	Lookup(addr string) (Account, bool)
	// Put writes an account.
	Put(addr string, acc Account)
	// Delete removes one. Deleting an absent account is a no-op.
	Delete(addr string)
	// Len is how many accounts exist.
	Len() int
	// Range yields every account in ascending address order, stopping early if
	// fn returns false.
	Range(fn func(addr string, acc Account) bool)
	// Snapshot returns an independent copy, for speculative application (a
	// candidate block's state root, a reorg validated before it is committed).
	Snapshot() StateDB
}

// memState is the resident map, which is what every chain uses today.
type memState struct{ m map[string]Account }

// NewMemState returns an in-memory ledger.
func NewMemState() StateDB { return &memState{m: map[string]Account{}} }

// memStateOf adopts an existing map without copying it. It is how the snapshot
// and load paths hand their accounts over.
func memStateOf(m map[string]Account) StateDB {
	if m == nil {
		m = map[string]Account{}
	}
	return &memState{m: m}
}

func (s *memState) Get(addr string) Account { return s.m[addr] }

func (s *memState) Lookup(addr string) (Account, bool) {
	acc, ok := s.m[addr]
	return acc, ok
}

func (s *memState) Put(addr string, acc Account) { s.m[addr] = acc }
func (s *memState) Delete(addr string)           { delete(s.m, addr) }
func (s *memState) Len() int                     { return len(s.m) }

func (s *memState) Range(fn func(addr string, acc Account) bool) {
	addrs := make([]string, 0, len(s.m))
	for a := range s.m {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	for _, a := range addrs {
		if !fn(a, s.m[a]) {
			return
		}
	}
}

func (s *memState) Snapshot() StateDB {
	c := make(map[string]Account, len(s.m))
	for k, v := range s.m {
		c[k] = v
	}
	return &memState{m: c}
}

// stateAsMap materializes a ledger as a plain map.
//
// It exists for the two places that genuinely need every account at once and
// are not on the application path: writing a state snapshot, and serializing a
// chain. Both are already O(ledger) by definition. Nothing in applyBlock may
// call it — that would put the whole ledger back in memory through the side
// door, which is the thing StateDB exists to stop.
func stateAsMap(s StateDB) map[string]Account {
	out := make(map[string]Account, s.Len())
	s.Range(func(addr string, acc Account) bool {
		out[addr] = acc
		return true
	})
	return out
}
