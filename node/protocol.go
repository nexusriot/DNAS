package node

import "github.com/nexusriot/DNAS/core"

// MsgType tags a P2P message.
type MsgType string

const (
	MsgHello    MsgType = "hello"    // announce our advertised address
	MsgGetChain MsgType = "getchain" // request the peer's full chain
	MsgChain    MsgType = "chain"    // full chain in response to getchain
	MsgBlock    MsgType = "block"    // a single newly mined/received block
	MsgTx       MsgType = "tx"       // a pending transaction
	MsgGetPeers MsgType = "getpeers" // request the peer's known addresses
	MsgPeers    MsgType = "peers"    // known peer addresses (for discovery)
)

// maxGossipPeers caps how many addresses we accept in a single peers message,
// so a malicious peer cannot flood us.
const maxGossipPeers = 256

// Message is the single JSON envelope exchanged between peers. Messages are
// streamed with json.Encoder/Decoder over the (encrypted) connection, so they
// are not bounded by any fixed buffer size as the chain grows.
type Message struct {
	Type  MsgType           `json:"type"`
	Addr  string            `json:"addr,omitempty"`
	Block *core.Block       `json:"block,omitempty"`
	Tx    *core.Transaction `json:"tx,omitempty"`
	Chain []core.Block      `json:"chain,omitempty"`
	Peers []string          `json:"peers,omitempty"`
}
