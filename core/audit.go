package core

import (
	"fmt"
	"time"
)

// A running node's self-audit.
//
// Two global invariants decide whether this node's ledger is the one everyone
// else has: the account state must hash to the root its tip committed, and the
// coin accounts hold must equal what the chain minted minus what it burned.
// Both are enforced per block as it connects — the state root in applyBlock,
// conservation in checkConservation — so a node that got here has checked every
// block it accepted.
//
// That is not quite the same as the invariants still holding NOW, and the gap
// is exactly where the interesting failures live. The per-block state check is
// made against an INCREMENTAL commitment backed by a bounded cache
// (statetrie.go): it compares a root the committer produced against the root in
// the header, so a committer and a header that are wrong in the same way agree.
// A full rebuild from the account map shares none of that machinery, which is
// what makes it a second opinion rather than a second copy of the first. The
// same argument the differential fuzz targets rest on (difffuzz_test.go), moved
// from the test suite to a running node.
//
// It is off by default and polled rather than pushed. A rebuild is O(ledger) —
// the cost the incremental committer exists to avoid — so it belongs on a
// schedule an operator chooses, never on the block-application path.

// StateAudit is one independent recomputation of what the chain says it holds.
type StateAudit struct {
	Height uint64 `json:"height"`
	// Committed is the state root in the tip's header; Rebuilt is what the
	// account map hashes to when the trie is built from scratch.
	Committed string `json:"committed"`
	Rebuilt   string `json:"rebuilt"`
	RootOK    bool   `json:"root_ok"`
	// Minted, Burned and Circulating are the supply identity's three terms, and
	// SupplyOK is whether minted − burned == circulating still holds.
	Minted      uint64 `json:"minted"`
	Burned      uint64 `json:"burned"`
	Circulating uint64 `json:"circulating"`
	SupplyOK    bool   `json:"supply_ok"`
	Accounts    int    `json:"accounts"`
	TookMS      int64  `json:"took_ms"`
	At          int64  `json:"at"` // unix seconds
}

// OK reports whether every invariant held.
func (a StateAudit) OK() bool { return a.RootOK && a.SupplyOK }

// Problem describes the first invariant that failed, or "" when none did.
//
// It is phrased for whoever has to act on it: a divergence here means this node
// is building on a ledger no other node shares, and no amount of further
// syncing will reconcile it — the chain has to be re-verified from the store or
// re-synced from a peer.
func (a StateAudit) Problem() string {
	switch {
	case !a.RootOK:
		return fmt.Sprintf(
			"state root divergence at height %d: the account state hashes to %s, but the header commits %s — "+
				"this node's ledger is not the one its own chain commits to",
			a.Height, short12(a.Rebuilt), short12(a.Committed))
	case !a.SupplyOK:
		return fmt.Sprintf(
			"supply identity broken at height %d: minted %d − burned %d = %d, but accounts hold %d",
			a.Height, a.Minted, a.Burned, a.Minted-a.Burned, a.Circulating)
	default:
		return ""
	}
}

// AuditState recomputes the committed state root and the supply identity from
// the account map, independently of the incremental committer and of the
// running burn total.
//
// It takes a read lock for the duration, so it blocks writers (a connecting
// block) for as long as the rebuild takes. That is the price of auditing a
// consistent snapshot rather than a moving one, and it is why the schedule is
// the operator's to choose.
func (bc *Blockchain) AuditState() StateAudit {
	start := time.Now()
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	tip := bc.blocks[len(bc.blocks)-1]
	// stateRoot, not the committer: a throwaway trie over the whole account set,
	// sharing no cache and no cursor with the path that produced the header.
	rebuilt := stateRoot(bc.state)
	minted := CumulativeSubsidy(tip.Index)
	circulating := totalBalance(bc.state)

	a := StateAudit{
		Height:      tip.Index,
		Committed:   tip.StateRoot,
		Rebuilt:     rebuilt,
		RootOK:      rebuilt == tip.StateRoot,
		Minted:      minted,
		Burned:      bc.burned,
		Circulating: circulating,
		SupplyOK:    minted >= bc.burned && minted-bc.burned == circulating,
		Accounts:    bc.state.Len(),
		At:          time.Now().Unix(),
	}
	a.TookMS = time.Since(start).Milliseconds()
	return a
}
