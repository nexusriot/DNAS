package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

func TestSelectFeeOrderAndNonceOrder(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	if err := bc.AddBlock(mineOn(t, bc, bob.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc) // both coinbases must mature before they're spendable

	mp := NewMempool()
	a0 := signedTx(t, alice, bob.Address(), Coin, 5, 0)
	a1 := signedTx(t, alice, bob.Address(), Coin, 1, 1)
	b0 := signedTx(t, bob, alice.Address(), Coin, 9, 0)
	for _, tx := range []Transaction{a0, a1, b0} {
		if ok, err := mp.Add(tx); !ok || err != nil {
			t.Fatalf("add: ok=%v err=%v", ok, err)
		}
	}

	sel := mp.Select(bc, 10)
	if len(sel) != 3 {
		t.Fatalf("selected %d, want 3", len(sel))
	}
	// The highest-fee ready transaction (bob's, fee 9) is chosen first.
	if sel[0].From != bob.Address() || sel[0].Fee != 9 {
		t.Fatalf("first pick = %+v, want bob fee 9", sel[0])
	}
	// Alice's two transactions must appear in nonce order.
	var aliceNonces []uint64
	for _, tx := range sel {
		if tx.From == alice.Address() {
			aliceNonces = append(aliceNonces, tx.Nonce)
		}
	}
	if len(aliceNonces) != 2 || aliceNonces[0] != 0 || aliceNonces[1] != 1 {
		t.Fatalf("alice nonces out of order: %v", aliceNonces)
	}
}

func TestSelectSkipsUnaffordable(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	mp := NewMempool()
	// More than the whole balance.
	big := signedTx(t, alice, "dnasx", BlockReward(1)+Coin, 0, 0)
	if _, err := mp.Add(big); err != nil {
		t.Fatal(err)
	}
	if sel := mp.Select(bc, 10); len(sel) != 0 {
		t.Fatalf("selected %d, want 0 (unaffordable)", len(sel))
	}
}

func TestSelectSkipsExpired(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	mp := NewMempool()
	// Chain height is 1, so Select targets height 2; expiry 1 is already past.
	tx := Transaction{From: alice.Address(), To: "dnasx", Amount: Coin, Nonce: 0, Expiry: 1}
	if err := tx.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if _, err := mp.Add(tx); err != nil {
		t.Fatal(err)
	}
	if sel := mp.Select(bc, 10); len(sel) != 0 {
		t.Fatalf("selected %d, want 0 (expired)", len(sel))
	}
}

func TestSelectRespectsMax(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	mp := NewMempool()
	for n := uint64(0); n < 3; n++ {
		if _, err := mp.Add(signedTx(t, alice, "dnasx", Coin, 1, n)); err != nil {
			t.Fatal(err)
		}
	}
	if sel := mp.Select(bc, 2); len(sel) != 2 {
		t.Fatalf("selected %d, want 2 (max)", len(sel))
	}
}
