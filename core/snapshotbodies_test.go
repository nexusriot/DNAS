package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// A chain bootstrapped from a snapshot must validate the blocks above it under
// the SAME rules a full node does. Two rules read the bodies of the blocks just
// below the next one — the coinbase-maturity reserve (the last CoinbaseMaturity
// coinbases) and the base fee (the parent's fullness) — and a snapshot used to
// turn every block at or below its height into a body-less placeholder, so
// both silently read "nothing there": an immature coinbase counted as mature,
// and the parent counted as empty.

// snapshotChainAt builds a booted chain from bc's snapshot at height `at`,
// handing it the recent bodies the way fastsync and a pruned reopen do.
func snapshotChainAt(t *testing.T, bc *Blockchain, at uint64, withBodies bool) *Blockchain {
	t.Helper()
	snap, ok := bc.SnapshotAt(at)
	if !ok {
		t.Fatalf("no snapshot at %d", at)
	}
	headers := make([]Header, at+1)
	for i := range headers {
		headers[i] = bc.blocks[i].Header()
	}
	var recent []Block
	if withBodies {
		recent = bc.blocks[SnapshotBodiesFrom(at) : at+1]
	}
	booted, err := NewFromSnapshotWithBodies(snap, headers, recent)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return booted
}

func TestSnapshotChainEnforcesCoinbaseMaturityAcrossTheSnapshot(t *testing.T) {
	bc := NewBlockchain()
	sink, _ := wallet.New()
	miner, _ := wallet.New()
	bob, _ := wallet.New()
	for i := 0; i < 4; i++ {
		mustAdd(t, bc, mineOn(t, bc, sink.Address(), nil))
	}
	// The snapshot block's coinbase is the miner's only coin, and it is immature
	// for the very next block.
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	at := bc.Height()

	booted := snapshotChainAt(t, bc, at, true)
	spend := signedTx(t, miner, bob.Address(), Coin, bc.NextBaseFee()*2000, 0)
	b := mineOn(t, booted, sink.Address(), []Transaction{spend})
	if err := bc.AddBlock(b); err == nil {
		t.Fatal("precondition: a full node must refuse a spend of an immature coinbase")
	}
	if err := booted.AddBlock(b); err == nil {
		t.Fatal("a snapshot-bootstrapped chain accepted a spend of an immature coinbase the full chain refuses")
	}
}

func TestSnapshotChainDerivesTheSameBaseFee(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	matureCoinbase(t, bc)
	// The snapshot block carries transactions, so its fullness is not "empty".
	var txs []Transaction
	for n := uint64(0); n < 3; n++ {
		txs = append(txs, signedTx(t, alice, bob.Address(), Coin, bc.NextBaseFee()*2000, n))
	}
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), txs))
	at := bc.Height()

	booted := snapshotChainAt(t, bc, at, true)
	// Compare the parent-fullness reading directly: with a small base fee the
	// integer rounding can hide the difference, which is exactly why this went
	// unnoticed — so compare against a high base fee too.
	for _, base := range []uint64{bc.blocks[at].BaseFee, 1_000_000} {
		full := append([]Block(nil), bc.blocks[:at+1]...)
		full[at].BaseFee = base
		boot := append([]Block(nil), booted.blocks[:at+1]...)
		boot[at].BaseFee = base
		if got, want := expectedBaseFee(boot, at+1), expectedBaseFee(full, at+1); got != want {
			t.Fatalf("base %d: booted chain derives next base fee %d, full chain %d", base, got, want)
		}
	}
	if got, want := booted.NextBaseFee(), bc.NextBaseFee(); got != want {
		t.Fatalf("next base fee: booted %d, full %d", got, want)
	}
}

func TestSnapshotBodiesAreCheckedAgainstTheHeaders(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 6; i++ {
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	}
	at := bc.Height()
	snap, _ := bc.SnapshotAt(at)
	headers := make([]Header, at+1)
	for i := range headers {
		headers[i] = bc.blocks[i].Header()
	}
	from := SnapshotBodiesFrom(at)
	recent := append([]Block(nil), bc.blocks[from:at+1]...)

	// A body whose transactions do not match the header's merkle root.
	tampered := append([]Block(nil), recent...)
	other, _ := wallet.New()
	tampered[len(tampered)-1].Transactions = []Transaction{NewCoinbase(other.Address(), BlockReward(at))}
	if _, err := NewFromSnapshotWithBodies(snap, headers, tampered); err == nil {
		t.Fatal("a recent body whose transactions do not match its header was accepted")
	}
	// Bodies that are not the contiguous range ending at the snapshot.
	if _, err := NewFromSnapshotWithBodies(snap, headers, recent[:len(recent)-1]); err == nil {
		t.Fatal("recent bodies that stop short of the snapshot height were accepted")
	}
	// The real ones are fine, and installed as bodies rather than placeholders.
	booted, err := NewFromSnapshotWithBodies(snap, headers, recent)
	if err != nil {
		t.Fatal(err)
	}
	for h := from; h <= at; h++ {
		if booted.blocks[h].IsPlaceholder() {
			t.Fatalf("height %d is still a placeholder", h)
		}
	}
}
