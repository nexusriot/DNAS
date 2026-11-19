package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// The filter-header chain commits a filter's N and Data, nothing else. A server
// that keeps those and rewrites the Key produces a filter that still folds to
// the committed root — and matches none of the addresses actually in the
// block. CheckFilterBinding is what a client holding the header uses to refuse
// it; these tests forge exactly that filter, at both key derivations.

func bindingFixture(t *testing.T, height uint64) (Block, BlockFilter, []string) {
	t.Helper()
	a, _ := wallet.New()
	b, _ := wallet.New()
	blk := filterBlockAt(t, height, a.Address(), b.Address())
	return blk, BuildBlockFilter(blk), []string{a.Address(), b.Address()}
}

func TestCheckFilterBindingAcceptsTheBuiltFilter(t *testing.T) {
	for _, active := range []bool{false, true} {
		if active {
			withUpgradeAt(t, UpgradeFilterCommit, 1)
		}
		blk, f, _ := bindingFixture(t, 5)
		if err := CheckFilterBinding(f, blk.Header()); err != nil {
			t.Fatalf("upgrade active=%v: the honest filter was refused: %v", active, err)
		}
		empty := BuildBlockFilter(filterBlockAt(t, 6))
		hdr := filterBlockAt(t, 6).Header()
		if err := CheckFilterBinding(empty, hdr); err != nil {
			t.Fatalf("upgrade active=%v: an honest empty filter was refused: %v", active, err)
		}
	}
}

func TestCheckFilterBindingRefusesASwappedKey(t *testing.T) {
	for _, active := range []bool{false, true} {
		if active {
			withUpgradeAt(t, UpgradeFilterCommit, 1)
		}
		blk, f, addrs := bindingFixture(t, 5)
		forged := f
		forged.Key = "0000000000000000000000000000000000000000000000000000000000000001"

		// The forgery survives everything a fold can check...
		honestFold, _ := FoldFilterHeaders("", []BlockFilter{f})
		forgedFold, _ := FoldFilterHeaders("", []BlockFilter{forged})
		if honestFold[0] != forgedFold[0] {
			t.Fatal("the fold now covers the key; this test no longer forges what it means to")
		}
		// ...and hides the block's addresses: the false negative that matters.
		hidden := 0
		for _, a := range addrs {
			if !forged.Match(a) {
				hidden++
			}
		}
		if hidden == 0 {
			t.Fatal("a re-keyed filter still matched every address; the forgery is not doing anything")
		}
		if err := CheckFilterBinding(forged, blk.Header()); err == nil {
			t.Fatalf("upgrade active=%v: a re-keyed filter was accepted", active)
		}
	}
}

func TestCheckFilterBindingRefusesTheWrongBlock(t *testing.T) {
	blk, f, _ := bindingFixture(t, 5)
	moved := f
	moved.Index = 6
	if err := CheckFilterBinding(moved, blk.Header()); err == nil {
		t.Fatal("a filter for another height was accepted")
	}
	renamed := f
	renamed.BlockHash = blk.PrevHash
	if err := CheckFilterBinding(renamed, blk.Header()); err == nil {
		t.Fatal("a filter naming another block was accepted")
	}
	// Below the upgrade the seed IS the block hash, and the builder leaves Key
	// empty. Spelling it out is a different filter, not the same one.
	spelled := f
	spelled.Key = blk.Hash
	if err := CheckFilterBinding(spelled, blk.Header()); err == nil {
		t.Fatal("a filter with a key the builder never writes was accepted")
	}
}

// The browser's check (dnas.verifyCommittedFilter) goes through this: binding
// and committed fold against ONE header, so a re-keyed filter that still folds
// is refused, and a header with no commitment is not treated as a pass.
func TestVerifyFilterAgainstHeader(t *testing.T) {
	withUpgradeAt(t, UpgradeFilterCommit, 1)
	blk, f, _ := bindingFixture(t, 5)
	root, err := BlockFilterRoot("", blk)
	if err != nil {
		t.Fatal(err)
	}
	blk.FilterRoot = root
	hdr := blk.Header()
	if err := VerifyFilterAgainstHeader(f, "", hdr); err != nil {
		t.Fatalf("the honest filter was refused: %v", err)
	}
	forged := f
	forged.Key = "0000000000000000000000000000000000000000000000000000000000000001"
	if !VerifyCommittedFilter(forged, "", root) {
		t.Fatal("the fold now covers the key; this test no longer forges what it means to")
	}
	if err := VerifyFilterAgainstHeader(forged, "", hdr); err == nil {
		t.Fatal("a re-keyed filter that folds to the committed root was accepted")
	}
	other := f
	other.N++
	if err := VerifyFilterAgainstHeader(other, "", hdr); err == nil {
		t.Fatal("a filter that does not fold to the committed root was accepted")
	}
	bare := hdr
	bare.FilterRoot = ""
	if err := VerifyFilterAgainstHeader(f, "", bare); err == nil {
		t.Fatal("a header committing no filter root was treated as a pass")
	}
}
