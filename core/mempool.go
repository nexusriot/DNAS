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

// MaxPerSender bounds how many queued transactions one address may have. The
// nonce-contiguity rule below already stops a sender from queueing work that can
// never be mined, and the affordability rule stops them queueing more than they
// can pay for; this is a third, blunter bound so one address cannot occupy the
// whole pool even when it is rich and its nonces are in order.
const MaxPerSender = 64

// AccountSource is the confirmed chain state the mempool validates against:
// which nonce an address is at, and how much it can actually spend right now.
// *Blockchain implements it.
//
// A mempool without one is *permissive* — it can still check everything internal
// to a transaction, but not whether the sender could ever pay for it. Nodes always
// bind one (node.New does it), so this only affects tests that exercise the pool
// in isolation.
type AccountSource interface {
	Account(addr string) Account
	SpendableBalance(addr string) uint64
}

// Mempool holds validated, not-yet-mined transactions keyed by hash. It is
// bounded: once full, a new transaction is admitted only if it pays a strictly
// higher fee than the cheapest one already queued, which it then evicts.
//
// It also enforces a dynamic minimum relay fee (see MinFee) as local relay
// policy — NOT a consensus rule. A transaction below the current floor is
// refused entry here, but if it reaches a node in a mined block it is still
// accepted; the floor only governs what this node will queue and gossip.
//
// Admission additionally requires that a transaction could *plausibly* be mined
// (see admissibleLocked): the pool holds, per sender, a contiguous run of nonces
// starting at the sender's confirmed nonce, whose total cost the sender can
// afford. Without that rule the pool is free to fill with work that can never be
// mined — an address holding nothing can sign transactions at nonces 1..N,
// skipping 0, and they are admitted, never selected, never expire and never pay a
// fee, evicting everyone's real payments for nothing.
type Mempool struct {
	mu  sync.Mutex
	txs map[string]Transaction
	// bySender indexes sender -> nonce -> txid, so conflict lookup, the
	// contiguity check and gap-free eviction are all O(1) rather than a scan.
	bySender map[string]map[uint64]string
	// sponsored indexes fee payer -> total queued fees that payer has agreed to
	// cover, so a sponsor's affordability can be checked across every transaction
	// it sponsors rather than one at a time (see admissibleLocked).
	sponsored map[string]uint64
	// bytes is the running total of queued transaction sizes, and maxBytes bounds
	// it. The count cap alone does not bound memory (see DefaultMempoolBytes).
	bytes       int
	maxBytes    int
	max         int
	minRelayFee uint64           // base per-byte relay floor when empty; 0 disables the fee floor
	accounts    AccountSource    // confirmed state to validate against (may be nil)
	sigCache    *ValidationCache // shared with the chain, so a signature is verified once (may be nil)
	// chainHeight reports the current tip height, so admission can tell whether a
	// queued transaction is mineable into the NEXT block (see unmineableLocked).
	// Bound alongside accounts; nil in an isolated pool.
	chainHeight func() uint64
}

// NewMempool returns an empty mempool with the default size limit and no fee
// floor (base relay fee 0).
func NewMempool() *Mempool { return NewMempoolWithPolicy(DefaultMempoolSize, 0) }

// NewMempoolWithPolicy returns an empty mempool bounded at max transactions
// (values <= 0 fall back to the default) with the given base minimum relay fee.
// The effective floor rises with occupancy (see MinFee). A minRelayFee of 0
// disables the floor entirely.
func NewMempoolWithPolicy(max int, minRelayFee uint64) *Mempool {
	return NewMempoolWithLimits(max, DefaultMempoolBytes, minRelayFee)
}

// NewMempoolWithLimits bounds the pool by BOTH a transaction count and a total
// byte budget (values <= 0 fall back to the defaults). Both matter: the count
// stops an unbounded number of tiny transactions, and the budget stops a bounded
// number of enormous ones — 5000 entries at MaxRelayTxBytes apiece is ~500 MB
// that the count cap happily permits.
func NewMempoolWithLimits(max, maxBytes int, minRelayFee uint64) *Mempool {
	if max <= 0 {
		max = DefaultMempoolSize
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMempoolBytes
	}
	return &Mempool{
		txs:         map[string]Transaction{},
		bySender:    map[string]map[uint64]string{},
		sponsored:   map[string]uint64{},
		max:         max,
		maxBytes:    maxBytes,
		minRelayFee: minRelayFee,
	}
}

// UseAccounts binds the confirmed state the pool validates admissions against.
// It returns the mempool so it can be chained onto a constructor.
func (m *Mempool) UseAccounts(src AccountSource) *Mempool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accounts = src
	if hs, ok := src.(heightSource); ok {
		m.chainHeight = hs.Height
	}
	return m
}

// heightSource is the part of the chain the pool needs to judge whether a
// transaction is mineable right now. *Blockchain satisfies it; an AccountSource
// that does not simply leaves that check off.
type heightSource interface{ Height() uint64 }

// UseValidationCache binds the signature-verification cache the pool shares with
// the chain, so a transaction verified on admission costs nothing to verify again
// when the block carrying it is applied. It returns the mempool for chaining.
func (m *Mempool) UseValidationCache(c *ValidationCache) *Mempool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sigCache = c
	return m
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
	// Occupancy is whichever budget is fuller: a pool holding few but enormous
	// transactions is just as full as one holding many small ones, and the floor
	// should rise for both.
	fill := len(m.txs) * 100 / m.max
	if m.maxBytes > 0 {
		if byFill := m.bytes * 100 / m.maxBytes; byFill > fill {
			fill = byFill
		}
	}
	if fill > 100 {
		fill = 100
	}
	// extra = (multiplier-1) * fill^2 / 100^2, so fill=0 -> 0 and fill=100 ->
	// multiplier-1. The square keeps the floor near the base until the pool is
	// genuinely congested, then ramps it steeply.
	extra := uint64(feeFloorMaxMultiplier-1) * uint64(fill*fill) / 10_000
	return m.minRelayFee * (1 + extra)
}

// insertLocked stores tx under h and indexes it by sender+nonce. m.mu held.
func (m *Mempool) insertLocked(h string, tx Transaction) {
	m.txs[h] = tx
	byNonce := m.bySender[tx.From]
	if byNonce == nil {
		byNonce = map[uint64]string{}
		m.bySender[tx.From] = byNonce
	}
	byNonce[tx.Nonce] = h
	m.bytes += tx.Size()
	if tx.IsSponsored() {
		m.sponsored[tx.FeePayer] += tx.Fee
	}
}

// deleteLocked removes the transaction stored under h from both indexes. m.mu held.
func (m *Mempool) deleteLocked(h string) {
	tx, ok := m.txs[h]
	if !ok {
		return
	}
	delete(m.txs, h)
	if m.bytes -= tx.Size(); m.bytes < 0 {
		m.bytes = 0 // defensive: the total is derived, never authoritative
	}
	if tx.IsSponsored() {
		if m.sponsored[tx.FeePayer] -= tx.Fee; m.sponsored[tx.FeePayer] == 0 {
			delete(m.sponsored, tx.FeePayer)
		}
	}
	if byNonce := m.bySender[tx.From]; byNonce != nil {
		if byNonce[tx.Nonce] == h {
			delete(byNonce, tx.Nonce)
		}
		if len(byNonce) == 0 {
			delete(m.bySender, tx.From)
		}
	}
}

// txCoinCost is the coin a transaction takes from its sender's spendable
// balance: an asset move or an issuance pays only the fee (the asset amount comes
// out of the asset ledger), anything else pays amount + fee.
func txCoinCost(tx Transaction) uint64 {
	fee := senderFee(tx) // zero when a sponsor pays; charged to the sponsor instead
	if tx.IsIssue() || tx.IsAssetTransfer() {
		return fee
	}
	out, ok := tx.TotalOut()
	if !ok {
		return ^uint64(0) // overflowing: unaffordable by construction (CheckTxSanity rejects it)
	}
	return out + fee
}

// admissibleLocked reports whether tx could plausibly be mined, given the
// confirmed state and what this sender already has queued. replacing is the hash
// of the queued transaction tx would replace (replace-by-fee), or "".
//
// Two rules, both about the sender rather than the transaction:
//
//   - No nonce gaps. The queue for a sender must stay a contiguous run starting
//     at their confirmed nonce, so every entry in it is reachable. A transaction
//     at nonce confirmed+5 with nothing in between can never be mined, and would
//     otherwise sit in the pool forever without ever paying its fee.
//   - Affordability. The sender's whole queue, this transaction included, must fit
//     in their spendable balance — otherwise the tail is unminable for the same
//     reason, just via the balance check instead of the nonce check.
//
// Requires a bound AccountSource; without one there is nothing to check against
// and everything is admissible. m.mu held.
func (m *Mempool) admissibleLocked(tx Transaction, replacing string) error {
	if m.accounts == nil {
		return nil
	}
	queued := m.bySender[tx.From]
	confirmed := m.accounts.Account(tx.From).Nonce
	if tx.Nonce < confirmed {
		return fmt.Errorf("nonce %d already used (account is at %d)", tx.Nonce, confirmed)
	}
	if replacing == "" {
		if len(queued) >= MaxPerSender {
			return fmt.Errorf("sender already has %d queued transactions (max %d)", len(queued), MaxPerSender)
		}
		// The next free slot is the confirmed nonce plus the contiguous run already
		// queued. Anything above it would leave a gap.
		next := confirmed
		for queued[next] != "" {
			next++
		}
		if tx.Nonce != next {
			return fmt.Errorf("nonce %d leaves a gap: the next usable nonce for this sender is %d", tx.Nonce, next)
		}
	}
	// Affordability across the sender's whole queue. Overflow is impossible here:
	// CheckTxSanity has already rejected an amount+fee that wraps, and the running
	// total is compared against a balance every step, so it cannot exceed it.
	spendable := m.accounts.SpendableBalance(tx.From)
	total := txCoinCost(tx)
	for _, h := range queued {
		if h == replacing {
			continue
		}
		total += txCoinCost(m.txs[h])
		if total > spendable {
			break
		}
	}
	if total > spendable {
		return fmt.Errorf("sender cannot afford its queued transactions: %d needed, %d spendable", total, spendable)
	}
	return m.sponsorAffordableLocked(tx, replacing)
}

// sponsorAffordableLocked checks that a fee sponsor can cover every fee it has
// agreed to pay across the pool, not merely this one. Without the running total
// a payer could sponsor a hundred transactions it can afford one at a time, and
// only the first would be mineable — the same "queue full of work that can never
// be mined" problem the sender rules exist to prevent, moved one account over.
// m.mu held.
func (m *Mempool) sponsorAffordableLocked(tx Transaction, replacing string) error {
	if !tx.IsSponsored() {
		return nil
	}
	committed := m.sponsored[tx.FeePayer]
	if replacing != "" {
		if old, ok := m.txs[replacing]; ok && old.IsSponsored() && old.FeePayer == tx.FeePayer {
			committed -= old.Fee // the replacement displaces it, so its fee is released
		}
	}
	total := committed + tx.Fee
	if spendable := m.accounts.SpendableBalance(tx.FeePayer); total > spendable {
		return fmt.Errorf("fee sponsor %s cannot afford the fees it has queued: %d needed, %d spendable",
			tx.FeePayer, total, spendable)
	}
	return nil
}

// Add verifies the transaction's signature and stores it. Returns whether it
// was newly added. Behaviour:
//   - an exact duplicate (same hash) is a no-op: (false, nil);
//   - a transaction with the same sender and nonce as one already queued
//     replaces it if and only if it pays a strictly higher fee (replace-by-fee /
//     fee-bumping); a same-or-lower fee is rejected with an error;
//   - a transaction that could never be mined — a nonce gap, or more than the
//     sender can afford — is refused (see admissibleLocked);
//   - otherwise, if the pool is full, it is admitted only by out-bidding the
//     cheapest queued transaction, which it evicts.
func (m *Mempool) Add(tx Transaction) (bool, error) {
	if tx.IsCoinbase() {
		return false, errors.New("cannot add coinbase to mempool")
	}
	// Consensus sanity first: a transaction no block can ever contain must not be
	// queued, or the miner will keep selecting it and every block it builds will be
	// rejected — block production stops until the transaction is evicted. This is
	// the same function block application uses, so the two cannot drift apart.
	if err := CheckTxSanity(tx); err != nil {
		return false, err
	}
	// Then the cheap policy checks, and only then the signature — verification is
	// by far the most expensive step, and an unauthenticated peer must not be able
	// to make us pay for it on a transaction we would refuse anyway.
	size := tx.Size()
	if size > MaxRelayTxBytes {
		return false, fmt.Errorf("transaction too large to relay: %d bytes (max %d)", size, MaxRelayTxBytes)
	}
	// Relay policy: refuse anything paying below the current per-byte floor for its
	// size (floor is a rate; a bigger transaction must pay proportionally more).
	if floor := m.MinFee(); floor > 0 && tx.Fee < floor*uint64(size) {
		return false, fmt.Errorf("fee %d below current relay floor %d/byte × %d bytes = %d",
			tx.Fee, floor, size, floor*uint64(size))
	}
	h := tx.Hash()
	if _, ok := m.Get(h); ok {
		return false, nil
	}
	if err := m.admissible(tx); err != nil {
		return false, err
	}
	if err := m.verify(tx); err != nil {
		return false, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.txs[h]; ok {
		return false, nil
	}

	// Replace-by-fee: a conflicting tx (same sender+nonce) may only be replaced
	// by a higher fee.
	oldHash, old, conflict := m.conflictLocked(tx)
	// Re-check admissibility under the lock (the tip may have moved while we were
	// verifying), now knowing which entry a replacement would displace.
	if err := m.admissibleLocked(tx, oldHash); err != nil {
		return false, err
	}
	if conflict {
		// A queued transaction that CANNOT be mined into the next block has no claim
		// on the slot it is occupying, so a replacement that can be mined displaces
		// it regardless of fee. Without this, an unmineable transaction jams its
		// sender's nonce until someone out-bids it — which is exactly the attack a
		// time-delayed vault invites: a thief holding the hot key parks a spend that
		// no block will accept until the unlock height, and the cold key's rescue
		// (same account, same nonce) would have to pay more than the thief to get in.
		// See VaultHotNotReady / HTLCRefundNotReady for what "unmineable" means here.
		if tx.Fee <= old.Fee && !(m.unmineableLocked(old) && !m.unmineableLocked(tx)) {
			return false, errors.New("replacement fee not higher than existing transaction")
		}
		m.deleteLocked(oldHash)
		m.insertLocked(h, tx)
		return true, nil
	}

	// When full, admit only by out-bidding the cheapest evictable transaction (fee
	// per byte), which this transaction then displaces — so block space, a per-byte
	// resource, is allocated to the highest-paying bytes.
	//
	// "Full" means either budget: too many transactions, or too many bytes. A big
	// transaction can therefore need to displace SEVERAL small ones to fit, which
	// is the whole point — it is asking for their room.
	for m.overBudgetLocked(size) {
		victimHash, victimRate := m.evictionCandidateLocked()
		if victimHash == "" {
			return false, errors.New("mempool full and nothing may be evicted")
		}
		if txRate(tx) <= victimRate {
			return false, errors.New("mempool full and fee rate too low")
		}
		victim := m.txs[victimHash]
		if victim.From == tx.From && victim.Nonce < tx.Nonce {
			// The only room to be had is this sender's own predecessor. Taking it would
			// strand the arriving transaction behind the gap it created, so it loses.
			return false, errors.New("mempool full: this sender cannot displace its own queue to add to it")
		}
		m.deleteLocked(victimHash)
	}
	m.insertLocked(h, tx)
	return true, nil
}

// overBudgetLocked reports whether admitting a transaction of `size` bytes would
// exceed either the count or the byte budget. m.mu held.
func (m *Mempool) overBudgetLocked(size int) bool {
	return len(m.txs) >= m.max || m.bytes+size > m.maxBytes
}

// Bytes is the total serialized size of the queued transactions, and MaxBytes
// the budget it is held to — both reported so an operator can see how full the
// pool is by the measure that actually bounds memory.
func (m *Mempool) Bytes() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bytes
}

func (m *Mempool) MaxBytes() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.maxBytes
}

// verify checks tx's authorization through the shared cache, so the block that
// later carries it does not pay for the same signatures again.
func (m *Mempool) verify(tx Transaction) error {
	m.mu.Lock()
	cache := m.sigCache
	m.mu.Unlock()
	return cache.Verify(tx)
}

// admissible runs admissibleLocked under the lock, for the pre-verification
// check (so an inadmissible transaction costs no signature verification).
func (m *Mempool) admissible(tx Transaction) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.admissibleLocked(tx, m.bySender[tx.From][tx.Nonce])
}

// unmineableLocked reports whether tx would be rejected by the height rules if it
// were put in the next block — an expired transaction, one whose time-lock has
// not opened, an HTLC refund before its timeout, or a vault hot-key spend before
// its unlock. Such a transaction is validly signed and may become mineable
// later, so it is not dropped; it simply cannot outrank one that is mineable now.
//
// It needs the chain's height and so is a no-op on a pool with no AccountSource
// (which is only the case in isolated tests). m.mu held.
func (m *Mempool) unmineableLocked(tx Transaction) bool {
	if m.chainHeight == nil {
		return false
	}
	return checkTxAtHeight(tx, m.chainHeight()+1) != nil
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
	h, found := m.bySender[tx.From][tx.Nonce]
	if !found {
		return "", Transaction{}, false
	}
	return h, m.txs[h], true
}

// PruneExpired removes transactions that can no longer be included in any block
// built on top of the current tip height, and returns how many were dropped.
func (m *Mempool) PruneExpired(tipHeight uint64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for h, tx := range m.txs {
		if tx.IsExpiredAt(tipHeight + 1) { // the next block is at tipHeight+1
			m.deleteLocked(h)
			n++
		}
	}
	return n
}

// Reconcile drops everything the pool should no longer be holding, and returns
// how many entries it removed. It is the counterpart to the admission rules: a
// new block moves nonces and balances, so entries that were admissible when they
// arrived may no longer be mineable. Per sender it keeps the contiguous,
// affordable run starting at the confirmed nonce and drops the rest.
//
// Without this, a reorg or a block from another sender's payment could leave the
// pool holding permanently unmineable work — occupying slots that real payments
// need. Nodes call it after every tip change.
func (m *Mempool) Reconcile(tipHeight uint64) int {
	dropped := m.PruneExpired(tipHeight)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.accounts == nil {
		return dropped
	}
	// Sponsors first: a payer that can no longer cover everything it promised has
	// its cheapest sponsorships dropped. Doing this before the per-sender pass lets
	// that pass clean up anything left stranded behind the resulting nonce gap.
	dropped += m.reconcileSponsorsLocked()
	for sender, byNonce := range m.bySender {
		confirmed := m.accounts.Account(sender).Nonce
		spendable := m.accounts.SpendableBalance(sender)
		nonces := make([]uint64, 0, len(byNonce))
		for n := range byNonce {
			nonces = append(nonces, n)
		}
		sort.Slice(nonces, func(i, j int) bool { return nonces[i] < nonces[j] })
		want := confirmed
		var total uint64
		for _, n := range nonces {
			h := byNonce[n]
			keep := n == want
			if keep {
				if total += txCoinCost(m.txs[h]); total > spendable {
					keep = false
				}
			}
			if !keep {
				m.deleteLocked(h)
				dropped++
				continue
			}
			want++
		}
	}
	return dropped
}

// reconcileSponsorsLocked drops sponsored transactions whose fee payer can no
// longer cover every fee it has queued, cheapest fee rate first, until what
// remains fits the payer's spendable balance. m.mu held.
func (m *Mempool) reconcileSponsorsLocked() int {
	if len(m.sponsored) == 0 {
		return 0
	}
	byPayer := make(map[string][]string, len(m.sponsored))
	for h, tx := range m.txs {
		if tx.IsSponsored() {
			byPayer[tx.FeePayer] = append(byPayer[tx.FeePayer], h)
		}
	}
	dropped := 0
	for payer, hashes := range byPayer {
		spendable := m.accounts.SpendableBalance(payer)
		if m.sponsored[payer] <= spendable {
			continue
		}
		sort.Slice(hashes, func(i, j int) bool { return txRate(m.txs[hashes[i]]) < txRate(m.txs[hashes[j]]) })
		for _, h := range hashes {
			if m.sponsored[payer] <= spendable {
				break
			}
			m.deleteLocked(h)
			dropped++
		}
	}
	return dropped
}

// evictionCandidateLocked returns the hash and fee rate of the cheapest queued
// transaction that may be dropped. Only the LAST entry in a sender's nonce run is
// eligible: dropping from the middle would strand every higher nonce behind a gap
// it can never cross, which is precisely the state the admission rules exist to
// prevent. Among those, the lowest fee per byte goes first, so scarce space still
// ends up with the highest-paying bytes. The caller must hold m.mu and the pool
// must be non-empty.
func (m *Mempool) evictionCandidateLocked() (hash string, rate float64) {
	first := true
	for _, byNonce := range m.bySender {
		var top uint64
		var topHash string
		for nonce, h := range byNonce {
			if topHash == "" || nonce > top {
				top, topHash = nonce, h
			}
		}
		if topHash == "" {
			continue
		}
		if r := txRate(m.txs[topHash]); first || r < rate {
			rate, hash, first = r, topHash, false
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

// Get returns a pending transaction by hash.
func (m *Mempool) Get(hash string) (Transaction, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tx, ok := m.txs[hash]
	return tx, ok
}

// Remove deletes the given transactions (e.g. after they are mined).
func (m *Mempool) Remove(txs []Transaction) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tx := range txs {
		m.deleteLocked(tx.Hash())
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

// FeeBucket is one band of the mempool's fee-rate distribution: how many
// transactions pay between From and To base units per byte, and how much block
// space they occupy.
type FeeBucket struct {
	From  uint64 `json:"from_rate"`
	To    uint64 `json:"to_rate"` // 0 means "and above"
	Count int    `json:"count"`
	Bytes int    `json:"bytes"`
}

// MempoolStats is the shape of the pending queue: how much is waiting, how much
// space it wants, and how the fees people are paying are distributed. A single
// "mempool: 40" number cannot tell a sender whether their fee will be picked
// next block or sit behind a wall of higher bidders; a distribution can.
type MempoolStats struct {
	Count      int         `json:"count"`
	Bytes      int         `json:"bytes"`
	MinRate    uint64      `json:"min_rate"`
	MaxRate    uint64      `json:"max_rate"`
	MedianRate uint64      `json:"median_rate"`
	Buckets    []FeeBucket `json:"buckets"`
}

// feeBucketEdges are the fee-rate (base units per byte) band boundaries. The
// last band is open-ended.
var feeBucketEdges = []uint64{1, 5, 10, 25, 50, 100, 250, 500}

// Stats summarizes the pending queue by fee rate.
func (m *Mempool) Stats() MempoolStats {
	txs := m.All()
	st := MempoolStats{Count: len(txs), Buckets: make([]FeeBucket, 0, len(feeBucketEdges))}
	for i, edge := range feeBucketEdges {
		b := FeeBucket{From: edge}
		if i+1 < len(feeBucketEdges) {
			b.To = feeBucketEdges[i+1] - 1
		}
		st.Buckets = append(st.Buckets, b)
	}
	if len(txs) == 0 {
		return st
	}
	rates := make([]uint64, 0, len(txs))
	for _, tx := range txs {
		size := tx.Size()
		rate := tx.Fee / uint64(size) // integer per-byte rate, the unit fees are priced in
		rates = append(rates, rate)
		st.Bytes += size
		for i := len(st.Buckets) - 1; i >= 0; i-- {
			if rate >= st.Buckets[i].From || i == 0 {
				st.Buckets[i].Count++
				st.Buckets[i].Bytes += size
				break
			}
		}
	}
	sort.Slice(rates, func(i, j int) bool { return rates[i] < rates[j] })
	st.MinRate, st.MaxRate = rates[0], rates[len(rates)-1]
	st.MedianRate = rates[len(rates)/2]
	return st
}

// selCandidate is one pool transaction prepared for selection. Hash, size, fee
// rate and verification cost are all derived from the canonical encoding, so
// computing them means serializing the transaction; doing that inside the
// selection loop meant re-serializing (and re-hashing) every queued transaction
// once per chosen transaction. They are fixed for the life of one Select call,
// so they are computed once here instead.
type selCandidate struct {
	tx   Transaction
	hash string
	size int
	ops  int
	rate float64
}

// Select greedily chooses transactions that form a valid sequence on top of the
// current chain state: each must have the sender's next nonce, be affordable, and
// pay at least its per-byte base fee. It is bounded by both max transactions and
// MaxBlockBytes of total size. Among ready candidates it prefers the highest fee
// rate (fee per byte), so scarce block space goes to the best-paying bytes.
// Recipients are credited in the simulation so chained spends within one block
// are possible.
//
// Two things keep it from costing more to build a block than to mine one:
//
//   - Everything static is computed once. A transaction's hash, canonical size,
//     verification cost, base-fee floor and standalone validity do not change
//     while we choose, so they are evaluated in a single pass up front rather
//     than on every pass (which was O(pool × block) sha256 work).
//   - Only one transaction per sender can ever be ready. Readiness requires
//     tx.Nonce == the sender's simulated nonce, and the pool holds at most one
//     transaction per (sender, nonce), so each round examines one head per
//     SENDER rather than the whole pool. Heads are kept in nonce order and the
//     cursor advances past anything already confirmed.
//
// What it chooses is unchanged, with one deliberate exception: ties on fee rate
// now break on the transaction hash instead of on Go's map iteration order, so
// two nodes with the same mempool build the same block template.
func (m *Mempool) Select(bc *Blockchain, max int) []Transaction {
	mineHeight := bc.Height() + 1 // the block we're selecting for
	baseFee := bc.NextBaseFee()   // the next block's base fee (per byte); txs must cover it

	// One pass over the pool: drop anything that can never go into this block,
	// and cache what the loop below would otherwise recompute.
	bySender := map[string][]selCandidate{}
	for _, tx := range m.All() {
		size := tx.Size()
		// Every consensus rule the block we are building will apply is checked
		// here too. Selecting a transaction the chain then rejects does not just
		// waste a slot: it invalidates the whole candidate, so the miner would
		// hash and lose block after block while the transaction sat in the queue.
		// None of these depend on what else we pick, so once is enough.
		if tx.Fee < baseFee*uint64(size) {
			continue
		}
		if CheckTxSanity(tx) != nil || checkTxAtHeight(tx, mineHeight) != nil {
			continue
		}
		bySender[tx.From] = append(bySender[tx.From], selCandidate{
			tx: tx, hash: tx.Hash(), size: size, ops: VerifyOps(tx), rate: txRate(tx),
		})
	}
	if len(bySender) == 0 {
		return nil
	}
	// Nonce order per sender, so the head is always the only one that can match
	// the sender's next nonce.
	for _, cs := range bySender {
		sort.Slice(cs, func(i, j int) bool { return cs[i].tx.Nonce < cs[j].tx.Nonce })
	}
	senders := make([]string, 0, len(bySender))
	for from := range bySender {
		senders = append(senders, from)
	}
	sort.Strings(senders) // deterministic scan order; ties break on hash below
	cursor := make(map[string]int, len(bySender))

	type sim struct {
		balance uint64 // spendable coin
		nonce   uint64
		assets  map[string]uint64
	}
	cache := map[string]sim{}
	get := func(addr string) sim {
		if s, ok := cache[addr]; ok {
			return s
		}
		// Use the spendable balance so immature coinbase isn't selected — the
		// miner would otherwise build a block its own consensus rules reject.
		acc := bc.Account(addr)
		assets := make(map[string]uint64, len(acc.Assets))
		for k, v := range acc.Assets {
			assets[k] = v
		}
		s := sim{balance: bc.SpendableBalance(addr), nonce: acc.Nonce, assets: assets}
		cache[addr] = s
		return s
	}
	// affordable reports whether tx can be paid for on the simulated state. The
	// nonce is already known to match by the time this is called.
	affordable := func(tx Transaction) bool {
		s := get(tx.From)
		// A sponsored fee comes out of the payer's simulated balance, so several
		// transactions sharing one sponsor cannot each be selected on the strength of
		// the same coin.
		if tx.IsSponsored() && get(tx.FeePayer).balance < tx.Fee {
			return false
		}
		switch {
		case tx.IsIssue():
			return s.balance >= senderFee(tx)
		case tx.IsAssetTransfer():
			return s.balance >= senderFee(tx) && s.assets[tx.AssetID] >= tx.Amount
		default:
			return s.balance >= txCoinCost(tx)
		}
	}
	// debitSponsor charges a selected transaction's fee to its sponsor in the
	// simulation, in the same order block application does it: after the sender is
	// debited, before any recipient is credited.
	debitSponsor := func(tx Transaction) {
		if !tx.IsSponsored() {
			return
		}
		p := get(tx.FeePayer)
		p.balance -= tx.Fee
		cache[tx.FeePayer] = p
	}

	var selected []Transaction
	weight := 0 // running total of selected transaction bytes (<= MaxBlockBytes)
	ops := 0    // running total of verification cost (<= MaxBlockVerifyOps)
	for len(selected) < max {
		var best *selCandidate
		for _, from := range senders {
			cs := bySender[from]
			i := cursor[from]
			// Skip anything the simulation has moved past: a nonce below the
			// sender's current one is already confirmed (or already selected) and
			// can never become ready again.
			want := get(from).nonce
			for i < len(cs) && cs[i].tx.Nonce < want {
				i++
			}
			cursor[from] = i
			if i >= len(cs) || cs[i].tx.Nonce != want {
				continue // this sender has a gap at its next nonce
			}
			c := &cs[i]
			if weight+c.size > MaxBlockBytes { // wouldn't fit the block's byte budget
				continue
			}
			if ops+c.ops > MaxBlockVerifyOps { // nor its verification budget
				continue
			}
			if !affordable(c.tx) {
				continue
			}
			if best == nil || c.rate > best.rate || (c.rate == best.rate && c.hash < best.hash) {
				best = c
			}
		}
		if best == nil {
			break
		}
		pick := best.tx

		s := get(pick.From)
		s.nonce++
		switch {
		case pick.IsIssue():
			s.balance -= senderFee(pick)
			s.assets[AssetID(pick.From, pick.Issue.Ticker, pick.Nonce)] += pick.Issue.Supply
			cache[pick.From] = s
			debitSponsor(pick)
		case pick.IsAssetTransfer():
			s.balance -= senderFee(pick)
			s.assets[pick.AssetID] -= pick.Amount
			cache[pick.From] = s
			debitSponsor(pick)
			r := get(pick.To)
			r.assets[pick.AssetID] += pick.Amount
			cache[pick.To] = r
		default:
			s.balance -= txCoinCost(pick)
			cache[pick.From] = s
			debitSponsor(pick)
			// Credit every recipient, re-reading the simulated account each time so a
			// repeated recipient (or the sender paying itself) accumulates correctly.
			for _, o := range pick.outputs() {
				r := get(o.To)
				r.balance += o.Amount
				cache[o.To] = r
			}
		}

		selected = append(selected, pick)
		weight += best.size
		ops += best.ops
	}
	return selected
}
