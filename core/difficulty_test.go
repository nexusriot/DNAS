package core

import (
	"math/big"
	"testing"
)

// synthBits builds n blocks (indices 0..n-1) spaced `spacing` seconds apart, all
// committing the given compact target, for driving expectedBits directly.
func synthBits(n int, spacing int64, bits uint32) []Block {
	b := make([]Block, n)
	for i := range b {
		b[i] = Block{Index: uint64(i), Timestamp: int64(i) * spacing, Bits: bits}
	}
	return b
}

func TestExpectedBitsGenesisWindow(t *testing.T) {
	// Until a full window of history exists, the genesis target holds.
	blocks := synthBits(lwmaWindow+1, TargetBlockTime, GenesisBits)
	for h := uint64(0); h <= lwmaWindow; h++ {
		if got := expectedBits(blocks, h); got != GenesisBits {
			t.Fatalf("expectedBits(height=%d) = %#x, want GenesisBits %#x", h, got, GenesisBits)
		}
	}
}

func TestExpectedBitsStableAtTargetSpacing(t *testing.T) {
	blocks := synthBits(lwmaWindow+1, TargetBlockTime, GenesisBits)
	if got := expectedBits(blocks, lwmaWindow+1); got != GenesisBits {
		t.Fatalf("on-target spacing should leave the target unchanged: got %#x, want %#x", got, GenesisBits)
	}
}

func TestExpectedBitsHardensAndEases(t *testing.T) {
	fast := expectedBits(synthBits(lwmaWindow+1, 1, GenesisBits), lwmaWindow+1)
	if CompactToBig(fast).Cmp(GenesisTarget) >= 0 {
		t.Fatalf("fast blocks should harden the target (smaller number), got %#x", fast)
	}
	slow := expectedBits(synthBits(lwmaWindow+1, 6*TargetBlockTime, GenesisBits), lwmaWindow+1)
	if CompactToBig(slow).Cmp(GenesisTarget) <= 0 {
		t.Fatalf("slow blocks should ease the target (larger number), got %#x", slow)
	}
}

func TestExpectedBitsClamped(t *testing.T) {
	// Already at the hardest target + fast blocks: cannot go below MinTarget.
	if got := expectedBits(synthBits(lwmaWindow+1, 1, MinTargetBits), lwmaWindow+1); got != MinTargetBits {
		t.Fatalf("target should clamp at MinTargetBits, got %#x", got)
	}
	// Already at the easiest target + slow blocks: cannot exceed PowLimit.
	if got := expectedBits(synthBits(lwmaWindow+1, 6*TargetBlockTime, PowLimitBits), lwmaWindow+1); got != PowLimitBits {
		t.Fatalf("target should clamp at PowLimitBits, got %#x", got)
	}
}

func TestExpectedBitsGenesisGapDoesNotCollapse(t *testing.T) {
	// The old bug: the far-past genesis timestamp made the first solve time
	// enormous, which the old retarget let collapse difficulty to the floor. With
	// per-block solve-time clamping (and that gap landing on the lowest LWMA
	// weight), the target stays close to genesis instead of blowing out.
	blocks := synthBits(lwmaWindow+1, TargetBlockTime, GenesisBits)
	for i := 1; i < len(blocks); i++ {
		blocks[i].Timestamp = 1_000_000_000 + int64(i)*TargetBlockTime // far after genesis (ts 0)
	}
	got := CompactToBig(expectedBits(blocks, lwmaWindow+1))
	lo := new(big.Int).Rsh(GenesisTarget, 1) // GenesisTarget/2
	hi := new(big.Int).Lsh(GenesisTarget, 1) // GenesisTarget*2
	if got.Cmp(lo) < 0 || got.Cmp(hi) > 0 {
		t.Fatalf("genesis gap moved the target too far: got %s, want within [%s, %s]", got, lo, hi)
	}
}
