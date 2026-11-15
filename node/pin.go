package node

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// Identity pinning.
//
// The open handshake is anonymous: anyone can complete it, so a node that dials
// "seed.example:3000" learns only that SOMETHING answered there and proved some
// key. An on-path attacker can be that something. Pinning names the key the
// operator expects — "<pubkey>@host:port" — and a dial whose peer proves any
// other key is dropped before a single block or transaction is exchanged.
//
// It applies to the connections this node (or a light client) DIALS, which are
// the ones whose identity the dialer has an opinion about. Pins come only from
// configuration: an address learned from gossip carries no pin, and an address
// with a pin is never gossiped with it.

// ParsePeerAddr splits a configured peer address into the address to dial and
// an optional pinned identity: "host:port" has none, and "<pubkey>@host:port"
// pins the 64-hex Ed25519 node key the peer must prove. The pin is returned in
// lowercase.
func ParsePeerAddr(s string) (addr, pinned string, err error) {
	s = strings.TrimSpace(s)
	at := strings.LastIndex(s, "@")
	if at < 0 {
		return s, "", nil
	}
	id, addr := strings.ToLower(s[:at]), s[at+1:]
	if addr == "" {
		return "", "", fmt.Errorf("peer %q: no address after the pinned identity", s)
	}
	raw, err := hex.DecodeString(id)
	if err != nil || len(raw) != 32 {
		return "", "", fmt.Errorf("peer %q: a pinned identity is the node's 64-hex public key (the \"identity\" in that node's /info, or in another node's /peers)", s)
	}
	return addr, id, nil
}

// setPin records the identity a dialed address must prove. An empty pin leaves
// any existing one in place: naming an address without a key is not a request
// to stop checking it.
func (n *Node) setPin(addr, pinned string) {
	if pinned == "" {
		return
	}
	n.pinMu.Lock()
	defer n.pinMu.Unlock()
	if n.pins == nil {
		n.pins = map[string]string{}
	}
	n.pins[addr] = pinned
}

// pinFor returns the identity pinned for a dialed address, if any.
func (n *Node) pinFor(addr string) string {
	n.pinMu.RLock()
	defer n.pinMu.RUnlock()
	return n.pins[addr]
}

// pinnedIdentityOK reports whether a dialed peer proved the identity pinned for
// the address it was dialed at. An unpinned address accepts any identity.
func pinnedIdentityOK(pinned, proven string) bool {
	return pinned == "" || strings.EqualFold(pinned, proven)
}
