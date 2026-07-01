package node

import "testing"

func TestPeerbookExcludesSelfAndEmpty(t *testing.T) {
	pb := newPeerbook("self:1", 8)
	if pb.shouldDial("self:1") {
		t.Error("must not dial self")
	}
	if pb.shouldDial("") {
		t.Error("must not dial empty address")
	}
}

func TestPeerbookDedupDialing(t *testing.T) {
	pb := newPeerbook("self:1", 8)
	if !pb.shouldDial("a:1") {
		t.Fatal("first shouldDial(a) should be true")
	}
	if pb.shouldDial("a:1") {
		t.Fatal("second shouldDial(a) should be false (already dialing)")
	}
}

func TestPeerbookRespectsMaxPeers(t *testing.T) {
	pb := newPeerbook("self:1", 2)
	if !pb.shouldDial("a:1") || !pb.shouldDial("b:1") {
		t.Fatal("first two dials should be allowed")
	}
	if pb.shouldDial("c:1") {
		t.Fatal("third dial should be capped")
	}
}

func TestPeerbookNoteAndAll(t *testing.T) {
	pb := newPeerbook("self:1", 8)
	pb.note("a:1")
	pb.note("a:1") // dup
	pb.note("self:1")
	pb.note("")
	all := pb.all()
	if len(all) != 1 || all[0] != "a:1" {
		t.Fatalf("all() = %v, want [a:1]", all)
	}
}
