package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

func TestCommonPrefix(t *testing.T) {
	a := buildIndependentChain(t, 3)
	b := buildIndependentChain(t, 3)
	if got := commonPrefix(a, b); got != 1 {
		t.Errorf("independent chains share only genesis, got %d", got)
	}
	if got := commonPrefix(a, a); got != len(a) {
		t.Errorf("identical chains share all %d, got %d", len(a), got)
	}
}

func TestReorgRollsBackState(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	carol, _ := wallet.New()

	// Shared prefix: alice's coinbase, then maturity fillers so it's spendable.
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	shared := bc.Blocks()
	forkHeight := uint64(len(shared) - 1)

	// Our branch X extends the shared prefix with a transfer alice -> bob.
	tx := signedTx(t, alice, bob.Address(), 5*Coin, testFee, 0)
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err != nil {
		t.Fatal(err)
	}
	if bc.Balance(bob.Address()) != 5*Coin {
		t.Fatalf("bob should hold 5 on branch X, got %d", bc.Balance(bob.Address()))
	}

	// Competing branch Y shares the prefix but is heavier (two more blocks) and
	// has no bob transfer.
	y := NewBlockchain()
	for _, b := range shared[1:] {
		if err := y.AddBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := y.AddBlock(mineOn(t, y, carol.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	if err := y.AddBlock(mineOn(t, y, carol.Address(), nil)); err != nil {
		t.Fatal(err)
	}

	ok, err := bc.ReplaceChain(y.Blocks())
	if !ok || err != nil {
		t.Fatalf("reorg to heavier chain: ok=%v err=%v", ok, err)
	}
	if bc.Height() != forkHeight+2 || bc.Tip().Hash != y.Tip().Hash {
		t.Fatal("did not adopt branch Y")
	}
	// The abandoned transfer is rolled back.
	if bc.Balance(bob.Address()) != 0 {
		t.Fatalf("bob's transfer should be rolled back, got %d", bc.Balance(bob.Address()))
	}
	// Alice keeps only her coinbase (the b2x transfer is gone).
	if bc.Balance(alice.Address()) != BlockReward(1) {
		t.Fatalf("alice = %d, want %d", bc.Balance(alice.Address()), BlockReward(1))
	}
	// Carol earned the two coinbases on Y.
	if want := BlockReward(forkHeight+1) + BlockReward(forkHeight+2); bc.Balance(carol.Address()) != want {
		t.Fatalf("carol = %d, want %d", bc.Balance(carol.Address()), want)
	}
}

func TestReorgRejectsInvalidSuffixLeavesChainIntact(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	carol, _ := wallet.New()

	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	shared := bc.Blocks()
	tx := signedTx(t, alice, bob.Address(), 5*Coin, testFee, 0)
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err != nil {
		t.Fatal(err)
	}
	xTip := bc.Tip().Hash
	xHeight := bc.Height()

	// Y: a valid block, then one that over-pays its coinbase (invalid) but claims
	// enough height/work to pass the fork-choice gate.
	y := NewBlockchain()
	for _, b := range shared[1:] {
		if err := y.AddBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := y.AddBlock(mineOn(t, y, carol.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	ytip := y.Tip()
	bad := Block{
		Index:        ytip.Index + 1,
		Timestamp:    ytip.Timestamp + 1,
		Transactions: []Transaction{NewCoinbase(carol.Address(), BlockReward(ytip.Index+1)+Coin)},
		PrevHash:     ytip.Hash,
		BaseFee:      y.NextBaseFee(), // structurally valid so it's rejected for the coinbase overpay
		Bits:         y.NextBits(),
	}
	bad, _ = Mine(bad, nil)
	badChain := append(y.Blocks(), bad)

	ok, err := bc.ReplaceChain(badChain)
	if ok || err == nil {
		t.Fatalf("invalid reorg should be rejected: ok=%v err=%v", ok, err)
	}
	// The live chain and state must be untouched.
	if bc.Tip().Hash != xTip || bc.Height() != xHeight {
		t.Fatal("failed reorg changed the live chain")
	}
	if bc.Balance(bob.Address()) != 5*Coin {
		t.Fatalf("failed reorg corrupted state: bob = %d", bc.Balance(bob.Address()))
	}
}
