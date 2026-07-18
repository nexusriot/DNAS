package core

import "math/big"

// BlockWork is the expected number of hashes behind a block at the given target
// (compact bits): work = 2^256 / (target + 1), the standard measure of how much
// proof-of-work a target represents. A smaller target (harder) yields more work.
func BlockWork(bits uint32) *big.Int {
	target := CompactToBig(bits)
	if target.Sign() <= 0 {
		return new(big.Int)
	}
	e := new(big.Int).Lsh(big.NewInt(1), 256)
	d := new(big.Int).Add(target, big.NewInt(1))
	return e.Div(e, d)
}

// ChainWork sums the work of every block in the chain. Comparing chains by
// total work (rather than length) is the correct fork-choice rule: a shorter
// chain built on harder blocks can represent more real work than a longer easy
// one.
func ChainWork(blocks []Block) *big.Int {
	total := new(big.Int)
	for _, b := range blocks {
		total.Add(total, BlockWork(b.Bits))
	}
	return total
}
