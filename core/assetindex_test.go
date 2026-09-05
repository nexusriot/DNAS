package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// An asset id is a hash of (issuer, ticker, nonce) and cannot be unpacked, so
// without a registry a holder sees an opaque id and no way to learn what it is.
func TestAssetRegistryDescribesWhatWasIssued(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	miner, _ := wallet.New()

	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	if len(bc.Assets()) != 0 {
		t.Fatalf("a chain with no issuances lists %d assets", len(bc.Assets()))
	}

	issue := Transaction{From: alice.Address(), Fee: testFee, Nonce: 0,
		Issue: &AssetIssue{Ticker: "GOLD", Supply: 1000}}
	if err := issue.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{issue})); err != nil {
		t.Fatal(err)
	}
	id := AssetID(alice.Address(), "GOLD", 0)

	info, ok := bc.Asset(id)
	if !ok {
		t.Fatal("the issued asset is not in the registry")
	}
	if info.Ticker != "GOLD" || info.Issuer != alice.Address() || info.Supply != 1000 {
		t.Fatalf("registry says %+v", info)
	}
	if info.TxHash != issue.Hash() || info.Height == 0 {
		t.Fatalf("the registry does not point at the issuance: %+v", info)
	}
	if _, ok := bc.Asset("toknotreal"); ok {
		t.Fatal("an asset that was never issued is in the registry")
	}

	// Holders, and the conservation check that makes the supply verifiable rather
	// than merely asserted.
	xfer := Transaction{From: alice.Address(), To: bob.Address(), Amount: 300,
		AssetID: id, Fee: testFee, Nonce: 1}
	if err := xfer.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{xfer})); err != nil {
		t.Fatal(err)
	}
	holders := bc.AssetHolders(id)
	if len(holders) != 2 {
		t.Fatalf("holders = %+v, want two", holders)
	}
	// Largest first, so the list is useful without sorting it again.
	if holders[0].Amount < holders[1].Amount {
		t.Fatalf("holders are not ordered by size: %+v", holders)
	}
	var held uint64
	for _, h := range holders {
		held += h.Amount
	}
	if held != info.Supply {
		t.Fatalf("holders account for %d of a %d supply", held, info.Supply)
	}
}

// A ticker is not an identifier: two issuers may both mint "GOLD", and a lookup
// by ticker must return both rather than pick one.
func TestAssetRegistryTreatsTickersAsNonUnique(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	miner, _ := wallet.New()

	for _, w := range []*wallet.Wallet{alice, bob} {
		if err := bc.AddBlock(mineOn(t, bc, w.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}
	matureCoinbase(t, bc)

	var txs []Transaction
	for _, w := range []*wallet.Wallet{alice, bob} {
		issue := Transaction{From: w.Address(), Fee: testFee, Nonce: 0,
			Issue: &AssetIssue{Ticker: "GOLD", Supply: 500}}
		if err := issue.Sign(w); err != nil {
			t.Fatal(err)
		}
		txs = append(txs, issue)
	}
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), txs)); err != nil {
		t.Fatal(err)
	}

	byTicker := bc.AssetsByTicker("GOLD")
	if len(byTicker) != 2 {
		t.Fatalf("ticker GOLD resolves to %d assets, want 2", len(byTicker))
	}
	if byTicker[0].ID == byTicker[1].ID {
		t.Fatal("two issuances of the same ticker produced the same id")
	}
	if got := bc.AssetsByTicker("SILVER"); len(got) != 0 {
		t.Fatalf("an unissued ticker resolved to %d assets", len(got))
	}
	// The full list is ordered deterministically, not by map iteration.
	first := bc.Assets()
	for i := 0; i < 5; i++ {
		again := bc.Assets()
		for j := range first {
			if again[j].ID != first[j].ID {
				t.Fatal("the asset list order is not stable")
			}
		}
	}
}

// An issuance that gets reorged away did not happen: the balances go with it,
// and an entry left behind would advertise a token that nothing holds.
func TestAssetRegistryFollowsReorgs(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	miner, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	shared := bc.Blocks()

	issue := Transaction{From: alice.Address(), Fee: testFee, Nonce: 0,
		Issue: &AssetIssue{Ticker: "GOLD", Supply: 1000}}
	if err := issue.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{issue})); err != nil {
		t.Fatal(err)
	}
	id := AssetID(alice.Address(), "GOLD", 0)
	if _, ok := bc.Asset(id); !ok {
		t.Fatal("the issuance was not registered")
	}
	if len(bc.AssetHolders(id)) != 1 {
		t.Fatal("the issuer does not hold the asset")
	}

	// A heavier branch off the shared prefix that never carried the issuance.
	y := NewBlockchain()
	for _, b := range shared[1:] {
		if err := y.AddBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := y.AddBlock(mineOn(t, y, miner.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}
	ok, _, err := bc.ReplaceChain(y.Blocks())
	if !ok || err != nil {
		t.Fatalf("reorg: ok=%v err=%v", ok, err)
	}
	if _, ok := bc.Asset(id); ok {
		t.Fatal("an asset whose issuance was reorged away is still registered")
	}
	if len(bc.AssetHolders(id)) != 0 {
		t.Fatal("the reorged-away asset still has holders")
	}
	if len(bc.Assets()) != 0 {
		t.Fatalf("the registry still lists %d assets", len(bc.Assets()))
	}
}
