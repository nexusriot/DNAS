package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
)

// Block is a batch of transactions committed by proof of work. The hash commits
// to the header fields plus the merkle root of the transactions, so tampering
// with any transaction invalidates the root and therefore the hash.
type Block struct {
	// Version carries BIP9-style miner signaling (see versionbits.go). It is part
	// of the proof-of-work preimage, so a bit a miner sets is committed by the work
	// that block cost — a relay cannot flip it. Consensus places no constraint on
	// the value itself; only a deployment reads it, and only through Signals.
	Version      uint32        `json:"version"`
	Index        uint64        `json:"index"`
	Timestamp    int64         `json:"timestamp"`
	Transactions []Transaction `json:"transactions"`
	PrevHash     string        `json:"prev_hash"`
	MerkleRoot   string        `json:"merkle_root"`
	StateRoot    string        `json:"state_root"` // merkle root of account state after this block
	BaseFee      uint64        `json:"base_fee"`   // EIP-1559 base fee (burned), fixed per block
	Bits         uint32        `json:"bits"`       // proof-of-work target in compact form (nBits)
	Nonce        uint64        `json:"nonce"`
	Hash         string        `json:"hash"`
}

func hashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// MerkleRoot computes a binary merkle root over the transaction hashes,
// duplicating the last node on odd layers (Bitcoin-style).
func MerkleRoot(txs []Transaction) string {
	return merkleRootOf(txHashes(txs))
}

// Header is a block stripped of its transactions. It is all a light (SPV)
// client needs: it hashes identically to the full block, so a client can verify
// proof-of-work and the hash chain from headers alone, then use MerkleRoot to
// check transaction-inclusion proofs — without ever downloading the bodies.
type Header struct {
	Version    uint32 `json:"version"`
	Index      uint64 `json:"index"`
	Timestamp  int64  `json:"timestamp"`
	PrevHash   string `json:"prev_hash"`
	MerkleRoot string `json:"merkle_root"`
	StateRoot  string `json:"state_root"`
	BaseFee    uint64 `json:"base_fee"`
	Bits       uint32 `json:"bits"`
	Nonce      uint64 `json:"nonce"`
	Hash       string `json:"hash"`
}

// Header returns this block's header.
func (b Block) Header() Header {
	return Header{
		Version:    b.Version,
		Index:      b.Index,
		Timestamp:  b.Timestamp,
		PrevHash:   b.PrevHash,
		MerkleRoot: b.MerkleRoot,
		StateRoot:  b.StateRoot,
		BaseFee:    b.BaseFee,
		Bits:       b.Bits,
		Nonce:      b.Nonce,
		Hash:       b.Hash,
	}
}

// headerString is everything the proof-of-work hash commits to (all fields
// except Hash itself). Transactions are covered indirectly via MerkleRoot; the
// post-block account state is committed via StateRoot, so a light client can
// verify balances against a PoW-verified header.
func (h Header) headerString() string { return string(appendPreimage(nil, h)) }

// appendPreimage appends the preimage headerString describes to dst. It is the
// same bytes fmt.Sprintf("%d|%d|%d|%s|%s|%s|%d|%d|%d", ...) would produce — that
// equivalence is consensus-critical and pinned by the test vectors — built by
// hand because this runs once per hash attempt, and reflection-based formatting
// dominated the mining loop.
func appendPreimage(dst []byte, h Header) []byte {
	return strconv.AppendUint(appendPreimagePrefix(dst, h), h.Nonce, 10)
}

// appendPreimagePrefix appends everything before the nonce, up to and including
// the separator. Nonce is the last field, so a miner can build this once and
// append only the nonce per attempt.
func appendPreimagePrefix(dst []byte, h Header) []byte {
	dst = strconv.AppendUint(dst, uint64(h.Version), 10)
	dst = append(dst, '|')
	dst = strconv.AppendUint(dst, h.Index, 10)
	dst = append(dst, '|')
	dst = strconv.AppendInt(dst, h.Timestamp, 10)
	dst = append(dst, '|')
	dst = append(dst, h.PrevHash...)
	dst = append(dst, '|')
	dst = append(dst, h.MerkleRoot...)
	dst = append(dst, '|')
	dst = append(dst, h.StateRoot...)
	dst = append(dst, '|')
	dst = strconv.AppendUint(dst, h.BaseFee, 10)
	dst = append(dst, '|')
	dst = strconv.AppendUint(dst, uint64(h.Bits), 10)
	return append(dst, '|')
}

// ComputeHash returns the hash the header should have.
func (h Header) ComputeHash() string { return hashBytes(appendPreimage(nil, h)) }

// HasValidPoW reports whether the stored hash is correct and meets the target
// the header commits to (Bits).
func (h Header) HasValidPoW() bool {
	return h.Hash == h.ComputeHash() && meetsTarget(h.Hash, h.Bits)
}

// ComputeHash returns the hash the block should have given its header.
func (b Block) ComputeHash() string { return b.Header().ComputeHash() }

// IsPlaceholder reports whether this is a header-only stand-in for a block whose
// body the node does not have — pruned, or below a fast-sync snapshot.
//
// Every real block above genesis carries at least a coinbase, so an empty
// transaction list above height 0 can only be a placeholder. It matters that
// this is checkable: anything derived from a body (a compact filter, an
// inclusion proof) would otherwise be computed over nothing and answer
// confidently that the block is empty.
func (b Block) IsPlaceholder() bool { return b.Index > 0 && len(b.Transactions) == 0 }

// HasValidPoW reports whether the stored hash is correct and meets the block's
// stated difficulty.
func (b Block) HasValidPoW() bool { return b.Header().HasValidPoW() }

// SelfValid reports whether a block is internally consistent without any chain
// context: valid proof-of-work, a merkle root matching its transactions, and a
// leading coinbase. A block that fails this is definitively malformed (peer
// misbehaviour worth a ban), as distinct from a well-formed block that merely
// fails to link to our tip (an honest fork, which is not).
func (b Block) SelfValid() error {
	if !b.HasValidPoW() {
		return errors.New("invalid proof of work")
	}
	if MerkleRoot(b.Transactions) != b.MerkleRoot {
		return errors.New("merkle root mismatch")
	}
	if len(b.Transactions) == 0 || !b.Transactions[0].IsCoinbase() {
		return errors.New("first transaction must be coinbase")
	}
	return nil
}

// Mine sets the merkle root and searches for a nonce whose hash meets the
// block's target (Bits). abort is polled between attempts; if it ever returns
// true, Mine stops early and returns ok=false (e.g. a new tip arrived).
func Mine(b Block, abort func() bool) (Block, bool) {
	b.MerkleRoot = MerkleRoot(b.Transactions)
	// Everything hoisted out of the hot loop: the target as raw bytes (so an
	// attempt compares digests instead of allocating a big.Int), and the preimage
	// up to the nonce (the only field that changes between attempts).
	target := targetBytes(b.Bits)
	prefix := appendPreimagePrefix(nil, b.Header())
	buf := make([]byte, 0, len(prefix)+20) // 20 = digits in the largest uint64
	for {
		if abort != nil && abort() {
			return b, false
		}
		buf = strconv.AppendUint(append(buf[:0], prefix...), b.Nonce, 10)
		sum := sha256.Sum256(buf)
		if bytes.Compare(sum[:], target[:]) <= 0 {
			b.Hash = hex.EncodeToString(sum[:])
			return b, true
		}
		b.Nonce++
	}
}
