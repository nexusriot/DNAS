package core

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
)

// A Merkle Mountain Range over block hashes: an append-only accumulator whose
// root commits to every block appended so far.
//
// The problem it solves. A header chain proves ancestry by induction — block N
// names block N-1, which names N-2 — so proving that block 10 is an ancestor of
// block 1,000,000 means holding every header in between. That is fine for a
// node and ruinous for anything smaller: a client that wants one historical
// fact pays for the whole history to learn it.
//
// An MMR replaces the induction with a commitment. Each block's header carries
// the root of the range over all earlier block hashes, so a proof that block i
// is in the chain under header H is a path through that structure: O(log n)
// hashes, independent of how far apart i and H are.
//
// Shape. An MMR is a list of perfect binary trees ("peaks") of strictly
// decreasing height, which is exactly the binary representation of the number
// of leaves: 11 leaves = 1011b = peaks of 8, 2 and 1. Appending a leaf adds a
// peak of height 0 and then merges equal-height neighbours, which is binary
// increment with carrying — amortized O(1) per append, O(log n) worst case. The
// root is a fold over the peaks, so a proof is a Merkle path inside one peak
// plus the other peaks' hashes.
//
// Domain separation. Leaves and internal nodes are tagged, so a 32-byte leaf
// value can never be reinterpreted as an internal node — without the tags, a
// chosen block hash could be made to parse as a node and forge a path through
// it. The peak fold is tagged separately again for the same reason.

const (
	mmrLeafTag byte = 0x00
	mmrNodeTag byte = 0x01
	mmrRootTag byte = 0x02
)

// MMR is an append-only accumulator over block hashes. The zero value is an
// empty range.
//
// Only the peaks are retained — O(log n) hashes — because that is all the root
// and the next append need. Proving requires the leaves too, which the chain
// has anyway; see Blockchain.ProveAncestry.
type MMR struct {
	// peaks are the roots of the perfect subtrees, tallest first. Parallel to
	// peakHeights.
	peaks       []string
	peakHeights []int
	leaves      uint64
}

// mmrLeaf is the hash committed for one block hash.
func mmrLeaf(blockHash string) string {
	h := sha256.New()
	h.Write([]byte{mmrLeafTag})
	h.Write([]byte(blockHash))
	return hex.EncodeToString(h.Sum(nil))
}

// mmrNode joins two subtree hashes.
func mmrNode(left, right string) string {
	h := sha256.New()
	h.Write([]byte{mmrNodeTag})
	h.Write([]byte(left))
	h.Write([]byte(right))
	return hex.EncodeToString(h.Sum(nil))
}

// EmptyMMRRoot is the root of a range with nothing in it. It is a fixed
// sentinel so that "no history yet" is a value rather than an empty string that
// could be confused with an unset field.
var EmptyMMRRoot = hashBytes([]byte("dnas-mmr-empty"))

// Append adds a block hash to the range.
func (m *MMR) Append(blockHash string) {
	m.peaks = append(m.peaks, mmrLeaf(blockHash))
	m.peakHeights = append(m.peakHeights, 0)
	// Merge equal-height neighbours: binary increment with carrying.
	for len(m.peaks) >= 2 {
		n := len(m.peaks)
		if m.peakHeights[n-1] != m.peakHeights[n-2] {
			break
		}
		merged := mmrNode(m.peaks[n-2], m.peaks[n-1])
		m.peaks = append(m.peaks[:n-2], merged)
		m.peakHeights = append(m.peakHeights[:n-2], m.peakHeights[n-1]+1)
	}
	m.leaves++
}

// Len reports how many block hashes the range covers.
func (m *MMR) Len() uint64 { return m.leaves }

// Root commits to every leaf appended so far.
//
// The number of leaves is folded in alongside the peaks so that two ranges with
// different contents cannot share a root by coincidence of shape — without it,
// a range's root says what it contains but not how much.
func (m *MMR) Root() string {
	if m.leaves == 0 {
		return EmptyMMRRoot
	}
	return foldPeaks(m.peaks, m.leaves)
}

// foldPeaks bags the peaks right to left, which is the conventional MMR fold:
// the result is a single hash over peaks of decreasing height.
func foldPeaks(peaks []string, leaves uint64) string {
	acc := peaks[len(peaks)-1]
	for i := len(peaks) - 2; i >= 0; i-- {
		acc = mmrNode(peaks[i], acc)
	}
	h := sha256.New()
	h.Write([]byte{mmrRootTag})
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], leaves)
	h.Write(n[:])
	h.Write([]byte(acc))
	return hex.EncodeToString(h.Sum(nil))
}

// Peaks returns the current peak hashes, tallest first.
func (m *MMR) Peaks() []string { return append([]string(nil), m.peaks...) }

// Clone returns an independent copy, so a speculative append (a candidate
// block) cannot disturb the chain's range.
func (m *MMR) Clone() *MMR {
	return &MMR{
		peaks:       append([]string(nil), m.peaks...),
		peakHeights: append([]int(nil), m.peakHeights...),
		leaves:      m.leaves,
	}
}

// AncestryProof shows that a block hash was the leaf at position Position of a
// range whose root a header commits to.
type AncestryProof struct {
	// BlockHash is the hash being proven.
	BlockHash string `json:"block_hash"`
	// Height is the block's height, for the caller's benefit; the proof itself
	// is positional and does not trust this field.
	Height uint64 `json:"height"`
	// Position is the leaf's index within the range.
	Position uint64 `json:"position"`
	// Leaves is how many leaves the range held when the root was taken.
	Leaves uint64 `json:"leaves"`
	// Path is the sibling hashes from the leaf up to its peak, bottom first.
	Path []string `json:"path"`
	// PathRight says, for each step, whether the sibling is on the right.
	PathRight []bool `json:"path_right"`
	// Peaks are all the peaks of the range, tallest first. The one this leaf
	// climbs to is recomputed from Path rather than taken from here.
	Peaks []string `json:"peaks"`
	// PeakIndex is which entry of Peaks the leaf's path reconstructs.
	PeakIndex int `json:"peak_index"`
	// Root is the committed root the proof is checked against. A verifier should
	// take the root from a proof-of-work-verified header, not from here; it is
	// carried so a proof is self-describing.
	Root string `json:"root"`
}

// BuildAncestryProof proves that leaves[position] is in the range formed by
// appending all of `leaves` in order.
//
// It rebuilds the range rather than maintaining proof state, because the caller
// (a full node answering a query) has the leaves on hand and this runs per
// request, not per block.
func BuildAncestryProof(leaves []string, position uint64) (AncestryProof, error) {
	if position >= uint64(len(leaves)) {
		return AncestryProof{}, fmt.Errorf("position %d is past the end of a %d-leaf range", position, len(leaves))
	}
	// Split the leaves into the perfect subtrees (peaks) they form, which is the
	// binary decomposition of the count, tallest first.
	sizes := peakSizes(uint64(len(leaves)))
	var (
		peaks     []string
		path      []string
		pathRight []bool
		peakIndex = -1
		offset    uint64
	)
	for i, size := range sizes {
		chunk := leaves[offset : offset+size]
		if position >= offset && position < offset+size {
			peakIndex = i
			p, pr, root := subtreePath(chunk, position-offset)
			path, pathRight = p, pr
			peaks = append(peaks, root)
		} else {
			peaks = append(peaks, subtreeRoot(chunk))
		}
		offset += size
	}
	if peakIndex < 0 {
		return AncestryProof{}, errors.New("internal: leaf fell outside every peak")
	}
	return AncestryProof{
		BlockHash: leaves[position],
		Position:  position,
		Leaves:    uint64(len(leaves)),
		Path:      path,
		PathRight: pathRight,
		Peaks:     peaks,
		PeakIndex: peakIndex,
		Root:      foldPeaks(peaks, uint64(len(leaves))),
	}, nil
}

// peakSizes decomposes a leaf count into perfect-subtree sizes, tallest first.
// This is just the set bits of n, from the most significant down.
func peakSizes(n uint64) []uint64 {
	var out []uint64
	for bit := 63; bit >= 0; bit-- {
		if n&(1<<uint(bit)) != 0 {
			out = append(out, 1<<uint(bit))
		}
	}
	return out
}

// subtreeRoot folds a perfect subtree of leaves into its root.
func subtreeRoot(leaves []string) string {
	level := make([]string, len(leaves))
	for i, l := range leaves {
		level[i] = mmrLeaf(l)
	}
	for len(level) > 1 {
		next := make([]string, 0, len(level)/2)
		for i := 0; i < len(level); i += 2 {
			next = append(next, mmrNode(level[i], level[i+1]))
		}
		level = next
	}
	return level[0]
}

// subtreePath returns the sibling path from leaf `idx` to the root of a perfect
// subtree, along with that root.
func subtreePath(leaves []string, idx uint64) (path []string, right []bool, root string) {
	level := make([]string, len(leaves))
	for i, l := range leaves {
		level[i] = mmrLeaf(l)
	}
	for len(level) > 1 {
		sibling := idx ^ 1
		path = append(path, level[sibling])
		right = append(right, sibling > idx)
		next := make([]string, 0, len(level)/2)
		for i := 0; i < len(level); i += 2 {
			next = append(next, mmrNode(level[i], level[i+1]))
		}
		level = next
		idx /= 2
	}
	return path, right, level[0]
}

// VerifyAncestryProof checks a proof against a root taken from a
// proof-of-work-verified header.
//
// Everything the proof asserts is recomputed: the leaf from the claimed block
// hash, the peak from the path, and the root from the peaks. Nothing in the
// proof is believed except as input to that recomputation — in particular the
// proof's own Root field is ignored, because a prover that could choose it
// could prove anything.
func VerifyAncestryProof(p AncestryProof, root string) bool {
	if root == "" || p.Leaves == 0 || len(p.Peaks) == 0 {
		return false
	}
	if p.PeakIndex < 0 || p.PeakIndex >= len(p.Peaks) {
		return false
	}
	if len(p.Path) != len(p.PathRight) {
		return false
	}
	// The peak layout is determined by the leaf count, so a prover cannot invent
	// a shape that makes an unrelated hash fit.
	sizes := peakSizes(p.Leaves)
	if len(sizes) != len(p.Peaks) {
		return false
	}
	// The claimed position must fall inside the peak the proof says it climbs,
	// and that peak must be tall enough for a path of this length.
	var offset uint64
	for i := 0; i < p.PeakIndex; i++ {
		offset += sizes[i]
	}
	size := sizes[p.PeakIndex]
	if p.Position < offset || p.Position >= offset+size {
		return false
	}
	if uint64(1)<<uint(len(p.Path)) != size {
		return false
	}

	acc := mmrLeaf(p.BlockHash)
	idx := p.Position - offset
	for i, sib := range p.Path {
		if p.PathRight[i] != (idx%2 == 0) {
			return false // the stated side must match the position's parity
		}
		if p.PathRight[i] {
			acc = mmrNode(acc, sib)
		} else {
			acc = mmrNode(sib, acc)
		}
		idx /= 2
	}
	if acc != p.Peaks[p.PeakIndex] {
		return false
	}
	return foldPeaks(p.Peaks, p.Leaves) == root
}
