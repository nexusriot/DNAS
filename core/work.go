package core

import "math/big"

// BlockWork is the expected amount of proof-of-work behind a block at the given
// difficulty. Each required leading hex zero narrows the valid-hash space by a
// factor of 16, so the work is 16^difficulty (== 2^(4*difficulty)).
func BlockWork(difficulty int) *big.Int {
	if difficulty < 0 {
		difficulty = 0
	}
	return new(big.Int).Lsh(big.NewInt(1), uint(4*difficulty))
}

// ChainWork sums the work of every block in the chain. Comparing chains by
// total work (rather than length) is the correct fork-choice rule: a shorter
// chain built on harder blocks can represent more real work than a longer easy
// one.
func ChainWork(blocks []Block) *big.Int {
	total := new(big.Int)
	for _, b := range blocks {
		total.Add(total, BlockWork(b.Difficulty))
	}
	return total
}
