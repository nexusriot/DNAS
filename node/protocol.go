package node

import "github.com/nexusriot/DNAS/core"

// MsgType tags a P2P message.
type MsgType string

const (
	MsgIdentity MsgType = "identity" // authenticated node identity (post-handshake)
	MsgHello    MsgType = "hello"    // announce our advertised address
	MsgGetPeers MsgType = "getpeers" // request the peer's known addresses
	MsgPeers    MsgType = "peers"    // known peer addresses (for discovery)
	MsgTx       MsgType = "tx"       // a pending transaction

	// Mempool reconciliation: ask a peer for what it has pending. Transactions are
	// otherwise only ever PUSHED as they arrive, so a node that starts up after a
	// payment was broadcast never learns of it until someone rebroadcasts or it is
	// mined — and a miner that just joined builds emptier blocks than it should.
	MsgGetMempool MsgType = "getmempool"
	MsgMempool    MsgType = "mempool"

	// Block propagation: announce a hash, let peers pull the body they lack.
	MsgInv     MsgType = "inv"     // announce a block (index + hash)
	MsgGetData MsgType = "getdata" // request a block body by index
	MsgBlock   MsgType = "block"   // a single block body

	// Headers-first, ranged catch-up sync.
	MsgGetHeaders MsgType = "getheaders" // request headers starting at an index
	MsgHeaders    MsgType = "headers"    // a batch of headers
	MsgGetBlocks  MsgType = "getblocks"  // request a range of block bodies
	MsgBlocks     MsgType = "blocks"     // a batch of block bodies

	// Whole-chain exchange, used only as a fork/bootstrap fallback.
	MsgGetChain MsgType = "getchain"
	MsgChain    MsgType = "chain"

	// Liveness keepalive: an idle peer is pinged and expected to pong, so a
	// half-open connection (a peer that died without closing) is detected and
	// dropped instead of holding a goroutine and peer slot forever.
	MsgPing MsgType = "ping"
	MsgPong MsgType = "pong"

	// MsgVersion carries the wire-protocol version and capability list, exchanged
	// once right after the authenticated handshake so incompatible peers are
	// dropped and optional features (e.g. Dandelion++) can be negotiated.
	MsgVersion MsgType = "version"
)

// Protocol version and capabilities.
const (
	// ProtocolVersion is the wire-protocol version this node speaks; peers below
	// MinProtocolVersion are dropped. Capability strings let features roll out
	// without a version bump.
	ProtocolVersion    = 2
	MinProtocolVersion = 1

	// CapDandelion advertises support for Dandelion++ stem/fluff transaction relay.
	CapDandelion = "dand"
	// CapMempool advertises that the peer answers MsgGetMempool. Gating on a
	// capability rather than the version number means a node that does not want to
	// serve its pool simply stops advertising it.
	CapMempool = "mpool"
)

// Protocol limits (caps on what a single message may carry, to bound work).
const (
	maxGossipPeers  = 256
	maxHeadersBatch = 2000
	maxBlocksBatch  = 256
	// maxMempoolBatch bounds how many pending transactions one mempool answer
	// carries, so serving a peer's pool request cannot be turned into an unbounded
	// message (the pool itself holds up to core.DefaultMempoolSize).
	maxMempoolBatch = 256
)

// Message is the single JSON envelope exchanged between peers over the
// encrypted connection. Only the fields relevant to Type are set.
type Message struct {
	Type MsgType `json:"type"`

	// identity
	PubKey string `json:"pubkey,omitempty"`
	Sig    string `json:"sig,omitempty"`

	// discovery
	Addr  string   `json:"addr,omitempty"`
	Peers []string `json:"peers,omitempty"`

	// version / capability negotiation
	Version int      `json:"version,omitempty"`
	Caps    []string `json:"caps,omitempty"`
	// Network is the peer's network name (mainnet/testnet/regtest). Nodes on
	// different networks have different genesis blocks and incompatible
	// signatures, so they disconnect here rather than failing to converge later.
	// An empty value comes from a pre-network peer and is read as mainnet.
	Network string `json:"network,omitempty"`
	// Time is the peer's own Unix clock at handshake. Nodes take the MEDIAN of
	// their peers' offsets and apply it (bounded) when validating timestamps, so
	// one machine's wrong clock does not isolate it from the chain. Zero means a
	// peer that predates the field, and contributes no sample.
	Time int64 `json:"time,omitempty"`

	// transactions / blocks
	Tx    *core.Transaction `json:"tx,omitempty"`
	Stem  bool              `json:"stem,omitempty"` // Dandelion++: tx still in the private stem phase
	Block *core.Block       `json:"block,omitempty"`

	Txs    []core.Transaction `json:"txs,omitempty"` // a batch of pending transactions
	Blocks []core.Block       `json:"blocks,omitempty"`
	Chain  []core.Block       `json:"chain,omitempty"`

	// headers-first sync
	Headers []core.Header `json:"headers,omitempty"`
	Locator []string      `json:"locator,omitempty"` // block-locator for fork discovery

	// inventory / ranges
	Index uint64 `json:"index,omitempty"`
	Hash  string `json:"hash,omitempty"`
	From  uint64 `json:"from,omitempty"`
	To    uint64 `json:"to,omitempty"`
}
