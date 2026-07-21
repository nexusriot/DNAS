package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// TestUpgradeActivation checks the height-based activation predicate.
func TestUpgradeActivation(t *testing.T) {
	defer ClearUpgrades()
	if IsUpgradeActive("nope", 1_000_000) {
		t.Fatal("an unscheduled upgrade must never be active")
	}
	SetUpgradeHeight("x", 10)
	if IsUpgradeActive("x", 9) {
		t.Fatal("upgrade must be inactive below its activation height")
	}
	if !IsUpgradeActive("x", 10) || !IsUpgradeActive("x", 11) {
		t.Fatal("upgrade must be active at and after its activation height")
	}
	ClearUpgrades()
	if IsUpgradeActive("x", 10) {
		t.Fatal("cleared upgrades must be inactive")
	}
}

// TestDustLimitUpgradeGated exercises a real height-activated consensus rule: a
// sub-threshold transfer is valid before the upgrade and invalid at/after it.
func TestDustLimitUpgradeGated(t *testing.T) {
	defer ClearUpgrades()
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	miner, _ := wallet.New()

	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)

	// Before the upgrade: a dust (sub-threshold) transfer is accepted.
	dust := signedTx(t, alice, bob.Address(), DustThreshold-1, testFee, 0)
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{dust})); err != nil {
		t.Fatalf("dust should be allowed before the upgrade: %v", err)
	}

	// Schedule the upgrade at the next height; the same dust amount is now rejected.
	SetUpgradeHeight(UpgradeDustLimit, bc.Height()+1)
	dust2 := signedTx(t, alice, bob.Address(), DustThreshold-1, testFee, 1)
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{dust2})); err == nil {
		t.Fatal("dust must be rejected once the upgrade is active")
	}

	// A transfer at the threshold is still accepted (nonce 1 is free — the dust2
	// block was rejected, so it never consumed it).
	ok := signedTx(t, alice, bob.Address(), DustThreshold, testFee, 1)
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{ok})); err != nil {
		t.Fatalf("at-threshold transfer should be accepted under the upgrade: %v", err)
	}
}
