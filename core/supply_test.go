package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

func TestCumulativeSubsidy(t *testing.T) {
	if got := CumulativeSubsidy(0); got != 0 {
		t.Errorf("genesis minted %d, want 0", got)
	}
	if got := CumulativeSubsidy(1); got != InitialBlockReward {
		t.Errorf("CumulativeSubsidy(1) = %d, want %d", got, InitialBlockReward)
	}

	// The closed form walks halving epochs; check it against the definition it is
	// meant to be a shortcut for, across two halving boundaries.
	var running uint64
	for h := uint64(1); h <= 2*HalvingInterval+2; h++ {
		running += BlockReward(h)
		if got := CumulativeSubsidy(h); got != running {
			t.Fatalf("CumulativeSubsidy(%d) = %d, want %d (summed block by block)", h, got, running)
		}
	}

	// Past the last halving the subsidy is zero, so the total stops growing.
	tail := CumulativeSubsidy(64 * HalvingInterval)
	if CumulativeSubsidy(65*HalvingInterval) != tail {
		t.Error("supply kept growing after the subsidy halved away to nothing")
	}
	// And it never exceeds the asymptotic cap of 2 * interval * initial reward.
	if limit := 2 * HalvingInterval * InitialBlockReward; tail > limit {
		t.Errorf("total issuance %d exceeds the cap %d", tail, limit)
	}
}

// The whole point of tracking minted and burned separately is that they can be
// checked against the balances actually held. A chain with real fee-paying
// traffic must satisfy minted − burned == circulating at every height.
func TestSupplyConservation(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()

	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)

	for i := uint64(0); i < 4; i++ {
		tx := signedTx(t, alice, bob.Address(), Coin, testFee, i)
		if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err != nil {
			t.Fatal(err)
		}
		s := bc.Supply()
		if !s.Consistent {
			t.Fatalf("height %d: minted %d − burned %d != circulating %d",
				s.Height, s.Minted, s.Burned, s.Circulating)
		}
		if s.Minted != CumulativeSubsidy(bc.Height()) {
			t.Fatalf("minted %d, want %d", s.Minted, CumulativeSubsidy(bc.Height()))
		}
	}

	s := bc.Supply()
	if s.Burned == 0 {
		t.Fatal("four fee-paying transactions burned nothing")
	}
	if s.Circulating >= s.Minted {
		t.Fatal("burning fees did not reduce circulating supply below what was minted")
	}
	if s.Accounts != 3 { // alice, bob, and the maturity sink matureCoinbase mines to
		t.Fatalf("expected 3 accounts holding coin, got %d", s.Accounts)
	}
	if s.Subsidy != BlockReward(bc.Height()+1) {
		t.Fatalf("next subsidy %d, want %d", s.Subsidy, BlockReward(bc.Height()+1))
	}
}

// Burn is accumulated per connected block, so a reorg has to un-burn the losing
// branch or the running total would drift away from the accounts every fork.
func TestBurnedFollowsReorg(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	carol, _ := wallet.New()

	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	shared := bc.Blocks()
	sharedBurn := bc.Burned()

	// Branch X burns the base fee of one transaction.
	tx := signedTx(t, alice, bob.Address(), Coin, testFee, 0)
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err != nil {
		t.Fatal(err)
	}
	if bc.Burned() <= sharedBurn {
		t.Fatal("a fee-paying block burned nothing")
	}

	// Branch Y is heavier and carries no transactions at all.
	y := NewBlockchain()
	for _, b := range shared[1:] {
		if err := y.AddBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := y.AddBlock(mineOn(t, y, carol.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}
	if adopted, _, err := bc.ReplaceChain(y.Blocks()); !adopted || err != nil {
		t.Fatalf("reorg: adopted=%v err=%v", adopted, err)
	}

	if bc.Burned() != sharedBurn {
		t.Fatalf("burn total %d after reorg, want the shared prefix's %d", bc.Burned(), sharedBurn)
	}
	if s := bc.Supply(); !s.Consistent {
		t.Fatalf("supply inconsistent after reorg: minted %d − burned %d != circulating %d",
			s.Minted, s.Burned, s.Circulating)
	}
}

// A chain reopened from its store must recover the same burn total, since the
// value is rebuilt by replaying blocks rather than persisted alongside them.
func TestBurnedSurvivesReopen(t *testing.T) {
	path := t.TempDir() + "/chain.db"
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	tx := signedTx(t, alice, bob.Address(), Coin, testFee, 0)
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err != nil {
		t.Fatal(err)
	}
	want := bc.Supply()
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got := reopened.Supply()
	if got.Burned != want.Burned || got.Minted != want.Minted || got.Circulating != want.Circulating {
		t.Fatalf("supply after reopen %+v, want %+v", got, want)
	}
	if !got.Consistent {
		t.Fatal("supply inconsistent after reopen")
	}
	if !reopened.HasTx(tx.Hash()) {
		t.Fatal("transaction index not rebuilt on reopen")
	}
}

// A fast-synced node prunes the bodies it would need to sum the burn, so it
// derives the total from the snapshot's verified state instead. That derived
// value must satisfy the same conservation identity.
func TestSupplyAfterSnapshotBootstrap(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	tx := signedTx(t, alice, bob.Address(), Coin, testFee, 0)
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err != nil {
		t.Fatal(err)
	}

	snap, ok := bc.SnapshotAt(bc.Height())
	if !ok {
		t.Fatal("no snapshot at the tip")
	}
	fresh, err := NewFromSnapshot(snap, bc.Headers())
	if err != nil {
		t.Fatal(err)
	}
	got, want := fresh.Supply(), bc.Supply()
	if !got.Consistent {
		t.Fatal("bootstrapped supply fails the conservation check")
	}
	if got.Burned != want.Burned {
		t.Fatalf("bootstrapped burn %d, want %d", got.Burned, want.Burned)
	}
}
