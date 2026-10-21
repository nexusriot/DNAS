package node

import "github.com/nexusriot/DNAS/core"

// What a peer is allowed to make this node allocate.
//
// The frame header carries a length, and the reader used to believe it up to
// one flat ceiling: maxFrame, 64 MiB, for every message type alike. So a peer
// could announce a 64 MiB `ping` and the reader would allocate 64 MiB to find
// out it was a ping. Nothing is spent verifying anything first — the cost is
// paid before the payload is even read, which is what makes it cheap to send
// and expensive to receive.
//
// A flat ceiling cannot be lowered to fix this, because the legitimate messages
// differ by orders of magnitude: a `pong` is a few bytes and a `chain` is the
// whole history. The bound has to be per type, which means knowing the type
// BEFORE allocating. The binary wire format puts the type first, so a short
// peek at the head of the frame identifies it while the body is still on the
// socket (msgReader.Decode). The JSON format has no fixed layout, so it keeps
// the global ceiling for the allocation and is checked against its per-type cap
// once decoded — refused either way, just later. Binary is what peers
// negotiate, so the path that matters is the one that is covered.

const (
	// smallMsgCap covers the control and request messages: pings, inventory
	// announcements, range requests, hash lists, short-id lists. Generous by
	// orders of magnitude for every one of them, and still far below maxFrame.
	smallMsgCap = 1 << 20 // 1 MiB

	// blockOverhead is the slack allowed above a payload's consensus-derived
	// size for framing, field tags and hex expansion. Transactions travel as
	// hex-bearing JSON-ish fields rather than raw consensus bytes, so the wire
	// form is a multiple of the canonical size rather than equal to it.
	blockOverhead = 4

	// chainCap bounds the whole-chain fallback (MsgChain). It is the one
	// message with no natural limit — it serializes every block this node has —
	// so the cap is what makes it bounded at all. A chain too large to fit was
	// already unsendable (writeMsg refuses a payload over maxFrame); this makes
	// the limit explicit and lets the RECEIVER stop believing an arbitrary
	// length. Ranging MsgGetChain so it needs no such cap is ROADMAP §2.
	chainCap = maxFrame
)

// maxPayloadFor is the largest payload a message of this type may carry.
//
// The block-bearing caps are derived from consensus limits rather than chosen,
// so they cannot drift from what a valid block actually is: change
// MaxBlockBytes and these change with it.
//
// An unknown type gets smallMsgCap. That is the right default: a type this
// build does not understand cannot need room, and a future type that does must
// be added here — where the cost of forgetting is a refused message rather than
// an unbounded allocation.
func maxPayloadFor(t MsgType) int {
	switch t {
	case MsgChain:
		return chainCap
	case MsgBlocks:
		return maxBlocksBatch * core.MaxBlockBytes * blockOverhead
	case MsgBlock, MsgCmpctBlock, MsgBlockTxn:
		return core.MaxBlockBytes * blockOverhead
	case MsgTxs, MsgMempool:
		// A batch of loose transactions, bounded by what a block could hold:
		// anything beyond that could not be mined together anyway.
		return core.MaxBlockBytes * blockOverhead
	case MsgTx:
		// One transaction. It cannot exceed what a block can hold, and a block
		// holds at least one.
		return core.MaxBlockBytes * blockOverhead
	case MsgHeaders:
		// maxHeadersBatch headers, each a fixed set of hex fields.
		return maxHeadersBatch * maxHeaderBytes
	default:
		return smallMsgCap
	}
}

// maxHeaderBytes is a generous bound on one serialized header: ten fields, the
// largest of which are 64-character hex digests.
const maxHeaderBytes = 1024
