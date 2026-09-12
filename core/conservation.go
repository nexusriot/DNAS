package core

import "fmt"

// Supply conservation, enforced per block during application.
//
// supply.go tracks minted and burned independently and REPORTS whether
//
//	minted − burned == circulating
//
// still holds at the tip. That is an observation: a node whose accounting had
// silently inflated the coin supply would print `consistent: false` and go right
// on building on the block that did it. This file makes the identity a RULE, so
// such a block is rejected instead.
//
// The rule is stated per block rather than chain-wide, which is what makes it
// affordable. A block's effect on the total coin held by accounts must be
// exactly
//
//	subsidy(height) − (base fee burned by its transactions)
//
// because every other movement is a transfer: a payment debits the sender
// exactly what it credits the recipients, and the fee splits into a burned part
// (destroyed) and a tip (paid to the coinbase recipient). Summing the per-block
// identity over a chain gives back the chain-wide one, so enforcing the local
// form enforces the global form without ever walking the whole ledger.
//
// The same argument applies to each native asset: a block may only change an
// asset's total by what its issuances mint and what its management operations
// mint or burn (see asset.go). A transfer must net to zero.
//
// It is NOT height-activated, unlike the rules in upgrade.go. Those are
// tightenings that can refuse a transaction which was legal when it was mined,
// so they need a flag day to keep an existing chain replayable. This one cannot:
// any chain that fails it was already invalid under the rules that produced it,
// so there is no valid history for an activation height to protect.

// touchedBefore reconstructs each account's pre-block value from the undo log.
// The log records the prior value before EVERY mutation, so an address written
// twice appears twice; the first entry is the one that predates the block.
func touchedBefore(undo []undoEntry) map[string]Account {
	before := make(map[string]Account, len(undo))
	for _, e := range undo {
		if _, seen := before[e.addr]; seen {
			continue
		}
		// A non-existent account is the zero Account, which is what e.prev already
		// holds when e.existed is false — so the two cases need no distinction here.
		before[e.addr] = e.prev
	}
	return before
}

// coinDelta is the net change in coin held by accounts that a block produced.
// It reads only the accounts the block touched (via the undo log), so it costs
// the block's own footprint rather than a walk of the whole account set.
//
// int64 is wide enough by construction: the total coin ever minted is bounded by
// the halving schedule at well under 2^62 base units, and a sum over a subset of
// accounts cannot exceed it.
func coinDelta(state map[string]Account, before map[string]Account) int64 {
	var sumBefore, sumAfter uint64
	for addr, prev := range before {
		sumBefore += prev.Balance
		sumAfter += state[addr].Balance
	}
	return int64(sumAfter) - int64(sumBefore)
}

// assetDeltas is the net change per asset id that a block produced, over the
// accounts it touched. Ids absent from the result were not moved at all.
func assetDeltas(state map[string]Account, before map[string]Account) map[string]int64 {
	deltas := map[string]int64{}
	for addr, prev := range before {
		for id, amt := range prev.Assets {
			deltas[id] -= int64(amt)
		}
		for id, amt := range state[addr].Assets {
			deltas[id] += int64(amt)
		}
	}
	for id, d := range deltas {
		if d == 0 {
			delete(deltas, id)
		}
	}
	return deltas
}

// checkConservation verifies that a block moved exactly as much coin into
// existence as its subsidy, minus what its transactions burned — and that it
// changed each asset's total only by what it issued.
//
// txs is the block's full transaction list (the coinbase included, and skipped);
// burned is the base-fee total the caller already accumulated while applying it.
func checkConservation(state map[string]Account, undo []undoEntry, txs []Transaction, reward, burned uint64) error {
	before := touchedBefore(undo)

	wantCoin := int64(reward) - int64(burned)
	if got := coinDelta(state, before); got != wantCoin {
		return fmt.Errorf("supply not conserved: accounts changed by %d coin, want %d (subsidy %d − burned %d)",
			got, wantCoin, reward, burned)
	}

	// Only an issuance or a management operation may change an asset's total;
	// everything else is a move between accounts and must net to zero.
	wantAssets := map[string]int64{}
	for i := 1; i < len(txs); i++ {
		tx := txs[i]
		switch {
		case tx.Issue != nil:
			wantAssets[AssetID(tx.From, tx.Issue.Ticker, tx.Nonce)] += int64(tx.Issue.Supply)
		case tx.AssetOp != nil:
			switch tx.AssetOp.Op {
			case AssetOpMint:
				wantAssets[tx.AssetID] += int64(tx.AssetOp.Amount)
			case AssetOpBurn:
				wantAssets[tx.AssetID] -= int64(tx.AssetOp.Amount)
			}
		}
	}
	// An operation that nets to zero against another in the same block leaves no
	// delta to check, and must not be reported as an unexplained change.
	for id, want := range wantAssets {
		if want == 0 {
			delete(wantAssets, id)
		}
	}
	got := assetDeltas(state, before)
	for id, want := range wantAssets {
		if got[id] != want {
			return fmt.Errorf("asset %s not conserved: changed by %d, want %d (issued)", id, got[id], want)
		}
	}
	for id, d := range got {
		if _, issued := wantAssets[id]; !issued {
			return fmt.Errorf("asset %s not conserved: changed by %d with no issuance in this block", id, d)
		}
	}
	return nil
}
