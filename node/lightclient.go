package node

import (
	"fmt"
	"net"
	"strings"
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

// lightSession is one completed light-client connection: encrypted,
// identity-authenticated and version-negotiated, ready for requests.
type lightSession struct {
	addr string
	conn net.Conn
	sc   *secureConn
	r    *msgReader
	w    *msgWriter
	ver  Message // the peer's version message (caps, version, network)
}

func (ls *lightSession) Close() error { return ls.conn.Close() }

// dialLight dials a peer and runs the same handshake a node does, with an
// ephemeral identity (a light client holds no coin). addr may carry a pinned
// node identity as "<pubkey>@host:port" (see ParsePeerAddr); the peer must
// then prove it holds that key, or the session is refused.
func dialLight(addr string, psk []byte, caps []string) (*lightSession, error) {
	hostport, pinned, err := ParsePeerAddr(addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("tcp", hostport, handshakeTimeout)
	if err != nil {
		return nil, fmt.Errorf("%s: dial: %w", hostport, err)
	}
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()
	sc, sid, err := secureHandshake(conn, psk)
	if err != nil {
		return nil, fmt.Errorf("%s: handshake: %w", hostport, err)
	}
	id, err := wallet.New()
	if err != nil {
		return nil, err
	}
	w := newMsgWriter(sc)
	r := newMsgReader(sc)
	if err := w.Encode(Message{Type: MsgIdentity, PubKey: id.PublicKeyHex(), Sig: id.Sign(sid)}); err != nil {
		return nil, err
	}
	var peer Message
	_ = sc.SetReadDeadline(time.Now().Add(handshakeTimeout))
	if err := r.Decode(&peer); err != nil {
		return nil, fmt.Errorf("%s: identity: %w", hostport, err)
	}
	if peer.Type != MsgIdentity || !wallet.Verify(peer.PubKey, peer.Sig, sid) {
		return nil, fmt.Errorf("%s: identity authentication failed", hostport)
	}
	if pinned != "" && !strings.EqualFold(peer.PubKey, pinned) {
		return nil, fmt.Errorf("%s: identity %s does not match the pinned %s", hostport, short(peer.PubKey), short(pinned))
	}
	if err := w.Encode(Message{
		Type: MsgVersion, Version: ProtocolVersion, Caps: caps,
		Network: core.NetworkName(), Time: time.Now().Unix(),
	}); err != nil {
		return nil, err
	}
	_ = sc.SetReadDeadline(time.Now().Add(handshakeTimeout))
	var ver Message
	if err := r.Decode(&ver); err != nil {
		return nil, fmt.Errorf("%s: version: %w", hostport, err)
	}
	if ver.Type != MsgVersion || ver.Version < MinProtocolVersion {
		return nil, fmt.Errorf("%s: incompatible protocol version", hostport)
	}
	peerNet := ver.Network
	if peerNet == "" {
		peerNet = core.MainNet
	}
	if peerNet != core.NetworkName() {
		return nil, fmt.Errorf("%s is on %s, this client is on %s", hostport, peerNet, core.NetworkName())
	}
	w.useBinary(ver.Version)
	ok = true
	return &lightSession{addr: hostport, conn: conn, sc: sc, r: r, w: w, ver: ver}, nil
}

// readFor reads until a message of type want arrives, answering pings so a
// slow peer is not dropped for silence. Anything else the node sends during
// the handshake (hello, peer lists, its own header request) is ignored: this
// client asked one question.
func (ls *lightSession) readFor(want MsgType) (Message, error) {
	deadline := time.Now().Add(handshakeTimeout)
	for i := 0; i < 64; i++ {
		_ = ls.sc.SetReadDeadline(deadline)
		var m Message
		if err := ls.r.Decode(&m); err != nil {
			return Message{}, err
		}
		switch m.Type {
		case want:
			return m, nil
		case MsgPing:
			_ = ls.w.Encode(Message{Type: MsgPong})
		}
	}
	return Message{}, fmt.Errorf("no %s answer", want)
}

// HeadersAfter is one peer's answer to a locator: the headers it holds after
// the highest locator block it shares (Fork), up to one batch.
type HeadersAfter struct {
	Addr    string
	Fork    uint64        // the highest height the locator and the peer share
	Headers []core.Header // the peer's headers from Fork+1, at most one batch
}

// FetchHeadersAfter dials addr and asks for the headers after the fork point of
// locator (see Blockchain.Locator), which is how a light client learns whether
// a peer is on the same chain as the node it is otherwise talking to, ahead of
// it, behind it, or on a branch of its own. addr may carry a pinned identity.
func FetchHeadersAfter(addr string, psk []byte, locator []string) (HeadersAfter, error) {
	out := HeadersAfter{Addr: addr}
	if len(locator) == 0 {
		return out, fmt.Errorf("empty locator")
	}
	ls, err := dialLight(addr, psk, nil)
	if err != nil {
		return out, err
	}
	defer ls.Close()
	out.Addr = ls.addr
	if err := ls.w.Encode(Message{Type: MsgGetHeaders, Locator: locator}); err != nil {
		return out, err
	}
	m, err := ls.readFor(MsgHeaders)
	if err != nil {
		return out, fmt.Errorf("%s: %w", ls.addr, err)
	}
	if len(m.Headers) > maxHeadersBatch {
		return out, fmt.Errorf("%s: answered %d headers, more than a batch", ls.addr, len(m.Headers))
	}
	out.Headers = m.Headers
	switch {
	case len(m.Headers) > 0:
		if m.Headers[0].Index == 0 {
			return out, fmt.Errorf("%s: answered with a genesis header", ls.addr)
		}
		out.Fork = m.Headers[0].Index - 1
	case m.From > 0:
		out.Fork = m.From - 1
	default:
		return out, fmt.Errorf("%s: an empty answer that names no fork point (a node from before this field)", ls.addr)
	}
	return out, nil
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
	ls, err := dialLight(addr, psk, []string{CapCFilters})
	if err != nil {
		return out, err
	}
	defer ls.Close()
	addr = ls.addr
	out.Addr = addr
	w, ver := ls.w, ls.ver
	hasCF := false
	for _, c := range ver.Caps {
		if c == CapCFilters {
			hasCF = true
		}
	}
	if !hasCF {
		return out, fmt.Errorf("%s does not serve compact filters", addr)
	}

	next := from
	out.From = from
	for next <= to {
		if err := w.Encode(Message{Type: MsgGetCFHeaders, From: next, To: to}); err != nil {
			return out, err
		}
		m, err := ls.readFor(MsgCFHeaders)
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
