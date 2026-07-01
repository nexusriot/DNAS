package core

import (
	"math/big"
	"testing"
)

func TestBlockWork(t *testing.T) {
	// 16^difficulty
	cases := map[int]int64{0: 1, 1: 16, 2: 256, 4: 65536}
	for diff, want := range cases {
		if got := BlockWork(diff); got.Cmp(big.NewInt(want)) != 0 {
			t.Errorf("BlockWork(%d) = %s, want %d", diff, got, want)
		}
	}
}

func TestChainWorkSumsBlocks(t *testing.T) {
	blocks := []Block{{Difficulty: 2}, {Difficulty: 2}, {Difficulty: 3}}
	// 256 + 256 + 4096
	want := big.NewInt(256 + 256 + 4096)
	if got := ChainWork(blocks); got.Cmp(want) != 0 {
		t.Fatalf("ChainWork = %s, want %s", got, want)
	}
}

func TestMoreWorkBeatsLength(t *testing.T) {
	// A short chain of hard blocks can outweigh a longer chain of easy ones.
	longEasy := ChainWork([]Block{{Difficulty: 1}, {Difficulty: 1}, {Difficulty: 1}, {Difficulty: 1}}) // 4*16 = 64
	shortHard := ChainWork([]Block{{Difficulty: 3}})                                                   // 4096
	if shortHard.Cmp(longEasy) <= 0 {
		t.Fatalf("expected shortHard(%s) > longEasy(%s)", shortHard, longEasy)
	}
}

func TestChainTracksWork(t *testing.T) {
	bc := NewBlockchain()
	genesisWork := BlockWork(GenesisDifficulty)
	if bc.Work().Cmp(genesisWork) != 0 {
		t.Fatalf("fresh chain work = %s, want %s", bc.Work(), genesisWork)
	}
}
