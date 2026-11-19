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

// LightPeer is one open light-client session with a full node, for fetching
// compact filters and block bodies over the peer protocol rather than from one
// node's HTTP API.
//
// Nothing a LightPeer returns is checked against a header chain: it does not
// hold one. Its callers do — a filter against the verified filter-header chain
// and CheckFilterBinding, a block against its header's hash and merkle root —
// which is what makes the source of the bytes irrelevant to their validity and
// lets a client take each answer from whichever peer gives one. What it does
// check is SHAPE: heights in the order asked, nothing outside the range, no
// pruned placeholder passed off as a body.
//
// A LightPeer is not safe for concurrent use; it carries one request at a time.
type LightPeer struct {
	s *lightSession
}

// DialLightPeer opens a session with addr (optionally "<pubkey>@host:port",
// see ParsePeerAddr). psk is the network key; empty is the open network.
func DialLightPeer(addr string, psk []byte) (*LightPeer, error) {
	s, err := dialLight(addr, psk, []string{CapCFilters})
	if err != nil {
		return nil, err
	}
	return &LightPeer{s: s}, nil
}

// Addr is the peer's host:port, without any pinned identity.
func (lp *LightPeer) Addr() string { return lp.s.addr }

// Close ends the session.
func (lp *LightPeer) Close() error { return lp.s.Close() }

// ServesFilters reports whether the peer advertised the compact-filter
// capability. Block bodies are served by every node.
func (lp *LightPeer) ServesFilters() bool {
	for _, c := range lp.s.ver.Caps {
		if c == CapCFilters {
			return true
		}
	}
	return false
}

// CFilterSet is one peer's answer for a range of compact filters. Filters[i]
// is the filter at height From+i. Base is the lowest height the peer said it
// can answer for; a peer that has pruned the start of the range starts its
// answer above it (From > the height asked) rather than inventing filters.
type CFilterSet struct {
	Addr    string
	From    uint64
	Base    uint64
	Filters []core.BlockFilter
}

// CFilters fetches the compact filters for heights from..to inclusive, paging
// as the peer's batch limit requires.
//
// A short answer is not an error: the set covers what the peer said it holds,
// and From says where that starts. Callers that need the whole range check
// len(Filters) themselves — and must not read the heights missing from it as
// blocks without the address, which is the whole reason a pruned node answers
// with less rather than with empty filters.
func (lp *LightPeer) CFilters(from, to uint64) (CFilterSet, error) {
	out := CFilterSet{Addr: lp.s.addr, From: from}
	if to < from {
		return out, fmt.Errorf("filter range %d..%d is empty", from, to)
	}
	if !lp.ServesFilters() {
		return out, fmt.Errorf("%s does not serve compact filters", lp.s.addr)
	}
	next := from
	for next <= to {
		if err := lp.s.w.Encode(Message{Type: MsgGetCFilters, From: next, To: to}); err != nil {
			return out, err
		}
		m, err := lp.s.readFor(MsgCFilters)
		if err != nil {
			return out, fmt.Errorf("%s: %w", lp.s.addr, err)
		}
		out.Base = m.Base
		if len(m.Filters) == 0 {
			if len(out.Filters) == 0 && m.Base > next {
				out.From = m.Base
			}
			return out, nil
		}
		if m.From != next {
			if m.From > next && len(out.Filters) == 0 {
				// It skipped a stretch it cannot serve, at the very start:
				// take the rest, starting where it said it starts.
				next = m.From
				out.From = m.From
			} else {
				return out, fmt.Errorf("%s: answered filters from height %d, asked from %d", lp.s.addr, m.From, next)
			}
		}
		fs := m.Filters
		if want := to - next + 1; uint64(len(fs)) > want {
			fs = fs[:want]
		}
		for i, f := range fs {
			if f.Index != next+uint64(i) {
				return out, fmt.Errorf("%s: filter %d of a batch from %d is for height %d", lp.s.addr, i, next, f.Index)
			}
		}
		out.Filters = append(out.Filters, fs...)
		next += uint64(len(fs))
	}
	return out, nil
}

// Blocks fetches the block bodies at heights from..to inclusive, paging as the
// peer's batch limit requires. Unlike filters, a partial answer is an error:
// a body is wanted because a filter matched, and a client that silently got
// fewer blocks than it asked for would under-report a wallet's history.
//
// A node that has nothing in the range answers with an empty batch, which fails
// here at once; one from before that change stays silent, and costs a read
// timeout instead of a hang.
func (lp *LightPeer) Blocks(from, to uint64) ([]core.Block, error) {
	if to < from {
		return nil, fmt.Errorf("block range %d..%d is empty", from, to)
	}
	var out []core.Block
	next := from
	for next <= to {
		if err := lp.s.w.Encode(Message{Type: MsgGetBlocks, From: next, To: to}); err != nil {
			return out, err
		}
		m, err := lp.s.readFor(MsgBlocks)
		if err != nil {
			return out, fmt.Errorf("%s: blocks from %d: %w", lp.s.addr, next, err)
		}
		if len(m.Blocks) == 0 {
			return out, fmt.Errorf("%s: no blocks from height %d", lp.s.addr, next)
		}
		bs := m.Blocks
		if want := to - next + 1; uint64(len(bs)) > want {
			bs = bs[:want]
		}
		for i, b := range bs {
			h := next + uint64(i)
			if b.Index != h {
				return out, fmt.Errorf("%s: block %d of a batch from %d is height %d", lp.s.addr, i, next, b.Index)
			}
			if b.IsPlaceholder() {
				// A pruned height keeps a header-only record. Handing it back as
				// a body would read as a block with no transactions in it.
				return out, fmt.Errorf("%s has pruned the body of block %d", lp.s.addr, h)
			}
		}
		out = append(out, bs...)
		next += uint64(len(bs))
	}
	return out, nil
}
