package core

import (
	"testing"
	"time"
)

// Timestamp validation used to read the local clock, which made one machine's
// wrong clock that machine's consensus problem. These tests pin the two things
// that make the peer-agreed offset safe to depend on: the median resists a
// lying minority, and the bound resists a lying majority.

func TestMedianOffsetIgnoresASingleSample(t *testing.T) {
	// A lone peer is not evidence. Trusting one would let any single peer set
	// this node's clock, which is strictly worse than trusting our own.
	if got := MedianOffset([]int64{9999}); got != 0 {
		t.Errorf("one sample gave offset %d, want 0", got)
	}
	if got := MedianOffset(nil); got != 0 {
		t.Errorf("no samples gave offset %d, want 0", got)
	}
}

func TestMedianOffsetPicksTheMiddle(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []int64
		want int64
	}{
		{"odd count", []int64{-5, 0, 5}, 0},
		{"even count averages the middle pair", []int64{0, 10}, 5},
		{"unsorted input", []int64{100, -100, 3, 4, 5}, 4},
		{"all equal", []int64{7, 7, 7}, 7},
		{"negative", []int64{-30, -20, -10}, -20},
	} {
		if got := MedianOffset(tc.in); got != tc.want {
			t.Errorf("%s: MedianOffset(%v) = %d, want %d", tc.name, tc.in, got, tc.want)
		}
	}
}

// The median must not be movable by a minority, however extreme their claims.
func TestMedianResistsALyingMinority(t *testing.T) {
	honest := []int64{0, 1, -1, 2, -2, 0, 1}
	base := MedianOffset(honest)

	// Three liars against seven honest peers.
	poisoned := append(append([]int64(nil), honest...), 1<<40, -(1 << 40), 1<<40)
	got := MedianOffset(poisoned)
	if got < base-3 || got > base+3 {
		t.Errorf("three extreme liars moved the median from %d to %d", base, got)
	}
}

// And a majority that DOES control the vote must still be capped.
func TestOffsetIsClampedToTheBound(t *testing.T) {
	t.Cleanup(func() { SetTimeOffset(0) })

	if got := SetTimeOffset(1 << 40); got != MaxTimeOffset {
		t.Errorf("a huge positive offset applied as %d, want the bound %d", got, MaxTimeOffset)
	}
	if got := TimeOffset(); got != MaxTimeOffset {
		t.Errorf("TimeOffset = %d after clamping, want %d", got, MaxTimeOffset)
	}
	if got := SetTimeOffset(-(1 << 40)); got != -MaxTimeOffset {
		t.Errorf("a huge negative offset applied as %d, want %d", got, -MaxTimeOffset)
	}
	// Inside the bound it is applied verbatim.
	if got := SetTimeOffset(42); got != 42 {
		t.Errorf("an in-bound offset applied as %d, want 42", got)
	}
}

// The bound must stay below MaxFutureDrift, or a hostile peer majority could
// use the offset to widen the very window it is checked against.
func TestOffsetBoundCannotWidenTheDriftWindow(t *testing.T) {
	if MaxTimeOffset >= MaxFutureDrift*60 {
		t.Errorf("MaxTimeOffset (%d) is large relative to MaxFutureDrift (%d); "+
			"peers could shift the accepted window substantially", MaxTimeOffset, MaxFutureDrift)
	}
}

func TestNetworkTimeAppliesTheOffset(t *testing.T) {
	t.Cleanup(func() { SetTimeOffset(0) })

	SetTimeOffset(0)
	local := time.Now().Unix()
	if got := NetworkTime(); got < local-1 || got > local+1 {
		t.Errorf("with no offset NetworkTime = %d, want ~%d", got, local)
	}

	SetTimeOffset(600)
	if got := NetworkTime() - time.Now().Unix(); got < 599 || got > 601 {
		t.Errorf("with a +600 offset NetworkTime is %d ahead, want ~600", got)
	}
}

// The behaviour that motivated all of this: a node whose own clock runs behind
// must still accept the blocks its peers are producing.
func TestSkewedLocalClockStillAcceptsPeerBlocks(t *testing.T) {
	t.Cleanup(func() { SetTimeOffset(0) })

	// A block stamped with the network's real time, on a node whose clock is
	// half an hour behind it. Without the offset this is "too far in the future".
	behindBy := int64(30 * 60)
	blockTime := time.Now().Unix() + behindBy

	SetTimeOffset(0) // pretend we have no peers: the old behaviour
	if blockTime <= NetworkTime()+MaxFutureDrift {
		t.Fatalf("the fixture is not exercising the drift check "+
			"(block %d vs limit %d)", blockTime, NetworkTime()+MaxFutureDrift)
	}

	SetTimeOffset(behindBy) // peers tell us we are behind
	if blockTime > NetworkTime()+MaxFutureDrift {
		t.Errorf("with the peer-agreed offset applied the block should be accepted: "+
			"block %d, limit %d", blockTime, NetworkTime()+MaxFutureDrift)
	}
}
