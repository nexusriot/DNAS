package node

import (
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// mineToShare searches for a nonce that meets the SHARE target but (usually)
// not the block target, which is what an ordinary miner spends its time
// producing. It returns the first candidate meeting the share target.
func mineToShare(t *testing.T, tmpl core.Block, shareBits uint32) core.Block {
	t.Helper()
	tmpl.MerkleRoot = core.MerkleRoot(tmpl.Transactions)
	for i := 0; i < 1_000_000; i++ {
		tmpl.Hash = tmpl.ComputeHash()
		if core.MeetsShareTarget(tmpl.Hash, shareBits) {
			return tmpl
		}
		tmpl.Nonce++
	}
	t.Fatal("no share found in a million attempts")
	return tmpl
}

func TestShareTargetIsEasierThanTheBlockTarget(t *testing.T) {
	blockBits := core.GenesisBits
	shareBits := core.ShareBits(blockBits, core.DefaultShareFactor)
	if core.CompactToBig(shareBits).Cmp(core.CompactToBig(blockBits)) <= 0 {
		t.Fatal("the share target must be numerically larger (easier) than the block target")
	}
	// Anything that clears the block target clears the share target too, so a
	// miner never has to decide which of the two it found.
	if core.ShareBits(blockBits, 0) != blockBits {
		t.Fatal("a factor of zero should leave the target alone")
	}
}

func TestSubmitShareCreditsTheCoinbaseAddress(t *testing.T) {
	n, _, w := fundedNode(t)
	tmpl, err := n.BuildTemplate(w.Address())
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	share := mineToShare(t, tmpl, n.ShareBits())
	res, err := n.SubmitShare(share)
	if err != nil {
		t.Fatalf("submit share: %v", err)
	}
	if !res.Accepted || res.Address != w.Address() || res.Shares != 1 {
		t.Fatalf("unexpected result %+v", res)
	}
	report := n.Shares()
	if report.Submitted != 1 || report.Accepted != 1 {
		t.Fatalf("report = %+v, want one submitted and one accepted", report)
	}
	if len(report.Miners) != 1 || report.Miners[0].Address != w.Address() {
		t.Fatalf("miners = %+v, want one row for the coinbase address", report.Miners)
	}
}

// A share that happens to clear the real target is a block, and the node must
// treat it as one rather than merely counting it.
func TestShareThatMeetsTheBlockTargetBecomesABlock(t *testing.T) {
	n, _, w := fundedNode(t)
	tmpl, err := n.BuildTemplate(w.Address())
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	mined, ok := core.Mine(tmpl, nil)
	if !ok {
		t.Fatal("mining aborted")
	}
	before := n.Chain().Height()
	res, err := n.SubmitShare(mined)
	if err != nil {
		t.Fatalf("submit share: %v", err)
	}
	if !res.Block {
		t.Fatal("a share meeting the block target was not accepted as a block")
	}
	if got := n.Chain().Height(); got != before+1 {
		t.Fatalf("height = %d, want %d", got, before+1)
	}
	if n.Shares().Blocks != 1 {
		t.Fatalf("share report should record one block, got %d", n.Shares().Blocks)
	}
}

func TestStaleAndUnderTargetSharesRejected(t *testing.T) {
	n, _, w := fundedNode(t)
	tmpl, err := n.BuildTemplate(w.Address())
	if err != nil {
		t.Fatalf("template: %v", err)
	}

	// A hash that meets nothing: nonce 0 on a fresh template is overwhelmingly
	// unlikely to clear even the share target.
	weak := tmpl
	weak.MerkleRoot = core.MerkleRoot(weak.Transactions)
	weak.Hash = weak.ComputeHash()
	if !core.MeetsShareTarget(weak.Hash, n.ShareBits()) {
		if _, err := n.SubmitShare(weak); err == nil {
			t.Fatal("a hash below the share target was accepted")
		}
	}

	// A share for a height that has already been mined is stale.
	share := mineToShare(t, tmpl, n.ShareBits())
	if _, err := n.Generate(1); err != nil {
		t.Fatalf("generate: %v", err)
	}
	_, err = n.SubmitShare(share)
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("expected a stale-share rejection, got %v", err)
	}
	if n.Shares().Stale != 1 {
		t.Fatalf("stale count = %d, want 1", n.Shares().Stale)
	}
}

func TestSubmitShareRejectsATamperedTemplate(t *testing.T) {
	n, _, w := fundedNode(t)
	tmpl, err := n.BuildTemplate(w.Address())
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	share := mineToShare(t, tmpl, n.ShareBits())
	// Swapping the coinbase recipient after the fact must not credit the thief:
	// the hash no longer matches the header it claims.
	thief, _ := wallet.New()
	share.Transactions[0].To = thief.Address()
	if _, err := n.SubmitShare(share); err == nil {
		t.Fatal("a share whose transactions were changed after hashing was accepted")
	}
}

// The long poll must return as soon as the tip moves, and time out otherwise.
func TestWaitForTip(t *testing.T) {
	n, _, _ := fundedNode(t)
	tip := n.Chain().Tip().Hash

	if n.WaitForTip(tip, 100*time.Millisecond) {
		t.Fatal("WaitForTip reported a change while the tip stood still")
	}
	if !n.WaitForTip("some-other-hash", time.Second) {
		t.Fatal("a caller holding a stale tip should be answered immediately")
	}

	done := make(chan bool, 1)
	go func() { done <- n.WaitForTip(tip, 10*time.Second) }()
	time.Sleep(50 * time.Millisecond)
	if _, err := n.Generate(1); err != nil {
		t.Fatalf("generate: %v", err)
	}
	select {
	case changed := <-done:
		if !changed {
			t.Fatal("WaitForTip returned without seeing the new tip")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitForTip did not wake when the tip changed")
	}
}
