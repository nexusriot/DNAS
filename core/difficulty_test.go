package core

import (
	"math/big"
	"testing"
)

// withRetarget re-enables the LWMA for a test (the suite's TestMain holds
// difficulty fixed for speed; these tests specifically exercise retargeting).
func withRetarget(t *testing.T) {
	t.Helper()
	NoRetarget = false
	t.Cleanup(func() { NoRetarget = true })
}

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
	withRetarget(t)
	// Until a full window of history exists, the genesis target holds.
	blocks := synthBits(lwmaWindow+1, TargetBlockTime, GenesisBits)
	for h := uint64(0); h <= lwmaWindow; h++ {
		if got := expectedBits(blocks, h); got != GenesisBits {
			t.Fatalf("expectedBits(height=%d) = %#x, want GenesisBits %#x", h, got, GenesisBits)
		}
	}
}

func TestExpectedBitsStableAtTargetSpacing(t *testing.T) {
	withRetarget(t)
	blocks := synthBits(lwmaWindow+1, TargetBlockTime, GenesisBits)
	if got := expectedBits(blocks, lwmaWindow+1); got != GenesisBits {
		t.Fatalf("on-target spacing should leave the target unchanged: got %#x, want %#x", got, GenesisBits)
	}
}

func TestExpectedBitsHardensAndEases(t *testing.T) {
	withRetarget(t)
	fast := expectedBits(synthBits(lwmaWindow+1, 1, GenesisBits), lwmaWindow+1)
	if CompactToBig(fast).Cmp(GenesisTarget) >= 0 {
		t.Fatalf("fast blocks should harden the target (smaller number), got %#x", fast)
	}
	slow := expectedBits(synthBits(lwmaWindow+1, 6*TargetBlockTime, GenesisBits), lwmaWindow+1)
	if CompactToBig(slow).Cmp(GenesisTarget) <= 0 {
		t.Fatalf("slow blocks should ease the target (larger number), got %#x", slow)
	}
}

func TestExpectedBitsUnboundedAndFloor(t *testing.T) {
	withRetarget(t)
	// No hard difficulty cap: fast blocks already at the old "hardest" reference
	// push the target BELOW MinTarget (harder than the toy ever used to allow).
	got := expectedBits(synthBits(lwmaWindow+1, 1, MinTargetBits), lwmaWindow+1)
	if CompactToBig(got).Cmp(MinTarget) >= 0 {
		t.Fatalf("fast blocks should push the target below MinTarget (unbounded difficulty), got %#x", got)
	}
	// But the easy side is floored: slow blocks at the easiest target stay at PowLimit.
	if got := expectedBits(synthBits(lwmaWindow+1, 6*TargetBlockTime, PowLimitBits), lwmaWindow+1); got != PowLimitBits {
		t.Fatalf("target should clamp at PowLimitBits (the floor), got %#x", got)
	}
}

// TestDifficultyRisesUnbounded simulates sustained fast mining — each block's
// target is the retarget of the chain so far, blocks 1s apart — and confirms
// difficulty climbs far past the old MinTarget cap (proving PoW now costs real,
// unbounded work rather than being clamped to a trivial band).
func TestDifficultyRisesUnbounded(t *testing.T) {
	withRetarget(t)
	blocks := []Block{{Index: 0, Timestamp: 0, Bits: PowLimitBits}}
	for h := 1; h <= 80; h++ {
		bits := expectedBits(blocks, uint64(h))
		blocks = append(blocks, Block{Index: uint64(h), Timestamp: int64(h), Bits: bits})
	}
	final := CompactToBig(blocks[len(blocks)-1].Bits)
	if final.Cmp(MinTarget) >= 0 {
		t.Fatalf("sustained fast blocks should drive the target well below the old MinTarget cap; got %s (MinTarget %s)", final, MinTarget)
	}
}

func TestExpectedBitsGenesisGapDoesNotCollapse(t *testing.T) {
	withRetarget(t)
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
