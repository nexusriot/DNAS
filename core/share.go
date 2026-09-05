package core

import "math/big"

// Mining shares.
//
// A block target is, by design, hard enough that one small miner may search for
// hours without ever producing a hash that meets it — which tells the pool it is
// mining for nothing, and tells the miner nothing at all. A SHARE is the standard
// answer: a deliberately easier target that the same search satisfies often, so
// hashes that miss the block target still prove work was done. Shares are pool
// accounting, not consensus: no share is ever stored in the chain, and a node
// that ignores them is not on a different network.
//
// A share that happens to also meet the real block target is a block, and is
// submitted as one — so a miner never has to decide which it found.

// DefaultShareFactor is how many times easier the share target is than the
// block's. It is a plain multiplier on the target, so a miner produces about this
// many shares per block it would otherwise find alone.
const DefaultShareFactor = 256

// maxShareTargetBits is the ceiling for a share target: 2^248, far easier than
// any block target but still comfortably inside 256 bits, so multiplying a block
// target by the factor can never wrap around into an impossible value.
var maxShareTarget = new(big.Int).Lsh(big.NewInt(1), 248)

// ShareBits is the compact share target for a block target, `factor` times
// easier. A factor of 0 or 1 makes a share exactly as hard as a block.
func ShareBits(blockBits uint32, factor uint32) uint32 {
	target := CompactToBig(blockBits)
	if target.Sign() <= 0 {
		return blockBits
	}
	if factor > 1 {
		target = new(big.Int).Mul(target, new(big.Int).SetUint64(uint64(factor)))
	}
	if target.Cmp(maxShareTarget) > 0 {
		target = new(big.Int).Set(maxShareTarget)
	}
	return BigToCompact(target)
}

// MeetsShareTarget reports whether a hash satisfies a share target. Unlike
// meetsTarget it does NOT clamp to PowLimit: a share target is meant to be easier
// than any target consensus would accept, which is the whole point of it.
func MeetsShareTarget(hash string, shareBits uint32) bool {
	target := CompactToBig(shareBits)
	if target.Sign() <= 0 {
		return false
	}
	return hashToBig(hash).Cmp(target) <= 0
}
