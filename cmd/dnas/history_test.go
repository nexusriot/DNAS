package main

import (
	"testing"

	"github.com/nexusriot/DNAS/core"
)

func TestWalletHistory(t *testing.T) {
	alice, bob, miner := "dnasalice", "dnasbob", "dnasminer"
	blocks := []core.Block{
		{Index: 1, Transactions: []core.Transaction{core.NewCoinbase(alice, 50*core.Coin)}},
		{Index: 2, Transactions: []core.Transaction{
			core.NewCoinbase(miner, core.BlockReward(2)),
			{From: alice, To: bob, Amount: 5 * core.Coin, Fee: core.Coin},
		}},
		{Index: 3, Transactions: []core.Transaction{
			core.NewCoinbase(miner, core.BlockReward(3)),
			{From: bob, To: alice, Amount: 2 * core.Coin, Fee: core.Coin},
		}},
	}
	entries, received, sent, fees := walletHistory(alice, blocks, 3)

	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3 (mined, sent, received)", len(entries))
	}
	if entries[0].Kind != "mined" || entries[1].Kind != "sent" || entries[2].Kind != "received" {
		t.Fatalf("entry kinds = %s/%s/%s", entries[0].Kind, entries[1].Kind, entries[2].Kind)
	}
	if received != 52*core.Coin { // 50 mined + 2 received
		t.Errorf("received = %d, want 52", received/core.Coin)
	}
	if sent != 5*core.Coin {
		t.Errorf("sent = %d, want 5", sent/core.Coin)
	}
	if fees != core.Coin {
		t.Errorf("fees = %d, want 1", fees/core.Coin)
	}
	// Block 1 with tip height 3 has 3 confirmations.
	if entries[0].Confirmations != 3 {
		t.Errorf("block 1 confirmations = %d, want 3", entries[0].Confirmations)
	}
	// Net = 52 − 5 − 1 = 46 (equals the address's on-chain balance).
	if net := int64(received) - int64(sent) - int64(fees); net != int64(46*core.Coin) {
		t.Errorf("net = %d, want 46 DNAS", net/int64(core.Coin))
	}
}
