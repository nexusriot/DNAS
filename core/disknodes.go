package core

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
)

// A trie node store that lives on disk.
//
// boundedNodeStore (statetrie.go) keeps the commitment cache in RAM and evicts
// past a node count. That bounds it, but it bounds it in the one resource the
// node has least of, and it throws the whole cache away at every restart — so
// the first block after a restart pays a full rebuild, and so does the first
// state proof, and so does the first reorg.
//
// This is the same cache on disk, and it is possible at all because of what a
// trie node IS: content-addressed and immutable. A node named by its own hash
// can never be stale, so there is no invalidation, no write ordering, and no
// consistency problem — only "present" or "absent".
//
// That turns the design into a fixed-size open-addressed hash table, which is
// what makes it genuinely bounded rather than bounded-ish:
//
//   - The file is allocated once, at a size the operator chooses, and never
//     grows. No compaction, no append log, no free list.
//   - There is NO resident index. A node's slot is derived from its own hash,
//     so a lookup is one pread at a computed offset. Memory is O(1) in the
//     number of nodes, where an offset index would have been O(n) — which is
//     the whole point, since an index large enough for a big ledger is itself
//     the memory problem being solved.
//   - A collision EVICTS. Two nodes whose hashes land in the same slot cannot
//     both be held, so the newer one wins and the older one is forgotten.
//
// Every one of those is safe for exactly one reason, and it is worth stating
// because it is what the whole design leans on: the store is a CACHE, and a
// miss costs a rebuild rather than a wrong answer (see statetrie.go). Eviction
// by collision, a torn write from a crash, a file from an older build, a
// corrupted slot — all of them are misses. None of them can produce a node that
// is not the node its hash names, because the hash is checked on the way out.

const (
	diskNodeMagic   = "DNASTRIE"
	diskNodeVersion = uint32(1)
	// diskSlotBytes is the fixed room each slot gives one node: a 32-byte hash,
	// a 4-byte length, and the encoded node.
	//
	// 512 holds every node this trie produces in practice — an inner node is two
	// hex digests, a leaf is a key plus an account's encoding — with room for an
	// account carrying a good many asset balances. A node that does not fit is
	// simply not cached, which costs a rebuild and nothing else.
	diskSlotBytes = 512
	diskSlotHdr   = 32 + 4
	diskMaxNode   = diskSlotBytes - diskSlotHdr
	diskHeaderLen = 64

	// DefaultDiskTrieSlots is how many nodes a disk store holds by default:
	// 1,048,576 slots at 512 bytes is a 512 MB file, four times the node count
	// the in-memory default retains and at no cost in RAM.
	DefaultDiskTrieSlots = 1 << 20
)

// diskNodeStore is a fixed-size, content-addressed node cache in a file.
type diskNodeStore struct {
	f     *os.File
	slots uint64
	// mu guards nothing about the data — pread/pwrite are independent — but
	// serializes Close against in-flight operations.
	mu     sync.RWMutex
	closed bool
}

// OpenDiskNodeStore opens (or creates) a disk-backed trie node cache at path,
// holding at most slots nodes.
//
// An existing file whose header does not match this build is REPLACED rather
// than read: the contents are a cache, so discarding them costs rebuild time
// and nothing else, and refusing to start because a cache is from an older
// version would be the wrong trade entirely.
func OpenDiskNodeStore(path string, slots int) (NodeStore, error) {
	if slots <= 0 {
		slots = DefaultDiskTrieSlots
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open trie cache %s: %w", path, err)
	}
	s := &diskNodeStore{f: f, slots: uint64(slots)}
	ok, err := s.readHeader()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !ok {
		if err := s.initFile(); err != nil {
			f.Close()
			return nil, err
		}
	}
	return s, nil
}

// readHeader reports whether the file already holds a usable store of this
// shape.
func (s *diskNodeStore) readHeader() (bool, error) {
	info, err := s.f.Stat()
	if err != nil {
		return false, err
	}
	if info.Size() < diskHeaderLen {
		return false, nil // new or truncated: initialize
	}
	var hdr [diskHeaderLen]byte
	if _, err := s.f.ReadAt(hdr[:], 0); err != nil {
		return false, nil
	}
	if string(hdr[:len(diskNodeMagic)]) != diskNodeMagic {
		return false, nil
	}
	if binary.BigEndian.Uint32(hdr[8:12]) != diskNodeVersion {
		return false, nil
	}
	if binary.BigEndian.Uint64(hdr[12:20]) != s.slots {
		return false, nil // a different size: re-initialize at the requested one
	}
	want := int64(diskHeaderLen) + int64(s.slots)*diskSlotBytes
	return info.Size() >= want, nil
}

// initFile writes a fresh header and sizes the file. The slots are left as a
// sparse hole: an unwritten slot reads as zeroes, which is exactly the "empty"
// marker, so nothing has to be written to mean "nothing here".
func (s *diskNodeStore) initFile() error {
	var hdr [diskHeaderLen]byte
	copy(hdr[:], diskNodeMagic)
	binary.BigEndian.PutUint32(hdr[8:12], diskNodeVersion)
	binary.BigEndian.PutUint64(hdr[12:20], s.slots)
	// Zero the file FIRST. Truncating down and back up leaves whatever was in
	// the surviving prefix, and the slot a node lands in depends on the slot
	// COUNT — so a store reopened at a different size would keep serving nodes
	// that happen to still sit where the new arithmetic looks, which is a stale
	// cache pretending to be a fresh one.
	if err := s.f.Truncate(0); err != nil {
		return err
	}
	if _, err := s.f.WriteAt(hdr[:], 0); err != nil {
		return err
	}
	return s.f.Truncate(int64(diskHeaderLen) + int64(s.slots)*diskSlotBytes)
}

// slotOf maps a node hash to its one possible slot.
func (s *diskNodeStore) slotOf(hash string) (int64, [32]byte, bool) {
	raw, err := hex.DecodeString(hash)
	if err != nil || len(raw) != 32 {
		return 0, [32]byte{}, false
	}
	var key [32]byte
	copy(key[:], raw)
	idx := binary.BigEndian.Uint64(key[:8]) % s.slots
	return int64(diskHeaderLen) + int64(idx)*diskSlotBytes, key, true
}

func (s *diskNodeStore) GetNode(hash string) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, false
	}
	off, key, ok := s.slotOf(hash)
	if !ok {
		return nil, false
	}
	buf := make([]byte, diskSlotBytes)
	if _, err := s.f.ReadAt(buf, off); err != nil {
		return nil, false // short read, torn write, anything: a miss
	}
	// The hash in the slot is what decides. A different node living here is a
	// collision, and a zero hash is an empty slot; both are misses, and neither
	// can return bytes that are not the node asked for.
	if [32]byte(buf[:32]) != key {
		return nil, false
	}
	n := binary.BigEndian.Uint32(buf[32:36])
	if n == 0 || int(n) > diskMaxNode {
		return nil, false
	}
	out := make([]byte, n)
	copy(out, buf[diskSlotHdr:diskSlotHdr+int(n)])
	return out, true
}

func (s *diskNodeStore) PutNode(hash string, data []byte) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil
	}
	if len(data) == 0 || len(data) > diskMaxNode {
		// Too big to cache. Not an error: the node is still perfectly valid, it
		// just will not be found again, and a miss is only slow.
		return nil
	}
	off, key, ok := s.slotOf(hash)
	if !ok {
		return nil
	}
	buf := make([]byte, diskSlotBytes)
	copy(buf[:32], key[:])
	binary.BigEndian.PutUint32(buf[32:36], uint32(len(data)))
	copy(buf[diskSlotHdr:], data)
	// A failed write is a cache that did not take the node. Reporting it would
	// propagate an I/O error into block application, which would turn a full
	// disk into a chain halt — for a structure whose absence costs only time.
	_, _ = s.f.WriteAt(buf, off)
	return nil
}

// Close releases the file. Further operations become misses rather than panics,
// because a store closed under a reader is a cache that went away.
func (s *diskNodeStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.f.Close()
}

// DiskTrieCachePath is where a chain's disk trie cache sits, beside its store.
func DiskTrieCachePath(storePath string) string { return storePath + ".trie" }

// UseDiskTrieCache points the chain's commitment cache at a file instead of
// memory, and reports the store it installed so a caller can close it.
//
// It is opt-in. The in-memory cache is faster per lookup; this one is bounded
// in RAM rather than in node count and survives a restart, which is the trade a
// node with a large ledger wants and a devnet does not.
func (bc *Blockchain) UseDiskTrieCache(path string, slots int) error {
	store, err := OpenDiskNodeStore(path, slots)
	if err != nil {
		return err
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()
	if bc.nodes != nil {
		if c, ok := bc.nodes.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	}
	bc.nodes = store
	return nil
}
