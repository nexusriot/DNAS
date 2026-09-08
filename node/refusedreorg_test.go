package node

import (
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// A reorg the finality guards refuse is the most consequential thing a node can
// do quietly: fork choice preferred another chain, the guard declined, and the
// same guard will decline the same switch every time it is offered. If that
// chain is the network's, the node is diverged for good.
//
// Before this, the error was discarded at the call site (`err == nil && replaced`)
// and counted nowhere — a diverged node looked perfectly healthy. These tests
// pin that it is now counted, reported and surfaced as unhealthy.

func TestReorgRefusedErrorCarriesTheReason(t *testing.T) {
	deep := &core.ReorgRefusedError{Depth: 150, ForkHeight: 10, Limit: core.MaxReorgDepth}
	if deep.Reason() != "too_deep" {
		t.Errorf("depth refusal reason = %q, want too_deep", deep.Reason())
	}
	if !strings.Contains(deep.Error(), "150") {
		t.Errorf("error should name the depth: %q", deep.Error())
	}

	cp := &core.ReorgRefusedError{Depth: 3, ForkHeight: 2, Checkpoint: 5}
	if cp.Reason() != "below_checkpoint" {
		t.Errorf("checkpoint refusal reason = %q, want below_checkpoint", cp.Reason())
	}

	// It must be recognisable through the error chain, since that is how the node
	// tells "the guard refused" from "the peer sent garbage".
	if got, ok := core.AsReorgRefused(error(deep)); !ok || got.Depth != 150 {
		t.Errorf("AsReorgRefused failed to recover the typed error")
	}
	if _, ok := core.AsReorgRefused(errPlain{}); ok {
		t.Error("AsReorgRefused matched an unrelated error")
	}
}

type errPlain struct{}

func (errPlain) Error() string { return "some other failure" }

func TestRefusedReorgIsCountedAndReported(t *testing.T) {
	n := &Node{reorgs: newReorgLog(), orphans: newOrphanPool(orphanPoolCapacity, orphanPoolBytes)}

	if rep := n.Reorgs(); rep.Refused != 0 || rep.RefusedWhy != "" {
		t.Fatalf("a fresh node reports %d refusals", rep.Refused)
	}

	n.noteRefusedReorg(&core.ReorgRefusedError{Depth: 120, ForkHeight: 40, Limit: core.MaxReorgDepth})
	n.noteRefusedReorg(&core.ReorgRefusedError{Depth: 200, ForkHeight: 10, Limit: core.MaxReorgDepth})
	n.noteRefusedReorg(&core.ReorgRefusedError{Depth: 5, ForkHeight: 2, Checkpoint: 7})

	rep := n.Reorgs()
	if rep.Refused != 3 {
		t.Errorf("Refused = %d, want 3", rep.Refused)
	}
	if rep.RefusedDeepest != 200 {
		t.Errorf("RefusedDeepest = %d, want 200 (the high-water mark, not the latest)", rep.RefusedDeepest)
	}
	if rep.RefusedWhy != "below_checkpoint" {
		t.Errorf("RefusedWhy = %q, want the most recent reason", rep.RefusedWhy)
	}
	if rep.RefusedAt == "" {
		t.Error("RefusedAt should be set once a refusal has happened")
	}
	if _, err := time.Parse(time.RFC3339, rep.RefusedAt); err != nil {
		t.Errorf("RefusedAt %q is not RFC3339: %v", rep.RefusedAt, err)
	}

	// Adopted-reorg counters must not be disturbed by refusals: they answer
	// different questions and conflating them would hide both.
	if rep.Total != 0 || rep.Deepest != 0 {
		t.Errorf("refusals leaked into the adopted counters: total=%d deepest=%d", rep.Total, rep.Deepest)
	}
}

// The end-to-end case: a chain that fork choice prefers but the depth guard
// refuses must produce a typed error, not a generic one.
func TestDeepReorgIsRefusedWithATypedError(t *testing.T) {
	bc := core.NewBlockchain()
	miner, _ := wallet.New()

	// Build a chain deep enough that forking at genesis exceeds MaxReorgDepth.
	depth := core.MaxReorgDepth + 5
	mineBlocks(t, bc, miner, depth)

	// A competing chain from genesis, longer still, so fork choice would prefer
	// it if the guard were not there.
	other := core.NewBlockchain()
	rival, _ := wallet.New()
	mineBlocks(t, other, rival, depth+3)

	replaced, _, err := bc.ReplaceChain(other.Blocks())
	if replaced {
		t.Fatal("a reorg deeper than MaxReorgDepth should have been refused")
	}
	ref, ok := core.AsReorgRefused(err)
	if !ok {
		t.Fatalf("refusal produced an untyped error: %v", err)
	}
	if ref.Reason() != "too_deep" {
		t.Errorf("reason = %q, want too_deep", ref.Reason())
	}
	if ref.Depth <= core.MaxReorgDepth {
		t.Errorf("refused depth %d should exceed the limit %d", ref.Depth, core.MaxReorgDepth)
	}
}
