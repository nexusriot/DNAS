package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// withUpgradeAt schedules an upgrade for the duration of a test and unschedules
// it afterwards, so an activation in one test cannot leak into another. The
// restore goes back to the profile baseline, not to "unscheduled" — see
// core/main_test.go.
func withUpgradeAt(t *testing.T, name string, height uint64) {
	t.Helper()
	SetUpgradeHeight(name, height)
	t.Cleanup(func() { restoreUpgrade(name) })
}

// Scheduling a commitment must not change any block hash below its activation
// height — genesis most of all, because a changed genesis is a different chain.
func TestCommitmentsLeaveGenesisAndOldBlocksUntouched(t *testing.T) {
	before := GenesisBlock().Hash
	withUpgradeAt(t, UpgradeFilterCommit, 5)
	withUpgradeAt(t, UpgradeHistoryRoot, 5)
	if after := GenesisBlock().Hash; after != before {
		t.Fatalf("genesis hash moved from %s to %s when an upgrade was scheduled", short12(before), short12(after))
	}

	// A header below the activation height hashes identically with the upgrade
	// scheduled and unscheduled.
	h := Header{Index: 4, Timestamp: 100, PrevHash: "aa", MerkleRoot: "bb", StateRoot: "cc", BaseFee: 1, Bits: 0x1f0fffff, Nonce: 7}
	withScheduled := h.ComputeHash()
	ClearUpgradeHeight(UpgradeFilterCommit)
	ClearUpgradeHeight(UpgradeHistoryRoot)
	if plain := h.ComputeHash(); plain != withScheduled {
		t.Fatal("a block below the activation height hashed differently with the upgrade scheduled")
	}
}

// Above the activation height the commitments ARE in the preimage, so changing
// one must change the hash — otherwise the proof of work does not cover them
// and the whole exercise is decorative.
func TestCommitmentsAreCoveredByProofOfWork(t *testing.T) {
	withUpgradeAt(t, UpgradeFilterCommit, 1)
	withUpgradeAt(t, UpgradeHistoryRoot, 1)
	h := Header{Index: 9, Timestamp: 100, PrevHash: "aa", MerkleRoot: "bb", StateRoot: "cc",
		BaseFee: 1, Bits: 0x1f0fffff, Nonce: 7, FilterRoot: "f00d", HistoryRoot: "beef"}
	base := h.ComputeHash()

	alt := h
	alt.FilterRoot = "f00e"
	if alt.ComputeHash() == base {
		t.Fatal("changing the filter root did not change the header hash")
	}
	alt = h
	alt.HistoryRoot = "beee"
	if alt.ComputeHash() == base {
		t.Fatal("changing the history root did not change the header hash")
	}
}

// A chain mined with both commitments active must build, and every block must
// carry the values the rules demand.
func TestChainBuildsWithCommitmentsActive(t *testing.T) {
	withUpgradeAt(t, UpgradeFilterCommit, 1)
	withUpgradeAt(t, UpgradeHistoryRoot, 1)
	bc := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 6; i++ {
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	}
	tip := bc.Tip()
	if tip.FilterRoot == "" {
		t.Fatal("tip carries no filter root with the commitment active")
	}
	if tip.HistoryRoot == "" {
		t.Fatal("tip carries no history root with the accumulator active")
	}

	// Every block's filter must fold to the root its header commits.
	for i := 1; i < len(bc.blocks); i++ {
		b, prev := bc.blocks[i], bc.blocks[i-1]
		f := BuildBlockFilter(b)
		if !VerifyCommittedFilter(f, prev.FilterRoot, b.FilterRoot) {
			t.Fatalf("block %d: its own filter does not fold to its committed root", i)
		}
	}
}

// The attack the commitment exists to stop: a node serving a filter that is not
// the block's, to hide a payment or invent one. Before the upgrade a client
// cannot tell; after it, the forgery does not fold to the committed value.
func TestCommittedFilterRejectsAForgedFilter(t *testing.T) {
	withUpgradeAt(t, UpgradeFilterCommit, 1)
	bc := NewBlockchain()
	miner, _ := wallet.New()
	alice, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	matureCoinbase(t, bc)
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{
		signedTx(t, miner, alice.Address(), Coin, testFee, 0),
	}))

	tip := bc.Tip()
	prev := bc.blocks[len(bc.blocks)-2]
	honest := BuildBlockFilter(tip)
	if !VerifyCommittedFilter(honest, prev.FilterRoot, tip.FilterRoot) {
		t.Fatal("the honest filter should verify against the committed root")
	}

	// An empty filter is the dangerous forgery: it is a claim that NOBODY was
	// paid in this block, which is exactly what a light client treats as proof.
	forged := honest
	forged.Data = ""
	forged.N = 0
	if VerifyCommittedFilter(forged, prev.FilterRoot, tip.FilterRoot) {
		t.Fatal("an emptied filter verified against the committed root")
	}
}

// A block whose committed filter root is wrong must be rejected, and the
// rejection must say which commitment failed.
func TestBlockWithWrongFilterRootIsRejected(t *testing.T) {
	withUpgradeAt(t, UpgradeFilterCommit, 1)
	bc := NewBlockchain()
	miner, _ := wallet.New()
	b := mineOn(t, bc, miner.Address(), nil)
	b.FilterRoot = "00ff"
	b.Hash = b.ComputeHash() // re-hash so this is a commitment failure, not a PoW one
	err := bc.AddBlock(b)
	if err == nil {
		t.Fatal("a block with a forged filter root was accepted")
	}
	if got := err.Error(); got == "" || !contains(got, "filter root") {
		t.Fatalf("rejection said %q, expected it to name the filter root", got)
	}
}

func TestBlockWithWrongHistoryRootIsRejected(t *testing.T) {
	withUpgradeAt(t, UpgradeHistoryRoot, 1)
	bc := NewBlockchain()
	miner, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	b := mineOn(t, bc, miner.Address(), nil)
	b.HistoryRoot = EmptyMMRRoot // the range is not empty at this height
	b.Hash = b.ComputeHash()
	err := bc.AddBlock(b)
	if err == nil {
		t.Fatal("a block with a forged history root was accepted")
	}
	if got := err.Error(); !contains(got, "history root") {
		t.Fatalf("rejection said %q, expected it to name the history root", got)
	}
}

// Before activation the fields must be empty. A field nobody checks is a field
// anyone can stuff, and it would change the hash the moment the rule turns on.
func TestCommitmentFieldsRejectedBeforeActivation(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	b := mineOn(t, bc, miner.Address(), nil)
	b.FilterRoot = "deadbeef"
	b.Hash = b.ComputeHash()
	if err := bc.AddBlock(b); err == nil {
		t.Fatal("a filter root set before activation was accepted")
	}
}

// Ancestry: the point of the accumulator. A block far below the tip must be
// provable against the tip's committed root with a logarithmic proof.
func TestProveAncestryAgainstCommittedRoot(t *testing.T) {
	withUpgradeAt(t, UpgradeHistoryRoot, 1)
	bc := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 12; i++ {
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	}
	tip := bc.Tip()

	for _, height := range []uint64{1, 2, 7, 11} {
		p, err := bc.ProveAncestry(height, tip.Index)
		if err != nil {
			t.Fatalf("prove %d as of %d: %v", height, tip.Index, err)
		}
		if p.BlockHash != bc.blocks[height].Hash {
			t.Fatalf("proof is for the wrong block at height %d", height)
		}
		// The verifier gets the root from the header, exactly as a light client
		// that has checked the header's proof of work would.
		if !VerifyAncestryProof(p, tip.HistoryRoot) {
			t.Fatalf("ancestry proof for %d did not verify against the tip's committed root", height)
		}
	}

	// A block cannot be proven against its own header: the accumulator a header
	// commits covers what came BEFORE it.
	if _, err := bc.ProveAncestry(tip.Index, tip.Index); err == nil {
		t.Fatal("a block was proven against its own committed root")
	}
}

// A proof for a block that is NOT in the chain must not verify — the structure
// is only useful if it can say no.
func TestAncestryProofRejectsAForeignBlock(t *testing.T) {
	withUpgradeAt(t, UpgradeHistoryRoot, 1)
	bc := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 8; i++ {
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	}
	tip := bc.Tip()
	p, err := bc.ProveAncestry(3, tip.Index)
	if err != nil {
		t.Fatal(err)
	}
	p.BlockHash = "00" // a block the chain never had
	if VerifyAncestryProof(p, tip.HistoryRoot) {
		t.Fatal("a proof naming a block the chain never had verified")
	}
}

// Both commitments must survive a reorg: the accumulator is append-only, so the
// fork point has to be reconstructed rather than rewound.
func TestCommitmentsSurviveReorg(t *testing.T) {
	withUpgradeAt(t, UpgradeFilterCommit, 1)
	withUpgradeAt(t, UpgradeHistoryRoot, 1)
	bc := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 4; i++ {
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	}
	forkHeight := 2

	alt := NewBlockchain()
	for i := 1; i <= forkHeight; i++ {
		mustAdd(t, alt, bc.blocks[i])
	}
	var suffix []Block
	for i := 0; i < 4; i++ {
		b := mineOn(t, alt, miner.Address(), nil)
		mustAdd(t, alt, b)
		suffix = append(suffix, b)
	}

	replaced, _, err := bc.ReorgFrom(uint64(forkHeight), suffix)
	if err != nil || !replaced {
		t.Fatalf("reorg: replaced=%v err=%v", replaced, err)
	}
	// The accumulator must now describe the NEW chain. The cheapest honest
	// check is that the next block it would produce validates.
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))

	tip := bc.Tip()
	p, err := bc.ProveAncestry(1, tip.Index)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyAncestryProof(p, tip.HistoryRoot) {
		t.Fatal("ancestry proof failed against the post-reorg committed root")
	}
}

// The filter re-keying is the half of the change that makes the commitment
// possible. Above the activation height the key must NOT come from the block
// hash, or the definition is circular.
func TestFilterKeyMovesOffTheBlockHashAtActivation(t *testing.T) {
	b := Block{Index: 5, PrevHash: "aabb", MerkleRoot: "ccdd", Hash: "eeff"}

	ClearUpgradeHeight(UpgradeFilterCommit)
	k0Old, k1Old := filterKeyFor(b)
	wantOld0, wantOld1 := filterKey(b.Hash)
	if k0Old != wantOld0 || k1Old != wantOld1 {
		t.Fatal("below activation the filter should still be keyed by the block hash")
	}

	withUpgradeAt(t, UpgradeFilterCommit, 1)
	k0New, k1New := filterKeyFor(b)
	if k0New == wantOld0 && k1New == wantOld1 {
		t.Fatal("above activation the filter is still keyed by the block hash (circular)")
	}
	// And the new key must not depend on the hash at all: changing it changes
	// nothing, which is what lets the filter be computed before mining.
	alt := b
	alt.Hash = "0000"
	k0Alt, k1Alt := filterKeyFor(alt)
	if k0Alt != k0New || k1Alt != k1New {
		t.Fatal("the re-keyed filter still depends on the block hash")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
