package main

import (
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// buildChain mines a chain with one signed transfer and returns the headers, a
// proof for that transfer, and its hash — the exact inputs an SPV client fetches.
func buildChain(t *testing.T) ([]core.Header, core.TxProof, string) {
	t.Helper()
	bc := core.NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	sink, _ := wallet.New()

	mine := func(miner string, txs []core.Transaction) {
		tip := bc.Tip()
		var fees uint64
		for _, x := range txs {
			fees += x.Fee
		}
		cb := core.NewCoinbase(miner, core.BlockReward(tip.Index+1)+fees)
		b := core.Block{
			Index: tip.Index + 1, Timestamp: tip.Timestamp + 1,
			Transactions: append([]core.Transaction{cb}, txs...),
			PrevHash:     tip.Hash, Difficulty: bc.NextDifficulty(),
		}
		mined, ok := core.Mine(b, nil)
		if !ok {
			t.Fatal("mine aborted")
		}
		if err := bc.AddBlock(mined); err != nil {
			t.Fatal(err)
		}
	}

	mine(alice.Address(), nil) // block 1: reward to alice
	for i := 0; i < core.CoinbaseMaturity; i++ {
		mine(sink.Address(), nil) // let alice's coinbase mature
	}
	tx := core.Transaction{From: alice.Address(), To: bob.Address(), Amount: 5 * core.Coin, Fee: core.Coin, Nonce: 0}
	if err := tx.Sign(alice); err != nil {
		t.Fatal(err)
	}
	mine(alice.Address(), []core.Transaction{tx}) // block contains tx

	pr, ok := bc.FindTxProof(tx.Hash())
	if !ok {
		t.Fatal("proof not found")
	}
	return bc.Headers(), pr, tx.Hash()
}

func TestVerifySPVProven(t *testing.T) {
	headers, pr, txh := buildChain(t)
	msg, err := verifySPV(headers, pr, txh)
	if err != nil {
		t.Fatalf("expected PROVEN, got error: %v", err)
	}
	if msg == "" {
		t.Fatal("empty proof message")
	}
}

func TestVerifySPVRejectsTamperedHeaders(t *testing.T) {
	headers, pr, txh := buildChain(t)
	headers[1].Nonce ^= 1 // invalidate block 1's proof of work
	if _, err := verifySPV(headers, pr, txh); err == nil {
		t.Fatal("a tampered header chain must be rejected")
	}
}

func TestVerifySPVRejectsForgedProof(t *testing.T) {
	headers, pr, txh := buildChain(t)
	pr.MerkleRoot = "deadbeef" // proof claims a root that doesn't match the header
	if _, err := verifySPV(headers, pr, txh); err == nil {
		t.Fatal("a proof whose root disagrees with the header must be rejected")
	}
}

func TestVerifyHeaderChainWork(t *testing.T) {
	headers, _, _ := buildChain(t)
	tip, work, err := verifyHeaderChain(headers)
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(core.CoinbaseMaturity + 2); tip != want { // b1 + maturity fillers + tx block
		t.Fatalf("tip = %d, want %d", tip, want)
	}
	if work.Sign() <= 0 {
		t.Fatal("cumulative work should be positive")
	}
}
