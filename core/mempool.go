package core

import (
	"errors"
	"sort"
	"sync"
)

// DefaultMempoolSize is the number of pending transactions kept before the
// mempool starts evicting the lowest-fee transaction to make room.
const DefaultMempoolSize = 5000

// Mempool holds validated, not-yet-mined transactions keyed by hash. It is
// bounded: once full, a new transaction is admitted only if it pays a strictly
// higher fee than the cheapest one already queued, which it then evicts.
type Mempool struct {
	mu  sync.Mutex
	txs map[string]Transaction
	max int
}

// NewMempool returns an empty mempool with the default size limit.
func NewMempool() *Mempool { return NewMempoolWithLimit(DefaultMempoolSize) }

// NewMempoolWithLimit returns an empty mempool holding at most max transactions
// (values <= 0 fall back to the default).
func NewMempoolWithLimit(max int) *Mempool {
	if max <= 0 {
		max = DefaultMempoolSize
	}
	return &Mempool{txs: map[string]Transaction{}, max: max}
}

// Add verifies the transaction's signature and stores it. Returns whether it
// was newly added. Behaviour:
//   - an exact duplicate (same hash) is a no-op: (false, nil);
//   - a transaction with the same sender and nonce as one already queued
//     replaces it if and only if it pays a strictly higher fee (replace-by-fee /
//     fee-bumping); a same-or-lower fee is rejected with an error;
//   - otherwise, if the pool is full, it is admitted only by out-bidding the
//     cheapest queued transaction, which it evicts.
func (m *Mempool) Add(tx Transaction) (bool, error) {
	if tx.IsCoinbase() {
		return false, errors.New("cannot add coinbase to mempool")
	}
	if err := tx.VerifySignature(); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	h := tx.Hash()
	if _, ok := m.txs[h]; ok {
		return false, nil
	}

	// Replace-by-fee: a conflicting tx (same sender+nonce) may only be replaced
	// by a higher fee.
	if oldHash, old, ok := m.conflictLocked(tx); ok {
		if tx.Fee <= old.Fee {
			return false, errors.New("replacement fee not higher than existing transaction")
		}
		delete(m.txs, oldHash)
		m.txs[h] = tx
		return true, nil
	}

	if len(m.txs) >= m.max {
		minHash, minFee := m.lowestFeeLocked()
		if tx.Fee <= minFee {
			return false, errors.New("mempool full and fee too low")
		}
		delete(m.txs, minHash)
	}
	m.txs[h] = tx
	return true, nil
}

// conflictLocked finds a queued transaction with the same sender and nonce as
// tx (a replace-by-fee candidate). The caller must hold m.mu.
func (m *Mempool) conflictLocked(tx Transaction) (hash string, existing Transaction, ok bool) {
	for h, t := range m.txs {
		if t.From == tx.From && t.Nonce == tx.Nonce {
			return h, t, true
		}
	}
	return "", Transaction{}, false
}

// PruneExpired removes transactions that can no longer be included in any block
// built on top of the current tip height, and returns how many were dropped.
func (m *Mempool) PruneExpired(tipHeight uint64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for h, tx := range m.txs {
		if tx.IsExpiredAt(tipHeight + 1) { // the next block is at tipHeight+1
			delete(m.txs, h)
			n++
		}
	}
	return n
}

// lowestFeeLocked returns the hash and fee of the cheapest queued transaction.
// The caller must hold m.mu and the pool must be non-empty.
func (m *Mempool) lowestFeeLocked() (hash string, fee uint64) {
	fee = ^uint64(0)
	for h, tx := range m.txs {
		if tx.Fee < fee {
			fee, hash = tx.Fee, h
		}
	}
	return hash, fee
}

// All returns a snapshot of pending transactions.
func (m *Mempool) All() []Transaction {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Transaction, 0, len(m.txs))
	for _, tx := range m.txs {
		out = append(out, tx)
	}
	return out
}

// Remove deletes the given transactions (e.g. after they are mined).
func (m *Mempool) Remove(txs []Transaction) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tx := range txs {
		delete(m.txs, tx.Hash())
	}
}

// Size returns the number of pending transactions.
func (m *Mempool) Size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.txs)
}

// Select greedily chooses up to max transactions that form a valid sequence on
// top of the current chain state: each must have the sender's next nonce and be
// affordable. Among ready candidates it prefers higher fees. Recipients are
// credited in the simulation so chained spends within one block are possible.
func (m *Mempool) Select(bc *Blockchain, max int) []Transaction {
	all := m.All()
	mineHeight := bc.Height() + 1 // the block we're selecting for

	type sim struct {
		balance uint64
		nonce   uint64
	}
	cache := map[string]sim{}
	get := func(addr string) sim {
		if s, ok := cache[addr]; ok {
			return s
		}
		acc := bc.Account(addr)
		s := sim{balance: acc.Balance, nonce: acc.Nonce}
		cache[addr] = s
		return s
	}

	var selected []Transaction
	used := make(map[string]bool)
	for len(selected) < max {
		var ready []Transaction
		for _, tx := range all {
			if used[tx.Hash()] || tx.IsExpiredAt(mineHeight) {
				continue
			}
			s := get(tx.From)
			if tx.Nonce != s.nonce || s.balance < tx.Amount+tx.Fee {
				continue
			}
			ready = append(ready, tx)
		}
		if len(ready) == 0 {
			break
		}
		sort.Slice(ready, func(i, j int) bool { return ready[i].Fee > ready[j].Fee })
		pick := ready[0]

		s := get(pick.From)
		s.balance -= pick.Amount + pick.Fee
		s.nonce++
		cache[pick.From] = s
		r := get(pick.To)
		r.balance += pick.Amount
		cache[pick.To] = r

		selected = append(selected, pick)
		used[pick.Hash()] = true
	}
	return selected
}
