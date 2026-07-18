package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// TestSnapshotBootstrap builds a chain, takes a snapshot partway up, and confirms
// a fresh chain seeded from that snapshot (a) verifies against the header state
// root, (b) reports the same balances without replaying history, (c) reaches the
// same work after applying only the suffix, and (d) rejects a tampered snapshot.
func TestSnapshotBootstrap(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()

	// Block 1 pays alice; let it mature; alice pays bob; then a few more blocks.
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	tx := signedTx(t, alice, bob.Address(), 5*Coin, testFee, 0)
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err != nil {
		t.Fatal(err)
	}
	sink, _ := wallet.New()
	for i := 0; i < 4; i++ {
		if err := bc.AddBlock(mineOn(t, bc, sink.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}

	snapHeight := bc.Height() - 2
	snap, ok := bc.SnapshotAt(snapHeight)
	if !ok {
		t.Fatal("SnapshotAt failed")
	}
	if err := VerifySnapshot(snap); err != nil {
		t.Fatalf("snapshot should verify against its header state root: %v", err)
	}

	// Seed a fresh chain from the snapshot + the header chain through that height.
	headers := bc.Headers()
	seeded, err := NewFromSnapshot(snap, headers[:snapHeight+1])
	if err != nil {
		t.Fatalf("NewFromSnapshot: %v", err)
	}
	if seeded.Height() != snapHeight {
		t.Fatalf("seeded height = %d, want %d", seeded.Height(), snapHeight)
	}
	// Balances match the full chain's at that height (bob was paid before the snapshot).
	full, _ := bc.SnapshotAt(snapHeight)
	if seeded.Balance(bob.Address()) != full.Accounts[bob.Address()].Balance {
		t.Fatalf("seeded bob balance = %d, want %d", seeded.Balance(bob.Address()), full.Accounts[bob.Address()].Balance)
	}
	if seeded.Balance(bob.Address()) != 5*Coin {
		t.Fatalf("bob balance = %d, want %d", seeded.Balance(bob.Address()), 5*Coin)
	}

	// Apply only the suffix above the snapshot; the seeded chain must reach the
	// same tip hash and cumulative work as the full chain — no replay below it.
	for h := snapHeight + 1; h <= bc.Height(); h++ {
		b, _ := bc.BlockAt(h)
		if err := seeded.AddBlock(b); err != nil {
			t.Fatalf("apply suffix block %d: %v", h, err)
		}
	}
	if seeded.Tip().Hash != bc.Tip().Hash {
		t.Fatal("seeded chain tip does not match the full chain")
	}
	if seeded.Work().Cmp(bc.Work()) != 0 {
		t.Fatalf("seeded work %s != full work %s", seeded.Work(), bc.Work())
	}

	// A tampered snapshot (inflated balance) must fail verification.
	bad := snap
	bad.Accounts = cloneState(snap.Accounts)
	acc := bad.Accounts[bob.Address()]
	acc.Balance += Coin
	bad.Accounts[bob.Address()] = acc
	if err := VerifySnapshot(bad); err == nil {
		t.Fatal("a tampered snapshot must fail verification")
	}
	if _, err := NewFromSnapshot(bad, headers[:snapHeight+1]); err == nil {
		t.Fatal("NewFromSnapshot must reject a tampered snapshot")
	}
}
