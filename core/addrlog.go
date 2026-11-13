package core

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// A durable address index.
//
// The in-memory index answers "what has this address done?" with one map
// lookup, and it is also the thing whose size is not bounded by the chain: an
// address in a million transactions has a million entries, rebuilt from
// scratch every time the node starts. This log is that index on disk.
//
// Records are appended as blocks connect and as a reorg disconnects them
// ('+' and '-'). RAM holds only where each address's records are (8 bytes a
// record, no transaction data); a query reads and folds that address's
// records alone, under the log's own lock rather than the chain's. The header records the
// chain tip the log was built for; a mismatch — a crash mid-reorg, a log from
// another chain — rebuilds from the blocks rather than trusting a torn
// history. Close compacts the '-' records away.

const (
	addrLogMagic  = "DNASAIDX"
	addrLogHeader = 64
	addrOpAdd     = byte('+')
	addrOpDel     = byte('-')
)

type addrLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	// size is the end of the last whole record: the next append goes here.
	size int64
	// offs is every record ('+' and '-') written for an address, in file
	// order. It holds file offsets, not postings, so a query reads only its
	// own records instead of the whole log. Built once by sync.
	offs map[string][]int64
	// broken is the first write that failed. From then on the log no longer
	// matches the chain, so it answers no queries and takes no more writes;
	// its header has been reset so the next start rebuilds it.
	broken error
}

var errAddrLogClosed = errors.New("address index is closed")

func openAddrLog(path string) (*addrLog, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	l := &addrLog{path: path, f: f, offs: map[string][]int64{}}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	l.size = info.Size()
	if l.size == 0 {
		if err := l.writeHeader(0, ""); err != nil {
			f.Close()
			return nil, err
		}
		l.size = addrLogHeader
	}
	return l, nil
}

func (l *addrLog) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	l.offs = nil
	return err
}

// sync keeps the log if it was built for this exact tip and its records read
// back whole, and rebuilds it otherwise. A torn tail under a matching tip
// fails the load and is rebuilt rather than served.
func (l *addrLog) sync(blocks []Block) error {
	if len(blocks) == 0 {
		return fmt.Errorf("address index: no blocks")
	}
	tip := blocks[len(blocks)-1]
	h, hash, err := l.readTip()
	if err != nil {
		return err
	}
	if hash == tip.Hash && h == tip.Index {
		l.mu.Lock()
		err := l.loadIndexLocked()
		l.mu.Unlock()
		if err == nil {
			return nil
		}
	}
	return l.rebuild(blocks)
}

// loadIndexLocked walks the log once and records where each address's
// records are.
func (l *addrLog) loadIndexLocked() error {
	info, err := l.f.Stat()
	if err != nil {
		return err
	}
	offs := map[string][]int64{}
	off := int64(addrLogHeader)
	for off < info.Size() {
		r, n, err := l.readRecLocked(off)
		if err != nil {
			return fmt.Errorf("address index record at %d: %w", off, err)
		}
		offs[r.addr] = append(offs[r.addr], off)
		off += n
	}
	l.offs = offs
	l.size = off
	return nil
}

func (l *addrLog) rebuild(blocks []Block) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	if err := l.writeHeaderLocked(0, ""); err != nil {
		return err
	}
	l.size = addrLogHeader
	l.offs = map[string][]int64{}
	for _, b := range blocks {
		if err := l.addBlockLocked(b); err != nil {
			return err
		}
	}
	tip := blocks[len(blocks)-1]
	return l.writeHeaderLocked(tip.Index, tip.Hash)
}

func (l *addrLog) addBlock(b Block) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return errAddrLogClosed
	}
	if l.broken != nil {
		return l.broken
	}
	if err := l.addBlockLocked(b); err != nil {
		return l.breakLocked(err)
	}
	// The records must be durable before a header claims the tip they bring
	// the log to. Without the sync, a power loss can persist the header and not
	// the records, and the next start trusts a log with postings missing.
	if err := l.f.Sync(); err != nil {
		return l.breakLocked(err)
	}
	if err := l.writeHeaderLocked(b.Index, b.Hash); err != nil {
		return l.breakLocked(err)
	}
	return nil
}

// breakLocked records a failed write and resets the header to "no tip", so the
// next start rebuilds the log from the blocks instead of serving a history
// with a hole in it. The reset is best effort: if it fails too, the log's tip
// still names a block this log does not fully describe, which is no worse.
func (l *addrLog) breakLocked(err error) error {
	if l.broken == nil {
		l.broken = fmt.Errorf("address index write failed (rebuilt on restart): %w", err)
		_ = l.writeHeaderLocked(0, "")
		_ = l.f.Sync()
	}
	return l.broken
}

func (l *addrLog) addBlockLocked(b Block) error {
	for i, tx := range b.Transactions {
		for _, addr := range txAddresses(tx) {
			if err := l.appendLocked(addrOpAdd, addr, b.Index, i); err != nil {
				return err
			}
		}
	}
	return nil
}

func (l *addrLog) dropBlock(b Block) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return errAddrLogClosed
	}
	if l.broken != nil {
		return l.broken
	}
	for i, tx := range b.Transactions {
		for _, addr := range txAddresses(tx) {
			if err := l.appendLocked(addrOpDel, addr, b.Index, i); err != nil {
				return l.breakLocked(err)
			}
		}
	}
	return nil
}

type addrRec struct {
	op     byte
	addr   string
	height uint64
	index  int
}

const addrRecHeader = 1 + 2 + 8 + 4

// appendLocked writes one record at l.size in a single write. The offset is
// indexed only once the write succeeded; a failed write leaves l.size where it
// was, so the next append overwrites the partial bytes.
func (l *addrLog) appendLocked(op byte, addr string, height uint64, index int) error {
	if len(addr) > 65535 {
		return fmt.Errorf("address is %d bytes", len(addr))
	}
	buf := make([]byte, addrRecHeader, addrRecHeader+len(addr))
	buf[0] = op
	binary.BigEndian.PutUint16(buf[1:3], uint16(len(addr)))
	binary.BigEndian.PutUint64(buf[3:11], height)
	binary.BigEndian.PutUint32(buf[11:15], uint32(index))
	buf = append(buf, addr...)
	if _, err := l.f.WriteAt(buf, l.size); err != nil {
		return err
	}
	l.offs[addr] = append(l.offs[addr], l.size)
	l.size += int64(len(buf))
	return nil
}

// readRecLocked reads the record at off and returns it with its length.
func (l *addrLog) readRecLocked(off int64) (addrRec, int64, error) {
	var hdr [addrRecHeader]byte
	if _, err := l.f.ReadAt(hdr[:], off); err != nil {
		return addrRec{}, 0, err
	}
	if hdr[0] != addrOpAdd && hdr[0] != addrOpDel {
		return addrRec{}, 0, fmt.Errorf("unknown op %q", hdr[0])
	}
	n := int(binary.BigEndian.Uint16(hdr[1:3]))
	buf := make([]byte, n)
	if _, err := l.f.ReadAt(buf, off+addrRecHeader); err != nil {
		return addrRec{}, 0, err
	}
	return addrRec{
		op:     hdr[0],
		addr:   string(buf),
		height: binary.BigEndian.Uint64(hdr[3:11]),
		index:  int(binary.BigEndian.Uint32(hdr[11:15])),
	}, addrRecHeader + int64(n), nil
}

// history returns the live postings for addr, oldest first. It reads only
// addr's own records and takes no chain lock.
func (l *addrLog) history(addr string) ([]TxLoc, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil, errAddrLogClosed
	}
	if l.broken != nil {
		return nil, l.broken
	}
	return l.foldLocked(addr)
}

// foldLocked applies addr's '+' and '-' records in file order.
func (l *addrLog) foldLocked(addr string) ([]TxLoc, error) {
	var locs []TxLoc
	for _, off := range l.offs[addr] {
		r, _, err := l.readRecLocked(off)
		if err != nil {
			return nil, err
		}
		if r.addr != addr {
			return nil, fmt.Errorf("address index record at %d is not %s", off, addr)
		}
		if r.op == addrOpDel {
			kept := locs[:0]
			for _, loc := range locs {
				if loc.Height != r.height || loc.Index != r.index {
					kept = append(kept, loc)
				}
			}
			locs = kept
			continue
		}
		locs = append(locs, TxLoc{Height: r.height, Index: r.index})
	}
	return locs, nil
}

// compact rewrites the log as the live '+' records only.
func (l *addrLog) compact(tipHeight uint64, tipHash string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return errAddrLogClosed
	}
	if l.broken != nil {
		// The header is already reset; compacting would stamp a tip on a log
		// that is missing records.
		return nil
	}
	// Deterministic, so two compactions of the same index match byte for byte.
	addrs := make([]string, 0, len(l.offs))
	for a := range l.offs {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	live := make(map[string][]TxLoc, len(addrs))
	for _, a := range addrs {
		locs, err := l.foldLocked(a)
		if err != nil {
			return err
		}
		live[a] = locs
	}

	tmp := l.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	oldF, oldSize, oldOffs := l.f, l.size, l.offs
	l.f, l.size, l.offs = f, addrLogHeader, map[string][]int64{}
	fail := func(err error, closeTmp bool) error {
		if closeTmp {
			f.Close()
		}
		l.f, l.size, l.offs = oldF, oldSize, oldOffs
		_ = os.Remove(tmp)
		return err
	}
	if err := l.writeHeaderLocked(0, ""); err != nil {
		return fail(err, true)
	}
	for _, addr := range addrs {
		for _, loc := range live[addr] {
			if err := l.appendLocked(addrOpAdd, addr, loc.Height, loc.Index); err != nil {
				return fail(err, true)
			}
		}
	}
	if err := l.writeHeaderLocked(tipHeight, tipHash); err != nil {
		return fail(err, true)
	}
	if err := f.Sync(); err != nil {
		return fail(err, true)
	}
	if err := f.Close(); err != nil {
		return fail(err, false)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return fail(err, false)
	}
	_ = syncDir(filepath.Dir(l.path)) // the rename is derivative data; a lost one rebuilds
	_ = oldF.Close()
	// l.offs and l.size already describe the renamed file.
	nf, err := os.OpenFile(l.path, os.O_RDWR, 0o644)
	if err != nil {
		l.f, l.offs = nil, nil
		return err
	}
	l.f = nf
	return nil
}

func (l *addrLog) readTip() (uint64, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var hdr [addrLogHeader]byte
	if _, err := l.f.ReadAt(hdr[:], 0); err != nil {
		return 0, "", err
	}
	if string(hdr[:len(addrLogMagic)]) != addrLogMagic {
		return 0, "", fmt.Errorf("address index: unrecognized file")
	}
	height := binary.BigEndian.Uint64(hdr[12:20])
	var sum [32]byte
	copy(sum[:], hdr[20:52])
	var zero [32]byte
	if sum == zero {
		return height, "", nil
	}
	return height, hex.EncodeToString(sum[:]), nil
}

func (l *addrLog) writeHeader(height uint64, hash string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.writeHeaderLocked(height, hash)
}

func (l *addrLog) writeHeaderLocked(height uint64, hash string) error {
	var hdr [addrLogHeader]byte
	copy(hdr[:], addrLogMagic)
	binary.BigEndian.PutUint32(hdr[8:12], 1)
	binary.BigEndian.PutUint64(hdr[12:20], height)
	if hash != "" {
		raw, err := hex.DecodeString(hash)
		if err != nil || len(raw) != 32 {
			return fmt.Errorf("address index: tip hash %q", hash)
		}
		copy(hdr[20:52], raw)
	}
	_, err := l.f.WriteAt(hdr[:], 0)
	return err
}
