package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

func TestLocatorStartsAtTipEndsAtGenesis(t *testing.T) {
	bc := NewBlockchain()
	w, _ := wallet.New()
	for i := 0; i < 5; i++ {
		if err := bc.AddBlock(mineOn(t, bc, w.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}
	loc := bc.Locator()
	if loc[0] != bc.Tip().Hash {
		t.Fatal("locator should start at the tip")
	}
	if loc[len(loc)-1] != GenesisBlock().Hash {
		t.Fatal("locator should end at genesis")
	}
	// A peer with the identical chain identifies the tip as the fork point.
	if h := bc.LocatorFork(loc); h != bc.Height() {
		t.Fatalf("fork point = %d, want %d", h, bc.Height())
	}
}

func TestLocatorFindsCommonAncestor(t *testing.T) {
	bc := NewBlockchain()
	w, _ := wallet.New()
	b1 := mineOn(t, bc, w.Address(), nil)
	if err := bc.AddBlock(b1); err != nil {
		t.Fatal(err)
	}
	b2 := mineOn(t, bc, w.Address(), nil)
	if err := bc.AddBlock(b2); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // bc continues to height 5 on its own branch
		if err := bc.AddBlock(mineOn(t, bc, w.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}

	// A divergent chain sharing only genesis+b1+b2.
	other := NewBlockchain()
	o, _ := wallet.New()
	other.AddBlock(b1)
	other.AddBlock(b2)
	other.AddBlock(mineOn(t, other, o.Address(), nil))
	other.AddBlock(mineOn(t, other, o.Address(), nil))

	if h := bc.LocatorFork(other.Locator()); h != 2 {
		t.Fatalf("common ancestor height = %d, want 2 (b2)", h)
	}
}

func TestReorgFromSuffix(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	carol, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	shared := bc.Blocks()
	forkHeight := uint64(len(shared) - 1)
	tx := signedTx(t, alice, bob.Address(), 5*Coin, testFee, 0)
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err != nil { // branch X
		t.Fatal(err)
	}

	// Heavier fork built on the shared prefix.
	y := NewBlockchain()
	for _, b := range shared[1:] {
		if err := y.AddBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	y.AddBlock(mineOn(t, y, carol.Address(), nil))
	y.AddBlock(mineOn(t, y, carol.Address(), nil))
	suffix := y.Blocks()[forkHeight+1:] // the two blocks past the fork point

	adopted, err := bc.ReorgFrom(forkHeight, suffix)
	if !adopted || err != nil {
		t.Fatalf("reorg from suffix: adopted=%v err=%v", adopted, err)
	}
	if bc.Height() != forkHeight+2 || bc.Tip().Hash != y.Tip().Hash {
		t.Fatal("did not adopt the fork")
	}
	if bc.Balance(bob.Address()) != 0 {
		t.Fatalf("bob's transfer should be rolled back, got %d", bc.Balance(bob.Address()))
	}
	if want := BlockReward(forkHeight+1) + BlockReward(forkHeight+2); bc.Balance(carol.Address()) != want {
		t.Fatalf("carol = %d, want %d", bc.Balance(carol.Address()), want)
	}
}

func TestReorgFromLosingSuffixRejected(t *testing.T) {
	bc := NewBlockchain()
	a, _ := wallet.New()
	c, _ := wallet.New()
	b1 := mineOn(t, bc, a.Address(), nil)
	if err := bc.AddBlock(b1); err != nil {
		t.Fatal(err)
	}
	if err := bc.AddBlock(mineOn(t, bc, a.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	if err := bc.AddBlock(mineOn(t, bc, a.Address(), nil)); err != nil { // our chain: height 3
		t.Fatal(err)
	}

	y := NewBlockchain()
	y.AddBlock(b1)
	y.AddBlock(mineOn(t, y, c.Address(), nil)) // only reaches height 2
	suffix := y.Blocks()[2:]

	adopted, err := bc.ReorgFrom(1, suffix)
	if adopted || err != nil {
		t.Fatalf("a lighter fork must not be adopted: adopted=%v err=%v", adopted, err)
	}
	if bc.Height() != 3 {
		t.Fatal("the live chain must be unchanged")
	}
}
