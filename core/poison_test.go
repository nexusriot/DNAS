package core

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// A reorg is the one persistence sequence that cannot be rolled back: the store
// is truncated to the fork point before the winning suffix is appended, so once
// the truncate lands there is no "old chain" left on disk to return to. If an
// append then fails, disk holds a prefix of a chain memory is not running, and
// the next AddBlock would stack the old chain's continuation on top of it.
//
// These tests pin the guard: such a failure poisons the store, and a poisoned
// store refuses every later write rather than deepening the divergence.

// failStoreWrites makes every subsequent write to the store fail, by closing the
// file out from under it. That is the cheapest honest stand-in for a full or
// failing disk: the syscalls return errors exactly where a real ENOSPC would.
func failStoreWrites(t *testing.T, bc *Blockchain) {
	t.Helper()
	if bc.store == nil {
		t.Fatal("blockchain has no store")
	}
	if err := bc.store.f.Close(); err != nil {
		t.Fatalf("closing the store file: %v", err)
	}
}

func TestPoisonedStoreRefusesWrites(t *testing.T) {
	s := &blockStore{}
	s.poison(errTest{"disk full"})

	if err := s.append(GenesisBlock()); err == nil {
		t.Error("a poisoned store must refuse append")
	} else if !strings.Contains(err.Error(), "poisoned") {
		t.Errorf("append error should name the poison, got %v", err)
	}
	if err := s.truncateAfter(0); err == nil {
		t.Error("a poisoned store must refuse truncateAfter")
	}
	// The error names the original cause and the tool that inspects a store, so
	// the operator is not left guessing what happened or what to run.
	err := s.append(GenesisBlock())
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("error should carry the original cause, got %v", err)
	}
	if !strings.Contains(err.Error(), "dnas db verify") {
		t.Errorf("error should point at `dnas db verify`, got %v", err)
	}
}

func TestPoisonKeepsFirstCause(t *testing.T) {
	s := &blockStore{}
	s.poison(errTest{"first"})
	s.poison(errTest{"second"})
	if got := s.poisoned.Error(); got != "first" {
		t.Errorf("poison should keep the first cause, got %q", got)
	}
}

// TestFailedReorgPersistPoisonsStore is the end-to-end case: a real reorg whose
// persistence fails must not leave the node quietly writing to a diverged log.
func TestFailedReorgPersistPoisonsStore(t *testing.T) {
	dir := t.TempDir()
	bc, err := Open(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Close()

	alice, _ := wallet.New()
	carol, _ := wallet.New()

	// A shared prefix, then our branch extends it by one block.
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	shared := bc.Blocks()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	ourTip := bc.Tip().Hash

	// A heavier competing branch off the same prefix: two blocks instead of one.
	y := NewBlockchain()
	for _, b := range shared[1:] {
		if err := y.AddBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := y.AddBlock(mineOn(t, y, carol.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}

	// Break the disk, then reorg. The truncate or the suffix append must fail.
	failStoreWrites(t, bc)
	ok, _, err := bc.ReplaceChain(y.Blocks())
	if err == nil {
		t.Fatal("reorg onto a broken store should have failed")
	}
	if ok {
		t.Error("a reorg that could not persist must not report success")
	}

	// Memory is unchanged: the node still runs the chain it was running.
	if bc.Tip().Hash != ourTip {
		t.Error("failed reorg must leave the in-memory tip alone")
	}

	// And the store is now poisoned, so the node cannot deepen the divergence by
	// appending the old chain's continuation on top of the partial new suffix.
	if bc.store.poisoned == nil {
		t.Fatal("a failed reorg persist must poison the store")
	}
	err = bc.AddBlock(mineOn(t, bc, alice.Address(), nil))
	if err == nil {
		t.Fatal("AddBlock must fail once the store is poisoned")
	}
	if !strings.Contains(err.Error(), "poisoned") {
		t.Errorf("AddBlock error should name the poison, got %v", err)
	}

	// The refused block must not have been half-applied to state either.
	if bc.Tip().Hash != ourTip {
		t.Error("a block refused by a poisoned store must not change the tip")
	}
}

// errTest is a minimal error with a stable message.
type errTest struct{ msg string }

func (e errTest) Error() string { return e.msg }
