package node

import (
	"testing"

	"github.com/nexusriot/DNAS/core"
)

// A compact block carries a header and short ids; the receiver rebuilds the
// body from its own pool. Rebuilding means widening that header back into a
// block — and the widening used to be written out field by field here, which
// means it fell behind the moment the header grew.
//
// It did. The scheduled commitments (FilterRoot, HistoryRoot) were dropped, so
// from their activation height every compactly-relayed block was rejected as
// "wrong filter root". Nothing caught it: reconstruction checks the merkle
// root, which is over transactions alone and still matched, so the node
// believed it had rebuilt the block correctly and did not even fall back to
// fetching it in full. Compact relay is the default block path, so this is
// block propagation failing on a chain that has these upgrades on.

func withNodeUpgrade(t *testing.T, name string, height uint64) {
	t.Helper()
	core.SetUpgradeHeight(name, height)
	t.Cleanup(func() { core.ClearUpgradeHeight(name) })
}

func TestCompactReconstructionKeepsTheHeaderCommitments(t *testing.T) {
	withNodeUpgrade(t, core.UpgradeFilterCommit, 1)
	withNodeUpgrade(t, core.UpgradeHistoryRoot, 1)

	n, blk, _ := blockWithPending(t, 3)
	if blk.FilterRoot == "" || blk.HistoryRoot == "" {
		t.Fatalf("precondition: the mined block carries no commitments (filter=%q history=%q)",
			blk.FilterRoot, blk.HistoryRoot)
	}

	// The widening on its own: whatever a receiver rebuilds from the header must
	// still hash to the hash the header claims.
	rebuilt := core.BlockFromHeader(blk.Header())
	rebuilt.Transactions = blk.Transactions
	if rebuilt.FilterRoot != blk.FilterRoot || rebuilt.HistoryRoot != blk.HistoryRoot {
		t.Fatalf("rebuilding from the header lost a commitment: filter %q->%q, history %q->%q",
			blk.FilterRoot, rebuilt.FilterRoot, blk.HistoryRoot, rebuilt.HistoryRoot)
	}
	if rebuilt.ComputeHash() != blk.Hash {
		t.Fatal("the rebuilt block does not hash to the hash its own header carries")
	}

	// And end to end, through the relay path the network actually uses.
	before := n.Chain().Height()
	p, _ := fakePeer(CapCompact)
	attach(n, p)
	n.onCompactBlock(p, NewCompactBlock(blk))
	if got := n.Chain().Height(); got != before+1 {
		t.Fatalf("height = %d after a compact block with commitments active, want %d", got, before+1)
	}
}
