package core

import (
	"path/filepath"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// assertHashIndex checks the maintained index against one rebuilt from the
// blocks: same size, and every block's hash at its own height.
func assertHashIndex(t *testing.T, bc *Blockchain, when string) {
	t.Helper()
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if len(bc.byHash) != len(bc.blocks) {
		t.Fatalf("%s: index holds %d hashes, chain has %d blocks", when, len(bc.byHash), len(bc.blocks))
	}
	for i, b := range bc.blocks {
		if h, ok := bc.byHash[b.Hash]; !ok || h != uint64(i) {
			t.Fatalf("%s: block %d indexed at %d (present %v)", when, i, h, ok)
		}
	}
}

func TestHashIndexFollowsConnectReorgAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	miner, _ := wallet.New()
	other, _ := wallet.New()
	for i := 0; i < 5; i++ {
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	}
	assertHashIndex(t, bc, "after connecting")
	if h, ok := bc.HeightOf(bc.Tip().Hash); !ok || h != bc.Height() {
		t.Fatalf("HeightOf(tip) = %d, %v", h, ok)
	}

	// A longer rival from height 2 replaces the top three blocks.
	rival := NewBlockchain()
	for i := 1; i <= 2; i++ {
		mustAdd(t, rival, bc.Blocks()[i])
	}
	for i := 0; i < 5; i++ {
		mustAdd(t, rival, mineOn(t, rival, other.Address(), nil))
	}
	dropped := bc.Blocks()[3:]
	if ok, _, err := bc.ReplaceChain(rival.Blocks()); err != nil || !ok {
		t.Fatalf("reorg: %v, %v", ok, err)
	}
	assertHashIndex(t, bc, "after the reorg")
	for _, b := range dropped {
		if _, ok := bc.HeightOf(b.Hash); ok {
			t.Fatalf("a disconnected block %d is still indexed", b.Index)
		}
	}
	if got := bc.LocatorFork([]string{dropped[len(dropped)-1].Hash, bc.Blocks()[2].Hash}); got != 2 {
		t.Fatalf("locator naming a disconnected block: fork %d, want 2", got)
	}

	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}
	bc, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Close()
	assertHashIndex(t, bc, "after reopening")
}

func TestHashIndexAfterSnapshotAndPruning(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 8; i++ {
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	}
	booted := snapshotChainAt(t, bc, bc.Height(), true)
	assertHashIndex(t, booted, "after a snapshot bootstrap")
	if _, ok := booted.HeightOf(bc.Blocks()[1].Hash); !ok {
		t.Fatal("a placeholder below the snapshot is not findable by hash")
	}

	// Pruning swaps bodies for header-only placeholders, which keep their hash.
	pruneEvery(bc, 2)
	if !bc.Blocks()[1].IsPlaceholder() {
		t.Fatal("precondition: block 1 should have been pruned")
	}
	assertHashIndex(t, bc, "after pruning")
}
