package core

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// coinbaseOnly is the transaction list of a block that carries no payments: the
// conservation rule skips index 0, so its contents never matter to these tests.
var coinbaseOnly = []Transaction{NewCoinbase("miner", 0)}

// The rule exists to catch coin appearing from nowhere. Applying a block that
// credited an account more than its subsidy must be refused, not merely reported
// as an inconsistency at the tip.
func TestConservationRejectsInflation(t *testing.T) {
	state := map[string]Account{"a": {Balance: 100}}
	undo := []undoEntry{{addr: "a"}}

	if err := checkConservation(state, undo, coinbaseOnly, 100, 0); err != nil {
		t.Fatalf("an honest block was rejected: %v", err)
	}
	err := checkConservation(state, undo, coinbaseOnly, 50, 0)
	if err == nil {
		t.Fatal("a block that created 100 coin from a 50 subsidy was accepted")
	}
	if !strings.Contains(err.Error(), "supply not conserved") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

// The mirror case: coin quietly vanishing. A block that destroys more than its
// transactions burned is as wrong as one that mints, and neither shows up in any
// per-transaction check.
func TestConservationRejectsUnaccountedBurn(t *testing.T) {
	state := map[string]Account{"a": {Balance: 10}}
	undo := []undoEntry{{addr: "a", prev: Account{Balance: 100}, existed: true}}

	// Lost 90: legal only if the subsidy was 0 and the block burned exactly 90.
	if err := checkConservation(state, undo, coinbaseOnly, 0, 90); err != nil {
		t.Fatalf("an honest burn was rejected: %v", err)
	}
	if err := checkConservation(state, undo, coinbaseOnly, 0, 40); err == nil {
		t.Fatal("a block that destroyed 90 coin while burning 40 was accepted")
	}
}

// An account written more than once in a block (a sender that is also the miner,
// say) appears once per mutation in the undo log. Only the FIRST entry predates
// the block, so reading the last one would compare the block's own intermediate
// state against its final state and mis-measure the delta.
func TestConservationUsesPreBlockValuePerAccount(t *testing.T) {
	state := map[string]Account{"a": {Balance: 50}}
	undo := []undoEntry{
		{addr: "a"}, // created by the block: 0 before
		{addr: "a", prev: Account{Balance: 30}, existed: true}, // touched again mid-block
	}
	if err := checkConservation(state, undo, coinbaseOnly, 50, 0); err != nil {
		t.Fatalf("delta measured from the wrong undo entry: %v", err)
	}
}

// Assets are conserved too, and more strictly: only an issuance may change a
// total. An account's asset balance growing with no issuance in the block is an
// asset minted out of nothing.
func TestConservationRejectsAssetMintedWithoutIssuance(t *testing.T) {
	state := map[string]Account{"a": {Assets: map[string]uint64{"tokdead": 5}}}
	undo := []undoEntry{{addr: "a"}}

	err := checkConservation(state, undo, coinbaseOnly, 0, 0)
	if err == nil {
		t.Fatal("5 units of an asset appeared with no issuance and the block was accepted")
	}
	if !strings.Contains(err.Error(), "no issuance") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

// ...and the issuance it does carry must account for the exact amount. A block
// that mints 1000 while its issuance says 10 is refused.
func TestConservationChecksIssuedAmount(t *testing.T) {
	issue := Transaction{From: "issuer", Nonce: 7, Issue: &AssetIssue{Ticker: "GOLD", Supply: 10}}
	id := AssetID("issuer", "GOLD", 7)
	txs := []Transaction{NewCoinbase("miner", 0), issue}

	good := map[string]Account{"issuer": {Assets: map[string]uint64{id: 10}}}
	undo := []undoEntry{{addr: "issuer"}}
	if err := checkConservation(good, undo, txs, 0, 0); err != nil {
		t.Fatalf("a correct issuance was rejected: %v", err)
	}

	bad := map[string]Account{"issuer": {Assets: map[string]uint64{id: 1000}}}
	if err := checkConservation(bad, undo, txs, 0, 0); err == nil {
		t.Fatal("an issuance that credited 1000 for a stated supply of 10 was accepted")
	}
}

// An asset transfer nets to zero across the accounts it touches, so it must pass
// with no issuance present at all.
func TestConservationAllowsAssetTransfer(t *testing.T) {
	state := map[string]Account{
		"a": {Assets: map[string]uint64{"tokbeef": 700}},
		"b": {Assets: map[string]uint64{"tokbeef": 300}},
	}
	undo := []undoEntry{
		{addr: "a", prev: Account{Assets: map[string]uint64{"tokbeef": 1000}}, existed: true},
		{addr: "b"},
	}
	if err := checkConservation(state, undo, coinbaseOnly, 0, 0); err != nil {
		t.Fatalf("a plain asset transfer was rejected: %v", err)
	}
}

// The end-to-end statement: a real chain carrying fee-paying payments, an asset
// issuance and an asset transfer applies cleanly with the rule live, and the
// chain-wide identity supply.go reports agrees with it at every height. The rule
// would reject the block rather than let Supply report an inconsistency, so this
// asserts both and would fail on either.
func TestConservationHoldsOverRealChain(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	miner, _ := wallet.New()

	mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	matureCoinbase(t, bc)

	pay := signedTx(t, alice, bob.Address(), Coin, testFee, 0)
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{pay}))

	issue := Transaction{From: alice.Address(), Fee: testFee, Nonce: 1, Issue: &AssetIssue{Ticker: "GOLD", Supply: 1000}}
	if err := issue.Sign(alice); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{issue}))

	id := AssetID(alice.Address(), "GOLD", 1)
	xfer := Transaction{From: alice.Address(), To: bob.Address(), Amount: 300, AssetID: id, Fee: testFee, Nonce: 2}
	if err := xfer.Sign(alice); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{xfer}))

	// The miner is also a sender in the next block, which is the double-touch case
	// TestConservationUsesPreBlockValuePerAccount covers synthetically.
	matureCoinbase(t, bc)
	self := signedTx(t, miner, alice.Address(), Coin/2, testFee, 0)
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{self}))

	if s := bc.Supply(); !s.Consistent {
		t.Fatalf("minted %d − burned %d != circulating %d", s.Minted, s.Burned, s.Circulating)
	}
}
