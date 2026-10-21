package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// The audit's value is entirely in what it says when something IS wrong, so the
// tests corrupt a healthy chain in the two ways that matter and check it
// notices — a passing audit on a healthy chain proves only that it runs.

func auditedChain(t *testing.T) (*Blockchain, *wallet.Wallet) {
	t.Helper()
	bc := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 4; i++ {
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	}
	return bc, miner
}

func TestAuditPassesOnAHealthyChain(t *testing.T) {
	bc, _ := auditedChain(t)
	a := bc.AuditState()
	if !a.OK() {
		t.Fatalf("a chain built block by block failed its own audit: %s", a.Problem())
	}
	if a.Problem() != "" {
		t.Fatalf("a passing audit still reported a problem: %q", a.Problem())
	}
	if a.Rebuilt != bc.Tip().StateRoot {
		t.Fatalf("rebuilt root %s, tip commits %s", short12(a.Rebuilt), short12(bc.Tip().StateRoot))
	}
	if a.Accounts == 0 || a.Height != bc.Height() {
		t.Fatalf("audit reports %d accounts at height %d, want a non-empty ledger at %d",
			a.Accounts, a.Height, bc.Height())
	}
}

// The failure the audit exists for: the account map and the committed root have
// parted company. Every block was validated on the way in, so nothing else in
// the node notices — this is the only check that looks at the two together
// after the fact.
func TestAuditCatchesStateThatNoLongerMatchesTheCommittedRoot(t *testing.T) {
	bc, miner := auditedChain(t)

	// Credit an account behind consensus's back, exactly as a bug in the
	// application path would.
	bc.mu.Lock()
	acc := bc.state.Get(miner.Address())
	acc.Balance += Coin
	bc.state.Put(miner.Address(), acc)
	bc.mu.Unlock()

	a := bc.AuditState()
	if a.RootOK {
		t.Fatal("the audit did not notice that the account state stopped matching the committed root")
	}
	if a.OK() {
		t.Fatal("OK() reported true for a diverged ledger")
	}
	if a.Problem() == "" {
		t.Fatal("a failed audit described no problem")
	}
	// The message has to say what to do about it, not just that it happened.
	for _, want := range []string{"state root divergence", short12(a.Committed), short12(a.Rebuilt)} {
		if !contains(a.Problem(), want) {
			t.Errorf("problem %q does not mention %q", a.Problem(), want)
		}
	}
}

// The other global invariant: coin held by accounts must equal what the chain
// minted minus what it burned. A supply break is the failure with the worst
// consequence and the fewest symptoms — balances still add up locally, and only
// the total is wrong.
func TestAuditCatchesABrokenSupplyIdentity(t *testing.T) {
	bc, _ := auditedChain(t)

	bc.mu.Lock()
	bc.burned += Coin // claim a burn that never happened
	bc.mu.Unlock()

	a := bc.AuditState()
	if a.SupplyOK {
		t.Fatal("the audit did not notice a supply identity that no longer holds")
	}
	if !contains(a.Problem(), "supply identity broken") {
		t.Fatalf("problem %q does not name the supply identity", a.Problem())
	}
}

// The audit must not depend on the trie cache: that cache is what the
// incremental committer uses, so an audit that shared it would be agreeing with
// the thing it is supposed to check.
func TestAuditIsIndependentOfTheTrieCache(t *testing.T) {
	bc, _ := auditedChain(t)
	committed := bc.Tip().StateRoot

	// Throw the cache away entirely. A rebuild from the account map must still
	// arrive at the same root.
	bc.mu.Lock()
	bc.nodes = NewBoundedNodeStore(DefaultTrieCacheNodes)
	bc.mu.Unlock()

	if a := bc.AuditState(); !a.RootOK || a.Rebuilt != committed {
		t.Fatalf("with a cold cache the audit rebuilt %s, want %s (ok=%v)",
			short12(a.Rebuilt), short12(committed), a.RootOK)
	}
}
