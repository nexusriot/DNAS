package core

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// TestReorgDepthLimit confirms a reorg that would discard more than
// MaxReorgDepth committed blocks is refused (finality), while one at the boundary
// still passes the guard and proceeds to fork choice.
func TestReorgDepthLimit(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	ts := GenesisTimestamp + 20
	for i := 0; i < MaxReorgDepth+2; i++ { // tip index becomes MaxReorgDepth+2
		if err := bc.AddBlock(mineAt(t, bc, miner.Address(), nil, ts)); err != nil {
			t.Fatalf("add block %d: %v", i+1, err)
		}
		ts += 20 // wide spacing keeps difficulty (hence mining cost) at the minimum
	}

	// Fork at height 1 would discard MaxReorgDepth+1 blocks -> refused as too deep.
	if _, err := bc.ReorgFrom(1, []Block{{Index: 2}}); err == nil || !strings.Contains(err.Error(), "too deep") {
		t.Fatalf("a fork discarding %d blocks should be refused as too deep, got %v", MaxReorgDepth+1, err)
	}
	if bc.Height() != uint64(MaxReorgDepth+2) {
		t.Fatalf("the chain must be untouched after a refused reorg (height %d)", bc.Height())
	}

	// Fork at height 2 would discard exactly MaxReorgDepth blocks -> passes the
	// depth guard (then simply loses fork choice, with no error).
	if _, err := bc.ReorgFrom(2, []Block{{Index: 3}}); err != nil {
		t.Fatalf("a fork at the depth boundary should pass the guard, got %v", err)
	}
}

// TestCheckpointEnforced confirms a block at a checkpointed height must carry the
// pinned hash.
func TestCheckpointEnforced(t *testing.T) {
	defer ClearCheckpoints()

	ref := NewBlockchain()
	honest, _ := wallet.New()
	b1 := mineOn(t, ref, honest.Address(), nil)
	AddCheckpoint(1, b1.Hash)

	// The matching block is accepted.
	good := NewBlockchain()
	if err := good.AddBlock(b1); err != nil {
		t.Fatalf("the checkpointed block should be accepted: %v", err)
	}

	// A different block at the checkpointed height is rejected.
	bad := NewBlockchain()
	attacker, _ := wallet.New()
	other := mineOn(t, bad, attacker.Address(), nil) // different coinbase -> different hash
	if other.Hash == b1.Hash {
		t.Skip("coinbase recipients collided (astronomically unlikely)")
	}
	if err := bad.AddBlock(other); err == nil || !strings.Contains(err.Error(), "checkpoint") {
		t.Fatalf("a block at a checkpointed height with the wrong hash must be rejected, got %v", err)
	}
}

// TestReorgBelowCheckpointRejected confirms no reorg may fork below a checkpoint.
func TestReorgBelowCheckpointRejected(t *testing.T) {
	defer ClearCheckpoints()

	bc := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 3; i++ {
		if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}
	cp, _ := bc.BlockAt(2)
	AddCheckpoint(2, cp.Hash)

	// Forking at height 1 would discard the checkpointed block at height 2.
	if _, err := bc.ReorgFrom(1, []Block{{Index: 2}}); err == nil || !strings.Contains(err.Error(), "checkpoint") {
		t.Fatalf("a reorg below a checkpoint must be rejected, got %v", err)
	}
	// Forking at the checkpoint height is allowed past the guard.
	if _, err := bc.ReorgFrom(2, []Block{{Index: 3}}); err != nil {
		t.Fatalf("a reorg at the checkpoint height should pass the guard, got %v", err)
	}
}
