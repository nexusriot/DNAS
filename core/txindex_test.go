package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

func TestFindTxLocatesConfirmedTransaction(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()

	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	tx := signedTx(t, alice, bob.Address(), 5*Coin, testFee, 0)
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err != nil {
		t.Fatal(err)
	}
	height := bc.Height()

	got, loc, ok := bc.FindTx(tx.Hash())
	if !ok {
		t.Fatal("confirmed transaction not found in the index")
	}
	if loc.Height != height || loc.Index != 1 {
		t.Fatalf("located at %+v, want height %d index 1", loc, height)
	}
	if got.Hash() != tx.Hash() {
		t.Fatal("index returned a different transaction")
	}
	if !bc.HasTx(tx.Hash()) {
		t.Fatal("HasTx disagrees with FindTx")
	}
	if _, _, ok := bc.FindTx("deadbeef"); ok {
		t.Fatal("unknown hash reported as confirmed")
	}
}

// The index must agree with the merkle proof it now drives: a proof built from an
// index hit has to verify against the block's committed merkle root.
func TestFindTxProofFromIndexVerifies(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()

	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	tx := signedTx(t, alice, bob.Address(), Coin, testFee, 0)
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err != nil {
		t.Fatal(err)
	}

	pr, ok := bc.FindTxProof(tx.Hash())
	if !ok || !pr.Found {
		t.Fatal("no proof for a confirmed transaction")
	}
	if pr.BlockIndex != bc.Height() || pr.Confirmations != 1 {
		t.Fatalf("proof at height %d with %d confirmations, want %d and 1", pr.BlockIndex, pr.Confirmations, bc.Height())
	}
	if !VerifyMerkleProof(tx.Hash(), pr.MerkleRoot, pr.Proof) {
		t.Fatal("index-built proof does not verify against the merkle root")
	}
}

// A reorg must take the losing branch's transactions out of the index and put the
// winning branch's in, or SPV clients would prove payments that no longer exist.
func TestTxIndexFollowsReorg(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	carol, _ := wallet.New()

	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	shared := bc.Blocks()

	// Branch X confirms alice -> bob.
	lost := signedTx(t, alice, bob.Address(), 5*Coin, testFee, 0)
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{lost})); err != nil {
		t.Fatal(err)
	}
	if !bc.HasTx(lost.Hash()) {
		t.Fatal("branch X transaction should be indexed")
	}

	// Branch Y is heavier and confirms alice -> carol at the same nonce instead.
	y := NewBlockchain()
	for _, b := range shared[1:] {
		if err := y.AddBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	won := signedTx(t, alice, carol.Address(), 3*Coin, testFee, 0)
	if err := y.AddBlock(mineOn(t, y, carol.Address(), []Transaction{won})); err != nil {
		t.Fatal(err)
	}
	if err := y.AddBlock(mineOn(t, y, carol.Address(), nil)); err != nil {
		t.Fatal(err)
	}

	adopted, disconnected, err := bc.ReplaceChain(y.Blocks())
	if !adopted || err != nil {
		t.Fatalf("reorg: adopted=%v err=%v", adopted, err)
	}
	if len(disconnected) != 1 || disconnected[0].Transactions[1].Hash() != lost.Hash() {
		t.Fatalf("expected the one orphaned block carrying the lost tx, got %d block(s)", len(disconnected))
	}
	if bc.HasTx(lost.Hash()) {
		t.Fatal("orphaned transaction still indexed after the reorg")
	}
	if !bc.HasTx(won.Hash()) {
		t.Fatal("winning branch's transaction was not indexed")
	}
	if _, ok := bc.FindTxProof(lost.Hash()); ok {
		t.Fatal("FindTxProof still proves an orphaned transaction")
	}
}

// Two blocks paying the same miner the same subsidy carry byte-identical
// coinbases, so their txids collide. The index keeps the lowest height (matching
// the scan it replaces) and must not lose that entry when a later duplicate is
// disconnected.
func TestTxIndexKeepsFirstOccurrenceOfDuplicateCoinbase(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()

	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	first := bc.Height()
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	cb := bc.Tip().Transactions[0]
	if bc.blocks[first].Transactions[0].Hash() != cb.Hash() {
		t.Skip("coinbases are no longer duplicated across blocks; nothing to check")
	}

	_, loc, ok := bc.FindTx(cb.Hash())
	if !ok || loc.Height != first {
		t.Fatalf("duplicate coinbase located at %+v, want the first occurrence at height %d", loc, first)
	}

	// Disconnect the later duplicate; the first occurrence must survive.
	rival := NewBlockchain()
	for _, b := range bc.Blocks()[1 : first+1] {
		if err := rival.AddBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	other, _ := wallet.New()
	for i := 0; i < 3; i++ {
		if err := rival.AddBlock(mineOn(t, rival, other.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}
	adopted, _, err := bc.ReplaceChain(rival.Blocks())
	if !adopted || err != nil {
		t.Fatalf("reorg: adopted=%v err=%v", adopted, err)
	}
	_, loc, ok = bc.FindTx(cb.Hash())
	if !ok || loc.Height != first {
		t.Fatalf("first occurrence lost when its duplicate was disconnected: ok=%v loc=%+v", ok, loc)
	}
}

// The index is derived state: rebuilding it from the chain must reproduce it.
func TestTxIndexMatchesFullRebuild(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()

	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	for i := uint64(0); i < 3; i++ {
		tx := signedTx(t, alice, bob.Address(), Coin, testFee, i)
		if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err != nil {
			t.Fatal(err)
		}
	}

	rebuilt := buildTxIndex(bc.Blocks())
	if len(rebuilt) != len(bc.txIndex) {
		t.Fatalf("rebuilt index has %d entries, live index has %d", len(rebuilt), len(bc.txIndex))
	}
	for h, want := range rebuilt {
		got, ok := bc.txIndex[h]
		if !ok || got != want {
			t.Fatalf("index mismatch for %s: got %+v (%v), want %+v", h[:8], got, ok, want)
		}
	}
}
