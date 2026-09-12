package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"testing"
)

// referencePreimage is how the header preimage was formatted before it was
// hand-built for speed. Consensus is defined by these exact bytes — every hash
// ever mined commits to them — so the fast path is checked against the reference
// here across edge cases, not only through the two headers the consensus vectors
// pin.
func referencePreimage(h Header) string {
	return fmt.Sprintf("%d|%d|%d|%s|%s|%s|%d|%d|%d",
		h.Version, h.Index, h.Timestamp, h.PrevHash, h.MerkleRoot, h.StateRoot, h.BaseFee, h.Bits, h.Nonce)
}

// preimageCases covers the formatting corners: zero values, the widest value of
// every numeric field, and a timestamp before the epoch (the one signed field,
// which prints a minus sign).
func preimageCases() []Header {
	genesis := GenesisBlock().Header()
	return []Header{
		{},
		genesis,
		{
			Version: math.MaxUint32, Index: math.MaxUint64, Timestamp: math.MaxInt64,
			PrevHash: genesis.Hash, MerkleRoot: genesis.MerkleRoot, StateRoot: genesis.StateRoot,
			BaseFee: math.MaxUint64, Bits: math.MaxUint32, Nonce: math.MaxUint64,
		},
		{Version: 1, Index: 7, Timestamp: -1, Bits: GenesisBits, Nonce: 9},
		{Timestamp: math.MinInt64, BaseFee: InitialBaseFee, Bits: PowLimitBits, Nonce: 10},
	}
}

func TestHeaderPreimageMatchesTheFormattedReference(t *testing.T) {
	for i, h := range preimageCases() {
		want := referencePreimage(h)
		if got := h.headerString(); got != want {
			t.Errorf("case %d preimage:\n got %q\nwant %q", i, got, want)
		}
		if got := h.ComputeHash(); got != hashBytes([]byte(want)) {
			t.Errorf("case %d hash = %s, want the hash of the reference preimage", i, got)
		}
	}
}

// Mine builds the preimage prefix once and appends only the nonce per attempt,
// so the prefix must be the reference preimage minus its nonce digits — for any
// nonce, including ones of different digit lengths.
func TestPreimagePrefixPlusNonceIsTheWholePreimage(t *testing.T) {
	h := GenesisBlock().Header()
	for _, nonce := range []uint64{0, 1, 9, 10, 99, 100, 65536, math.MaxUint64} {
		h.Nonce = nonce
		got := string(strconv.AppendUint(appendPreimagePrefix(nil, h), nonce, 10))
		if want := referencePreimage(h); got != want {
			t.Errorf("nonce %d:\n got %q\nwant %q", nonce, got, want)
		}
	}
}

// The mining loop compares raw digests against targetBytes instead of parsing
// each hash into a big.Int. The two must answer identically, or a miner would
// either skip valid solutions or emit blocks consensus rejects.
func TestTargetBytesAgreesWithTheBigIntComparison(t *testing.T) {
	hashes := []string{
		"0000000000000000000000000000000000000000000000000000000000000000",
		"0000000000000000000000000000000000000000000000000000000000000001",
		"0000ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		"00000fffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		hex.EncodeToString(func() []byte { s := sha256.Sum256([]byte("a block")); return s[:] }()),
		"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	}
	for _, bits := range []uint32{
		0,                           // unsatisfiable: no hash passes
		BigToCompact(big.NewInt(1)), // only the all-but-zero hash passes
		GenesisBits,                 // what the tests and regtest mine at
		PowLimitBits,                // the easiest target consensus allows
		MinTargetBits,               // harder than the floor
		0x21000001,                  // exponent 33: wider than 256 bits, so everything passes
	} {
		target := targetBytes(bits)
		bigTarget := CompactToBig(bits)
		for _, h := range hashes {
			raw, err := hex.DecodeString(h)
			if err != nil {
				t.Fatal(err)
			}
			got := bytes.Compare(raw, target[:]) <= 0
			want := hashToBig(h).Cmp(bigTarget) <= 0
			if got != want {
				t.Errorf("bits %#08x hash %s: targetBytes says %v, big.Int says %v", bits, h[:8], got, want)
			}
		}
	}
}

// Mine hashes a preimage it builds itself, so the block it returns has to agree
// with ComputeHash — a drift between the two would produce blocks that the miner
// thinks are solved and every other node rejects.
func TestMineReturnsABlockThatValidates(t *testing.T) {
	bc := NewBlockchain()
	tip := bc.Tip()
	b := Block{
		Index:        tip.Index + 1,
		Timestamp:    tip.Timestamp + 1,
		Transactions: []Transaction{NewCoinbase("dnas-miner", 50*Coin)},
		PrevHash:     tip.Hash,
		BaseFee:      bc.NextBaseFee(),
		Bits:         PowLimitBits, // the easiest allowed target keeps this test cheap
	}
	mined, ok := Mine(b, nil)
	if !ok {
		t.Fatal("mining aborted with no abort function")
	}
	if mined.Hash != mined.ComputeHash() {
		t.Fatalf("mined hash %s is not the hash of its own header", mined.Hash)
	}
	if !mined.HasValidPoW() {
		t.Fatal("mined block does not satisfy the target it committed to")
	}
	if mined.MerkleRoot != MerkleRoot(mined.Transactions) {
		t.Fatal("Mine did not commit the merkle root of its transactions")
	}
	// It stopped at the FIRST solution: skipping nonces would be lost work, and
	// (worse) would mean the loop's preimage disagrees with ComputeHash somewhere.
	for n := uint64(0); n < mined.Nonce; n++ {
		probe := mined
		probe.Nonce = n
		if meetsTarget(probe.ComputeHash(), probe.Bits) {
			t.Fatalf("nonce %d already solved the block but Mine ran on to %d", n, mined.Nonce)
		}
	}
}

func TestMineStopsWhenAborted(t *testing.T) {
	b := Block{Index: 1, Transactions: []Transaction{NewCoinbase("dnas-miner", Coin)}, Bits: GenesisBits}
	if _, ok := Mine(b, func() bool { return true }); ok {
		t.Fatal("Mine reported success despite being aborted before its first attempt")
	}
}
