package node

// Compact filters on the peer wire.
//
// The filters themselves, and the header chain that commits them, already
// existed (core/cfilter.go) — but only over HTTP. That made every light client
// a client OF ONE NODE rather than a participant in the network, which
// undercuts the thing filters are for.
//
// A compact filter's value is that a NON-match is a proof of non-inclusion: the
// wallet can skip a block without downloading it and still be sure it missed
// nothing. Above UpgradeFilterCommit that proof is backed by the chain's own
// work, because the filter's header is inside the proof-of-work preimage. Below
// it, and for the question "is this the filter everyone else has", the
// safeguard is the same one the whole P2P design rests on: ask several
// independent peers and compare. A client that can only speak HTTP to one node
// has no way to do that — it is trusting that node's availability and its
// honesty about its own filter base.
//
// So these four messages exist to let a light client be a peer: ask several,
// compare their filter-header chains, and fetch filters from whoever answers.
//
// Two rules shape the handlers:
//
//  1. A node that cannot answer says so, rather than answering with less. An
//     empty filter is a proof of ABSENCE, so a pruned or fast-synced node
//     serving one for a body it never saw would be telling a client its address
//     is provably not in a block that nobody here can read. Every answer
//     carries Base — the lowest height this node can speak for — and a request
//     below it comes back empty WITH that base set, which is a different and
//     honest claim.
//  2. Ranges are clamped, never refused for being large. A client asking for
//     more than a batch gets a batch and can ask again; refusing would make the
//     batch size something clients have to know in advance.

// onGetCFilters serves a range of compact filters.
//
// A request reaching below what this node holds bodies for is clamped UP to
// that base, and the answer says both where it actually starts (From) and how
// far back this node can speak at all (Base). Serving the sparse remainder
// instead — the heights that happen to survive pruning — would be the one
// answer that is actually dangerous: the client would read the gaps as blocks
// that touched no addresses.
func (n *Node) onGetCFilters(p *peer, from, to uint64) {
	base := n.chain.BodyHeight()
	start := from
	if start < base {
		start = base
	}
	reply := Message{Type: MsgCFilters, From: start, Base: base}
	if count := rangeCount(start, to, maxCFilterBatch); count > 0 {
		reply.Filters = n.chain.BlockFiltersFrom(start, count)
	}
	n.cfiltersServed.Add(uint64(len(reply.Filters)))
	p.send(reply)
}

// onCFilters takes a peer's answer. The node itself has no use for filters — it
// has the bodies — so this exists for the client embedded in it and for tests;
// what matters on the serving side is that the request is answered.
func (n *Node) onCFilters(p *peer, m Message) {
	if len(m.Filters) == 0 {
		return
	}
	n.cfilterReplies.Add(1)
}

// onGetCFHeaders serves a stretch of the filter-header chain.
func (n *Node) onGetCFHeaders(p *peer, from, to uint64) {
	base := n.chain.FilterHeaderBase()
	start := from
	if start < base {
		// A fast-synced node never saw the bodies below its snapshot, so it
		// cannot fold their commitments. Start where it can and say so; an
		// empty list alone would read as "there are none".
		start = base
	}
	reply := Message{Type: MsgCFHeaders, From: start, Base: base}
	if count := rangeCount(start, to, maxCFHeaderBatch); count > 0 {
		reply.CFHeaders = n.chain.FilterHeadersFrom(start, count)
	}
	n.cfheadersServed.Add(uint64(len(reply.CFHeaders)))
	p.send(reply)
}

// onCFHeaders takes a peer's answer (see onCFilters).
func (n *Node) onCFHeaders(p *peer, m Message) {
	if len(m.CFHeaders) == 0 {
		return
	}
	n.cfheaderReplies.Add(1)
}

// rangeCount turns an inclusive [from, to] request into a bounded count.
//
// A `to` below `from` is a malformed request and yields nothing rather than
// wrapping — the subtraction is unsigned, and a wrap here would turn a typo
// into a demand for the whole chain.
func rangeCount(from, to uint64, max int) int {
	if to < from {
		return 0
	}
	span := to - from + 1
	if span > uint64(max) {
		span = uint64(max)
	}
	return int(span)
}

// CFilterStats reports what this node has served and received over the filter
// messages, so an operator can see whether light clients are actually using it.
type CFilterStats struct {
	FiltersServed   uint64 `json:"filters_served"`
	HeadersServed   uint64 `json:"headers_served"`
	FilterReplies   uint64 `json:"filter_replies"`
	CFHeaderReplies uint64 `json:"cfheader_replies"`
	// Base is the lowest height this node can answer filter questions for.
	Base uint64 `json:"base"`
	// HeaderBase is the lowest height its filter-header chain covers, which a
	// fast-synced node reports above its snapshot.
	HeaderBase uint64 `json:"header_base"`
}

// CFilters reports the node's compact-filter service counters.
func (n *Node) CFilters() CFilterStats {
	return CFilterStats{
		FiltersServed:   n.cfiltersServed.Load(),
		HeadersServed:   n.cfheadersServed.Load(),
		FilterReplies:   n.cfilterReplies.Load(),
		CFHeaderReplies: n.cfheaderReplies.Load(),
		Base:            n.chain.BodyHeight(),
		HeaderBase:      n.chain.FilterHeaderBase(),
	}
}

// requestCFilters asks a peer for a range of filters, and requestCFHeaders for
// a stretch of the filter-header chain.
//
// Unexported because they take a peer, which is: an exported method whose
// parameter type is not exported is one no caller outside this package can
// reach, which is a worse thing to publish than nothing. A P2P light client
// built on this node drives the exchange from inside; what the network needs
// from a full node is the SERVING half above.
func (n *Node) requestCFilters(p *peer, from, to uint64) {
	p.send(Message{Type: MsgGetCFilters, From: from, To: to})
}

func (n *Node) requestCFHeaders(p *peer, from, to uint64) {
	p.send(Message{Type: MsgGetCFHeaders, From: from, To: to})
}
