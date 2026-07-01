package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// distinctTxs builds n transactions with distinct hashes (no signing needed —
// the merkle math only depends on the leaf hashes).
func distinctTxs(n int) []Transaction {
	txs := make([]Transaction, n)
	for i := range txs {
		txs[i] = Transaction{From: "a", To: "b", Amount: uint64(i + 1), Nonce: uint64(i)}
	}
	return txs
}

func TestMerkleProofRoundTrip(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 5, 8, 9} {
		txs := distinctTxs(n)
		root := MerkleRoot(txs)
		for i := 0; i < n; i++ {
			proof, ok := MerkleProof(txs, i)
			if !ok {
				t.Fatalf("n=%d i=%d: no proof", n, i)
			}
			if !VerifyMerkleProof(txs[i].Hash(), root, proof) {
				t.Fatalf("n=%d i=%d: proof did not verify against root", n, i)
			}
		}
	}
}

func TestMerkleProofRejectsWrongLeaf(t *testing.T) {
	txs := distinctTxs(4)
	root := MerkleRoot(txs)
	proof, _ := MerkleProof(txs, 1)
	if VerifyMerkleProof(txs[2].Hash(), root, proof) {
		t.Fatal("a proof for index 1 must not verify a different leaf")
	}
}

func TestMerkleProofRejectsTamperedStep(t *testing.T) {
	txs := distinctTxs(4)
	root := MerkleRoot(txs)
	proof, _ := MerkleProof(txs, 0)
	if len(proof) == 0 {
		t.Fatal("expected a non-empty proof")
	}
	proof[0].Hash = "deadbeef"
	if VerifyMerkleProof(txs[0].Hash(), root, proof) {
		t.Fatal("a tampered proof must not verify")
	}
}

func TestMerkleProofOutOfRange(t *testing.T) {
	txs := distinctTxs(3)
	if _, ok := MerkleProof(txs, 3); ok {
		t.Fatal("out-of-range index should fail")
	}
	if _, ok := MerkleProof(txs, -1); ok {
		t.Fatal("negative index should fail")
	}
}

// TestSPVProofAgainstChain plays the light-client role end to end: find a
// confirmed transaction's proof, verify the block header's proof-of-work, and
// confirm the merkle proof folds to that header's merkle root.
func TestSPVProofAgainstChain(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	tx := signedTx(t, alice, bob.Address(), 5*Coin, Coin, 0)
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err != nil {
		t.Fatal(err)
	}

	pr, ok := bc.FindTxProof(tx.Hash())
	if !ok || !pr.Found {
		t.Fatal("transaction should be found in the chain")
	}
	if pr.Confirmations != 1 {
		t.Fatalf("confirmations = %d, want 1", pr.Confirmations)
	}

	hdr, ok := bc.HeaderAt(pr.BlockIndex)
	if !ok {
		t.Fatal("header for the proof's block is missing")
	}
	if !hdr.HasValidPoW() {
		t.Fatal("block header failed proof-of-work check")
	}
	if hdr.MerkleRoot != pr.MerkleRoot {
		t.Fatal("proof merkle root disagrees with the header")
	}
	if !VerifyMerkleProof(tx.Hash(), hdr.MerkleRoot, pr.Proof) {
		t.Fatal("merkle proof failed to verify against the header merkle root")
	}

	if _, ok := bc.FindTxProof("not-a-real-tx"); ok {
		t.Fatal("an unknown transaction should not be found")
	}
}

func TestHeaderChainVerifiable(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 3; i++ {
		if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}
	headers := bc.Headers()
	if len(headers) != 4 { // genesis + 3
		t.Fatalf("got %d headers, want 4", len(headers))
	}
	for i := 1; i < len(headers); i++ { // genesis is not mined to a target
		if !headers[i].HasValidPoW() {
			t.Fatalf("header %d failed proof-of-work", i)
		}
		if headers[i].PrevHash != headers[i-1].Hash {
			t.Fatalf("header %d does not link to its parent", i)
		}
	}
}
