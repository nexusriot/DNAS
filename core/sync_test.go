package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

func TestHeadersFromAndBlocksRange(t *testing.T) {
	bc := NewBlockchain()
	w, _ := wallet.New()
	for i := 0; i < 5; i++ {
		if err := bc.AddBlock(mineOn(t, bc, w.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}

	hs := bc.HeadersFrom(2, 10)
	if len(hs) != 4 || hs[0].Index != 2 || hs[3].Index != 5 {
		t.Fatalf("HeadersFrom(2,10) = %d headers %+v", len(hs), hs)
	}
	if len(bc.HeadersFrom(0, 3)) != 3 {
		t.Error("HeadersFrom did not honor the max cap")
	}
	if bc.HeadersFrom(99, 10) != nil {
		t.Error("HeadersFrom past the tip should be nil")
	}

	br := bc.BlocksRange(1, 3, 10)
	if len(br) != 3 || br[0].Index != 1 || br[2].Index != 3 {
		t.Fatalf("BlocksRange(1,3,10) = %d blocks", len(br))
	}
	if len(bc.BlocksRange(0, 5, 2)) != 2 {
		t.Error("BlocksRange did not honor the max cap")
	}
	if bc.BlocksRange(3, 1, 10) != nil {
		t.Error("BlocksRange with to<from should be nil")
	}
}

func TestValidateHeaderChain(t *testing.T) {
	bc := NewBlockchain()
	w, _ := wallet.New()
	for i := 0; i < 4; i++ {
		if err := bc.AddBlock(mineOn(t, bc, w.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}
	genesis := GenesisBlock()
	hs := bc.HeadersFrom(1, 10) // headers 1..4 built on genesis

	if err := ValidateHeaderChain(hs, genesis.Hash, 0); err != nil {
		t.Fatalf("valid header chain rejected: %v", err)
	}
	if err := ValidateHeaderChain(hs, "wrong-prev", 0); err == nil {
		t.Error("header chain with wrong prev hash should be rejected")
	}
	if err := ValidateHeaderChain([]Header{hs[0], hs[2], hs[3]}, genesis.Hash, 0); err == nil {
		t.Error("header chain with a gap should be rejected")
	}
	tampered := append([]Header(nil), hs...)
	tampered[1].Nonce ^= 1 // invalidates that header's proof of work
	if err := ValidateHeaderChain(tampered, genesis.Hash, 0); err == nil {
		t.Error("header chain with a tampered header should be rejected")
	}
}
