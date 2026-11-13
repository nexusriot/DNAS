package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// A real chain's headers pass; the same chain re-mined at the easiest target
// the header format allows (PowLimit) links and meets its OWN stated target, so
// ValidateHeaderChain accepts it — and ValidateHeaderDifficulty must not.
func TestValidateHeaderDifficultyRejectsAForgedEasyChain(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 5; i++ {
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	}
	real := bc.Headers()
	if err := ValidateHeaderDifficulty(real, 1); err != nil {
		t.Fatalf("the real chain was refused: %v", err)
	}

	forged := []Header{real[0]}
	prev := real[0]
	for i := 1; i <= 50; i++ {
		b := Block{Index: uint64(i), Timestamp: prev.Timestamp + TargetBlockTime, PrevHash: prev.Hash,
			MerkleRoot: "00", StateRoot: "deadbeef", Bits: PowLimitBits}
		mined, ok := Mine(b, nil)
		if !ok {
			t.Fatal("mining aborted")
		}
		forged = append(forged, mined.Header())
		prev = mined.Header()
	}
	if err := ValidateHeaderChain(forged[1:], forged[0].Hash, 0); err != nil {
		t.Fatalf("precondition: the forged chain should pass the self-declared-work check: %v", err)
	}
	if err := ValidateHeaderDifficulty(forged, 1); err == nil {
		t.Fatal("a chain forged at the easiest target passed the difficulty check")
	}
}

// With retargeting on, the check must agree with the full node's own retarget
// past the LWMA window, so it never refuses an honest chain.
func TestValidateHeaderDifficultyAgreesWithTheRetarget(t *testing.T) {
	withRetarget(t)
	headers := make([]Header, 0, lwmaWindow+10)
	blocks := synthBits(1, TargetBlockTime, GenesisBits)
	headers = append(headers, Header{Index: 0, Timestamp: blocks[0].Timestamp, Bits: GenesisBits})
	for i := 1; i < lwmaWindow+10; i++ {
		spacing := int64(TargetBlockTime / 2) // fast blocks: the target tightens
		b := Block{Index: uint64(i), Timestamp: blocks[i-1].Timestamp + spacing}
		b.Bits = expectedBits(blocks, uint64(i))
		blocks = append(blocks, b)
		headers = append(headers, Header{Index: b.Index, Timestamp: b.Timestamp, Bits: b.Bits})
	}
	if err := ValidateHeaderDifficulty(headers, 1); err != nil {
		t.Fatalf("an honest retargeting chain was refused: %v", err)
	}
	headers[len(headers)-1].Bits = PowLimitBits
	if err := ValidateHeaderDifficulty(headers, 1); err == nil {
		t.Fatal("a header past the window claiming the easiest target was accepted")
	}
}

// A run anchored mid-chain checks every header that has the retarget window
// beneath it inside the run, agrees with the full retarget there, says how many
// it checked, and refuses a forged target among them.
func TestValidateHeaderRunDifficultyAnchoredMidChain(t *testing.T) {
	withRetarget(t)
	n := 3*lwmaWindow + 5
	blocks := synthBits(1, TargetBlockTime, GenesisBits)
	for i := 1; i < n; i++ {
		spacing := int64(TargetBlockTime / 2)
		if i%3 == 0 {
			spacing = 2 * TargetBlockTime
		}
		b := Block{Index: uint64(i), Timestamp: blocks[i-1].Timestamp + spacing}
		b.Bits = expectedBits(blocks, uint64(i))
		blocks = append(blocks, b)
	}
	headers := make([]Header, n)
	for i, b := range blocks {
		headers[i] = Header{Index: b.Index, Timestamp: b.Timestamp, Bits: b.Bits}
	}

	anchor := lwmaWindow + 7
	run := headers[anchor:]
	checked, err := ValidateHeaderRunDifficulty(run)
	if err != nil {
		t.Fatalf("an honest run was refused: %v", err)
	}
	if want := len(run) - (lwmaWindow + 1); checked != want {
		t.Fatalf("checked %d headers, want %d (all but the first lwmaWindow+1)", checked, want)
	}

	forged := append([]Header(nil), run...)
	forged[len(forged)-1].Bits = PowLimitBits
	if _, err := ValidateHeaderRunDifficulty(forged); err == nil {
		t.Fatal("a forged target past the window was accepted")
	}

	// From genesis it agrees with ValidateHeaderDifficulty and checks them all.
	if checked, err := ValidateHeaderRunDifficulty(headers); err != nil || checked != n-1 {
		t.Fatalf("from genesis: checked %d, err %v; want %d, nil", checked, err, n-1)
	}
	gap := append([]Header(nil), run...)
	gap[3].Index++
	if _, err := ValidateHeaderRunDifficulty(gap); err == nil {
		t.Fatal("a non-contiguous run was accepted")
	}
}
