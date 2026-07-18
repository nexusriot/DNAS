package core

import (
	"math/big"
	"testing"
)

// bitsFor returns the compact target with the low `zeroBytes*8` bits free — i.e.
// work ≈ 2^(8*zeroBytes). Used to build blocks of known relative work for tests.
func bitsFor(highBit uint) uint32 { return BigToCompact(powOfTwoMinusOne(highBit)) }

func TestBlockWork(t *testing.T) {
	// work = 2^256/(target+1): a harder (smaller) target yields more work.
	if BlockWork(GenesisBits).Sign() <= 0 {
		t.Fatal("genesis work should be positive")
	}
	if BlockWork(MinTargetBits).Cmp(BlockWork(GenesisBits)) <= 0 {
		t.Fatal("hardest target should carry more work than genesis")
	}
	if BlockWork(GenesisBits).Cmp(BlockWork(PowLimitBits)) <= 0 {
		t.Fatal("genesis should carry more work than the easiest target")
	}
	if BlockWork(0).Sign() != 0 {
		t.Fatal("a zero/invalid target should carry zero work")
	}
}

func TestChainWorkSumsBlocks(t *testing.T) {
	blocks := []Block{{Bits: GenesisBits}, {Bits: GenesisBits}, {Bits: MinTargetBits}}
	want := new(big.Int).Add(BlockWork(GenesisBits), BlockWork(GenesisBits))
	want.Add(want, BlockWork(MinTargetBits))
	if got := ChainWork(blocks); got.Cmp(want) != 0 {
		t.Fatalf("ChainWork = %s, want %s", got, want)
	}
}

func TestMoreWorkBeatsLength(t *testing.T) {
	// A short chain of hard blocks can outweigh a longer chain of easy ones.
	longEasy := ChainWork([]Block{{Bits: PowLimitBits}, {Bits: PowLimitBits}, {Bits: PowLimitBits}, {Bits: PowLimitBits}})
	shortHard := ChainWork([]Block{{Bits: MinTargetBits}})
	if shortHard.Cmp(longEasy) <= 0 {
		t.Fatalf("expected shortHard(%s) > longEasy(%s)", shortHard, longEasy)
	}
}

func TestChainTracksWork(t *testing.T) {
	bc := NewBlockchain()
	if bc.Work().Cmp(BlockWork(GenesisBits)) != 0 {
		t.Fatalf("fresh chain work = %s, want %s", bc.Work(), BlockWork(GenesisBits))
	}
}

func TestCompactRoundTrip(t *testing.T) {
	// Compact encoding is lossy in the low bits but must be idempotent: decoding
	// then re-encoding a compact value returns the same compact value.
	for _, bits := range []uint32{GenesisBits, MinTargetBits, PowLimitBits, bitsFor(200)} {
		if got := BigToCompact(CompactToBig(bits)); got != bits {
			t.Errorf("round trip %#x -> %#x", bits, got)
		}
	}
	// Ordering is preserved: a smaller target encodes to represent less-or-equal.
	if CompactToBig(MinTargetBits).Cmp(CompactToBig(GenesisBits)) >= 0 {
		t.Fatal("MinTarget should be a smaller number (harder) than GenesisTarget")
	}
	if CompactToBig(GenesisBits).Cmp(CompactToBig(PowLimitBits)) >= 0 {
		t.Fatal("GenesisTarget should be smaller than PowLimit (easiest)")
	}
}
