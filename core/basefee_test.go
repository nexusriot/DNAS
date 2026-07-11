package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// parentWith builds a block with the given base fee and non-coinbase tx count,
// for driving expectedBaseFee directly (like difficulty_test does for retarget).
func parentWith(baseFee uint64, ntx int) Block {
	txs := make([]Transaction, ntx+1)
	txs[0] = NewCoinbase("dnasminer", 0)
	for i := 1; i <= ntx; i++ {
		txs[i] = Transaction{From: "a", To: "b", Nonce: uint64(i)}
	}
	return Block{BaseFee: baseFee, Transactions: txs}
}

func TestExpectedBaseFee(t *testing.T) {
	target := BaseFeeTargetTxs

	if got := expectedBaseFee(nil, 0); got != InitialBaseFee {
		t.Errorf("genesis base fee = %d, want %d", got, InitialBaseFee)
	}
	// Exactly at target: unchanged.
	if got := expectedBaseFee([]Block{parentWith(1000, target)}, 1); got != 1000 {
		t.Errorf("at-target base fee = %d, want 1000 (unchanged)", got)
	}
	// A full block (2× target) raises it by the max 1/8.
	if got := expectedBaseFee([]Block{parentWith(1000, 2*target)}, 1); got != 1125 {
		t.Errorf("over-target base fee = %d, want 1125 (+12.5%%)", got)
	}
	// An empty block lowers it.
	if got := expectedBaseFee([]Block{parentWith(1000, 0)}, 1); got != 875 {
		t.Errorf("under-target base fee = %d, want 875 (-12.5%%)", got)
	}
	// It never falls below the floor.
	if got := expectedBaseFee([]Block{parentWith(MinBaseFee, 0)}, 1); got != MinBaseFee {
		t.Errorf("base fee = %d, want the floor %d", got, MinBaseFee)
	}
}

// TestBaseFeeBurnedAndEnforced checks the consensus behaviour: a transaction must
// pay at least the base fee, the base-fee portion is burned, and the miner keeps
// only the tip.
func TestBaseFeeBurnedAndEnforced(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	miner, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	baseFee := bc.NextBaseFee()
	if baseFee == 0 {
		t.Fatal("base fee should be non-zero on a devnet")
	}

	// A transaction paying below the base fee cannot be mined.
	low := signedTx(t, alice, bob.Address(), Coin, baseFee-1, 0)
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{low})); err == nil {
		t.Fatal("a transaction paying below the base fee must be rejected")
	}

	// One paying base fee + a tip: the miner gets reward + tip; the base fee burns.
	tip := 7 * Coin
	good := signedTx(t, alice, bob.Address(), Coin, baseFee+tip, 0)
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{good})); err != nil {
		t.Fatalf("a transaction paying at/above the base fee should be accepted: %v", err)
	}
	h := bc.Height()
	if got, want := bc.Balance(miner.Address()), BlockReward(h)+uint64(tip); got != want {
		t.Fatalf("miner = %d, want reward+tip %d (base fee %d burned, not paid to miner)", got, want, baseFee)
	}
	if bc.Balance(bob.Address()) != Coin {
		t.Fatalf("bob = %d, want %d", bc.Balance(bob.Address()), Coin)
	}
}
