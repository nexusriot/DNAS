package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

func TestCoinbaseMaturity(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()

	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	// A freshly-mined coinbase is not yet spendable.
	if s := bc.SpendableBalance(alice.Address()); s != 0 {
		t.Fatalf("fresh coinbase should be unspendable, spendable=%d", s)
	}
	if bc.Balance(alice.Address()) != BlockReward(1) {
		t.Fatal("balance should still reflect the (immature) reward")
	}
	early := signedTx(t, alice, bob.Address(), Coin, testFee, 0)
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{early})); err == nil {
		t.Fatal("spending an immature coinbase must be rejected")
	}

	// After CoinbaseMaturity blocks it matures and can be spent.
	matureCoinbase(t, bc)
	if s := bc.SpendableBalance(alice.Address()); s != BlockReward(1) {
		t.Fatalf("matured coinbase should be fully spendable, spendable=%d", s)
	}
	spend := signedTx(t, alice, bob.Address(), Coin, testFee, 0)
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{spend})); err != nil {
		t.Fatalf("matured coinbase should be spendable: %v", err)
	}
	if bc.Balance(bob.Address()) != Coin {
		t.Fatalf("bob = %d, want %d", bc.Balance(bob.Address()), Coin)
	}
}
