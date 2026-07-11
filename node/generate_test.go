package node

import (
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// TestGenerateMinesOnDemand checks the regtest primitive: Generate mines exactly
// N blocks immediately to the node wallet, without the idle interval or the
// mining toggle.
func TestGenerateMinesOnDemand(t *testing.T) {
	w, _ := wallet.New()
	n := New(Config{ListenAddr: "127.0.0.1:0", Regtest: true}, core.NewBlockchain(), core.NewMempool(), w)

	start := n.chain.Height()
	hashes, err := n.Generate(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 5 {
		t.Fatalf("mined %d blocks, want 5", len(hashes))
	}
	if got := n.chain.Height(); got != start+5 {
		t.Fatalf("height = %d, want %d", got, start+5)
	}
	// Five coinbases to the wallet (some immature, but all counted in balance).
	if n.chain.Balance(w.Address()) == 0 {
		t.Fatal("miner wallet should hold the coinbase rewards")
	}
}

func TestGenerateNeedsWallet(t *testing.T) {
	n := New(Config{ListenAddr: "127.0.0.1:0", Regtest: true}, core.NewBlockchain(), core.NewMempool(), nil)
	if _, err := n.Generate(1); err == nil {
		t.Fatal("Generate must fail without a wallet to mine to")
	}
}
