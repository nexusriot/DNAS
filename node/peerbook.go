package node

import "sync"

// peerbook tracks the peer addresses a node knows about (for gossip) and which
// ones it has already started dialing (so it never opens duplicate dial loops
// or dials itself, and never exceeds a maximum number of outbound peers).
type peerbook struct {
	mu      sync.Mutex
	self    string          // our own advertised address, never dialed
	max     int             // maximum outbound dials
	known   map[string]bool // every address we've heard of (for gossip)
	dialing map[string]bool // addresses we've launched a dial loop for
}

func newPeerbook(self string, max int) *peerbook {
	if max <= 0 {
		max = 1
	}
	return &peerbook{
		self:    self,
		max:     max,
		known:   map[string]bool{},
		dialing: map[string]bool{},
	}
}

// note records addr as a known peer for future gossip. Empty and self
// addresses are ignored.
func (pb *peerbook) note(addr string) {
	if addr == "" || addr == pb.self {
		return
	}
	pb.mu.Lock()
	defer pb.mu.Unlock()
	pb.known[addr] = true
}

// shouldDial reports whether we should open a new dial loop to addr, and if so
// marks it as being dialed. It returns false for our own address, empty
// addresses, ones already being dialed, or once the outbound cap is reached.
func (pb *peerbook) shouldDial(addr string) bool {
	if addr == "" || addr == pb.self {
		return false
	}
	pb.mu.Lock()
	defer pb.mu.Unlock()
	if pb.dialing[addr] {
		return false
	}
	if len(pb.dialing) >= pb.max {
		return false
	}
	pb.known[addr] = true
	pb.dialing[addr] = true
	return true
}

// all returns a snapshot of every known peer address.
func (pb *peerbook) all() []string {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	out := make([]string, 0, len(pb.known))
	for a := range pb.known {
		out = append(out, a)
	}
	return out
}
