package core

import (
	"sync/atomic"
	"time"
)

// Network-adjusted time.
//
// Timestamp validation read the LOCAL clock: a block was rejected if it claimed
// to be more than MaxFutureDrift ahead of `time.Now()`. That makes one node's
// wrong clock a consensus problem for that node. A machine an hour behind
// rejects every block the network produces and stops following the chain; a
// machine an hour ahead accepts blocks nobody else will, and mines on a tip its
// peers refuse.
//
// So the check uses an offset agreed with peers instead, the way bitcoind does:
// each peer reports its own clock during the handshake, the node takes the
// MEDIAN of those offsets, and adds it to the local clock for validation only.
//
// Three properties make that safe to depend on:
//
//   - The median, not the mean. One peer lying wildly moves the median by at
//     most one position, and it takes a majority of a node's peers to move it
//     meaningfully.
//   - A hard bound. However the peers vote, the offset is clamped to
//     MaxTimeOffset. An attacker who did control a node's whole peer set could
//     otherwise walk its clock arbitrarily far and dictate which blocks it
//     accepts — the clamp turns that from "unbounded" into "at most this much,
//     and it is visible".
//   - It never touches the system clock. Nothing outside timestamp validation
//     sees this, so a wrong offset cannot corrupt wallets, logs or schedules.
//
// This is deliberately weaker than a real time protocol. It removes the failure
// where ONE node's clock isolates it; it cannot fix a network whose peers all
// agree on the wrong time. The threat model says so rather than implying NTP.

// MaxTimeOffset bounds how far peers may move this node's idea of the time,
// in either direction. Well under MaxFutureDrift, so a hostile peer majority
// cannot use the offset to widen the drift window it is checked against.
const MaxTimeOffset int64 = 70 * 60

// timeOffset is the agreed adjustment in seconds, applied to the local clock
// for timestamp validation. Atomic because peers update it while blocks are
// being validated.
var timeOffset atomic.Int64

// SetTimeOffset records the network-agreed clock offset, clamped to
// MaxTimeOffset. It returns the value actually applied, so a caller can log
// when peers wanted more than they were given.
func SetTimeOffset(seconds int64) int64 {
	if seconds > MaxTimeOffset {
		seconds = MaxTimeOffset
	}
	if seconds < -MaxTimeOffset {
		seconds = -MaxTimeOffset
	}
	timeOffset.Store(seconds)
	return seconds
}

// TimeOffset is the offset currently applied.
func TimeOffset() int64 { return timeOffset.Load() }

// NetworkTime is the current time as this node's peers see it: the local clock
// plus the agreed offset. Timestamp validation uses this rather than
// time.Now(), so a single skewed clock does not isolate a node.
func NetworkTime() int64 { return time.Now().Unix() + timeOffset.Load() }

// MedianOffset returns the median of a set of per-peer offsets, or 0 when there
// are none. Peers are the only evidence available and a lone peer is not
// evidence, so a single sample is deliberately ignored.
func MedianOffset(offsets []int64) int64 {
	if len(offsets) < 2 {
		return 0
	}
	sorted := make([]int64, len(offsets))
	copy(sorted, offsets)
	// Insertion sort: the list is bounded by the outbound peer count.
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j-1] > sorted[j]; j-- {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	// An even count has no single middle. Averaging the two keeps the result
	// inside the samples' range, which picking either side does not guarantee.
	return (sorted[mid-1] + sorted[mid]) / 2
}
