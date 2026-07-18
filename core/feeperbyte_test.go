package core

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// TestFeePerByteScalesWithSize checks the per-byte base-fee rule: a larger
// transaction owes proportionally more base fee, so a fee that clears a small
// transaction's requirement can be too low for a larger one occupying more bytes.
func TestFeePerByteScalesWithSize(t *testing.T) {
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

	mkBig := func(fee uint64) Transaction {
		tx := Transaction{From: alice.Address(), To: bob.Address(), Amount: Coin, Fee: fee, Nonce: 0, Memo: strings.Repeat("x", MaxMemoBytes)}
		if err := tx.Sign(alice); err != nil {
			t.Fatal(err)
		}
		return tx
	}
	small := signedTx(t, alice, bob.Address(), Coin, testFee, 0)
	big := mkBig(testFee)

	// A larger transaction owes more base fee at the same rate.
	if BaseFeeFor(big, baseFee) <= BaseFeeFor(small, baseFee) {
		t.Fatal("a larger transaction should owe more base fee than a smaller one")
	}

	// A fee between the two requirements clears the small tx but not the big one.
	fee := (BaseFeeFor(small, baseFee) + BaseFeeFor(big, baseFee)) / 2
	smallPay := signedTx(t, alice, bob.Address(), Coin, fee, 0)
	bigPay := mkBig(fee)

	// The big transaction underpays for its byte size and is rejected. (The failed
	// block leaves state untouched, so alice's nonce 0 is still available below.)
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{bigPay})); err == nil {
		t.Fatal("a large transaction underpaying for its byte size must be rejected")
	}
	// The small transaction paying the same fee clears its smaller requirement.
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{smallPay})); err != nil {
		t.Fatalf("a small transaction clearing its per-byte fee should be accepted: %v", err)
	}
}
