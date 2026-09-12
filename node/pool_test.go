package node

import (
	"testing"
	"time"

	"github.com/nexusriot/DNAS/core"
)

// A payout must track WORK, not share count: with per-miner difficulty, a miner
// on an easy target submits many cheap shares and one on a hard target submits
// few expensive ones. Weighting by each share's own difficulty is what makes
// those two miners earn in proportion to the hashing they actually did.
func TestPayoutsTrackWorkNotShareCount(t *testing.T) {
	w := newShareWindow(pplnsWindow)
	// "easy" submits 10 shares worth 1 each; "hard" submits 1 worth 10. Equal work.
	for i := 0; i < 10; i++ {
		w.add(PoolShare{Address: "easy", Weight: 1})
	}
	w.add(PoolShare{Address: "hard", Weight: 10})

	total, payouts := w.payouts(100)
	if total != 20 {
		t.Fatalf("window weight = %v, want 20", total)
	}
	if len(payouts) != 2 {
		t.Fatalf("got %d payouts, want 2", len(payouts))
	}
	byAddr := map[string]PoolPayout{}
	for _, p := range payouts {
		byAddr[p.Address] = p
	}
	if byAddr["easy"].Amount != 50 || byAddr["hard"].Amount != 50 {
		t.Fatalf("equal work paid unequally: easy=%d hard=%d",
			byAddr["easy"].Amount, byAddr["hard"].Amount)
	}
	if byAddr["easy"].Shares != 10 || byAddr["hard"].Shares != 1 {
		t.Errorf("share counts wrong: easy=%d hard=%d", byAddr["easy"].Shares, byAddr["hard"].Shares)
	}
}

// Integer division leaves a remainder. It must be assigned, not burned: a pool
// that pays out less than the block it found is quietly keeping the difference.
func TestPayoutsAssignTheWholeReward(t *testing.T) {
	w := newShareWindow(pplnsWindow)
	for _, addr := range []string{"a", "b", "c"} {
		w.add(PoolShare{Address: addr, Weight: 1})
	}
	const reward = 100 // not divisible by three
	_, payouts := w.payouts(reward)

	var sum uint64
	for _, p := range payouts {
		sum += p.Amount
	}
	if sum != reward {
		t.Fatalf("payouts sum to %d, want the whole reward %d", sum, reward)
	}
}

func TestEmptyWindowPaysNobody(t *testing.T) {
	w := newShareWindow(pplnsWindow)
	total, payouts := w.payouts(core.InitialBlockReward)
	if total != 0 || len(payouts) != 0 {
		t.Fatalf("an empty window produced %d payouts (weight %v)", len(payouts), total)
	}
}

// The window is what bounds a pool's memory, and dropping the oldest share must
// drop its weight too — otherwise the denominator grows forever and every payout
// shrinks toward zero.
func TestShareWindowIsBoundedAndKeepsItsTotalHonest(t *testing.T) {
	w := newShareWindow(4)
	for i := 0; i < 10; i++ {
		w.add(PoolShare{Address: "a", Weight: 1})
	}
	if got := len(w.snapshot()); got != 4 {
		t.Fatalf("window holds %d shares, cap is 4", got)
	}
	if total, _ := w.payouts(100); total != 4 {
		t.Fatalf("window weight = %v, want 4 (evicted shares still counted)", total)
	}
}

func TestRestoredWindowCannotExceedCapacity(t *testing.T) {
	w := newShareWindow(4)
	saved := make([]PoolShare, 10)
	for i := range saved {
		saved[i] = PoolShare{Address: "a", Weight: 1}
	}
	w.restore(saved)
	if got := len(w.snapshot()); got != 4 {
		t.Fatalf("restored %d shares into a window of 4", got)
	}
	if total, _ := w.payouts(100); total != 4 {
		t.Errorf("restored weight = %v, want 4", total)
	}
}

// Vardiff exists so that a miner's submission rate lands near the target however
// fast it is: too frequent and the factor tightens, too rare and it eases.
func TestVardiffTightensForAFastMiner(t *testing.T) {
	v := newVardiff(1024)
	now := time.Now()
	// 60 shares in 30s is one every half second — six times the target rate.
	for i := 0; i < 59; i++ {
		if _, changed := v.observe(now); changed {
			t.Fatal("adjusted before the interval elapsed")
		}
	}
	next, changed := v.observe(now.Add(vardiffInterval))
	if !changed {
		t.Fatal("a miner submitting six times too fast was left alone")
	}
	if next >= 1024 {
		t.Fatalf("factor rose to %d for a fast miner, want a tighter target", next)
	}
}

func TestVardiffEasesForASlowMiner(t *testing.T) {
	v := newVardiff(1024)
	now := time.Now()
	for i := 0; i < vardiffMinShare-1; i++ {
		v.observe(now)
	}
	// Four shares spread over four minutes: one a minute, six times too slow.
	next, changed := v.observe(now.Add(4 * time.Minute))
	if !changed {
		t.Fatal("a miner submitting six times too slowly was left alone")
	}
	if next <= 1024 {
		t.Fatalf("factor fell to %d for a slow miner, want an easier target", next)
	}
}

// A brief stall must not hand a miner a target so easy it floods the pool when
// it comes back, so one adjustment may move the factor by at most 4x.
func TestVardiffStepIsClamped(t *testing.T) {
	v := newVardiff(1000)
	now := time.Now()
	for i := 0; i < vardiffMinShare-1; i++ {
		v.observe(now)
	}
	next, _ := v.observe(now.Add(time.Hour)) // absurdly slow
	if next > 4000 {
		t.Fatalf("one adjustment moved the factor to %d, more than 4x", next)
	}
}

func TestVardiffStaysInRange(t *testing.T) {
	v := newVardiff(minShareFactor)
	now := time.Now()
	for round := 0; round < 20; round++ {
		for i := 0; i < vardiffMinShare; i++ {
			v.observe(now)
		}
		now = now.Add(vardiffInterval + time.Second)
		v.observe(now)
		if v.factor < minShareFactor || v.factor > maxShareFactor {
			t.Fatalf("factor left its range: %d", v.factor)
		}
	}
}

// The pool report is what a miner checks to see what it is owed.
func TestPoolReportNamesWhatIsOwed(t *testing.T) {
	n, _, _ := fundedNode(t)
	n.window.add(PoolShare{Address: "alice", Weight: 3})
	n.window.add(PoolShare{Address: "bob", Weight: 1})

	r := n.Pool()
	if r.Window != 2 || r.Weight != 4 {
		t.Fatalf("window = %d shares / weight %v, want 2 / 4", r.Window, r.Weight)
	}
	if r.Reward != core.BlockReward(n.Chain().Height()+1) {
		t.Errorf("reward = %d, want the next block's subsidy", r.Reward)
	}
	if len(r.Payouts) != 2 || r.Payouts[0].Address != "alice" {
		t.Fatalf("payouts = %+v, want alice first", r.Payouts)
	}
	if r.Payouts[0].Percent != 75 {
		t.Errorf("alice's share = %v%%, want 75", r.Payouts[0].Percent)
	}
}
