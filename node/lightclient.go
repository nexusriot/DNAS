package node

import (
	"fmt"
	"net"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// A light client that speaks the peer protocol.
//
// Compact filters are served on the wire so a client can ask more than one
// node. dnas spv used to be an HTTP client of one node, which is exactly the
// setup in which a non-match does not prove absence: the answer has no second
// opinion. FetchCFHeaders dials one peer, does the same handshake a node does,
// and pulls the filter-header chain. CompareCFHeaderChains is the check that
// makes a non-match mean something: the peers must report the same chain.

// CFHeaderSet is one peer's filter-header chain. Headers[i] is the header at
// height From+i. Base is the lowest height the peer said it can speak for.
type CFHeaderSet struct {
	Addr    string
	From    uint64
	Base    uint64
	Headers []string
}

// FetchCFHeaders dials addr and returns the filter-header chain for heights
// from..to inclusive. Errors from the peer are prefixed with addr. psk is the network key; empty is the open network.
// The identity used for the handshake is ephemeral: a light client holds no coin.
func FetchCFHeaders(addr string, psk []byte, from, to uint64) (CFHeaderSet, error) {
	var out CFHeaderSet
	out.Addr = addr
	if to < from {
		return out, fmt.Errorf("filter-header range %d..%d is empty", from, to)
	}
	conn, err := net.DialTimeout("tcp", addr, handshakeTimeout)
	if err != nil {
		return out, fmt.Errorf("%s: dial: %w", addr, err)
	}
	defer conn.Close()

	sc, sid, err := secureHandshake(conn, psk)
	if err != nil {
		return out, fmt.Errorf("%s: handshake: %w", addr, err)
	}
	id, err := wallet.New()
	if err != nil {
		return out, err
	}
	w := newMsgWriter(sc)
	r := newMsgReader(sc)
	if err := w.Encode(Message{Type: MsgIdentity, PubKey: id.PublicKeyHex(), Sig: id.Sign(sid)}); err != nil {
		return out, err
	}
	var peer Message
	_ = sc.SetReadDeadline(time.Now().Add(handshakeTimeout))
	if err := r.Decode(&peer); err != nil {
		return out, fmt.Errorf("%s: identity: %w", addr, err)
	}
	if peer.Type != MsgIdentity || !wallet.Verify(peer.PubKey, peer.Sig, sid) {
		return out, fmt.Errorf("%s: identity authentication failed", addr)
	}
	if err := w.Encode(Message{
		Type: MsgVersion, Version: ProtocolVersion, Caps: []string{CapCFilters},
		Network: core.NetworkName(), Time: time.Now().Unix(),
	}); err != nil {
		return out, err
	}
	_ = sc.SetReadDeadline(time.Now().Add(handshakeTimeout))
	var ver Message
	if err := r.Decode(&ver); err != nil {
		return out, fmt.Errorf("%s: version: %w", addr, err)
	}
	if ver.Type != MsgVersion || ver.Version < MinProtocolVersion {
		return out, fmt.Errorf("%s: incompatible protocol version", addr)
	}
	peerNet := ver.Network
	if peerNet == "" {
		peerNet = core.MainNet
	}
	if peerNet != core.NetworkName() {
		return out, fmt.Errorf("%s is on %s, this client is on %s", addr, peerNet, core.NetworkName())
	}
	hasCF := false
	for _, c := range ver.Caps {
		if c == CapCFilters {
			hasCF = true
		}
	}
	if !hasCF {
		return out, fmt.Errorf("%s does not serve compact filters", addr)
	}
	w.useBinary(ver.Version)

	next := from
	out.From = from
	for next <= to {
		if err := w.Encode(Message{Type: MsgGetCFHeaders, From: next, To: to}); err != nil {
			return out, err
		}
		m, err := readCFHeaders(sc, r, w)
		if err != nil {
			return out, fmt.Errorf("%s: %w", addr, err)
		}
		out.Base = m.Base
		if len(m.CFHeaders) == 0 {
			if m.Base > next {
				out.From = m.Base
				return out, nil
			}
			break
		}
		if m.From > next {
			// The peer skipped a stretch it cannot serve. Hand back what it
			// did cover, starting where it said it starts.
			if len(out.Headers) == 0 {
				out.From = m.From
			}
			return out, nil
		}
		if m.From < next {
			// An answer for heights already covered would duplicate them, and
			// would not advance next: the same request would go out forever.
			return out, fmt.Errorf("%s: answered from height %d, asked from %d", addr, m.From, next)
		}
		hs := m.CFHeaders
		if want := to - next + 1; uint64(len(hs)) > want {
			hs = hs[:want] // never past the range asked for
		}
		out.Headers = append(out.Headers, hs...)
		next += uint64(len(hs))
	}
	return out, nil
}

// readCFHeaders reads until a filter-header answer arrives, answering pings
// so a slow peer is not dropped for silence. Anything else the node sends
// during the handshake (hello, peer lists, its own header request) is ignored:
// this client asked one question.
func readCFHeaders(sc *secureConn, r *msgReader, w *msgWriter) (Message, error) {
	deadline := time.Now().Add(handshakeTimeout)
	for i := 0; i < 64; i++ {
		_ = sc.SetReadDeadline(deadline)
		var m Message
		if err := r.Decode(&m); err != nil {
			return Message{}, err
		}
		switch m.Type {
		case MsgCFHeaders:
			return m, nil
		case MsgPing:
			_ = w.Encode(Message{Type: MsgPong})
		}
	}
	return Message{}, fmt.Errorf("no filter-header answer")
}

// CompareCFHeaderChains reports whether every set is the same chain from
// height 0. A peer whose base is above 0 cannot confirm the prefix, and a
// single disagreeing header is a split: the caller must not treat a filter
// non-match as absence.
func CompareCFHeaderChains(sets []CFHeaderSet) error {
	if len(sets) < 2 {
		return fmt.Errorf("need at least two filter-header chains to compare, got %d", len(sets))
	}
	ref := sets[0]
	if ref.From != 0 {
		return fmt.Errorf("%s filter headers start at %d (base %d); the prefix is not covered",
			ref.Addr, ref.From, ref.Base)
	}
	for _, s := range sets[1:] {
		if s.From != 0 {
			return fmt.Errorf("%s filter headers start at %d (base %d); the prefix is not covered",
				s.Addr, s.From, s.Base)
		}
		if len(s.Headers) != len(ref.Headers) {
			return fmt.Errorf("filter-header length split: %s has %d, %s has %d",
				ref.Addr, len(ref.Headers), s.Addr, len(s.Headers))
		}
		for i := range ref.Headers {
			if s.Headers[i] != ref.Headers[i] {
				return fmt.Errorf("filter-header split at height %d: %s and %s disagree",
					uint64(i), ref.Addr, s.Addr)
			}
		}
	}
	return nil
}
