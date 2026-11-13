package node

import (
	"fmt"
	"strings"
	"testing"
)

// Gossip is attacker-controlled: the address book must stay bounded however
// many distinct addresses a peer sends, refuse entries no one could dial, and
// answer getpeers with a bounded sample.
func TestPeerbookIsBounded(t *testing.T) {
	pb := newPeerbook(":3000", 8)
	for i := 0; i < maxKnownPeers+500; i++ {
		pb.note(fmt.Sprintf("10.%d.%d.%d:3000", i>>16&0xff, i>>8&0xff, i&0xff))
	}
	if got := len(pb.all()); got != maxKnownPeers {
		t.Fatalf("book holds %d addresses, want the cap %d", got, maxKnownPeers)
	}
	pb.note(strings.Repeat("a", maxPeerAddrLen+1))
	for _, a := range pb.all() {
		if len(a) > maxPeerAddrLen {
			t.Fatal("an overlong address was recorded")
		}
	}
	if got := len(pb.sample(maxGossipPeers)); got != maxGossipPeers {
		t.Fatalf("sample returned %d, want %d", got, maxGossipPeers)
	}
}

// Addresses being dialed are never evicted to make room for gossip.
func TestPeerbookKeepsDialedAddresses(t *testing.T) {
	pb := newPeerbook(":3000", 8)
	if !pb.shouldDial("1.2.3.4:3000") {
		t.Fatal("first dial refused")
	}
	for i := 0; i < maxKnownPeers*2; i++ {
		pb.note(fmt.Sprintf("10.0.%d.%d:3000", i>>8&0xff, i&0xff))
	}
	found := false
	for _, a := range pb.all() {
		if a == "1.2.3.4:3000" {
			found = true
		}
	}
	if !found {
		t.Fatal("an address being dialed was evicted from the book")
	}
}
