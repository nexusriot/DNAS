package node

import (
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// TestBlockTemplateAndSubmit checks the external-miner path: a template pays an
// arbitrary address, mining it and submitting advances the chain and credits that
// address, and resubmitting a stale block is rejected.
func TestBlockTemplateAndSubmit(t *testing.T) {
	n, _, _ := testNode(t)
	extMiner, _ := wallet.New()

	tmpl, err := n.BuildTemplate(extMiner.Address())
	if err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	if tmpl.Index != 1 {
		t.Fatalf("template height = %d, want 1", tmpl.Index)
	}
	mined, ok := core.Mine(tmpl, nil)
	if !ok {
		t.Fatal("mining aborted")
	}
	if err := n.SubmitMinedBlock(mined); err != nil {
		t.Fatalf("SubmitMinedBlock: %v", err)
	}
	if n.chain.Height() != 1 {
		t.Fatalf("height after submit = %d, want 1", n.chain.Height())
	}
	if got := n.chain.Balance(extMiner.Address()); got != core.BlockReward(1) {
		t.Fatalf("external miner reward = %d, want %d", got, core.BlockReward(1))
	}
	// Resubmitting the same block (now stale) must be rejected.
	if err := n.SubmitMinedBlock(mined); err == nil {
		t.Fatal("resubmitting a stale block should be rejected")
	}

	// An empty-address template is refused.
	if _, err := n.BuildTemplate(""); err == nil {
		t.Fatal("BuildTemplate with no address should error")
	}
}
