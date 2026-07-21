package node

import (
	"strconv"
	"testing"
	"time"
)

// TestOpenHandshakeSucceeds confirms two peers with NO shared key still complete
// the encrypted handshake — the permissionless (open-network) mode.
func TestOpenHandshakeSucceeds(t *testing.T) {
	a, errA, b, errB := handshakePair(t, []byte(""), []byte(""))
	if errA != nil || errB != nil {
		t.Fatalf("open handshake should succeed: %v / %v", errA, errB)
	}
	if a == nil || b == nil {
		t.Fatal("open handshake should yield encrypted connections on both sides")
	}
}

// TestPrivateNetKeyMismatchRejected confirms a shared key still gates a private
// network: peers with different keys fail the handshake.
func TestPrivateNetKeyMismatchRejected(t *testing.T) {
	_, errA, _, errB := handshakePair(t, []byte("net-A"), []byte("net-B"))
	if errA == nil && errB == nil {
		t.Fatal("mismatched network keys must fail the handshake")
	}
}

// TestRateLimiter checks the per-peer token bucket: a burst is allowed, then
// further messages are denied until tokens refill.
func TestRateLimiter(t *testing.T) {
	base := time.Unix(1000, 0)
	rl := newRateLimiter(100, 5) // 100 msg/s, burst 5
	for i := 0; i < 5; i++ {
		if !rl.allow(base) {
			t.Fatalf("burst token %d should be allowed", i)
		}
	}
	if rl.allow(base) {
		t.Fatal("a 6th message at the same instant should be denied (bucket empty)")
	}
	if !rl.allow(base.Add(time.Second)) {
		t.Fatal("after ~1s of refill a message should be allowed again")
	}
}

// TestAdmitInboundCaps checks the eclipse/DoS inbound admission: a per-IP-group
// cap, a total cap, loopback exemption, and slot release.
func TestAdmitInboundCaps(t *testing.T) {
	n, _, _ := testNode(t)

	// Loopback is exempt: unlimited, never counted.
	for i := 0; i < maxInboundPerGroup+10; i++ {
		if !n.admitInbound("127.0.0.1") {
			t.Fatal("loopback must always be admitted")
		}
	}
	if n.inboundTotal != 0 {
		t.Fatalf("loopback must not be counted, total=%d", n.inboundTotal)
	}

	// Per-group cap: only maxInboundPerGroup from one /16 (203.0.x).
	for i := 0; i < maxInboundPerGroup; i++ {
		if !n.admitInbound("203.0.113." + strconv.Itoa(i)) {
			t.Fatalf("admit %d within the group cap should succeed", i)
		}
	}
	if n.admitInbound("203.0.113.250") {
		t.Fatal("a connection past the per-group cap must be refused")
	}
	// A different group is still allowed.
	if !n.admitInbound("198.51.100.1") {
		t.Fatal("a different IP group should be admitted")
	}

	// Release frees a slot in the full group so a new one is admitted again.
	n.releaseInbound("203.0.113.0")
	if !n.admitInbound("203.0.113.251") {
		t.Fatal("after releasing a slot, the group should admit again")
	}
}

// TestAdmitInboundTotalCap checks the global inbound cap across many groups.
func TestAdmitInboundTotalCap(t *testing.T) {
	n, _, _ := testNode(t)
	for i := 0; i < maxInbound; i++ { // one per distinct /16 group
		if !n.admitInbound("10." + strconv.Itoa(i) + ".0.1") {
			t.Fatalf("admit %d below the total cap should succeed", i)
		}
	}
	if n.admitInbound("10.200.0.1") {
		t.Fatal("a connection past the total inbound cap must be refused")
	}
}
