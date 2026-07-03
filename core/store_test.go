package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

func TestStorePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	miner, _ := wallet.New()
	for i := 0; i < 3; i++ {
		if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}
	tip := bc.Tip().Hash
	bal := bc.Balance(miner.Address())
	work := bc.Work()
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Height() != 3 {
		t.Fatalf("height = %d, want 3", reopened.Height())
	}
	if reopened.Tip().Hash != tip {
		t.Fatal("tip changed across reopen")
	}
	if reopened.Balance(miner.Address()) != bal {
		t.Fatal("balance not restored across reopen")
	}
	if reopened.Work().Cmp(work) != 0 {
		t.Fatal("work not restored across reopen")
	}
}

func TestStoreReorgPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	miner, _ := wallet.New()
	b1 := mineOn(t, bc, miner.Address(), nil)
	if err := bc.AddBlock(b1); err != nil {
		t.Fatal(err)
	}
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil { // branch X, height 2
		t.Fatal(err)
	}

	// Heavier branch Y (height 3) sharing genesis+b1.
	y := NewBlockchain()
	other, _ := wallet.New()
	if err := y.AddBlock(b1); err != nil {
		t.Fatal(err)
	}
	if err := y.AddBlock(mineOn(t, y, other.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	if err := y.AddBlock(mineOn(t, y, other.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	if ok, err := bc.ReplaceChain(y.Blocks()); !ok || err != nil {
		t.Fatalf("reorg: ok=%v err=%v", ok, err)
	}
	tip := bc.Tip().Hash
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Height() != 3 || reopened.Tip().Hash != tip {
		t.Fatalf("reorg not persisted: height=%d tip=%s", reopened.Height(), reopened.Tip().Hash[:8])
	}
	if reopened.Tip().Hash != y.Tip().Hash {
		t.Fatal("wrong chain persisted after reorg")
	}
}

func TestOpenRejectsForeignFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreign")
	content := []byte("this is not a DNAS block log, please do not eat me")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open should refuse a foreign, non-empty file")
	}
	if data, _ := os.ReadFile(path); len(data) != len(content) {
		t.Fatal("Open must not clobber a foreign file")
	}
}
