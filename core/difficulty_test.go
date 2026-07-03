package core

import "testing"

// synthBlocks builds n blocks (indices 0..n-1) evenly spaced in time, all at the
// given difficulty. Only fields read by expectedDifficulty are populated.
func synthBlocks(n int, spacing int64, diff int) []Block {
	b := make([]Block, n)
	for i := range b {
		b[i] = Block{Index: uint64(i), Timestamp: int64(i) * spacing, Difficulty: diff}
	}
	return b
}

func TestExpectedDifficultyGenesis(t *testing.T) {
	if got := expectedDifficulty(nil, 0); got != GenesisDifficulty {
		t.Fatalf("genesis difficulty = %d, want %d", got, GenesisDifficulty)
	}
}

func TestExpectedDifficultyNonBoundaryUnchanged(t *testing.T) {
	blocks := synthBlocks(10, 5, 4)
	// height 5 is not a retarget boundary, so it keeps the previous difficulty.
	if got := expectedDifficulty(blocks, 5); got != 4 {
		t.Fatalf("difficulty = %d, want 4 (unchanged off-boundary)", got)
	}
}

func TestExpectedDifficultyRetarget(t *testing.T) {
	// window = RetargetInterval blocks, expected span = window * TargetBlockTime.
	// spacing 1 -> very fast blocks -> difficulty increases.
	if got := expectedDifficulty(synthBlocks(10, 1, 4), 10); got != 5 {
		t.Errorf("fast blocks: difficulty = %d, want 5 (increase)", got)
	}
	// spacing 20 -> very slow blocks -> difficulty decreases.
	if got := expectedDifficulty(synthBlocks(10, 20, 4), 10); got != 3 {
		t.Errorf("slow blocks: difficulty = %d, want 3 (decrease)", got)
	}
	// spacing equal to the target -> on time -> unchanged.
	if got := expectedDifficulty(synthBlocks(10, TargetBlockTime, 4), 10); got != 4 {
		t.Errorf("on-time blocks: difficulty = %d, want 4 (unchanged)", got)
	}
}

func TestExpectedDifficultyBounds(t *testing.T) {
	// Fast blocks cannot push difficulty above MaxDifficulty.
	if got := expectedDifficulty(synthBlocks(10, 1, MaxDifficulty), 10); got != MaxDifficulty {
		t.Errorf("difficulty = %d, want capped at Max %d", got, MaxDifficulty)
	}
	// Slow blocks cannot push it below MinDifficulty.
	if got := expectedDifficulty(synthBlocks(10, 20, MinDifficulty), 10); got != MinDifficulty {
		t.Errorf("difficulty = %d, want floored at Min %d", got, MinDifficulty)
	}
}
