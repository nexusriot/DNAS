package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// writeChain marshals blocks to a chain file the way Save would.
func writeChain(t *testing.T, path string, blocks []Block) error {
	t.Helper()
	data, err := json.MarshalIndent(blocks, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func TestBlockchainSaveLoad(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 3; i++ {
		if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}

	path := filepath.Join(t.TempDir(), "chain.json")
	if err := bc.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if loaded.Height() != bc.Height() {
		t.Errorf("height = %d, want %d", loaded.Height(), bc.Height())
	}
	if loaded.Tip().Hash != bc.Tip().Hash {
		t.Errorf("tip mismatch after reload")
	}
	if loaded.Balance(miner.Address()) != bc.Balance(miner.Address()) {
		t.Errorf("balance not restored")
	}
	if loaded.Work().Cmp(bc.Work()) != 0 {
		t.Errorf("work not restored: %s vs %s", loaded.Work(), bc.Work())
	}
}

func TestLoadRejectsForeignGenesis(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	// A single block whose hash is not our genesis.
	foreign := GenesisBlock()
	foreign.Hash = "not-our-genesis"
	if err := writeChain(t, path, []Block{foreign}); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected Load to reject a foreign genesis")
	}
}
