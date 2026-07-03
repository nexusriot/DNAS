package core

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
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
		var b Block
		if err := json.Unmarshal(buf, &b); err != nil {
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

// append writes one block record and fsyncs.
func (s *blockStore) append(b Block) error {
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
