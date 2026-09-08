package node

import "sync"

// peerbook tracks the peer addresses a node knows about (for gossip) and which
// ones it has already started dialing (so it never opens duplicate dial loops
// or dials itself, and never exceeds a maximum number of outbound peers).
type peerbook struct {
	mu   sync.Mutex
	self string // our own advertised address, never dialed
	// selfAliases are other spellings of our own address, learned the hard way:
	// a peer that reached us as "localhost:3000" gossips that, while we advertise
	// ":3000", and a string comparison does not see they are the same host. The
	// self-connection is then detected by IDENTITY during the handshake (see
	// handleConn) and the address recorded here so we stop dialing it.
	selfAliases map[string]bool
	max         int             // maximum outbound dials
	known       map[string]bool // every address we've heard of (for gossip)
	dialing     map[string]bool // addresses we've launched a dial loop for
}

func newPeerbook(self string, max int) *peerbook {
	if max <= 0 {
		max = 1
	}
	return &peerbook{
		self:        self,
		selfAliases: map[string]bool{},
		max:         max,
		known:       map[string]bool{},
		dialing:     map[string]bool{},
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
	if pb.selfAliases[addr] {
		return // another spelling of us; gossiping it would send peers in a circle
	}
	pb.known[addr] = true
}

// noteSelf records an address that turned out to be this node, so it is never
// dialed or gossiped again. Called when a handshake reveals our own identity on
// the other end.
func (pb *peerbook) noteSelf(addr string) {
	if addr == "" {
		return
	}
	pb.mu.Lock()
	defer pb.mu.Unlock()
	pb.selfAliases[addr] = true
	delete(pb.known, addr)
	delete(pb.dialing, addr) // free the outbound slot it was holding
}

// isSelf reports whether addr is known to be this node under another name.
func (pb *peerbook) isSelf(addr string) bool {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	return pb.selfAliases[addr]
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
	if pb.selfAliases[addr] || pb.dialing[addr] {
		return false
	}
	if len(pb.dialing) >= pb.max {
		return false
	}
	pb.known[addr] = true
	pb.dialing[addr] = true
	return true
}

// dialCount reports how many outbound dial loops are currently held, so the
// address manager knows how many slots are still free to fill.
func (pb *peerbook) dialCount() int {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	return len(pb.dialing)
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
