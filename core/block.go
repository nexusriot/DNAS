package core

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Block is a batch of transactions committed by proof of work. The hash commits
// to the header fields plus the merkle root of the transactions, so tampering
// with any transaction invalidates the root and therefore the hash.
type Block struct {
	Index        uint64        `json:"index"`
	Timestamp    int64         `json:"timestamp"`
	Transactions []Transaction `json:"transactions"`
	PrevHash     string        `json:"prev_hash"`
	MerkleRoot   string        `json:"merkle_root"`
	StateRoot    string        `json:"state_root"` // merkle root of account state after this block
	BaseFee      uint64        `json:"base_fee"`   // EIP-1559 base fee (burned), fixed per block
	Difficulty   int           `json:"difficulty"`
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
	Index      uint64 `json:"index"`
	Timestamp  int64  `json:"timestamp"`
	PrevHash   string `json:"prev_hash"`
	MerkleRoot string `json:"merkle_root"`
	StateRoot  string `json:"state_root"`
	BaseFee    uint64 `json:"base_fee"`
	Difficulty int    `json:"difficulty"`
	Nonce      uint64 `json:"nonce"`
	Hash       string `json:"hash"`
}

// Header returns this block's header.
func (b Block) Header() Header {
	return Header{
		Index:      b.Index,
		Timestamp:  b.Timestamp,
		PrevHash:   b.PrevHash,
		MerkleRoot: b.MerkleRoot,
		StateRoot:  b.StateRoot,
		BaseFee:    b.BaseFee,
		Difficulty: b.Difficulty,
		Nonce:      b.Nonce,
		Hash:       b.Hash,
	}
}

// headerString is everything the proof-of-work hash commits to (all fields
// except Hash itself). Transactions are covered indirectly via MerkleRoot; the
// post-block account state is committed via StateRoot, so a light client can
// verify balances against a PoW-verified header.
func (h Header) headerString() string {
	return fmt.Sprintf("%d|%d|%s|%s|%s|%d|%d|%d",
		h.Index, h.Timestamp, h.PrevHash, h.MerkleRoot, h.StateRoot, h.BaseFee, h.Difficulty, h.Nonce)
}

// ComputeHash returns the hash the header should have.
func (h Header) ComputeHash() string { return hashBytes([]byte(h.headerString())) }

// HasValidPoW reports whether the stored hash is correct and meets the stated
// difficulty.
func (h Header) HasValidPoW() bool {
	return h.Hash == h.ComputeHash() && meetsDifficulty(h.Hash, h.Difficulty)
}

// ComputeHash returns the hash the block should have given its header.
func (b Block) ComputeHash() string { return b.Header().ComputeHash() }

func meetsDifficulty(hash string, difficulty int) bool {
	return strings.HasPrefix(hash, strings.Repeat("0", difficulty))
}

// HasValidPoW reports whether the stored hash is correct and meets the block's
// stated difficulty.
func (b Block) HasValidPoW() bool { return b.Header().HasValidPoW() }

// Mine sets the merkle root and searches for a nonce whose hash meets the
// block's difficulty. abort is polled between attempts; if it ever returns
// true, Mine stops early and returns ok=false (e.g. a new tip arrived).
func Mine(b Block, abort func() bool) (Block, bool) {
	b.MerkleRoot = MerkleRoot(b.Transactions)
	for {
		if abort != nil && abort() {
			return b, false
		}
		b.Hash = b.ComputeHash()
		if meetsDifficulty(b.Hash, b.Difficulty) {
			return b, true
		}
		b.Nonce++
	}
}
