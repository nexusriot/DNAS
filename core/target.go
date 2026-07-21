package core

import (
	"encoding/hex"
	"math/big"
)

// Proof of work uses a 256-bit target in compact "bits" form (like Bitcoin's
// nBits): a block's hash, read big-endian as a 256-bit integer, must be ≤ the
// target the block commits to (`Block.Bits`). A smaller target is exponentially
// harder. This replaces the older leading-zero-nibble difficulty (a coarse
// integer 3–5) with a continuous target, so the LWMA retarget (`expectedBits`)
// can make fine, smooth adjustments toward `TargetBlockTime` instead of ±1 steps.

// lwmaWindow is the number of recent blocks the LWMA retarget averages over.
const lwmaWindow = 20

// NoRetarget disables difficulty retargeting: expectedBits holds the genesis
// target so blocks stay instant regardless of how fast they are mined. It is set
// for regtest and the test suite (mirroring Bitcoin Core's fPowNoRetargeting); a
// real network leaves it false so difficulty tracks hashpower without bound.
var NoRetarget bool

var (
	// PowLimitBits / GenesisBits are the compact encodings stored in headers; the
	// *Target values are their canonical (compact-representable) big.Int forms, so
	// consensus never compares against a value a header can't commit. PowLimit
	// (~2^244) is the EASIEST allowed target — the difficulty floor and the value a
	// fresh/regtest chain sits at. There is deliberately no hardest-target clamp:
	// difficulty rises without bound as hashpower grows (see expectedBits).
	// MinTargetBits (~2^236) is retained only as a reference point in tests (the
	// difficulty the old toy used to cap at); it is no longer a consensus clamp.
	PowLimitBits  = BigToCompact(powOfTwoMinusOne(244)) // easiest allowed target (floor)
	MinTargetBits = BigToCompact(powOfTwoMinusOne(236)) // test reference only (not a clamp)
	GenesisBits   = BigToCompact(powOfTwoMinusOne(240)) // committed at genesis

	PowLimit      = CompactToBig(PowLimitBits)
	MinTarget     = CompactToBig(MinTargetBits)
	GenesisTarget = CompactToBig(GenesisBits)
)

// powOfTwoMinusOne returns 2^n − 1 (a target with the low n bits set).
func powOfTwoMinusOne(n uint) *big.Int {
	x := new(big.Int).Lsh(big.NewInt(1), n)
	return x.Sub(x, big.NewInt(1))
}

// hashToBig interprets a hex block hash as a big-endian 256-bit integer for
// target comparison. Non-hex sorts larger than any valid target (never passes).
func hashToBig(hash string) *big.Int {
	b, err := hex.DecodeString(hash)
	if err != nil {
		return new(big.Int).Lsh(big.NewInt(1), 256)
	}
	return new(big.Int).SetBytes(b)
}

// meetsTarget reports whether a block hash satisfies the target encoded by bits.
// A target above PowLimit (easier than allowed) never passes, so a peer can't
// slip through a trivially-mined block by claiming an out-of-range target.
func meetsTarget(hash string, bits uint32) bool {
	target := CompactToBig(bits)
	if target.Sign() <= 0 || target.Cmp(PowLimit) > 0 {
		return false
	}
	return hashToBig(hash).Cmp(target) <= 0
}

// CompactToBig decodes a compact "bits" value into the full 256-bit target it
// represents — mantissa × 256^(exponent−3) — mirroring Bitcoin's nBits.
func CompactToBig(bits uint32) *big.Int {
	mantissa := bits & 0x007fffff
	exponent := bits >> 24
	out := new(big.Int).SetUint64(uint64(mantissa))
	if exponent <= 3 {
		out.Rsh(out, 8*(3-uint(exponent)))
	} else {
		out.Lsh(out, 8*(uint(exponent)-3))
	}
	return out
}

// BigToCompact encodes a 256-bit target into compact "bits" form. Targets here
// are always positive and bounded, so the sign bit is never set.
func BigToCompact(n *big.Int) uint32 {
	if n.Sign() <= 0 {
		return 0
	}
	nBytes := uint32((n.BitLen() + 7) / 8)
	var mantissa uint32
	if nBytes <= 3 {
		mantissa = uint32(new(big.Int).Lsh(n, 8*(3-uint(nBytes))).Uint64())
	} else {
		mantissa = uint32(new(big.Int).Rsh(n, 8*(uint(nBytes)-3)).Uint64())
	}
	// A set top bit would read as negative; shift down a byte and bump the exponent.
	if mantissa&0x00800000 != 0 {
		mantissa >>= 8
		nBytes++
	}
	return (nBytes << 24) | (mantissa & 0x007fffff)
}

// TargetDifficulty is a human-readable difficulty ratio (PowLimit ÷ target),
// where 1.0 is the easiest allowed target and larger is harder. Display only —
// consensus works in targets and cumulative work, never this float.
func TargetDifficulty(bits uint32) float64 {
	target := CompactToBig(bits)
	if target.Sign() <= 0 {
		return 0
	}
	f, _ := new(big.Rat).SetFrac(PowLimit, target).Float64()
	return f
}
