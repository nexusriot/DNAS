package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// TestAssetIssueAndTransfer exercises the native asset layer: issuance credits
// the issuer, transfers move the asset (fee paid in coin), overspending the asset
// is rejected, and asset balances are committed in the state root (provable, and
// tamper-evident).
func TestAssetIssueAndTransfer(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	miner, _ := wallet.New()

	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)

	// Alice issues 1000 GOLD (nonce 0).
	issue := Transaction{From: alice.Address(), Fee: testFee, Nonce: 0, Issue: &AssetIssue{Ticker: "GOLD", Supply: 1000}}
	if err := issue.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{issue})); err != nil {
		t.Fatalf("issue: %v", err)
	}
	id := AssetID(alice.Address(), "GOLD", 0)
	if got := bc.Account(alice.Address()).Assets[id]; got != 1000 {
		t.Fatalf("alice GOLD after issue = %d, want 1000", got)
	}

	// Alice sends 300 GOLD to Bob (nonce 1); the fee is coin.
	xfer := Transaction{From: alice.Address(), To: bob.Address(), Amount: 300, AssetID: id, Fee: testFee, Nonce: 1}
	if err := xfer.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{xfer})); err != nil {
		t.Fatalf("asset transfer: %v", err)
	}
	if got := bc.Account(alice.Address()).Assets[id]; got != 700 {
		t.Fatalf("alice GOLD = %d, want 700", got)
	}
	if got := bc.Account(bob.Address()).Assets[id]; got != 300 {
		t.Fatalf("bob GOLD = %d, want 300", got)
	}

	// Overspending the asset is rejected (Alice has coin for the fee but not 800 GOLD).
	over := Transaction{From: alice.Address(), To: bob.Address(), Amount: 800, AssetID: id, Fee: testFee, Nonce: 2}
	if err := over.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{over})); err == nil {
		t.Fatal("overspending an asset must be rejected")
	}

	// The asset balance is committed in the state root: it proves, and a tampered
	// amount fails the proof.
	p, ok := bc.ProveAccount(alice.Address())
	if !ok {
		t.Fatal("ProveAccount failed")
	}
	tip := bc.Tip()
	if !VerifyAccountProof(p, tip.StateRoot) {
		t.Fatal("an account holding an asset should verify against the state root")
	}
	if p.Account.Assets[id] != 700 {
		t.Fatalf("proof should carry the asset balance, got %d", p.Account.Assets[id])
	}
	bad := p
	bad.Account.Assets = map[string]uint64{id: 999}
	if VerifyAccountProof(bad, tip.StateRoot) {
		t.Fatal("a tampered asset balance must fail the state proof")
	}
}

func TestValidTicker(t *testing.T) {
	for _, ok := range []string{"GOLD", "a", "USD1", "12345678"} {
		if err := validTicker(ok); err != nil {
			t.Errorf("ticker %q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "TOOLONGTICKER", "GO LD", "a|b", "€"} {
		if err := validTicker(bad); err == nil {
			t.Errorf("ticker %q should be rejected", bad)
		}
	}
}
