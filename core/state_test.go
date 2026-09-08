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
	if valid, present := VerifyAccountProof(p, tip.StateRoot); !valid || !present {
		t.Fatalf("valid proof should verify: valid=%v present=%v", valid, present)
	}
	// A wrong root must not verify.
	if valid, _ := VerifyAccountProof(p, "deadbeef"); valid {
		t.Fatal("proof must not verify against a wrong root")
	}
	// A tampered balance must not verify (the leaf no longer matches).
	bad := p
	bad.Account.Balance = 999 * Coin
	if valid, _ := VerifyAccountProof(bad, tip.StateRoot); valid {
		t.Fatal("a tampered balance must not verify")
	}
	// An absent address is now PROVABLY absent. This is the capability the old
	// sorted-leaf state root could not offer: a prover who omitted a leaf produced
	// a tree indistinguishable from the truth, so "holds nothing" and "I am not
	// showing you this" looked the same. A light client needs to tell them apart
	// to reject a forged "you were never paid".
	stranger, _ := wallet.New()
	absent, present := bc.ProveAccount(stranger.Address())
	if present {
		t.Fatal("a stranger should not be reported as present")
	}
	valid, gotPresent := VerifyAccountProof(absent, tip.StateRoot)
	if !valid {
		t.Fatal("an absence proof must verify against the tip's state root")
	}
	if gotPresent {
		t.Fatal("the verifier reported an absent address as present")
	}
	// And it must not verify against a different root, or it proves nothing about
	// the chain the client actually followed.
	if valid, _ := VerifyAccountProof(absent, "deadbeef"); valid {
		t.Fatal("an absence proof verified against a wrong root")
	}
}
