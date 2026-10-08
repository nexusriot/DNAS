package core

import "sort"

// Package relay: spending coin that is still in the mempool, and paying for a
// parent with a child's fee (CPFP).
//
// The restriction this lifts. Admission measured a sender against its CONFIRMED
// balance, so a recipient could not queue a spend of coin a queued transaction
// was paying them — even though Select would happily put both in one block,
// because it simulates credits as it goes. The pool was refusing transactions
// the miner two lines over was ready to mine. For anyone whose coin arrives and
// leaves inside one block time — an exchange sweeping deposits, a payment
// channel, anyone paid and paying on — that is the difference between working
// and waiting.
//
// Why it was there. A queue entry that can never be mined is worse than a
// rejection: it occupies a slot, holds a nonce, and never pays its fee. Letting
// a transaction depend on another in the pool reintroduces exactly that risk,
// because the parent might be replaced or evicted and take the child's
// mineability with it. So the dependency is allowed and then BOUNDED — the
// limits below are what make it safe rather than merely permissive.
//
// CPFP falls out of the same bookkeeping. Once the pool knows which queued
// transactions depend on which, a parent's worth to a miner is no longer its
// own fee rate but the best rate of any package it heads. A parent that
// underpaid can be rescued by a child that overpays, which is the only way a
// stuck payment gets unstuck in a pool that does not let the SENDER bump it —
// and the receiver, who is the one actually waiting, usually cannot.
//
// What this is NOT: a package SUBMISSION protocol. A parent and child still
// arrive separately and the parent must arrive first. Accepting an orphan child
// and holding it for a parent that may never come is a different feature with a
// different denial-of-service surface; the orphan pool already exists for
// blocks and has not been pointed at this.

const (
	// MaxPackageAncestors bounds how many unconfirmed transactions from OTHER
	// senders one entry may depend on. Every such ancestor is a way for this
	// transaction to become unmineable through no fault of its own, so the
	// count is the blast radius of a single eviction.
	//
	// A sender's own earlier nonces are ancestors too, and are deliberately NOT
	// counted here: they are already bounded by MaxPerSender, and the sender
	// controls every one of them, so a long run is a choice rather than an
	// exposure. Counting them would silently lower MaxPerSender from 64 to this
	// number.
	MaxPackageAncestors = 25

	// MaxPackageDescendants bounds how many unconfirmed transactions may depend
	// on one entry. This is the limit that stops PINNING: without it, an
	// attacker attaches a large, cheap descendant tree to a transaction and
	// makes replacing it require out-bidding the whole tree.
	MaxPackageDescendants = 25

	// MaxPackageBytes bounds a package's total size, because the count limits
	// alone do not: 25 ancestors at the relay size cap is megabytes of state
	// whose mineability all turns on one transaction.
	MaxPackageBytes = 101_000
)

// pendingCredit is the coin queued transactions would pay to addr if they were
// all mined — the balance admission may lend against on top of the confirmed
// one. m.mu held.
//
// Only COIN outputs count. An asset transfer credits an asset balance, and a
// sponsored fee is a debit on the payer rather than a credit to anyone, so
// neither belongs in this total.
func (m *Mempool) pendingCreditLocked(addr string) uint64 {
	var total uint64
	for _, h := range m.creditedBy[addr] {
		tx, ok := m.txs[h]
		if !ok {
			continue
		}
		if tx.IsAssetTransfer() || tx.IsIssue() {
			continue
		}
		for _, o := range tx.outputs() {
			if o.To == addr {
				// Saturating: a total past the coin supply is unreachable, and a
				// wrap here would hand out an unbounded credit.
				if total+o.Amount < total {
					return ^uint64(0)
				}
				total += o.Amount
			}
		}
	}
	return total
}

// indexCreditsLocked records that tx credits its recipients, so a later
// admission can find the transactions a sender is relying on. m.mu held.
func (m *Mempool) indexCreditsLocked(hash string, tx Transaction) {
	if tx.IsAssetTransfer() || tx.IsIssue() {
		return
	}
	for _, addr := range distinctRecipients(tx) {
		if addr == tx.From {
			continue // paying yourself creates no dependency
		}
		m.creditedBy[addr] = append(m.creditedBy[addr], hash)
	}
}

// unindexCreditsLocked undoes indexCreditsLocked. m.mu held.
func (m *Mempool) unindexCreditsLocked(hash string, tx Transaction) {
	if tx.IsAssetTransfer() || tx.IsIssue() {
		return
	}
	for _, addr := range distinctRecipients(tx) {
		list := m.creditedBy[addr]
		for i, h := range list {
			if h == hash {
				m.creditedBy[addr] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(m.creditedBy[addr]) == 0 {
			delete(m.creditedBy, addr)
		}
	}
}

// distinctRecipients is the deduplicated set of addresses a transfer pays, in a
// stable order.
func distinctRecipients(tx Transaction) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, o := range tx.outputs() {
		if o.To == "" {
			continue
		}
		if _, ok := seen[o.To]; ok {
			continue
		}
		seen[o.To] = struct{}{}
		out = append(out, o.To)
	}
	sort.Strings(out)
	return out
}

// ancestorsLocked is the set of queued transactions that must be mined before
// `tx` can be, as hashes.
//
// Two kinds of dependency, and both are real:
//
//   - The sender's own lower nonces. A nonce run is mined in order, so every
//     earlier entry is an ancestor.
//   - The transactions crediting the sender, when the sender's confirmed
//     spendable balance does not cover what it has queued. If the coin is
//     already confirmed there is no dependency, and treating one as present
//     would make unrelated transactions share a fate.
//
// m.mu held.
func (m *Mempool) ancestorsLocked(tx Transaction, extraCost uint64) map[string]struct{} {
	out := map[string]struct{}{}
	m.collectAncestors(tx, extraCost, out, 0)
	return out
}

// collectAncestors walks the dependency graph. depth guards against a cycle,
// which the nonce rules should make impossible but which must not be able to
// hang a node if it ever became possible.
func (m *Mempool) collectAncestors(tx Transaction, extraCost uint64, out map[string]struct{}, depth int) {
	// A cycle should be impossible (nonces strictly increase along a sender's
	// run, and a credit edge points at an already-queued transaction) but a
	// graph walk that could hang a node on one is not worth the risk. The bound
	// allows the deepest legitimate shape: a full per-sender run at every level
	// of the foreign ancestor limit.
	if depth > MaxPerSender+MaxPackageAncestors+1 {
		return
	}
	// The sender's own earlier nonces.
	queued := m.bySender[tx.From]
	var confirmed uint64
	if m.accounts != nil {
		confirmed = m.accounts.Account(tx.From).Nonce
	}
	var ownCost uint64
	for nonce := confirmed; nonce < tx.Nonce; nonce++ {
		h := queued[nonce]
		if h == "" {
			continue
		}
		if _, seen := out[h]; !seen {
			out[h] = struct{}{}
			ownCost += txCoinCost(m.txs[h])
			m.collectAncestors(m.txs[h], 0, out, depth+1)
		}
	}
	// Credits the sender needs. Only if the confirmed balance cannot cover the
	// cost — otherwise this transaction stands on its own.
	need := txCoinCost(tx) + ownCost + extraCost
	var spendable uint64
	if m.accounts != nil {
		spendable = m.accounts.SpendableBalance(tx.From)
	}
	if need <= spendable {
		return
	}
	for _, h := range m.creditedBy[tx.From] {
		parent, ok := m.txs[h]
		if !ok {
			continue
		}
		if _, seen := out[h]; seen {
			continue
		}
		out[h] = struct{}{}
		m.collectAncestors(parent, 0, out, depth+1)
	}
}

// descendantsLocked is the set of queued transactions from OTHER senders that
// depend on `hash`, transitively.
//
// It walks DOWN the credit graph rather than scanning the pool and asking each
// entry for its ancestors. The scan is the obvious implementation and it is
// quadratic on a path that runs on every admission; the walk visits only the
// accounts this transaction actually pays.
//
// It is deliberately conservative: once an account's confirmed balance cannot
// cover what it has queued, every queued transaction of that account counts as
// a descendant, without working out which individual entries needed the credit.
// Over-counting makes the limit stricter, which is the safe direction for a
// bound that exists to stop pinning.
//
// A sender's own later nonces are not counted, for the reason given at
// MaxPackageAncestors: they are bounded by MaxPerSender and the sender owns
// every one of them.
//
// m.mu held.
func (m *Mempool) descendantsLocked(hash string) map[string]struct{} {
	out := map[string]struct{}{}
	tx, ok := m.txs[hash]
	if !ok {
		return out
	}
	m.walkDescendants(tx, out, 0)
	delete(out, hash)
	return out
}

func (m *Mempool) walkDescendants(tx Transaction, out map[string]struct{}, depth int) {
	if depth > MaxPackageAncestors+1 {
		return
	}
	if tx.IsAssetTransfer() || tx.IsIssue() {
		return // credits no coin, so nothing downstream depends on it
	}
	for _, addr := range distinctRecipients(tx) {
		if addr == tx.From {
			continue
		}
		queued := m.bySender[addr]
		if len(queued) == 0 {
			continue
		}
		// Does this account actually need the credit? If its confirmed balance
		// covers everything it has queued, nothing here depends on tx.
		var cost uint64
		for _, h := range queued {
			cost += txCoinCost(m.txs[h])
		}
		var spendable uint64
		if m.accounts != nil {
			spendable = m.accounts.SpendableBalance(addr)
		}
		if cost <= spendable {
			continue
		}
		for _, h := range queued {
			if _, seen := out[h]; seen {
				continue
			}
			out[h] = struct{}{}
			m.walkDescendants(m.txs[h], out, depth+1)
		}
	}
}

// packageLimitsLocked checks that admitting tx keeps every package bound. It
// returns the ancestors it computed, so the caller does not walk the graph
// twice. m.mu held.
func (m *Mempool) packageLimitsLocked(tx Transaction) (map[string]struct{}, error) {
	anc := m.ancestorsLocked(tx, 0)
	// Foreign ancestors only — see MaxPackageAncestors for why the sender's own
	// run is bounded elsewhere.
	foreign := 0
	for h := range anc {
		if m.txs[h].From != tx.From {
			foreign++
		}
	}
	if foreign > MaxPackageAncestors {
		return nil, &PackageLimitError{
			What: "unconfirmed ancestors from other senders", Got: foreign, Max: MaxPackageAncestors,
		}
	}
	bytes := tx.Size()
	for h := range anc {
		bytes += m.txs[h].Size()
	}
	if bytes > MaxPackageBytes {
		return nil, &PackageLimitError{What: "package bytes", Got: bytes, Max: MaxPackageBytes}
	}
	// Each FOREIGN ancestor gains this transaction as a descendant. Only the
	// foreign ones: an ancestor from the same sender gains a descendant from
	// that same sender, which is the run MaxPerSender already bounds.
	for h := range anc {
		if m.txs[h].From == tx.From {
			continue
		}
		if n := len(m.descendantsLocked(h)); n+1 > MaxPackageDescendants {
			return nil, &PackageLimitError{
				What: "transactions depending on a queued ancestor", Got: n + 1, Max: MaxPackageDescendants,
			}
		}
	}
	return anc, nil
}

// PackageLimitError says which package bound a transaction broke. It is a typed
// error because a relay peer that keeps hitting one is behaving differently
// from one sending nonsense, and the node should be able to tell.
type PackageLimitError struct {
	What string
	Got  int
	Max  int
}

func (e *PackageLimitError) Error() string {
	return "package too large: " + e.What + " would be " + itoa(e.Got) + ", max " + itoa(e.Max)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// effectiveRates returns, for every queued transaction, the fee rate a miner
// should judge it by: the best rate of any package it heads.
//
// This is what CPFP means in a selector that walks each sender's nonce run in
// order. A parent sitting at the head of its run with a poor rate blocks
// everything behind it, however well those pay — so the parent is scored by the
// package {itself and its descendants} that pays best, and gets picked when
// that package deserves it rather than when its own fee does.
//
// Returned rates are never below the transaction's own: a package cannot make a
// transaction look worse than it is, only better.
func (m *Mempool) effectiveRates() map[string]float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.effectiveRatesLocked()
}

// m.mu held.
func (m *Mempool) effectiveRatesLocked() map[string]float64 {
	// Transaction.Size() is a full canonical re-encode, and the loop below
	// reads a size once per transaction PLUS once per member of its package.
	// Computing them all once first turns that from thousands of encodes into
	// one per transaction — without it, adding package pricing made Select
	// almost twice as slow on a full pool, which is a steep price for an
	// ordering refinement.
	sizes := make(map[string]int, len(m.txs))
	for h, tx := range m.txs {
		sizes[h] = tx.Size()
	}

	rates := make(map[string]float64, len(m.txs))
	for h, tx := range m.txs {
		own := 0.0
		if sizes[h] > 0 {
			own = float64(tx.Fee) / float64(sizes[h])
		}
		fees := tx.Fee
		size := sizes[h]
		// The package is this transaction plus everything that depends on it.
		// A sender's own later nonces are included here even though they are
		// excluded from the LIMITS: for pricing, a run that pays well behind a
		// cheap head is exactly the case CPFP exists to serve.
		for d := range m.packagePricingSetLocked(h, tx) {
			fees += m.txs[d].Fee
			size += sizes[d]
		}
		pkg := own
		if size > 0 {
			if r := float64(fees) / float64(size); r > pkg {
				pkg = r
			}
		}
		rates[h] = pkg
	}
	return rates
}

// packagePricingSetLocked is the set a transaction should be priced with: the
// foreign transactions that depend on it, plus its own sender's later nonces,
// which must be mined after it and so ride on its selection. m.mu held.
func (m *Mempool) packagePricingSetLocked(hash string, tx Transaction) map[string]struct{} {
	out := m.descendantsLocked(hash)
	for nonce, h := range m.bySender[tx.From] {
		if nonce > tx.Nonce {
			out[h] = struct{}{}
		}
	}
	delete(out, hash)
	return out
}
