package core

import "sort"

// An optional index from address to the transactions that touched it.
//
// Every address-history consumer without one has to reconstruct history the way
// a light client does — download compact filters, fetch the blocks they flag,
// and fold them — which is right for a client that trusts nothing, and wasteful
// for an explorer or a wallet talking to a full node that already holds every
// block. This index answers "what has this address done?" with one map lookup.
//
// It is OFF by default and opt-in per node (see EnableAddressIndex), because it
// is the one index whose size is not bounded by the chain: an address that
// appears in a million transactions has a million entries. Like the transaction
// index it is derived state — rebuildable by walking the chain — and it is
// maintained across reorgs by the same unindex-from-the-tip-down discipline.
//
// It can only index what the node has. On a fast-synced chain the bodies below
// the snapshot are pruned (they are header-only placeholders), so history there
// is simply absent rather than wrong — an index built on such a node starts at
// its snapshot height.

// AddressEntry is one appearance of an address in the chain: where the
// transaction is, and the transaction itself.
type AddressEntry struct {
	Height uint64      `json:"height"`
	Index  int         `json:"index"`
	Hash   string      `json:"hash"`
	Tx     Transaction `json:"tx"`
}

// txAddresses returns the distinct addresses one transaction touches: its sender
// (unless it is a coinbase, which has none), every recipient, and — since a
// sponsor's balance moves too — the fee payer.
func txAddresses(tx Transaction) []string {
	set := make(map[string]struct{}, 4)
	if !tx.IsCoinbase() && tx.From != "" {
		set[tx.From] = struct{}{}
	}
	if tx.To != "" {
		set[tx.To] = struct{}{}
	}
	for _, o := range tx.Outputs {
		if o.To != "" {
			set[o.To] = struct{}{}
		}
	}
	if tx.FeePayer != "" {
		set[tx.FeePayer] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// EnableAddressIndex turns the address index on and builds it from the chain as
// it stands. Call it at startup: from then on it is maintained incrementally as
// blocks connect and disconnect.
func (bc *Blockchain) EnableAddressIndex() {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	bc.addrIndex = map[string][]TxLoc{}
	for _, b := range bc.blocks {
		bc.indexBlockAddressesLocked(b)
	}
}

// AddressIndexed reports whether this node maintains the address index.
func (bc *Blockchain) AddressIndexed() bool {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.addrIndex != nil
}

// indexBlockAddressesLocked records b's transactions under every address they
// touch. bc.mu held for writing; a no-op when the index is disabled.
func (bc *Blockchain) indexBlockAddressesLocked(b Block) {
	if bc.addrIndex == nil {
		return
	}
	for i, tx := range b.Transactions {
		loc := TxLoc{Height: b.Index, Index: i}
		for _, addr := range txAddresses(tx) {
			bc.addrIndex[addr] = append(bc.addrIndex[addr], loc)
		}
	}
}

// unindexBlockAddressesLocked drops every entry pointing at b, for a block being
// disconnected by a reorg. bc.mu held for writing.
func (bc *Blockchain) unindexBlockAddressesLocked(b Block) {
	if bc.addrIndex == nil {
		return
	}
	for _, tx := range b.Transactions {
		for _, addr := range txAddresses(tx) {
			locs := bc.addrIndex[addr]
			kept := locs[:0]
			for _, loc := range locs {
				if loc.Height != b.Index {
					kept = append(kept, loc)
				}
			}
			if len(kept) == 0 {
				delete(bc.addrIndex, addr)
				continue
			}
			bc.addrIndex[addr] = kept
		}
	}
}

// AddressHistory returns the transactions that touched addr, oldest first,
// starting at height `from` and capped at `limit` entries (a limit <= 0 means
// DefaultAddressHistoryLimit). The bool is false when the node does not maintain
// the index, which callers must distinguish from an address with no history.
func (bc *Blockchain) AddressHistory(addr string, from uint64, limit int) ([]AddressEntry, bool) {
	if limit <= 0 {
		limit = DefaultAddressHistoryLimit
	}
	if limit > MaxAddressHistoryLimit {
		limit = MaxAddressHistoryLimit
	}
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if bc.addrIndex == nil {
		return nil, false
	}
	locs := bc.addrIndex[addr]
	out := make([]AddressEntry, 0, min(limit, len(locs)))
	for _, loc := range locs {
		if loc.Height < from {
			continue
		}
		if len(out) >= limit {
			break
		}
		if loc.Height >= uint64(len(bc.blocks)) {
			continue // index raced a disconnect; the entry is about to be removed
		}
		b := bc.blocks[loc.Height]
		if loc.Index >= len(b.Transactions) {
			continue
		}
		tx := b.Transactions[loc.Index]
		out = append(out, AddressEntry{Height: loc.Height, Index: loc.Index, Hash: tx.Hash(), Tx: tx})
	}
	return out, true
}

// AddressHistoryLen is how many indexed appearances an address has, for paging.
// The bool is false when the index is disabled.
func (bc *Blockchain) AddressHistoryLen(addr string) (int, bool) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if bc.addrIndex == nil {
		return 0, false
	}
	return len(bc.addrIndex[addr]), true
}
