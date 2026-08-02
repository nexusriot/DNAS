package core

// Confirmed transactions are located through an in-memory index built as blocks
// connect, so a lookup ("where was this txid mined?") is one map hit instead of a
// scan over every block body. The index is derived state — it can always be
// rebuilt by walking the chain — and is maintained across reorgs: blocks are
// unindexed from the tip downwards before the winning suffix is indexed upwards.
//
// A caveat the index inherits from the ledger: a coinbase transaction commits
// only (recipient, amount), so two blocks paying the same miner the same subsidy
// carry a byte-identical coinbase and therefore the same txid. The index keeps
// the FIRST (lowest) height for a duplicated txid, exactly matching the
// lowest-height-wins behaviour of the linear scan it replaces. Because blocks are
// only ever disconnected from the top, the surviving first occurrence is never
// removed by mistake.

// TxLoc is where a confirmed transaction sits: the height of the block holding
// it and its position in that block's transaction list.
type TxLoc struct {
	Height uint64 `json:"height"`
	Index  int    `json:"index"`
}

// indexBlock records b's transactions, keeping any existing (necessarily lower)
// entry for a duplicated txid. bc.mu must be held for writing.
func (bc *Blockchain) indexBlock(b Block) {
	for i, tx := range b.Transactions {
		h := tx.Hash()
		if _, exists := bc.txIndex[h]; exists {
			continue // first occurrence wins
		}
		bc.txIndex[h] = TxLoc{Height: b.Index, Index: i}
	}
}

// unindexBlock drops b's transactions from the index, leaving entries that point
// at a different (lower, still-connected) block untouched. bc.mu must be held for
// writing.
func (bc *Blockchain) unindexBlock(b Block) {
	for _, tx := range b.Transactions {
		h := tx.Hash()
		if loc, ok := bc.txIndex[h]; ok && loc.Height == b.Index {
			delete(bc.txIndex, h)
		}
	}
}

// buildTxIndex indexes a whole chain from scratch, for a Blockchain assembled
// other than by connecting blocks one at a time.
func buildTxIndex(blocks []Block) map[string]TxLoc {
	idx := make(map[string]TxLoc)
	for _, b := range blocks {
		for i, tx := range b.Transactions {
			h := tx.Hash()
			if _, exists := idx[h]; exists {
				continue
			}
			idx[h] = TxLoc{Height: b.Index, Index: i}
		}
	}
	return idx
}

// FindTx returns a confirmed transaction and where it is confirmed. The bool is
// false if no block on the current chain holds it (it may still be pending in the
// mempool, which the chain knows nothing about).
func (bc *Blockchain) FindTx(hash string) (Transaction, TxLoc, bool) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.findTxLocked(hash)
}

// findTxLocked is FindTx without the lock. bc.mu must be held.
func (bc *Blockchain) findTxLocked(hash string) (Transaction, TxLoc, bool) {
	loc, ok := bc.txIndex[hash]
	if !ok || loc.Height >= uint64(len(bc.blocks)) {
		return Transaction{}, TxLoc{}, false
	}
	b := bc.blocks[loc.Height]
	if loc.Index >= len(b.Transactions) {
		return Transaction{}, TxLoc{}, false
	}
	return b.Transactions[loc.Index], loc, true
}

// HasTx reports whether a transaction is confirmed on the current chain.
func (bc *Blockchain) HasTx(hash string) bool {
	_, _, ok := bc.FindTx(hash)
	return ok
}
