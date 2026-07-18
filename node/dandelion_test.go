package node

import (
	"testing"
	"time"

	"github.com/nexusriot/DNAS/core"
)

func dandPeers(ids ...string) []*peer {
	var ps []*peer
	for _, id := range ids {
		ps = append(ps, &peer{id: id, caps: map[string]bool{CapDandelion: true}})
	}
	return ps
}

func containsPeer(ps []*peer, x *peer) bool {
	for _, p := range ps {
		if p == x {
			return true
		}
	}
	return false
}

// TestDandelionPickEpochStable checks the stem successor is stable within an
// epoch and re-chosen afterwards — the property that makes relayed and originated
// transactions indistinguishable.
func TestDandelionPickEpochStable(t *testing.T) {
	d := newDandelion()
	peers := dandPeers("a", "b", "c")
	t0 := time.Unix(1000, 0)

	first := d.pick(peers, t0)
	if first == nil {
		t.Fatal("pick should return a successor when candidates exist")
	}
	if again := d.pick(peers, t0.Add(dandEpochDuration-time.Second)); again != first {
		t.Fatalf("stem successor should be stable within an epoch: %v vs %v", again.id, first.id)
	}
	if next := d.pick(peers, t0.Add(dandEpochDuration+time.Second)); next == nil || !containsPeer(peers, next) {
		t.Fatal("after the epoch, pick should choose a valid candidate")
	}
}

// TestDandelionPickRepicksWhenSuccessorGone confirms a disconnected successor is
// replaced even mid-epoch.
func TestDandelionPickRepicksWhenSuccessorGone(t *testing.T) {
	d := newDandelion()
	all := dandPeers("a", "b", "c")
	t0 := time.Unix(2000, 0)
	chosen := d.pick(all, t0)

	var remaining []*peer
	for _, p := range all {
		if p.id != chosen.id {
			remaining = append(remaining, p)
		}
	}
	got := d.pick(remaining, t0.Add(time.Second))
	if got == nil || got.id == chosen.id {
		t.Fatalf("pick should choose a still-connected successor, got %v", got)
	}
}

func TestDandelionPickNoCandidates(t *testing.T) {
	d := newDandelion()
	if p := d.pick(nil, time.Unix(3000, 0)); p != nil {
		t.Fatalf("pick with no candidates should be nil, got %v", p.id)
	}
}

// TestDandelionEmbargo confirms an un-cancelled embargo fluffs, and a cancelled
// one does not.
func TestDandelionEmbargo(t *testing.T) {
	old := dandEmbargo
	dandEmbargo = 20 * time.Millisecond
	defer func() { dandEmbargo = old }()

	d := newDandelion()
	fired := make(chan struct{}, 1)
	d.startEmbargo("h1", func() { fired <- struct{}{} })
	select {
	case <-fired:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("embargo should have fired a fluff")
	}

	d.startEmbargo("h2", func() { fired <- struct{}{} })
	d.cancelEmbargo("h2")
	select {
	case <-fired:
		t.Fatal("a cancelled embargo must not fire")
	case <-time.After(80 * time.Millisecond):
	}
}

// TestDandelionRollFluffDistribution sanity-checks the fluff probability.
func TestDandelionRollFluffDistribution(t *testing.T) {
	d := newDandelion()
	const n = 3000
	trues := 0
	for i := 0; i < n; i++ {
		if d.rollFluff() {
			trues++
		}
	}
	frac := float64(trues) / n
	lo, hi := 1.0/dandFluffDenom-0.1, 1.0/dandFluffDenom+0.1
	if frac < lo || frac > hi {
		t.Fatalf("rollFluff fraction %.3f out of expected band [%.2f, %.2f]", frac, lo, hi)
	}
}

// TestNodeCapsAndRelayFallback checks capability advertisement and that a stem
// relay with no capable successor safely fluffs without arming an embargo.
func TestNodeCapsAndRelayFallback(t *testing.T) {
	on := New(Config{ListenAddr: ":0", Dandelion: true}, core.NewBlockchain(), core.NewMempool(), nil)
	if caps := on.caps(); len(caps) != 1 || caps[0] != CapDandelion {
		t.Fatalf("caps = %v, want [%s]", caps, CapDandelion)
	}
	off := New(Config{ListenAddr: ":0"}, core.NewBlockchain(), core.NewMempool(), nil)
	if caps := off.caps(); len(caps) != 0 {
		t.Fatalf("caps = %v, want empty", caps)
	}

	// No peers: originating on the stem must fluff (no successor), not arm an embargo.
	on.relayTx(core.Transaction{From: "x", To: "y", Amount: 1}, nil, true)
	on.dand.mu.Lock()
	n := len(on.dand.embargoes)
	on.dand.mu.Unlock()
	if n != 0 {
		t.Fatalf("a no-peer stem relay should not arm an embargo, got %d", n)
	}
}
