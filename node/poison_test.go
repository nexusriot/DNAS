package node

import (
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// minerNode returns a mining-capable node with a funded, matured wallet.
func minerNode(t *testing.T) (*Node, *wallet.Wallet, *core.Mempool) {
	t.Helper()
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	bc := core.NewBlockchain()
	mp := core.NewMempoolWithPolicy(100, core.DefaultMinRelayFee)
	n := New(Config{ListenAddr: "127.0.0.1:0", EmptyBlockInterval: time.Millisecond}, bc, mp, w)
	if _, err := n.Generate(core.CoinbaseMaturity + 2); err != nil {
		t.Fatalf("funding the miner: %v", err)
	}
	return n, w, mp
}

// A transaction that consensus will never accept must not reach the mempool. If
// it did, the miner would select it into every candidate block, every candidate
// would be rejected, and the chain would stop advancing while the miner spun.
func TestUnmineableTxCannotStallBlockProduction(t *testing.T) {
	n, w, mp := minerNode(t)
	poison := core.Transaction{
		From: w.Address(), To: w.Address(),
		Amount: 1000, Fee: 1_000_000, Nonce: 0,
		Memo: strings.Repeat("x", core.MaxMemoBytes+1),
	}
	if err := poison.Sign(w); err != nil {
		t.Fatal(err)
	}
	if err := n.SubmitTx(poison); err == nil {
		t.Fatal("a transaction no block can contain was accepted for relay")
	}
	if mp.Size() != 0 {
		t.Fatalf("mempool holds %d unmineable transaction(s)", mp.Size())
	}

	before := n.Chain().Height()
	if _, err := n.Generate(3); err != nil {
		t.Fatalf("block production stopped: %v", err)
	}
	if got := n.Chain().Height(); got != before+3 {
		t.Fatalf("height %d after generating 3 blocks, want %d", got, before+3)
	}
}

// The same guarantee for a rule that cannot be checked at admission time: the
// dust limit activates at a height, so an already-queued transaction can become
// unmineable. The miner must leave it behind and keep producing blocks.
func TestMinerSkipsTxForbiddenByAnActivatedUpgrade(t *testing.T) {
	core.ClearUpgrades()
	defer core.ClearUpgrades()

	n, w, mp := minerNode(t)
	dest, _ := wallet.New()
	dust := core.Transaction{
		From: w.Address(), To: dest.Address(),
		Amount: core.DustThreshold - 1, Fee: 1_000_000, Nonce: 0,
	}
	if err := dust.Sign(w); err != nil {
		t.Fatal(err)
	}
	if err := n.SubmitTx(dust); err != nil {
		t.Fatalf("dust is relayable before activation: %v", err)
	}
	core.SetUpgradeHeight(core.UpgradeDustLimit, n.Chain().Height()+1)

	before := n.Chain().Height()
	if _, err := n.Generate(2); err != nil {
		t.Fatalf("block production stopped: %v", err)
	}
	if got := n.Chain().Height(); got != before+2 {
		t.Fatalf("height %d, want %d", got, before+2)
	}
	if mp.Size() != 1 {
		t.Fatalf("mempool size %d; the dust tx should still be queued, just not mined", mp.Size())
	}
	if n.Chain().HasTx(dust.Hash()) {
		t.Fatal("a transaction the dust limit forbids was mined")
	}
}

// buildBlockFor drops a selected transaction consensus refuses rather than
// returning a candidate that can never connect. Nothing should reach that path
// now (Select mirrors every rule), so this pins the recovery itself: given a
// mempool holding only mineable work, the candidate always carries a state root.
func TestBuildBlockAlwaysProducesACommittableCandidate(t *testing.T) {
	n, w, _ := minerNode(t)
	dest, _ := wallet.New()
	for i := uint64(0); i < 3; i++ {
		tx := core.Transaction{From: w.Address(), To: dest.Address(), Amount: 1000, Fee: 1_000_000, Nonce: i}
		if err := tx.Sign(w); err != nil {
			t.Fatal(err)
		}
		if err := n.SubmitTx(tx); err != nil {
			t.Fatalf("tx %d: %v", i, err)
		}
	}
	candidate, txs := n.buildBlock()
	if candidate.StateRoot == "" {
		t.Fatal("candidate has no state root, so the miner would refuse to mine it")
	}
	if len(txs) != 3 {
		t.Fatalf("selected %d txs, want 3", len(txs))
	}
	mined, ok := core.Mine(candidate, nil)
	if !ok {
		t.Fatal("mining aborted")
	}
	if err := n.Chain().AddBlock(mined); err != nil {
		t.Fatalf("the miner's own candidate was rejected: %v", err)
	}
}
