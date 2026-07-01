package node

import "testing"

func TestSeenSetDedup(t *testing.T) {
	s := newSeenSet(10)
	if s.seen("a") {
		t.Fatal("first seen(a) should report not-seen")
	}
	if !s.seen("a") {
		t.Fatal("second seen(a) should report seen")
	}
}

func TestSeenSetBoundedEviction(t *testing.T) {
	s := newSeenSet(2)
	s.seen("a")
	s.seen("b")
	if s.len() != 2 {
		t.Fatalf("len = %d, want 2", s.len())
	}
	// Adding "c" evicts the oldest ("a").
	if s.seen("c") {
		t.Fatal("c should be new")
	}
	if s.len() != 2 {
		t.Fatalf("len = %d, want 2 (bounded)", s.len())
	}
	if s.seen("a") {
		t.Fatal("a should have been evicted and read as not-seen")
	}
}
