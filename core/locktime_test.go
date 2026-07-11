package core

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

func TestLockUntilEnforced(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}

	// Locked until height 5: invalid at height 2.
	tx := Transaction{From: alice.Address(), To: bob.Address(), Amount: Coin, Fee: testFee, Nonce: 0, LockUntil: 5}
	if err := tx.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err == nil {
		t.Fatal("a time-locked tx should be rejected before its lock height")
	}

	// Advance to height 4, then the next block is height 5 == LockUntil: valid.
	for bc.Height() < 4 {
		if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err != nil {
		t.Fatalf("time-locked tx should be valid at its lock height: %v", err)
	}
	if bc.Balance(bob.Address()) != Coin {
		t.Fatalf("bob should have received the coin, got %d", bc.Balance(bob.Address()))
	}
}

func TestMemoBounded(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	big := Transaction{From: alice.Address(), To: bob.Address(), Amount: Coin, Fee: testFee, Nonce: 0, Memo: strings.Repeat("x", MaxMemoBytes+1)}
	if err := big.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{big})); err == nil {
		t.Fatal("an oversized memo should be rejected")
	}
	ok := Transaction{From: alice.Address(), To: bob.Address(), Amount: Coin, Fee: testFee, Nonce: 0, Memo: "gm"}
	if err := ok.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{ok})); err != nil {
		t.Fatalf("a bounded memo should be accepted: %v", err)
	}
}

func TestSelectSkipsLocked(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	mp := NewMempool()
	tx := Transaction{From: alice.Address(), To: "dnasx", Amount: Coin, Nonce: 0, LockUntil: 100}
	if err := tx.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if _, err := mp.Add(tx); err != nil {
		t.Fatal(err)
	}
	if sel := mp.Select(bc, 10); len(sel) != 0 {
		t.Fatalf("a not-yet-unlocked tx must not be selected, got %d", len(sel))
	}
}
