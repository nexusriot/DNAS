package core

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// maxStoredBlockBytes bounds a single record, guarding against a corrupt length
// prefix causing a huge allocation.
const maxStoredBlockBytes = 64 << 20

// blockStore is an append-only log of blocks. Each record is a 4-byte
// big-endian length followed by the block's JSON. Appending is O(1); a reorg
// truncates the log back to the fork point and appends the new suffix. This
// replaces rewriting the whole chain file on every block.
type blockStore struct {
	mu      sync.Mutex
	f       *os.File
	offsets []int64 // offsets[i] = byte offset of block i (len == blocks stored)
	size    int64   // total bytes of intact records

	// poisoned is set when a write failed partway through a sequence that cannot
	// be rolled back — specifically a reorg, which truncates to the fork point
	// before appending the winning suffix (see Blockchain.reorgLocked). Once the
	// truncate has happened the log can no longer represent the old chain, so a
	// failed append leaves disk holding a prefix of a chain that memory is not
	// running. Continuing to write would append the old chain's continuation on
	// top of the new suffix and produce a log that will not replay on restart.
	// Refusing every subsequent write keeps the damage bounded and legible: the
	// operator gets a loud error naming `dnas db verify` instead of a store that
	// silently stops loading days later.
	poisoned error
}

// poison marks the store unusable for further writes. It is idempotent: the
// first cause is the interesting one, so a later failure does not overwrite it.
func (s *blockStore) poison(cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned == nil {
		s.poisoned = cause
	}
}

// errPoisoned reports why the store is refusing writes, or nil if it is healthy.
// The caller must hold s.mu.
func (s *blockStore) errPoisonedLocked() error {
	if s.poisoned == nil {
		return nil
	}
	return fmt.Errorf("block store is poisoned by an earlier failed write (%w); "+
		"the on-disk chain may diverge from memory - stop this node and run `dnas db verify`", s.poisoned)
}

// readStore reads every intact block from the log at path WITHOUT opening it for
// writing and without repairing anything. It exists for inspection (see
// dbtool.go): openStore truncates a torn trailing record, which is the right
// thing when a node is taking ownership of its own store and completely the
// wrong thing when a tool is merely looking at one — a `db info` run against a
// live node's chain would otherwise destroy a block the node had just appended.
//
// It reports the file's size alongside the blocks so a caller can tell that a
// torn record IS present (bytes read < file size) without doing anything about it.
func readStore(path string) (blocks []Block, intact, total int64, err error) {
	f, err := os.Open(path) // read-only: no create, no truncate
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, 0, err
	}
	total = fi.Size()

	r := bufio.NewReader(f)
	var off int64
	for off < total {
		var lenBuf [4]byte
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			break
		}
		n := binary.BigEndian.Uint32(lenBuf[:])
		if n == 0 || uint64(n) > maxStoredBlockBytes {
			break
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			break
		}
		b, derr := decodeStoredBlock(buf)
		if derr != nil {
			break
		}
		blocks = append(blocks, b)
		off += 4 + int64(n)
	}
	if len(blocks) == 0 && total > 0 {
		return nil, 0, total, errors.New("not a DNAS block store (unrecognized or fully corrupt file)")
	}
	return blocks, off, total, nil
}

// openStore opens (or creates) the log at path and returns every stored block.
// A torn trailing record (from a crash mid-append) is truncated away; a
// non-empty file that yields no valid records is treated as foreign and left
// untouched (an error is returned rather than clobbering it).
func openStore(path string) (*blockStore, []Block, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	fileLen := fi.Size()

	s := &blockStore{f: f}
	var blocks []Block
	r := bufio.NewReader(f)
	var off int64
	corrupt := false
	for off < fileLen {
		var lenBuf [4]byte
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			corrupt = true
			break
		}
		n := binary.BigEndian.Uint32(lenBuf[:])
		if n == 0 || uint64(n) > maxStoredBlockBytes {
			corrupt = true
			break
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			corrupt = true
			break
		}
		b, derr := decodeStoredBlock(buf)
		if derr != nil {
			corrupt = true
			break
		}
		blocks = append(blocks, b)
		s.offsets = append(s.offsets, off)
		off += 4 + int64(n)
	}
	if corrupt && len(blocks) == 0 && fileLen > 0 {
		f.Close()
		return nil, nil, errors.New("not a DNAS block store (unrecognized or fully corrupt file)")
	}
	if off != fileLen { // drop any torn trailing record
		if err := f.Truncate(off); err != nil {
			f.Close()
			return nil, nil, err
		}
	}
	s.size = off
	return s, blocks, nil
}

// encodeStoredBlock / decodeStoredBlock are the store's record format, kept
// behind one pair of functions so it can change without touching the framing,
// the offsets or the reorg logic. It is deliberately NOT the consensus codec:
// records here are local, so the format may evolve freely as long as an older
// file still loads.
// Writing uses the compact binary form (see storecodec.go). Reading accepts
// both, because a store written by an older build must still load: a JSON
// record begins with '{', so the two are told apart by the leading byte rather
// than by trial and error.
func encodeStoredBlock(b Block) ([]byte, error) { return encodeBlockV2(b), nil }

func decodeStoredBlock(data []byte) (Block, error) {
	if len(data) == 0 {
		return Block{}, errEmptyRecord
	}
	switch data[0] {
	case storeRecordV2:
		return decodeBlockV2(data)
	case '{':
		var b Block
		if err := json.Unmarshal(data, &b); err != nil {
			return Block{}, err
		}
		return b, nil
	default:
		return Block{}, fmt.Errorf("unrecognized store record (leading byte %#x)", data[0])
	}
}

// append writes one block record and fsyncs.
func (s *blockStore) append(b Block) error {
	data, err := encodeStoredBlock(b)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.errPoisonedLocked(); err != nil {
		return err
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	if _, err := s.f.WriteAt(lenBuf[:], s.size); err != nil {
		return err
	}
	if _, err := s.f.WriteAt(data, s.size+4); err != nil {
		return err
	}
	s.offsets = append(s.offsets, s.size)
	s.size += 4 + int64(len(data))
	return s.f.Sync()
}

// truncateAfter drops all blocks above the given height (keeping 0..height).
func (s *blockStore) truncateAfter(height uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.errPoisonedLocked(); err != nil {
		return err
	}
	keep := int(height) + 1
	if keep >= len(s.offsets) {
		return nil
	}
	newSize := s.offsets[keep]
	if err := s.f.Truncate(newSize); err != nil {
		return err
	}
	s.offsets = s.offsets[:keep]
	s.size = newSize
	return s.f.Sync()
}

func (s *blockStore) close() error { return s.f.Close() }

// Compaction, in two phases.
//
// `-prune` bounds a node's resident size: bodies below the cutoff are replaced
// in memory with header-only placeholders. The file kept every original record,
// so a pruned node still paid full disk for the whole chain and still replayed
// all of it on restart. Rewriting the log to match the pruned chain fixes that.
//
// The reason it is split in two is latency. The rewrite is O(whole store), and
// the first version of this ran inside the chain's write lock — so on a pruning
// node every 128th block froze the miner, every API read and every peer handler
// for the length of a full file rewrite (measured: 49ms on a 198 KB store, and
// it grows with the file). Now the expensive part runs with no lock held, and
// only the swap — appending whatever blocks arrived meanwhile, then a rename —
// happens under it.
//
// What compaction does NOT do is delete pruned heights. A pruned height keeps a
// header-only record, because the header is not optional: linkage, median-time-
// past and the difficulty retarget all read it, and a restart that could not
// rebuild the header chain could not validate anything. So this shrinks the
// store by the size of the transaction bodies — large on a busy chain, close to
// nothing on a devnet mining empty blocks. Honest either way.

// compactionTmp is the file the rewrite is staged in.
func compactionTmp(path string) string { return path + ".compact" }

// writeCompacted stages a rewritten log at the temp path. It holds NO lock and
// touches nothing live: the caller passes an immutable snapshot of the chain.
func writeCompacted(path string, blocks []Block) (offsets []int64, size int64, err error) {
	tmp := compactionTmp(path)
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, 0, fmt.Errorf("compact: create temp: %w", err)
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	offsets = make([]int64, 0, len(blocks))
	for _, b := range blocks {
		off, n, werr := writeRecordAt(f, size, b)
		if werr != nil {
			return nil, 0, werr
		}
		offsets = append(offsets, off)
		size += n
	}
	if err = f.Sync(); err != nil {
		return nil, 0, fmt.Errorf("compact: sync: %w", err)
	}
	if err = f.Close(); err != nil {
		return nil, 0, fmt.Errorf("compact: close temp: %w", err)
	}
	return offsets, size, nil
}

// writeRecordAt appends one length-framed block record at `at`, returning the
// offset it was written to and how many bytes it consumed.
func writeRecordAt(f *os.File, at int64, b Block) (off int64, n int64, err error) {
	data, err := encodeStoredBlock(b)
	if err != nil {
		return 0, 0, fmt.Errorf("compact: encode block %d: %w", b.Index, err)
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	if _, err := f.WriteAt(lenBuf[:], at); err != nil {
		return 0, 0, fmt.Errorf("compact: write length: %w", err)
	}
	if _, err := f.WriteAt(data, at+4); err != nil {
		return 0, 0, fmt.Errorf("compact: write block %d: %w", b.Index, err)
	}
	return at, 4 + int64(len(data)), nil
}

// adoptCompacted finishes a staged rewrite: it appends `tail` (blocks that were
// committed while the rewrite ran), fsyncs, and renames the temp file over the
// live one. Short, and the only part that needs the store locked.
//
// Past the rename the old file is gone, so a failure there is the unrecoverable
// shape the poison flag exists for.
func (s *blockStore) adoptCompacted(path string, offsets []int64, size int64, tail []Block) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.errPoisonedLocked(); err != nil {
		return err
	}
	tmp := compactionTmp(path)
	f, err := os.OpenFile(tmp, os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("compact: reopen staged file: %w", err)
	}
	for _, b := range tail {
		off, n, werr := writeRecordAt(f, size, b)
		if werr != nil {
			f.Close()
			os.Remove(tmp)
			return werr
		}
		offsets = append(offsets, off)
		size += n
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("compact: sync tail: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("compact: close staged file: %w", err)
	}

	if err := s.f.Close(); err != nil {
		s.poisoned = fmt.Errorf("compact: closing the old store: %w", err)
		return s.errPoisonedLocked()
	}
	if err := os.Rename(tmp, path); err != nil {
		s.poisoned = fmt.Errorf("compact: rename: %w", err)
		return s.errPoisonedLocked()
	}
	reopened, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		s.poisoned = fmt.Errorf("compact: reopening the compacted store: %w", err)
		return s.errPoisonedLocked()
	}
	s.f, s.offsets, s.size = reopened, offsets, size
	return nil
}

// discardCompaction removes a staged rewrite that will not be adopted.
func discardCompaction(path string) { os.Remove(compactionTmp(path)) }

// sizeBytes reports the store's current on-disk size.
func (s *blockStore) sizeBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}
