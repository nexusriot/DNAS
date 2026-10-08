package core

import "errors"

// Binary encodings for the types that cross the peer connection.
//
// Consensus has been binary since the canonical transaction encoding landed,
// and the block store has been binary since the records changed — but the P2P
// envelope was still JSON, so every block relayed on this network was
// serialized to text, parsed back, and then re-encoded to hash it. The
// transport is the one place where the cost is paid per peer rather than once.
//
// These are thin exports of the store codec rather than a new format. A second
// binary encoding of a block would be a second thing to keep in step with the
// first, which is the mistake this repo has already made in JavaScript, Python
// and Go (see cmd/wasmverify). The store's record version travels with the
// bytes, so a peer decodes exactly what a disk record would.

// EncodeBlock returns a block's canonical binary record.
func EncodeBlock(b Block) []byte { return encodeBlockV5(b) }

// DecodeBlock parses a block from EncodeBlock's output, accepting every record
// version the store accepts.
func DecodeBlock(data []byte) (Block, error) { return decodeStoredBlock(data) }

// EncodeHeader returns a header's binary form.
//
// A header is encoded as a body-less block, so the field order is the one the
// block codec already defines and there is nothing extra to keep in step. The
// cost is a transaction count of zero on the wire, which is two bytes.
func EncodeHeader(h Header) []byte {
	return encodeBlockV5(Block{
		Version: h.Version, Index: h.Index, Timestamp: h.Timestamp,
		PrevHash: h.PrevHash, MerkleRoot: h.MerkleRoot, StateRoot: h.StateRoot,
		BaseFee: h.BaseFee, Bits: h.Bits, Nonce: h.Nonce, Hash: h.Hash,
		FilterRoot: h.FilterRoot, HistoryRoot: h.HistoryRoot,
	})
}

// DecodeHeader parses a header from EncodeHeader's output.
func DecodeHeader(data []byte) (Header, error) {
	b, err := DecodeBlock(data)
	if err != nil {
		return Header{}, err
	}
	return b.Header(), nil
}

// EncodeTransaction returns a transaction's binary form.
//
// This is the STORE encoding, not the canonical signing encoding: it carries
// the signature and the fields a signature does not cover, which a peer needs
// and a signing preimage must not have.
func EncodeTransaction(t Transaction) []byte {
	s := &sbuf{}
	s.byte(storeRecordV5)
	encodeTxV2(s, t, storeRecordV5)
	return s.b
}

// DecodeTransaction parses a transaction from EncodeTransaction's output.
func DecodeTransaction(data []byte) (Transaction, error) {
	if len(data) == 0 {
		return Transaction{}, errors.New("empty transaction record")
	}
	version := data[0]
	if version < storeRecordV2 || version > storeRecordV5 {
		return Transaction{}, errors.New("unrecognized transaction record version")
	}
	r := &srdr{b: data[1:]}
	tx := decodeTxV2(r, version)
	if r.err != nil {
		return Transaction{}, r.err
	}
	return tx, nil
}
