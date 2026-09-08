package core

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// The block store's record format.
//
// Records used to be the block's JSON. That is portable and readable and also
// the most expensive representation available: every hash, public key and
// signature is stored as hex TEXT, so a 32-byte key occupies 64 bytes and a
// 64-byte signature occupies 128. On a chain of real payments the signatures
// alone are most of the file.
//
// This is a compact binary encoding instead. It is explicitly NOT the consensus
// codec: records here are local to one node, never hashed and never sent to a
// peer, so the format may change whenever it is worth changing. The only hard
// requirement is that an older file still loads, which the leading tag byte
// provides — a JSON record begins with '{', so the two are distinguishable
// without guessing.
//
// Hex fields are decoded to raw bytes where they are known to be hex of a fixed
// shape, and stored as length-prefixed text otherwise. Getting that wrong would
// silently corrupt a store, so every field round-trips through a test that
// compares the decoded block's HASH, not just its fields.

const storeRecordV2 byte = 0x02

// hexish marks a string that is expected to be lowercase hex. Storing it as raw
// bytes halves it; anything that does not decode is kept verbatim so a
// hand-edited or unusual value is never lost.
const (
	fieldText byte = 0 // stored as-is
	fieldHex  byte = 1 // stored as decoded bytes
)

// sbuf is an append-only buffer for store records.
type sbuf struct{ b []byte }

func (s *sbuf) byte(v byte) { s.b = append(s.b, v) }

func (s *sbuf) u64(v uint64) {
	var x [8]byte
	binary.BigEndian.PutUint64(x[:], v)
	s.b = append(s.b, x[:]...)
}

func (s *sbuf) u32(v uint32) {
	var x [4]byte
	binary.BigEndian.PutUint32(x[:], v)
	s.b = append(s.b, x[:]...)
}

func (s *sbuf) i64(v int64) { s.u64(uint64(v)) }

// str writes a length-prefixed string, storing it as raw bytes when it is hex.
func (s *sbuf) str(v string) {
	if raw, ok := decodeHexLower(v); ok {
		s.byte(fieldHex)
		s.u32(uint32(len(raw)))
		s.b = append(s.b, raw...)
		return
	}
	s.byte(fieldText)
	s.u32(uint32(len(v)))
	s.b = append(s.b, v...)
}

func (s *sbuf) strs(vs []string) {
	s.u32(uint32(len(vs)))
	for _, v := range vs {
		s.str(v)
	}
}

// optional writes a presence flag.
func (s *sbuf) optional(present bool) { s.byte(boolByte(present)) }

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

// srdr reads a store record.
type srdr struct {
	b   []byte
	i   int
	err error
}

func (r *srdr) fail(format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf(format, args...)
	}
}

func (r *srdr) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || r.i+n > len(r.b) {
		r.fail("store record truncated: wanted %d bytes at offset %d of %d", n, r.i, len(r.b))
		return nil
	}
	out := r.b[r.i : r.i+n]
	r.i += n
	return out
}

func (r *srdr) byte() byte {
	b := r.take(1)
	if b == nil {
		return 0
	}
	return b[0]
}

func (r *srdr) u64() uint64 {
	b := r.take(8)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}

func (r *srdr) u32() uint32 {
	b := r.take(4)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}

func (r *srdr) i64() int64 { return int64(r.u64()) }

func (r *srdr) str() string {
	kind := r.byte()
	n := int(r.u32())
	// A record field is bounded by the record itself, which openStore already
	// caps; this guards against a corrupt length before the allocation.
	if n > maxStoredBlockBytes {
		r.fail("store record field claims %d bytes", n)
		return ""
	}
	raw := r.take(n)
	if raw == nil {
		return ""
	}
	switch kind {
	case fieldHex:
		return encodeHexLower(raw)
	case fieldText:
		return string(raw)
	default:
		r.fail("unknown store field kind %#x", kind)
		return ""
	}
}

func (r *srdr) strs() []string {
	n := int(r.u32())
	if n > MaxBlockTxs*MaxMultisigKeysGuess {
		r.fail("store record string list claims %d entries", n)
		return nil
	}
	if n == 0 {
		return nil
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, r.str())
		if r.err != nil {
			return nil
		}
	}
	return out
}

// MaxMultisigKeysGuess bounds a decoded string list. It is deliberately loose:
// this is corruption defence for a local file, not a consensus rule, and the
// authoritative limits are checked when the block is validated.
const MaxMultisigKeysGuess = 64

func (r *srdr) optional() bool { return r.byte() == 1 }

// ---------------------------------------------------------------------------

// encodeBlockV2 writes a block in the compact store format.
func encodeBlockV2(b Block) []byte {
	s := &sbuf{}
	s.byte(storeRecordV2)
	s.u64(b.Index)
	s.i64(b.Timestamp)
	s.str(b.PrevHash)
	s.str(b.MerkleRoot)
	s.str(b.StateRoot)
	s.u64(b.BaseFee)
	s.u32(b.Bits)
	s.u64(b.Nonce)
	s.str(b.Hash)
	s.u32(uint32(len(b.Transactions)))
	for _, tx := range b.Transactions {
		encodeTxV2(s, tx)
	}
	return s.b
}

func encodeTxV2(s *sbuf, t Transaction) {
	s.str(t.From)
	s.str(t.To)
	s.u64(t.Amount)
	s.u64(t.Fee)
	s.u64(t.Nonce)
	s.u64(t.Expiry)
	s.u64(t.LockUntil)
	s.str(t.Memo)

	s.u32(uint32(len(t.Outputs)))
	for _, o := range t.Outputs {
		s.str(o.To)
		s.u64(o.Amount)
	}

	s.str(t.AssetID)
	s.optional(t.Issue != nil)
	if t.Issue != nil {
		s.str(t.Issue.Ticker)
		s.u64(t.Issue.Supply)
	}

	s.str(t.PubKey)
	s.str(t.Signature)

	s.optional(t.Multisig != nil)
	if t.Multisig != nil {
		s.u32(uint32(t.Multisig.Threshold))
		s.strs(t.Multisig.PubKeys)
	}
	s.strs(t.Signatures)

	s.optional(t.HTLC != nil)
	if t.HTLC != nil {
		s.str(t.HTLC.Hash)
		s.str(t.HTLC.Recipient)
		s.str(t.HTLC.Sender)
		s.u64(t.HTLC.Timeout)
	}
	s.str(t.Preimage)

	s.optional(t.Vault != nil)
	if t.Vault != nil {
		s.str(t.Vault.Hot)
		s.str(t.Vault.Cold)
		s.u64(t.Vault.Unlock)
	}

	s.str(t.FeePayer)
	s.str(t.FeePayerPubKey)
	s.str(t.FeePayerSig)
}

// decodeBlockV2 parses a compact store record.
func decodeBlockV2(data []byte) (Block, error) {
	r := &srdr{b: data}
	if tag := r.byte(); tag != storeRecordV2 {
		return Block{}, fmt.Errorf("not a v2 store record (tag %#x)", tag)
	}
	var b Block
	b.Index = r.u64()
	b.Timestamp = r.i64()
	b.PrevHash = r.str()
	b.MerkleRoot = r.str()
	b.StateRoot = r.str()
	b.BaseFee = r.u64()
	b.Bits = r.u32()
	b.Nonce = r.u64()
	b.Hash = r.str()

	n := int(r.u32())
	if r.err != nil {
		return Block{}, r.err
	}
	// A block may legitimately hold MaxBlockTxs plus its coinbase; anything far
	// past that is a corrupt length rather than a block.
	if n > MaxBlockTxs+1 {
		return Block{}, fmt.Errorf("store record claims %d transactions", n)
	}
	if n > 0 {
		b.Transactions = make([]Transaction, 0, n)
		for i := 0; i < n; i++ {
			tx := decodeTxV2(r)
			if r.err != nil {
				return Block{}, fmt.Errorf("transaction %d: %w", i, r.err)
			}
			b.Transactions = append(b.Transactions, tx)
		}
	}
	if r.err != nil {
		return Block{}, r.err
	}
	if r.i != len(r.b) {
		return Block{}, fmt.Errorf("store record has %d trailing bytes", len(r.b)-r.i)
	}
	return b, nil
}

func decodeTxV2(r *srdr) Transaction {
	var t Transaction
	t.From = r.str()
	t.To = r.str()
	t.Amount = r.u64()
	t.Fee = r.u64()
	t.Nonce = r.u64()
	t.Expiry = r.u64()
	t.LockUntil = r.u64()
	t.Memo = r.str()

	if n := int(r.u32()); n > 0 {
		if n > MaxTxOutputs*2 { // loose corruption guard; consensus checks the real cap
			r.fail("transaction claims %d outputs", n)
			return t
		}
		t.Outputs = make([]Output, 0, n)
		for i := 0; i < n; i++ {
			to := r.str()
			amt := r.u64()
			if r.err != nil {
				return t
			}
			t.Outputs = append(t.Outputs, Output{To: to, Amount: amt})
		}
	}

	t.AssetID = r.str()
	if r.optional() {
		t.Issue = &AssetIssue{Ticker: r.str(), Supply: r.u64()}
	}

	t.PubKey = r.str()
	t.Signature = r.str()

	if r.optional() {
		threshold := int(r.u32())
		t.Multisig = &MultisigScript{Threshold: threshold, PubKeys: r.strs()}
	}
	t.Signatures = r.strs()

	if r.optional() {
		t.HTLC = &HTLCScript{
			Hash: r.str(), Recipient: r.str(), Sender: r.str(), Timeout: r.u64(),
		}
	}
	t.Preimage = r.str()

	if r.optional() {
		t.Vault = &VaultScript{Hot: r.str(), Cold: r.str(), Unlock: r.u64()}
	}

	t.FeePayer = r.str()
	t.FeePayerPubKey = r.str()
	t.FeePayerSig = r.str()
	return t
}

// ---------------------------------------------------------------------------

// decodeHexLower decodes a lowercase-hex string to bytes. It reports false for
// anything that is not an even-length run of [0-9a-f], so only values that
// re-encode identically are stored in the compact form.
//
// The nibble lookup is a table rather than a switch because this runs once per
// hex character of every field of every transaction — on a full block that is
// hundreds of thousands of iterations, and it showed up as the encoder being
// slower than the JSON it replaced.
var hexVal = func() [256]int8 {
	var t [256]int8
	for i := range t {
		t[i] = -1
	}
	for c := byte('0'); c <= '9'; c++ {
		t[c] = int8(c - '0')
	}
	// Uppercase is deliberately absent: it would re-encode as lowercase and so
	// change the string, which for a hash-bearing field changes the block.
	for c := byte('a'); c <= 'f'; c++ {
		t[c] = int8(c-'a') + 10
	}
	return t
}()

func decodeHexLower(s string) ([]byte, bool) {
	n := len(s)
	if n == 0 || n%2 != 0 {
		return nil, false
	}
	out := make([]byte, n/2)
	for i := 0; i < n; i += 2 {
		hi := hexVal[s[i]]
		lo := hexVal[s[i+1]]
		if hi < 0 || lo < 0 {
			return nil, false
		}
		out[i/2] = byte(hi)<<4 | byte(lo)
	}
	return out, true
}

const hexDigits = "0123456789abcdef"

func encodeHexLower(raw []byte) string {
	out := make([]byte, len(raw)*2)
	for i, b := range raw {
		out[i*2] = hexDigits[b>>4]
		out[i*2+1] = hexDigits[b&0x0f]
	}
	return string(out)
}

var errEmptyRecord = errors.New("empty store record")
