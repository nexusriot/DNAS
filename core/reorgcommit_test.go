package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

func TestReorgSideWithoutCommitIsDiscarded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	alice, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	tip := bc.Tip().Hash
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := writeReorgSide(path, 0, []Block{GenesisBlock()}); err != nil {
		t.Fatal(err)
	}
	got, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	if got.Tip().Hash != tip {
		t.Fatal("a side file with no commit record changed the chain")
	}
	if _, err := os.Stat(reorgSidePath(path)); !os.IsNotExist(err) {
		t.Fatal("uncommitted side file was not discarded")
	}
}

func TestReorgCommitFinishesOnOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	for i := 0; i < 3; i++ {
		mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	}
	prefix := bc.Blocks()
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}

	y := NewBlockchain()
	mustAdd(t, y, prefix[1])
	for i := 0; i < 3; i++ {
		mustAdd(t, y, mineOn(t, y, bob.Address(), nil))
	}
	suffix := y.Blocks()[2:]
	sum, err := writeReorgSide(path, 1, suffix)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeReorgCommit(path, 1, uint32(len(suffix)), sum); err != nil {
		t.Fatal(err)
	}
	got, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	if got.Tip().Hash != y.Tip().Hash {
		t.Fatalf("open did not finish the committed reorg\n got %s\nwant %s", got.Tip().Hash, y.Tip().Hash)
	}
	if _, err := os.Stat(reorgSidePath(path)); !os.IsNotExist(err) {
		t.Fatal("side file survived a finished reorg")
	}
	if _, err := os.Stat(reorgCommitPath(path)); !os.IsNotExist(err) {
		t.Fatal("commit record survived a finished reorg")
	}
}

func TestReorgCommitMismatchRefusesOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	alice, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}
	sum, err := writeReorgSide(path, 0, []Block{GenesisBlock()})
	if err != nil {
		t.Fatal(err)
	}
	sum[0] ^= 0xff
	if err := writeReorgCommit(path, 0, 1, sum); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "dnas db verify") {
		t.Fatalf("mismatched commit should refuse to open and name `dnas db verify`, got %v", err)
	}
}

// A commit record that outlived its reorg (cleanup lost to a crash) must not
// truncate blocks the node appended AFTER the reorg on the next open.
func TestStaleReorgCommitDoesNotTruncateLaterBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	miner, _ := wallet.New()
	for i := 0; i < 6; i++ {
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	}
	blocks := bc.Blocks()
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}
	// Leave a fully-applied reorg's record behind: the suffix (heights 2..3) is
	// what the log already holds, and heights 4..6 were appended after it.
	sum, err := writeReorgSide(path, 1, blocks[2:4])
	if err != nil {
		t.Fatal(err)
	}
	if err := writeReorgCommit(path, 1, 2, sum); err != nil {
		t.Fatal(err)
	}
	got, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	if got.Height() != 6 {
		t.Fatalf("height after reopen = %d, want 6: a stale commit record truncated the log", got.Height())
	}
	if _, err := os.Stat(reorgCommitPath(path)); !os.IsNotExist(err) {
		t.Fatal("stale commit record was not removed")
	}
}

// A cleanup that removed the side file and crashed before removing the record
// (the order an older build used) must not leave the store unopenable.
func TestCommitRecordWithoutSideFileOpensWhenApplied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	miner, _ := wallet.New()
	for i := 0; i < 3; i++ {
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	}
	blocks := bc.Blocks()
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}
	sum, err := writeReorgSide(path, 1, blocks[2:4])
	if err != nil {
		t.Fatal(err)
	}
	if err := writeReorgCommit(path, 1, 2, sum); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(reorgSidePath(path)); err != nil {
		t.Fatal(err)
	}
	got, err := Open(path)
	if err != nil {
		t.Fatalf("store with an applied reorg's orphaned record would not open: %v", err)
	}
	defer got.Close()
	if got.Height() != 3 {
		t.Fatalf("height = %d, want 3", got.Height())
	}
}
