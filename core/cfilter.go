package core

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/bits"
	"sort"
)

// Compact block filters are a BIP158-style Golomb-Coded Set (GCS) summarizing the
// addresses each block touches. A light client can test a filter for its own
// addresses: a non-match is a *proof of non-inclusion* (the address is definitely
// absent from that block — the coding never produces false negatives), while a
// match means "probably present, download the block to be sure" (false positives
// occur at a rate of about 1/gcsM). Scanning the filters for a whole range lets a
// wallet skip blocks that provably do not concern it, and prove a range contains
// none of its activity — the non-inclusion capability SPV inclusion proofs lack.
//
// Trust note: filters are computed and served by a node; like BIP157/158 they are
// not committed in the proof-of-work header, so their correctness rests on the
// honest-node / multi-peer assumption (a client cross-checks peers, or downloads
// the block). Committing a filter root in the header would make non-inclusion
// fully trustless but is a consensus change. Each filter is still tied to its
// PoW-verified block hash (the SipHash key), and a filter-header chain
// (FilterHeaderChain) lets a client detect internally inconsistent filter sets.
const (
	// gcsP is the Golomb-Rice parameter (remainder bit width); gcsM sets the
	// false-positive rate (~1/gcsM). These are the BIP158 values.
	gcsP = 19
	gcsM = 784931
)

// BlockFilter is the compact filter for one block. The SipHash key is derived
// from BlockHash, so the filter is bound to a specific (PoW-verifiable) block and
// a client re-derives the key from the header it already trusts.
type BlockFilter struct {
	Index     uint64 `json:"index"`
	N         int    `json:"n"`          // number of distinct items encoded
	BlockHash string `json:"block_hash"` // keys the SipHash; ties the filter to a header
	Data      string `json:"data"`       // hex of the Golomb-Rice encoded set
}

// blockAddresses returns the sorted, de-duplicated set of addresses a block
// touches: every recipient plus every (non-coinbase) sender.
func blockAddresses(b Block) []string {
	set := make(map[string]struct{})
	for _, tx := range b.Transactions {
		if !tx.IsCoinbase() {
			set[tx.From] = struct{}{}
		}
		if tx.To != "" {
			set[tx.To] = struct{}{}
		}
		// Every recipient of a multi-output transfer must be in the filter too, or a
		// light client would be told its address is provably absent from a block that
		// in fact paid it.
		for _, o := range tx.Outputs {
			if o.To != "" {
				set[o.To] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// BuildBlockFilter constructs the GCS filter for a block.
func BuildBlockFilter(b Block) BlockFilter {
	addrs := blockAddresses(b)
	n := len(addrs)
	f := BlockFilter{Index: b.Index, N: n, BlockHash: b.Hash}
	if n == 0 {
		return f
	}
	k0, k1 := filterKey(b.Hash)
	modulus := uint64(n) * gcsM
	vals := make([]uint64, n)
	for i, a := range addrs {
		vals[i] = fastReduce(sipHash24(k0, k1, []byte(a)), modulus)
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })

	var bw bitWriter
	var last uint64
	for _, v := range vals {
		delta := v - last
		last = v
		q := delta >> gcsP
		for j := uint64(0); j < q; j++ {
			bw.writeBit(1)
		}
		bw.writeBit(0)
		bw.writeBits(delta&((1<<gcsP)-1), gcsP)
	}
	f.Data = hex.EncodeToString(bw.buf)
	return f
}

// Match reports whether addr may be in the block. False means addr is definitely
// absent (no false negatives); true means probably present (small false-positive
// rate). A match should be confirmed by downloading the block.
func (f BlockFilter) Match(addr string) bool {
	if f.N == 0 {
		return false
	}
	k0, k1 := filterKey(f.BlockHash)
	target := fastReduce(sipHash24(k0, k1, []byte(addr)), uint64(f.N)*gcsM)
	data, err := hex.DecodeString(f.Data)
	if err != nil {
		return false
	}
	br := bitReader{buf: data}
	var val uint64
	for i := 0; i < f.N; i++ {
		d, ok := br.readDelta()
		if !ok {
			return false
		}
		val += d
		if val == target {
			return true
		}
		if val > target { // values are ascending, so we can stop early
			return false
		}
	}
	return false
}

// MatchAny reports whether any of addrs may be in the block.
func (f BlockFilter) MatchAny(addrs []string) bool {
	for _, a := range addrs {
		if f.Match(a) {
			return true
		}
	}
	return false
}

// commitment is the bytes a filter-header chain hashes: the item count followed
// by the encoded set. Two filters with the same commitment are identical.
func (f BlockFilter) commitment() []byte {
	data, _ := hex.DecodeString(f.Data)
	h := sha256.New()
	var nb [8]byte
	binary.BigEndian.PutUint64(nb[:], uint64(f.N))
	h.Write(nb[:])
	h.Write(data)
	return h.Sum(nil)
}

// BlockFilterAt returns the compact filter for the block at the given height.
func (bc *Blockchain) BlockFilterAt(height uint64) (BlockFilter, bool) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if height >= uint64(len(bc.blocks)) {
		return BlockFilter{}, false
	}
	// A filter built from a body the node does not have is a valid EMPTY filter,
	// and an empty filter is a proof that the block contains nothing. Serving one
	// would tell a light client that its address is provably absent from a block
	// this node simply cannot read (see prune.go). So a body-less height has no
	// filter to offer, and says so.
	if bc.blocks[height].IsPlaceholder() {
		return BlockFilter{}, false
	}
	return BuildBlockFilter(bc.blocks[height]), true
}

// BlockFilters returns the compact filter for every block, in order.
func (bc *Blockchain) BlockFilters() []BlockFilter {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	out := make([]BlockFilter, 0, len(bc.blocks))
	for _, b := range bc.blocks {
		if b.IsPlaceholder() {
			continue // no body, so no filter: see BlockFilterAt
		}
		out = append(out, BuildBlockFilter(b))
	}
	return out
}

// BlockFiltersFrom returns up to max compact filters starting at the given
// height, so a client can page through them instead of asking a node to
// serialize one per block in a single response.
func (bc *Blockchain) BlockFiltersFrom(from uint64, max int) []BlockFilter {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if from >= uint64(len(bc.blocks)) || max <= 0 {
		return nil
	}
	end := from + uint64(max)
	if end > uint64(len(bc.blocks)) {
		end = uint64(len(bc.blocks))
	}
	out := make([]BlockFilter, 0, end-from)
	for i := from; i < end; i++ {
		if bc.blocks[i].IsPlaceholder() {
			continue // no body, so no filter: see BlockFilterAt
		}
		out = append(out, BuildBlockFilter(bc.blocks[i]))
	}
	return out
}

// FilterHeaders returns the filter-header chain this node holds, from
// FilterHeaderBase upwards.
func (bc *Blockchain) FilterHeaders() []string {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return append([]string(nil), bc.filterHeaders...)
}

// FilterHeadersFrom returns up to max filter headers starting at the given
// height. The chain is a running hash, so the values below `from` still have to
// be folded to produce them — paging bounds the RESPONSE, not the work. A client
// that keeps its own verified prefix (see the SPV header cache) folds
// incrementally instead and pays neither.
func (bc *Blockchain) FilterHeadersFrom(from uint64, max int) []string {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if max <= 0 || from < bc.filterBase {
		// Below the base there is nothing to serve and nothing to invent: see
		// FilterHeaderBase. The caller reports the gap rather than filling it.
		return nil
	}
	start := from - bc.filterBase
	if start >= uint64(len(bc.filterHeaders)) {
		return nil
	}
	end := start + uint64(max)
	if end > uint64(len(bc.filterHeaders)) {
		end = uint64(len(bc.filterHeaders))
	}
	return append([]string(nil), bc.filterHeaders[start:end]...)
}

// FilterHeaderChain folds a sequence of block filters into a hash chain
// (Fh[i] = sha256(commitment(filter[i]) || Fh[i-1])), mirroring BIP157 filter
// headers. A client can fetch the chain once and then check that each filter it
// downloads hashes into it, detecting a node that serves an inconsistent set.
func FilterHeaderChain(filters []BlockFilter) []string {
	out, _ := FoldFilterHeaders("", filters)
	return out
}

// FoldFilterHeaders continues the filter-header chain from a previous value, so
// a client holding a verified prefix can extend it with just the new filters
// instead of re-folding from genesis. An empty prevHex starts at genesis, making
// this the general form of FilterHeaderChain.
func FoldFilterHeaders(prevHex string, filters []BlockFilter) ([]string, error) {
	var prev []byte
	if prevHex != "" {
		b, err := hex.DecodeString(prevHex)
		if err != nil {
			return nil, fmt.Errorf("previous filter header is not hex: %w", err)
		}
		prev = b
	}
	out := make([]string, len(filters))
	for i, f := range filters {
		h := sha256.New()
		h.Write(f.commitment())
		h.Write(prev)
		sum := h.Sum(nil)
		out[i] = hex.EncodeToString(sum)
		prev = sum
	}
	return out, nil
}

// filterKey derives the 128-bit SipHash key from a block hash (its first 16
// bytes, little-endian), so both builder and verifier key the filter identically.
func filterKey(blockHash string) (uint64, uint64) {
	raw, _ := hex.DecodeString(blockHash)
	var k [16]byte
	copy(k[:], raw)
	return binary.LittleEndian.Uint64(k[0:8]), binary.LittleEndian.Uint64(k[8:16])
}

// fastReduce maps a 64-bit hash uniformly into [0, n) via a multiply-shift
// (Lemire's method), avoiding modulo bias.
func fastReduce(x, n uint64) uint64 {
	hi, _ := bits.Mul64(x, n)
	return hi
}

// sipHash24 is SipHash-2-4, a fast keyed hash (the one BIP158 uses to place items
// in the filter's range).
func sipHash24(k0, k1 uint64, data []byte) uint64 {
	v0 := k0 ^ 0x736f6d6570736575
	v1 := k1 ^ 0x646f72616e646f6d
	v2 := k0 ^ 0x6c7967656e657261
	v3 := k1 ^ 0x7465646279746573
	round := func() {
		v0 += v1
		v1 = bits.RotateLeft64(v1, 13)
		v1 ^= v0
		v0 = bits.RotateLeft64(v0, 32)
		v2 += v3
		v3 = bits.RotateLeft64(v3, 16)
		v3 ^= v2
		v0 += v3
		v3 = bits.RotateLeft64(v3, 21)
		v3 ^= v0
		v2 += v1
		v1 = bits.RotateLeft64(v1, 17)
		v1 ^= v2
		v2 = bits.RotateLeft64(v2, 32)
	}
	n := len(data)
	end := n - (n % 8)
	for i := 0; i < end; i += 8 {
		m := binary.LittleEndian.Uint64(data[i : i+8])
		v3 ^= m
		round()
		round()
		v0 ^= m
	}
	b := uint64(n) << 56
	for i := end; i < n; i++ {
		b |= uint64(data[i]) << (8 * uint(i-end))
	}
	v3 ^= b
	round()
	round()
	v0 ^= b
	v2 ^= 0xff
	round()
	round()
	round()
	round()
	return v0 ^ v1 ^ v2 ^ v3
}

type bitWriter struct {
	buf   []byte
	nbits uint
}

func (w *bitWriter) writeBit(b uint64) {
	if w.nbits%8 == 0 {
		w.buf = append(w.buf, 0)
	}
	if b&1 == 1 {
		w.buf[len(w.buf)-1] |= 1 << (7 - w.nbits%8)
	}
	w.nbits++
}

func (w *bitWriter) writeBits(v uint64, n uint) {
	for i := int(n) - 1; i >= 0; i-- {
		w.writeBit((v >> uint(i)) & 1)
	}
}

type bitReader struct {
	buf []byte
	pos uint
}

func (r *bitReader) readBit() (uint64, bool) {
	if r.pos >= uint(len(r.buf))*8 {
		return 0, false
	}
	byteIdx := r.pos / 8
	bitIdx := 7 - r.pos%8
	r.pos++
	return uint64((r.buf[byteIdx] >> bitIdx) & 1), true
}

func (r *bitReader) readBits(n uint) (uint64, bool) {
	var v uint64
	for i := uint(0); i < n; i++ {
		b, ok := r.readBit()
		if !ok {
			return 0, false
		}
		v = (v << 1) | b
	}
	return v, true
}

// readDelta reads one Golomb-Rice value: a unary quotient (1-bits terminated by a
// 0) shifted by gcsP, plus a gcsP-bit remainder.
func (r *bitReader) readDelta() (uint64, bool) {
	var q uint64
	for {
		b, ok := r.readBit()
		if !ok {
			return 0, false
		}
		if b == 0 {
			break
		}
		q++
	}
	rem, ok := r.readBits(gcsP)
	if !ok {
		return 0, false
	}
	return (q << gcsP) | rem, true
}

// The filter-header chain is CACHED as blocks connect rather than recomputed on
// demand, because it is a running hash over block bodies: on a node that prunes
// (or that fast-synced from a snapshot) the old bodies are gone, and folding
// over the ones that remain would produce a chain that agrees with nobody. A
// cached chain stays correct for every height the node ever connected.
//
// It costs 32 bytes a block, against a body's kilobytes — which is the whole
// bargain of BIP157: keep the commitments, drop the data.

// extendFilterHeadersLocked appends the filter header for a newly connected
// block. bc.mu held for writing.
func (bc *Blockchain) extendFilterHeadersLocked(b Block) {
	if b.IsPlaceholder() {
		return // nothing to fold: a header-only block has no filter
	}
	prev := ""
	if n := len(bc.filterHeaders); n > 0 {
		prev = bc.filterHeaders[n-1]
	}
	next, err := FoldFilterHeaders(prev, []BlockFilter{BuildBlockFilter(b)})
	if err != nil || len(next) != 1 {
		return
	}
	bc.filterHeaders = append(bc.filterHeaders, next[0])
}

// truncateFilterHeadersLocked drops the cached headers above `fork`, for a reorg
// that is about to replace those blocks. bc.mu held for writing.
func (bc *Blockchain) truncateFilterHeadersLocked(fork uint64) {
	if fork+1 < bc.filterBase {
		return
	}
	keep := int(fork + 1 - bc.filterBase)
	if keep < 0 {
		keep = 0
	}
	if keep < len(bc.filterHeaders) {
		bc.filterHeaders = bc.filterHeaders[:keep]
	}
}

// FilterHeaderBase is the lowest height this node can serve a filter header for.
// It is 0 for a node that has followed the chain from genesis — including one
// that prunes bodies, since the cache outlives them — and the snapshot height
// plus one for a node that fast-synced, which never saw the bodies below it and
// so cannot fold their commitments.
func (bc *Blockchain) FilterHeaderBase() uint64 {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.filterBase
}
