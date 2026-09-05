package core

import (
	"errors"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// pruneEvery sets the keep window directly, bypassing the MinPruneKeep floor
// EnablePruning enforces.
//
// The floor is a policy on the exported API — a node must keep the bodies a
// reorg can reach — and honouring it here would mean mining 132+ blocks of real
// proof of work in every one of these tests, which under -race is minutes of
// hashing to observe a rule that has nothing to do with hashing. The floor
// itself is checked once, separately, where it costs nothing.
func pruneEvery(bc *Blockchain, keep uint64) {
	bc.mu.Lock()
	bc.pruneKeep = keep
	bc.pruneLocked()
	bc.mu.Unlock()
}

// chainOfHeight mines n blocks onto a fresh chain, all rewards to one miner.
func chainOfHeight(t *testing.T, bc *Blockchain, n int) {
	t.Helper()
	miner, _ := wallet.New()
	for i := 0; i < n; i++ {
		if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil {
			t.Fatalf("mine %d: %v", i, err)
		}
	}
}

// A Blockchain holds its blocks in memory, so a long chain costs its whole size
// in RAM. Pruning keeps the state and the headers and drops old bodies.
func TestPruningDropsOldBodiesAndKeepsTheChainUsable(t *testing.T) {
	bc := NewBlockchain()
	pruneEvery(bc, 4)
	chainOfHeight(t, bc, 12)

	// The bodies deeper than the keep window are gone; the recent ones are not.
	if bc.PrunedCount() == 0 {
		t.Fatal("nothing was pruned on a chain past the keep window")
	}
	tip := bc.Height()
	if !bc.HasBody(tip) || !bc.HasBody(tip-1) {
		t.Fatal("a recent body was pruned")
	}
	if bc.HasBody(1) {
		t.Fatal("a deep body survived pruning")
	}
	if got := bc.BodyHeight(); got <= 1 {
		t.Fatalf("BodyHeight = %d after pruning, want the start of the keep window", got)
	}
	if bc.HasBody(bc.BodyHeight() - 1) {
		t.Fatal("the height below BodyHeight still has a body")
	}

	// Everything validation needs survives: the height, the linkage, the state,
	// and the ability to extend.
	if bc.Tip().Index != tip {
		t.Fatalf("tip = %d, want %d", bc.Tip().Index, tip)
	}
	before := bc.Work().String()
	chainOfHeight(t, bc, 3)
	if bc.Height() != tip+3 {
		t.Fatalf("a pruned chain could not be extended: height %d", bc.Height())
	}
	if bc.Work().String() == before {
		t.Fatal("work did not accumulate over the new blocks")
	}
	// The header of a pruned height is still there, hash and all — that is what
	// keeps difficulty, median-time-past and linkage working.
	old, ok := bc.BlockAt(1)
	if !ok || old.Hash == "" || old.PrevHash == "" {
		t.Fatalf("the header of a pruned block is gone: %+v", old)
	}
	if !old.IsPlaceholder() {
		t.Fatal("the pruned block is not marked as a placeholder")
	}
}

// The distinction that matters to a client: "there is no such block" and "I
// cannot see that far back" are different answers.
func TestPrunedBodyIsReportedAsPrunedNotMissing(t *testing.T) {
	bc := NewBlockchain()
	pruneEvery(bc, 4)
	chainOfHeight(t, bc, 10)

	if _, err := bc.BlockBodyAt(1); !errors.Is(err, ErrPrunedBody) {
		t.Fatalf("a pruned body gave %v, want ErrPrunedBody", err)
	}
	if _, err := bc.BlockBodyAt(bc.Height()); err != nil {
		t.Fatalf("a kept body gave %v", err)
	}
	if _, err := bc.BlockBodyAt(bc.Height() + 100); err == nil {
		t.Fatal("a height above the tip was served")
	} else if errors.Is(err, ErrPrunedBody) {
		t.Fatal("a height that never existed was reported as pruned")
	}
}

// The trap in pruning: a compact filter built from a missing body is a valid
// EMPTY filter, and an empty filter is a proof of ABSENCE. Serving one would
// tell a light client its address is provably not in a block the node cannot
// even read.
func TestPrunedBlocksServeNoFilter(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	// A filter for a block that HAS a body must match its miner.
	f, ok := bc.BlockFilterAt(1)
	if !ok {
		t.Fatal("no filter for a block with a body")
	}
	if !f.Match(alice.Address()) {
		t.Fatal("the filter does not match the block's own miner")
	}

	pruneEvery(bc, 4)
	chainOfHeight(t, bc, 10)
	if bc.HasBody(1) {
		t.Fatal("height 1 was not pruned")
	}
	if _, ok := bc.BlockFilterAt(1); ok {
		t.Fatal("a pruned block served a filter, which would prove absence falsely")
	}
	// And the paged list skips it rather than including an empty one.
	for _, f := range bc.BlockFiltersFrom(0, 100) {
		if !bc.HasBody(f.Index) {
			t.Fatalf("the filter list includes pruned height %d", f.Index)
		}
	}
}

// The filter-header chain is a running hash over bodies, so it has to be cached:
// a pruning node that re-folded over what it still has would produce a chain
// agreeing with nobody.
func TestFilterHeaderChainSurvivesPruning(t *testing.T) {
	full := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 12; i++ {
		b := mineOn(t, full, miner.Address(), nil)
		if err := full.AddBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	want := full.FilterHeaders()

	// The same chain, replayed onto a pruning node.
	pruned := NewBlockchain()
	pruneEvery(pruned, 4)
	for _, b := range full.Blocks()[1:] {
		if err := pruned.AddBlock(b); err != nil {
			t.Fatalf("replay onto the pruning node: %v", err)
		}
	}
	if pruned.PrunedCount() == 0 {
		t.Fatal("the replay pruned nothing")
	}
	got := pruned.FilterHeaders()
	if len(got) != len(want) {
		t.Fatalf("pruned node holds %d filter headers, the full node %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("filter header %d differs: %s vs %s", i, got[i], want[i])
		}
	}
	if base := pruned.FilterHeaderBase(); base != 0 {
		t.Fatalf("a node that followed from genesis reports base %d", base)
	}
	// Paging must line up with height, or a client cross-checking a filter
	// against the chain compares the wrong entries.
	page := pruned.FilterHeadersFrom(5, 3)
	if len(page) != 3 || page[0] != want[5] || page[2] != want[7] {
		t.Fatalf("page from 5 = %v, want %v", page, want[5:8])
	}
}

// A reorg rewrites bodies, so the cached chain has to be truncated with them.
func TestFilterHeaderChainFollowsAReorg(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	carol, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	shared := bc.Blocks()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	beforeReorg := bc.FilterHeaders()

	// A heavier branch off the shared prefix, mined to somebody else, so its
	// filters (and therefore its filter headers) differ.
	y := NewBlockchain()
	for _, b := range shared[1:] {
		if err := y.AddBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := y.AddBlock(mineOn(t, y, carol.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}
	ok, _, err := bc.ReplaceChain(y.Blocks())
	if !ok || err != nil {
		t.Fatalf("reorg: ok=%v err=%v", ok, err)
	}

	got := bc.FilterHeaders()
	if len(got) != len(y.Blocks()) {
		t.Fatalf("after the reorg the chain holds %d headers for %d blocks", len(got), len(y.Blocks()))
	}
	// The shared prefix is unchanged and the replaced suffix is not.
	if got[1] != beforeReorg[1] {
		t.Fatal("the reorg rewrote the shared prefix's filter headers")
	}
	if len(beforeReorg) > 2 && got[2] == beforeReorg[2] {
		t.Fatal("the replaced block kept its old filter header")
	}
	// And it must equal what the winning branch computed independently.
	want := y.FilterHeaders()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("filter header %d after the reorg = %s, want %s", i, got[i], want[i])
		}
	}
}

// Pruning must not reach into the range a reorg can, or the node cannot follow
// the chain it is required to follow. That is a property of the FLOOR, which is
// why it is asserted on the constant rather than by mining 132 blocks: the
// exported API can only be given a window at or above it.
func TestPruningNeverTouchesTheReorgWindow(t *testing.T) {
	if MinPruneKeep <= MaxReorgDepth {
		t.Fatalf("MinPruneKeep (%d) must exceed MaxReorgDepth (%d)", MinPruneKeep, MaxReorgDepth)
	}
	bc := NewBlockchain()
	if keep := bc.EnablePruning(1); keep != MinPruneKeep {
		t.Fatalf("EnablePruning(1) kept %d, want the %d floor", keep, MinPruneKeep)
	}
	if keep := bc.EnablePruning(MinPruneKeep + 500); keep != MinPruneKeep+500 {
		t.Fatalf("EnablePruning above the floor was changed to %d", keep)
	}
	if bc.PruneKeep() != MinPruneKeep+500 {
		t.Fatalf("PruneKeep reports %d", bc.PruneKeep())
	}
	// And on a short chain nothing is pruned at all, whatever the window: the
	// cutoff is measured from the tip.
	chainOfHeight(t, bc, 3)
	if bc.PrunedCount() != 0 || !bc.HasBody(1) {
		t.Fatal("a chain shorter than the keep window was pruned")
	}

	// With a window applied directly, everything inside it keeps its body.
	short := NewBlockchain()
	pruneEvery(short, 4)
	chainOfHeight(t, short, 12)
	tip := short.Height()
	for h := tip; h > tip-4; h-- {
		if !short.HasBody(h) {
			t.Fatalf("height %d is inside the keep window (tip %d) and has no body", h, tip)
		}
	}
}

// An asset issued below the prune horizon still exists and is still held: what
// is lost is the transaction that issued it, not the fact of it.
func TestPruningKeepsTheAssetRegistry(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	miner, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	issue := Transaction{From: alice.Address(), Fee: testFee, Nonce: 0,
		Issue: &AssetIssue{Ticker: "GOLD", Supply: 1000}}
	if err := issue.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{issue})); err != nil {
		t.Fatal(err)
	}
	id := AssetID(alice.Address(), "GOLD", 0)

	pruneEvery(bc, 2)
	chainOfHeight(t, bc, 8)

	if info, ok := bc.Asset(id); !ok || info.Ticker != "GOLD" {
		t.Fatalf("the asset registry lost a pruned issuance: %+v %v", info, ok)
	}
	if bc.Account(alice.Address()).Assets[id] != 1000 {
		t.Fatal("the asset balance did not survive pruning")
	}
	// The transaction index, by contrast, must NOT keep pointing into a body that
	// is gone: a location holding nothing is worse than a miss.
	if _, _, ok := bc.FindTx(issue.Hash()); ok {
		t.Fatal("the transaction index still resolves a pruned transaction")
	}
	if _, ok := bc.FindTxProof(issue.Hash()); ok {
		t.Fatal("an inclusion proof was produced for a pruned transaction")
	}
}
