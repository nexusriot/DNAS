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
)

// Protocol limits (caps on what a single message may carry, to bound work).
const (
	maxGossipPeers  = 256
	maxHeadersBatch = 2000
	maxBlocksBatch  = 256
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

	// transactions / blocks
	Tx     *core.Transaction `json:"tx,omitempty"`
	Block  *core.Block       `json:"block,omitempty"`
	Blocks []core.Block      `json:"blocks,omitempty"`
	Chain  []core.Block      `json:"chain,omitempty"`

	// headers-first sync
	Headers []core.Header `json:"headers,omitempty"`
	Locator []string      `json:"locator,omitempty"` // block-locator for fork discovery

	// inventory / ranges
	Index uint64 `json:"index,omitempty"`
	Hash  string `json:"hash,omitempty"`
	From  uint64 `json:"from,omitempty"`
	To    uint64 `json:"to,omitempty"`
}
