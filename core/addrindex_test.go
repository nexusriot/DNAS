package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

func TestAddressIndexIsOptional(t *testing.T) {
	bc := NewBlockchain()
	if bc.AddressIndexed() {
		t.Fatal("the address index must be off unless a node asks for it")
	}
	if _, ok := bc.AddressHistory("dnasx", 0, 0); ok {
		t.Fatal("a disabled index reported history")
	}
	bc.EnableAddressIndex()
	if !bc.AddressIndexed() {
		t.Fatal("EnableAddressIndex did not enable it")
	}
	if _, ok := bc.AddressHistory("dnasx", 0, 0); !ok {
		t.Fatal("an enabled index must answer, even for an unknown address")
	}
}

// Enabling the index on an existing chain must produce exactly what incremental
// maintenance would have: the same history either way.
func TestAddressIndexBuildsFromAnExistingChain(t *testing.T) {
	build := func(enableFirst bool) []AddressEntry {
		bc := NewBlockchain()
		if enableFirst {
			bc.EnableAddressIndex()
		}
		miner, _ := wallet.New()
		bob, _ := wallet.New()
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
		matureCoinbase(t, bc)
		for i := uint64(0); i < 3; i++ {
			pay := signedTx(t, miner, bob.Address(), 100, testFee, i)
			mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{pay}))
		}
		if !enableFirst {
			bc.EnableAddressIndex()
		}
		entries, ok := bc.AddressHistory(bob.Address(), 0, 0)
		if !ok {
			t.Fatal("index disabled")
		}
		return entries
	}
	incremental, rebuilt := build(true), build(false)
	if len(incremental) != 3 || len(rebuilt) != 3 {
		t.Fatalf("history lengths: incremental=%d rebuilt=%d, want 3 each", len(incremental), len(rebuilt))
	}
	for i := range incremental {
		if incremental[i].Height != rebuilt[i].Height || incremental[i].Index != rebuilt[i].Index {
			t.Fatalf("entry %d differs: incremental=%+v rebuilt=%+v", i, incremental[i], rebuilt[i])
		}
	}
}

// Both sides of a payment, and a fee sponsor, must be findable by address.
func TestAddressIndexCoversSenderRecipientAndSponsor(t *testing.T) {
	withUpgrade(t, UpgradeFeeSponsor)
	bc := NewBlockchain()
	bc.EnableAddressIndex()
	sponsor, _ := wallet.New()
	sender, _ := wallet.New()
	recipient, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, sponsor.Address(), nil))
	matureCoinbase(t, bc)
	fund := signedTx(t, sponsor, sender.Address(), 5_000, testFee, 0)
	mustAdd(t, bc, mineOn(t, bc, sponsor.Address(), []Transaction{fund}))
	tx := sponsoredTx(t, sender, sponsor, recipient.Address(), 500, testFee, 0)
	mustAdd(t, bc, mineOn(t, bc, sponsor.Address(), []Transaction{tx}))

	for _, addr := range []string{sender.Address(), recipient.Address(), sponsor.Address()} {
		entries, ok := bc.AddressHistory(addr, 0, 0)
		if !ok {
			t.Fatal("index disabled")
		}
		found := false
		for _, e := range entries {
			if e.Hash == tx.Hash() {
				found = true
			}
		}
		if !found {
			t.Fatalf("sponsored transaction missing from the history of %s", addr)
		}
	}
}

func TestAddressIndexPagesAndFilters(t *testing.T) {
	bc := NewBlockchain()
	bc.EnableAddressIndex()
	miner, _ := wallet.New()
	bob, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	matureCoinbase(t, bc)
	for i := uint64(0); i < 5; i++ {
		pay := signedTx(t, miner, bob.Address(), 100, testFee, i)
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{pay}))
	}
	all, _ := bc.AddressHistory(bob.Address(), 0, 0)
	if len(all) != 5 {
		t.Fatalf("history length = %d, want 5", len(all))
	}
	if n, ok := bc.AddressHistoryLen(bob.Address()); !ok || n != 5 {
		t.Fatalf("AddressHistoryLen = %d (ok=%v), want 5", n, ok)
	}
	if page, _ := bc.AddressHistory(bob.Address(), 0, 2); len(page) != 2 {
		t.Fatalf("limited page length = %d, want 2", len(page))
	}
	from := all[3].Height
	rest, _ := bc.AddressHistory(bob.Address(), from, 0)
	if len(rest) != 2 {
		t.Fatalf("history from height %d = %d entries, want 2", from, len(rest))
	}
}

// A reorg must take the discarded blocks' entries with it, or the index would
// keep pointing at transactions the chain no longer contains.
func TestAddressIndexFollowsReorg(t *testing.T) {
	bc := NewBlockchain()
	bc.EnableAddressIndex()
	miner, _ := wallet.New()
	bob, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	matureCoinbase(t, bc)
	forkHeight := bc.Height()

	pay := signedTx(t, miner, bob.Address(), 100, testFee, 0)
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{pay}))
	if entries, _ := bc.AddressHistory(bob.Address(), 0, 0); len(entries) != 1 {
		t.Fatalf("history before the reorg = %d entries, want 1", len(entries))
	}

	// A longer competing branch that never paid bob.
	side := NewBlockchain()
	for _, b := range bc.Blocks()[1 : forkHeight+1] {
		mustAdd(t, side, b)
	}
	var suffix []Block
	for i := 0; i < 2; i++ {
		b := mineOn(t, side, miner.Address(), nil)
		mustAdd(t, side, b)
		suffix = append(suffix, b)
	}
	adopted, _, err := bc.ReorgFrom(forkHeight, suffix)
	if err != nil || !adopted {
		t.Fatalf("reorg not adopted: adopted=%v err=%v", adopted, err)
	}
	entries, _ := bc.AddressHistory(bob.Address(), 0, 0)
	if len(entries) != 0 {
		t.Fatalf("history after the reorg = %d entries, want 0 (the payment was discarded)", len(entries))
	}
}
