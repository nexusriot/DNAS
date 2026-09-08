package node

import (
	"testing"

	"github.com/nexusriot/DNAS/core"
)

// The node's job is to collect peer clock offsets and hand the median to core.
// These tests pin that plumbing: peers without a sample are skipped, and the
// offset that gets applied is the median of the rest.

func peerWithOffset(off int64, has bool) *peer {
	return &peer{timeOffset: off, hasTimeOffset: has}
}

func nodeWithPeers(ps ...*peer) *Node {
	n := &Node{peers: map[*peer]bool{}}
	for _, p := range ps {
		n.peers[p] = true
	}
	return n
}

func TestPeerTimeOffsetsSkipsPeersWithoutASample(t *testing.T) {
	n := nodeWithPeers(
		peerWithOffset(10, true),
		peerWithOffset(0, false), // a peer predating the Time field
		peerWithOffset(-4, true),
	)
	got := n.peerTimeOffsets()
	if len(got) != 2 {
		t.Fatalf("collected %d offsets from 3 peers (one has no sample), want 2: %v", len(got), got)
	}
	for _, v := range got {
		if v != 10 && v != -4 {
			t.Errorf("unexpected offset %d", v)
		}
	}
}

func TestSyncNetworkTimeAppliesTheMedian(t *testing.T) {
	t.Cleanup(func() { core.SetTimeOffset(0) })

	n := nodeWithPeers(
		peerWithOffset(30, true),
		peerWithOffset(31, true),
		peerWithOffset(29, true),
	)
	offset, samples := n.syncNetworkTime()
	if samples != 3 {
		t.Errorf("samples = %d, want 3", samples)
	}
	if offset != 30 {
		t.Errorf("applied offset = %d, want the median 30", offset)
	}
	if core.TimeOffset() != 30 {
		t.Errorf("core offset = %d, want 30", core.TimeOffset())
	}
}

// A node with no peers, or one peer, must not adjust anything: its own clock is
// better evidence than a single stranger's.
func TestSyncNetworkTimeIgnoresTooFewPeers(t *testing.T) {
	t.Cleanup(func() { core.SetTimeOffset(0) })
	core.SetTimeOffset(0)

	for _, n := range []*Node{
		nodeWithPeers(),
		nodeWithPeers(peerWithOffset(3600, true)),
	} {
		offset, _ := n.syncNetworkTime()
		if offset != 0 {
			t.Errorf("with %d peers the applied offset was %d, want 0", len(n.peers), offset)
		}
	}
}

// A hostile majority is still capped by core's bound, checked here through the
// node's own path rather than only in core.
func TestSyncNetworkTimeIsBounded(t *testing.T) {
	t.Cleanup(func() { core.SetTimeOffset(0) })

	var ps []*peer
	for i := 0; i < 8; i++ {
		ps = append(ps, peerWithOffset(48*3600, true)) // all claim to be 2 days ahead
	}
	offset, _ := nodeWithPeers(ps...).syncNetworkTime()
	if offset != core.MaxTimeOffset {
		t.Errorf("a unanimous hostile peer set moved the clock by %d, want the bound %d",
			offset, core.MaxTimeOffset)
	}
}
