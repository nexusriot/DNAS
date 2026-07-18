package core

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// DefaultMempoolSize is the number of pending transactions kept before the
// mempool starts evicting the lowest-fee transaction to make room.
const DefaultMempoolSize = 5000

// feeFloorMaxMultiplier is how many times the base relay fee the floor reaches
// when the mempool is completely full. The floor grows quadratically with
// occupancy between the base (empty) and base*multiplier (full), so light-fee
// transactions are cheap to relay on an idle network but priced out under load.
const feeFloorMaxMultiplier = 100

// Mempool holds validated, not-yet-mined transactions keyed by hash. It is
// bounded: once full, a new transaction is admitted only if it pays a strictly
// higher fee than the cheapest one already queued, which it then evicts.
//
// It also enforces a dynamic minimum relay fee (see MinFee) as local relay
// policy — NOT a consensus rule. A transaction below the current floor is
// refused entry here, but if it reaches a node in a mined block it is still
// accepted; the floor only governs what this node will queue and gossip.
type Mempool struct {
	mu          sync.Mutex
	txs         map[string]Transaction
	max         int
	minRelayFee uint64 // base per-byte relay floor when empty; 0 disables the fee floor
}

// NewMempool returns an empty mempool with the default size limit and no fee
// floor (base relay fee 0).
func NewMempool() *Mempool { return NewMempoolWithLimit(DefaultMempoolSize) }

// NewMempoolWithLimit returns an empty mempool holding at most max transactions
// (values <= 0 fall back to the default) and no fee floor.
func NewMempoolWithLimit(max int) *Mempool {
	return NewMempoolWithPolicy(max, 0)
}

// NewMempoolWithPolicy returns an empty mempool bounded at max transactions with
// the given base minimum relay fee. The effective floor rises with occupancy
// (see MinFee). A minRelayFee of 0 disables the floor entirely.
func NewMempoolWithPolicy(max int, minRelayFee uint64) *Mempool {
	if max <= 0 {
		max = DefaultMempoolSize
	}
	return &Mempool{txs: map[string]Transaction{}, max: max, minRelayFee: minRelayFee}
}

// MinFee returns the current dynamic relay-fee floor as a rate (base units PER
// BYTE): the least a transaction must pay per byte to be admitted right now. It
// equals the configured base relay fee when the pool is empty and climbs
// quadratically toward base*feeFloorMaxMultiplier as the pool fills. A
// transaction is admitted when its fee ≥ MinFee() × its size. Returns 0 when no
// floor is set.
func (m *Mempool) MinFee() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.minFeeLocked()
}

// minFeeLocked computes the occupancy-scaled fee floor. The caller must hold m.mu.
func (m *Mempool) minFeeLocked() uint64 {
	if m.minRelayFee == 0 || m.max <= 0 {
		return m.minRelayFee
	}
	fill := len(m.txs) * 100 / m.max // occupancy percent, 0..100
	if fill > 100 {
		fill = 100
	}
	// extra = (multiplier-1) * fill^2 / 100^2, so fill=0 -> 0 and fill=100 ->
	// multiplier-1. The square keeps the floor near the base until the pool is
	// genuinely congested, then ramps it steeply.
	extra := uint64(feeFloorMaxMultiplier-1) * uint64(fill*fill) / 10_000
	return m.minRelayFee * (1 + extra)
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

	// Relay policy: refuse anything paying below the current per-byte floor for its
	// size (floor is a rate; a bigger transaction must pay proportionally more).
	if floor := m.minFeeLocked(); floor > 0 && tx.Fee < floor*uint64(tx.Size()) {
		return false, fmt.Errorf("fee %d below current relay floor %d/byte × %d bytes = %d",
			tx.Fee, floor, tx.Size(), floor*uint64(tx.Size()))
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

	// When full, admit only by out-bidding the lowest fee *rate* (fee per byte),
	// which this transaction then evicts — so block space, a per-byte resource, is
	// allocated to the highest-paying transactions per byte.
	if len(m.txs) >= m.max {
		minHash, minRate := m.lowestRateLocked()
		if txRate(tx) <= minRate {
			return false, errors.New("mempool full and fee rate too low")
		}
		delete(m.txs, minHash)
	}
	m.txs[h] = tx
	return true, nil
}

// txRate is a transaction's fee per byte, used only to rank and evict within the
// mempool (relay policy). It is a float for ordering convenience; consensus never
// uses it (block validity is checked with the integer per-byte base fee rule).
func txRate(tx Transaction) float64 {
	return float64(tx.Fee) / float64(tx.Size())
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

// lowestRateLocked returns the hash and fee rate (fee per byte) of the queued
// transaction paying the least per byte. The caller must hold m.mu and the pool
// must be non-empty.
func (m *Mempool) lowestRateLocked() (hash string, rate float64) {
	first := true
	for h, tx := range m.txs {
		if r := txRate(tx); first || r < rate {
			rate, hash, first = r, h, false
		}
	}
	return hash, rate
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

// EstimateTip estimates the tip PER BYTE (the fee above the per-byte base fee,
// which is what the miner actually earns) a new transaction should pay to land
// within the next capacityBytes of block space, ranked by tip rate — a simple
// analog of Bitcoin's estimatesmartfee. It returns 0 when all pending
// transactions fit (no bidding needed), otherwise the tip rate of the marginal
// transaction at the byte cutoff, so a transaction paying just above it displaces
// the queue's tail.
func (m *Mempool) EstimateTip(baseFee uint64, capacityBytes int) uint64 {
	if capacityBytes <= 0 {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	type entry struct {
		rate    float64 // tip per byte, for ranking
		tipRate uint64  // integer tip per byte, the reported estimate
		size    int
	}
	entries := make([]entry, 0, len(m.txs))
	total := 0
	for _, tx := range m.txs {
		size := tx.Size()
		total += size
		var tip uint64
		if min := BaseFeeFor(tx, baseFee); tx.Fee > min {
			tip = tx.Fee - min
		}
		entries = append(entries, entry{rate: float64(tip) / float64(size), tipRate: tip / uint64(size), size: size})
	}
	if total <= capacityBytes {
		return 0 // uncongested: every pending tx fits in the target window
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rate > entries[j].rate })
	filled := 0
	for _, e := range entries {
		filled += e.size
		if filled >= capacityBytes {
			return e.tipRate // marginal tip rate at the byte cutoff
		}
	}
	return 0
}

// Select greedily chooses transactions that form a valid sequence on top of the
// current chain state: each must have the sender's next nonce, be affordable, and
// pay at least its per-byte base fee. It is bounded by both max transactions and
// MaxBlockBytes of total size. Among ready candidates it prefers the highest fee
// rate (fee per byte), so scarce block space goes to the best-paying bytes.
// Recipients are credited in the simulation so chained spends within one block
// are possible.
func (m *Mempool) Select(bc *Blockchain, max int) []Transaction {
	all := m.All()
	mineHeight := bc.Height() + 1 // the block we're selecting for
	baseFee := bc.NextBaseFee()   // the next block's base fee (per byte); txs must cover it

	type sim struct {
		balance uint64
		nonce   uint64
	}
	cache := map[string]sim{}
	get := func(addr string) sim {
		if s, ok := cache[addr]; ok {
			return s
		}
		// Use the spendable balance so immature coinbase isn't selected — the
		// miner would otherwise build a block its own consensus rules reject.
		s := sim{balance: bc.SpendableBalance(addr), nonce: bc.Account(addr).Nonce}
		cache[addr] = s
		return s
	}

	var selected []Transaction
	weight := 0 // running total of selected transaction bytes (<= MaxBlockBytes)
	used := make(map[string]bool)
	for len(selected) < max {
		var ready []Transaction
		for _, tx := range all {
			if used[tx.Hash()] || tx.IsExpiredAt(mineHeight) || tx.IsLockedAt(mineHeight) || tx.HTLCRefundNotReady(mineHeight) || tx.Fee < BaseFeeFor(tx, baseFee) {
				continue
			}
			if weight+tx.Size() > MaxBlockBytes { // wouldn't fit the block's byte budget
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
		sort.Slice(ready, func(i, j int) bool { return txRate(ready[i]) > txRate(ready[j]) })
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
		weight += pick.Size()
	}
	return selected
}
