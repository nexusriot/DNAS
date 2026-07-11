package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// TestStateRootDeterministicAndCommitted checks that identical chains produce
// identical state roots, and that the state root is part of the block hash (so a
// light client can trust it once the header's PoW is verified).
func TestStateRootDeterministicAndCommitted(t *testing.T) {
	build := func() Block {
		bc := NewBlockchain()
		for i := 0; i < 3; i++ {
			if err := bc.AddBlock(mineOn(t, bc, "dnasminer", nil)); err != nil {
				t.Fatal(err)
			}
		}
		return bc.Tip()
	}
	a, b := build(), build()
	if a.StateRoot == "" {
		t.Fatal("state root should be set on mined blocks")
	}
	if a.StateRoot != b.StateRoot {
		t.Fatal("identical chains produced different state roots")
	}
	// The state root is committed in the PoW hash: changing it changes the hash.
	tampered := a
	tampered.StateRoot = "0000000000000000000000000000000000000000000000000000000000000000"
	if tampered.ComputeHash() == a.Hash {
		t.Fatal("state root is not committed in the block hash")
	}
}

// TestAccountProofVerifies is the light-client balance-proof round trip.
func TestAccountProofVerifies(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	tx := signedTx(t, alice, bob.Address(), 5*Coin, Coin, 0)
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err != nil {
		t.Fatal(err)
	}

	tip := bc.Tip()
	p, ok := bc.ProveAccount(bob.Address())
	if !ok {
		t.Fatal("bob should have a provable account")
	}
	if p.Account.Balance != 5*Coin {
		t.Fatalf("proven balance = %d, want %d", p.Account.Balance, 5*Coin)
	}
	if !VerifyAccountProof(p, tip.StateRoot) {
		t.Fatal("valid proof should verify against the tip's state root")
	}
	// A wrong root must not verify.
	if VerifyAccountProof(p, "deadbeef") {
		t.Fatal("proof must not verify against a wrong root")
	}
	// A tampered balance must not verify (the leaf no longer matches).
	bad := p
	bad.Account.Balance = 999 * Coin
	if VerifyAccountProof(bad, tip.StateRoot) {
		t.Fatal("a tampered balance must not verify")
	}
	// An absent address is not provable (membership tree, no non-membership proof).
	stranger, _ := wallet.New()
	if _, ok := bc.ProveAccount(stranger.Address()); ok {
		t.Fatal("an address with no account must be unprovable (found=false)")
	}
}
