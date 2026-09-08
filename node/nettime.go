package node

import (
	"time"

	"github.com/nexusriot/DNAS/core"
)

// Keeping this node's idea of the time in step with its peers.
//
// Timestamp validation used the local clock, which makes one machine's wrong
// clock that machine's consensus problem: an hour behind and it rejects every
// block the network produces; an hour ahead and it mines on a tip its peers
// refuse. Peers report their own clock at handshake; the median of those
// offsets, bounded, is what validation adds to the local clock.
//
// The median is recomputed periodically rather than on every handshake, because
// the useful signal is the steady state across a stable peer set, not the jitter
// of one connection arriving.

// timeSyncInterval is how often the peer clock offsets are re-polled.
const timeSyncInterval = 60 * time.Second

// peerTimeOffsets collects the per-peer clock offsets currently known.
func (n *Node) peerTimeOffsets() []int64 {
	n.peersMu.Lock()
	defer n.peersMu.Unlock()
	out := make([]int64, 0, len(n.peers))
	for p := range n.peers {
		if p.hasTimeOffset {
			out = append(out, p.timeOffset)
		}
	}
	return out
}

// syncNetworkTime recomputes the agreed clock offset from this node's peers and
// applies it. It returns the offset applied and how many peers voted.
func (n *Node) syncNetworkTime() (offset int64, samples int) {
	offsets := n.peerTimeOffsets()
	// MedianOffset ignores fewer than two samples: a lone peer is not evidence,
	// and trusting one would let a single peer set this node's clock.
	applied := core.SetTimeOffset(core.MedianOffset(offsets))
	return applied, len(offsets)
}

// timeSyncLoop keeps the offset current for the life of the node.
func (n *Node) timeSyncLoop() {
	t := time.NewTicker(timeSyncInterval)
	defer t.Stop()
	var lastWarned int64
	for {
		select {
		case <-n.quit:
			return
		case <-t.C:
			offset, samples := n.syncNetworkTime()
			if samples == 0 {
				continue
			}
			Debugf("network time synced", "offset_s", offset, "peers", samples)
			// A large standing offset means this node's own clock is wrong, which
			// is worth saying once per change rather than every minute: the
			// adjustment keeps it on the chain, but the machine still needs fixing
			// before the offset hits its bound and stops compensating.
			if offset != lastWarned && (offset > 60 || offset < -60) {
				Warnf("local clock disagrees with peers",
					"offset_s", offset,
					"bound_s", core.MaxTimeOffset,
					"impact", "timestamp validation is being adjusted to compensate",
					"fix", "correct this machine's clock (NTP); the adjustment is capped")
				lastWarned = offset
			}
		}
	}
}
