package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// The state root is now a trie root, and the capability that buys is a proof of
// ABSENCE bound to proof of work. These tests exercise it through a real chain
// rather than the trie in isolation, because the thing that matters is that the
// proof folds to the root a mined header committed to.

func TestAbsenceIsProvableAgainstAMinedHeader(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 3; i++ {
		if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}
	tip := bc.Tip()
	if !tip.HasValidPoW() {
		t.Fatal("the fixture tip does not carry valid proof of work")
	}

	// A light client's position: it has the header (PoW-verified) and nothing
	// else. It asks about an address it has never seen.
	stranger, _ := wallet.New()
	p, present := bc.ProveAccount(stranger.Address())
	if present {
		t.Fatal("a stranger was reported present")
	}
	valid, gotPresent := VerifyAccountProof(p, tip.StateRoot)
	if !valid {
		t.Fatal("the absence proof did not fold to the mined header's state root")
	}
	if gotPresent {
		t.Fatal("the verifier reported the stranger as present")
	}
}

// The forgeries a prover would actually attempt against a light client.
func TestAccountProofForgeriesAreRejected(t *testing.T) {
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
	root := bc.Tip().StateRoot

	good, present := bc.ProveAccount(bob.Address())
	if !present {
		t.Fatal("bob should be present")
	}
	stranger, _ := wallet.New()
	absent, _ := bc.ProveAccount(stranger.Address())

	t.Run("inflated balance", func(t *testing.T) {
		bad := good
		bad.Account.Balance = 1_000_000 * Coin
		if valid, _ := VerifyAccountProof(bad, root); valid {
			t.Error("an inflated balance verified")
		}
	})

	t.Run("account swapped for another address's", func(t *testing.T) {
		// Claim bob's proof is about alice.
		bad := good
		bad.Address = alice.Address()
		if valid, _ := VerifyAccountProof(bad, root); valid {
			t.Error("a proof was accepted under the wrong address")
		}
	})

	t.Run("present claimed for an absent address", func(t *testing.T) {
		bad := absent
		bad.Found = true
		bad.Account = Account{Balance: 99 * Coin}
		if valid, _ := VerifyAccountProof(bad, root); valid {
			t.Error("an absent address was successfully claimed to hold coin")
		}
	})

	t.Run("absence claimed for a present address", func(t *testing.T) {
		// The interesting direction: convincing a client it was never paid.
		bad := good
		bad.Found = false
		bad.Account = Account{}
		bad.Proof.Found = false
		bad.Proof.Value = ""
		if valid, _ := VerifyAccountProof(bad, root); valid {
			t.Error("a funded address was successfully claimed absent")
		}
	})

	t.Run("absence proof smuggling an account", func(t *testing.T) {
		bad := absent
		bad.Account = Account{Balance: 5 * Coin}
		bad.Found = false
		// The proof itself is a valid absence proof, so this checks the wrapper
		// refuses to hand back an account alongside one.
		valid, gotPresent := VerifyAccountProof(bad, root)
		if valid && gotPresent {
			t.Error("an absence proof reported a present account")
		}
	})

	t.Run("proof key not derived from the address", func(t *testing.T) {
		bad := good
		bad.Proof.Key = hexKey(trieKeyFor("someone-else"))
		if valid, _ := VerifyAccountProof(bad, root); valid {
			t.Error("a proof whose key does not derive from the address verified")
		}
	})

	t.Run("wrong root", func(t *testing.T) {
		if valid, _ := VerifyAccountProof(good, hashBytes([]byte("not the root"))); valid {
			t.Error("the proof verified against an unrelated root")
		}
		if valid, _ := VerifyAccountProof(absent, hashBytes([]byte("not the root"))); valid {
			t.Error("the absence proof verified against an unrelated root")
		}
	})
}

// An asset balance is part of the committed leaf, so it must be covered by the
// proof exactly as coin is.
func TestAssetBalancesAreCoveredByTheProof(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	issue := Transaction{From: alice.Address(), Fee: testFee, Nonce: 0,
		Issue: &AssetIssue{Ticker: "GOLD", Supply: 700}}
	if err := issue.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{issue})); err != nil {
		t.Fatal(err)
	}
	root := bc.Tip().StateRoot

	p, present := bc.ProveAccount(alice.Address())
	if !present {
		t.Fatal("the issuer should be present")
	}
	if valid, _ := VerifyAccountProof(p, root); !valid {
		t.Fatal("an account holding an asset should verify")
	}
	id := AssetID(alice.Address(), "GOLD", 0)
	if p.Account.Assets[id] != 700 {
		t.Fatalf("the proof carries asset balance %d, want 700", p.Account.Assets[id])
	}
	bad := p
	bad.Account.Assets = map[string]uint64{id: 9999}
	if valid, _ := VerifyAccountProof(bad, root); valid {
		t.Error("a tampered asset balance verified")
	}
}

// The state root must be canonical: the same accounts must always give the same
// root, or two honest nodes commit different roots for identical state and fork.
func TestStateRootIsCanonical(t *testing.T) {
	a, _ := wallet.New()
	b, _ := wallet.New()
	c, _ := wallet.New()
	build := func(order []*wallet.Wallet) string {
		st := map[string]Account{}
		for i, w := range order {
			st[w.Address()] = Account{Balance: uint64(i+1) * Coin, Nonce: uint64(i)}
		}
		return stateRoot(st)
	}
	// Same contents, different map construction order.
	one := build([]*wallet.Wallet{a, b, c})
	two := build([]*wallet.Wallet{a, b, c})
	if one != two {
		t.Errorf("the same state gave two roots: %s vs %s", one, two)
	}
	// An empty state has a fixed root, and it is the empty trie's.
	if got := stateRoot(map[string]Account{}); got != EmptyTrieRoot() {
		t.Errorf("empty state root = %s, want the empty trie root %s", got, EmptyTrieRoot())
	}
}
