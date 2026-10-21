package core

import (
	"path/filepath"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// Rules that are each correct alone and wrong together.
//
// Every consensus upgrade here used to be exercised on its own: a test switched
// on the one it was about and left the rest unscheduled. That is enough to show
// a rule does what it says, and not enough to show it still does when another
// rule is also in force — which is how `checkedaddresses` came to forbid every
// asset and name operation, and how the header commitments came to break
// pruning and snapshot bootstrap. The all-upgrades profile
// (DNAS_TEST_UPGRADES=all, see main_test.go) runs the whole suite in
// combination; the tests below pin the specific defects it found, so they stay
// covered in the default pass too.

// The accumulator is reached two ways — appended to as the chain extends, and
// rebuilt to a fork point by a reorg — and they must cover the same blocks. For
// a chain that activates it at height 0 they did not: the rebuild counted
// genesis and forward extension never could, so the two roots disagreed and
// every reorg was refused with "wrong history root".
func TestHistoryAccumulatorAgreesAcrossBothPaths(t *testing.T) {
	for _, activation := range []uint64{0, 1, 3} {
		t.Run("activated_at_"+itoaTest(activation), func(t *testing.T) {
			withUpgradeAt(t, UpgradeHistoryRoot, activation)
			bc := NewBlockchain()
			miner, _ := wallet.New()
			for i := 0; i < 6; i++ {
				mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
			}
			// At every height, the range the chain maintains must equal the range
			// a reorg to that point would rebuild.
			for at := 0; at < len(bc.blocks); at++ {
				maintained := bc.historyUpTo(at)
				rebuilt := bc.rebuildHistoryTo(at)
				if maintained.Root() != rebuilt.Root() {
					t.Fatalf("at height %d the maintained range (%d leaves) and the rebuilt one (%d leaves) disagree: %s vs %s",
						at, maintained.Len(), rebuilt.Len(), short12(maintained.Root()), short12(rebuilt.Root()))
				}
			}
		})
	}
}

// And the consequence, end to end: a chain running the accumulator must still
// be able to reorg.
func TestChainWithTheAccumulatorCanStillReorg(t *testing.T) {
	withUpgradeAt(t, UpgradeHistoryRoot, 0)
	bc := NewBlockchain()
	miner, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	forkHeight := int(bc.Tip().Index)
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))

	alt := NewBlockchain()
	for i := 1; i <= forkHeight; i++ {
		mustAdd(t, alt, bc.blocks[i])
	}
	var suffix []Block
	for i := 0; i < 2; i++ {
		b := mineOn(t, alt, miner.Address(), nil)
		mustAdd(t, alt, b)
		suffix = append(suffix, b)
	}

	adopted, _, err := bc.ReorgFrom(uint64(forkHeight), suffix)
	if err != nil {
		t.Fatalf("a chain running the history accumulator refused a valid reorg: %v", err)
	}
	if !adopted {
		t.Fatal("the heavier branch was not adopted")
	}
}

// A pruned block keeps its header, and a header is only a header if it still
// hashes to its own hash. Dropping the scheduled commitments from the
// placeholder left every pruned height carrying a header that fails its own
// proof-of-work check — which is what a snapshot taken there is built from.
func TestPrunedPlaceholderKeepsItsHeaderValid(t *testing.T) {
	withUpgradeAt(t, UpgradeFilterCommit, 1)
	withUpgradeAt(t, UpgradeHistoryRoot, 1)

	bc := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 4; i++ {
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	}
	full := bc.blocks[3]
	if full.FilterRoot == "" || full.HistoryRoot == "" {
		t.Fatal("precondition: the block should carry both commitments at this height")
	}

	placeholder := BlockFromHeader(full.Header())
	if !placeholder.Header().HasValidPoW() {
		t.Fatalf("a pruned placeholder's header no longer verifies: filter=%q history=%q",
			placeholder.FilterRoot, placeholder.HistoryRoot)
	}
	if placeholder.Hash != placeholder.ComputeHash() {
		t.Fatal("the placeholder's stored hash does not recompute from its own fields")
	}
}

// A chain bootstrapped from a snapshot never appended to the accumulator block
// by block, so it has to rebuild one from the headers it was given. Asking for
// the range "at the tip" returned a clone of the empty range it started with,
// so the node expected an empty-range root from every block above the snapshot
// and refused all of them.
func TestSnapshotBootstrapRebuildsTheAccumulator(t *testing.T) {
	withUpgradeAt(t, UpgradeHistoryRoot, 1)

	bc := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 8; i++ {
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	}
	const snapHeight = 5
	snap, ok := bc.SnapshotAt(snapHeight)
	if !ok {
		t.Fatal("no snapshot at that height")
	}
	headers := make([]Header, snapHeight+1)
	for i := range headers {
		headers[i] = bc.blocks[i].Header()
	}

	booted, err := NewFromSnapshot(snap, headers)
	if err != nil {
		t.Fatalf("bootstrap from snapshot: %v", err)
	}
	if got, want := booted.HistoryRoot(), bc.historyUpTo(snapHeight).Root(); got != want {
		t.Fatalf("a snapshot-bootstrapped chain's accumulator is %s, want the full node's %s",
			short12(got), short12(want))
	}
	// And it must be able to take the blocks above the snapshot.
	for i := snapHeight + 1; i < len(bc.blocks); i++ {
		if err := booted.AddBlock(bc.blocks[i]); err != nil {
			t.Fatalf("replay block %d above the snapshot: %v", i, err)
		}
	}
}

// The whole round trip, with every commitment in force: build a chain, prune
// and compact it to disk, reopen it, and keep going. Each half of this was
// covered; the combination is what was broken.
func TestPrunedStoreReopensWithEveryCommitmentActive(t *testing.T) {
	withUpgradeAt(t, UpgradeFilterCommit, 1)
	withUpgradeAt(t, UpgradeHistoryRoot, 1)
	withUpgradeAt(t, UpgradeUniqueCoinbase, 1)

	path := filepath.Join(t.TempDir(), "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	miner, _ := wallet.New()
	for i := 0; i < int(MinPruneKeep)+6; i++ {
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	}
	bc.EnablePruning(MinPruneKeep)
	if err := bc.CompactStore(); err != nil {
		t.Fatalf("compact: %v", err)
	}
	tip, height := bc.Tip().Hash, bc.Height()
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen a pruned, compacted store with the commitments active: %v", err)
	}
	defer reopened.Close()
	if reopened.Tip().Hash != tip || reopened.Height() != height {
		t.Fatalf("reopened at %d/%s, want %d/%s", reopened.Height(), short12(reopened.Tip().Hash), height, short12(tip))
	}
	// It must still accept the next block, which means its accumulator and
	// filter-header chain both survived the round trip.
	if err := reopened.AddBlock(mineOn(t, reopened, miner.Address(), nil)); err != nil {
		t.Fatalf("a reopened pruned store could not take the next block: %v", err)
	}
}

func itoaTest(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}
