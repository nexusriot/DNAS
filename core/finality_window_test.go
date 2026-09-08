package core

import (
	"testing"
	"time"
)

// The finality guard refuses a reorg deeper than MaxReorgDepth. That stops a
// deep-reorg attack, and it also means two halves of a partition that diverged
// by more than that many blocks can NEVER reconverge: each would need a
// rollback the other's consensus refuses. The guard turns an attack into a
// permanent split.
//
// So the number that matters is not the block count, it is the wall-clock
// window that count represents. This test states the minimum that window may
// shrink to, and fails if a change to either MaxReorgDepth or TargetBlockTime
// quietly takes it below that. It is the guard that was missing when
// MaxReorgDepth = 100 and TargetBlockTime = 5 combined into eight minutes.

// minSurvivableWindow is the shortest partition the chain must be able to heal
// from unaided. Chosen as a deliberately low bar: anything under this is not a
// judgement call, it is a network that splits on a bad afternoon.
const minSurvivableWindow = 30 * time.Minute

func TestFinalityWindowIsSurvivable(t *testing.T) {
	window := time.Duration(FinalityWindow) * time.Second
	t.Logf("finality window = %d blocks x %ds = %s",
		MaxReorgDepth, TargetBlockTime, window)

	if window < minSurvivableWindow {
		t.Errorf(
			"the finality window is %s (MaxReorgDepth %d x TargetBlockTime %ds).\n"+
				"A partition longer than that can never reconverge: both sides would need\n"+
				"a rollback consensus refuses. Raise TargetBlockTime (cheap: it does not\n"+
				"affect MinPruneKeep) or MaxReorgDepth (costlier: MinPruneKeep tracks it,\n"+
				"so pruning nodes must retain more bodies). Minimum here is %s.",
			window, MaxReorgDepth, TargetBlockTime, minSurvivableWindow)
	}
}

// FinalityWindow must stay derived from the two inputs rather than drifting into
// a hand-maintained constant.
func TestFinalityWindowIsDerived(t *testing.T) {
	if want := int64(MaxReorgDepth) * TargetBlockTime; FinalityWindow != want {
		t.Errorf("FinalityWindow = %d but MaxReorgDepth x TargetBlockTime = %d", FinalityWindow, want)
	}
}

// Coinbase maturity is the other place block time turns into a real duration:
// it is how long a miner waits before a reward is spendable, and how deep a
// reorg has to be to strand one that was already spent.
func TestCoinbaseMaturityIsNotTrivial(t *testing.T) {
	maturity := time.Duration(int64(CoinbaseMaturity)*TargetBlockTime) * time.Second
	t.Logf("coinbase maturity = %d blocks x %ds = %s", CoinbaseMaturity, TargetBlockTime, maturity)

	// Deliberately a low bar, and deliberately separate from the finality window:
	// raising maturity slows every test and demo that mines then spends, so it is
	// its own decision. This only asserts it has not become meaningless.
	if maturity < time.Minute {
		t.Errorf("coinbase maturity is %s (%d blocks at %ds). A reorg that shallow is "+
			"routine, so a reward can be spent and then stranded.",
			maturity, CoinbaseMaturity, TargetBlockTime)
	}
}
