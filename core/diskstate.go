package core

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
)

// A disk-backed account ledger.
//
// memState is a map, and a map is the whole ledger in RAM. StateDB exists so
// that can be a choice. diskState is the other choice: committed accounts live
// in a sorted file, and everything not yet flushed lives in an overlay small
// enough to copy.
//
// Snapshot copies the overlay and shares the file. That is the whole reason
// the overlay exists. A reorg and a candidate block both call Snapshot and
// then mutate the copy; if Snapshot copied the file, speculative application
// would cost as much as the ledger it was meant to stop holding. Sharing the
// file is safe because the file is immutable: a flush writes a new file and
// points this state at it. The flush closes the old descriptor, so a snapshot
// must not outlive a flush of the state it was taken from (see Flush).
//
// The block log stays authoritative. OpenDisk rebuilds this file by replaying
// the log, flushing whenever the overlay grows past diskFlushEvery, so a crash
// cannot double-apply a half-written account file — there is nothing in it
// that replay does not recompute. The file is the runtime home of the ledger,
// not a second chain.

const (
	diskStateMagic   = "DNASACCT"
	diskStateVersion = uint32(1)
	diskStateHeader  = 64

	// diskFlushEvery is how many unflushed accounts justify rewriting the
	// file. Below it the overlay is cheaper to keep than a full rewrite.
	diskFlushEvery = 1024
)

// diskRec is one overlay entry. A tombstone marks a key the file still has
// and this state has deleted; deleting the map entry instead would make the
// next read find the file's stale copy.
type diskRec struct {
	acc  Account
	tomb bool
}

// diskBase is one immutable generation of the account file.
type diskBase struct {
	f        *os.File
	count    uint64
	dataOff  int64
	indexOff int64
}

// diskState is a StateDB. It has no lock of its own: concurrent reads are safe
// (ReadAt on a shared descriptor, read-only overlay), and every write and
// Flush happens under the chain's write lock.
type diskState struct {
	path  string
	base  *diskBase
	dirty map[string]diskRec
	live  int
}

// NewDiskState returns an empty ledger at path, replacing whatever file was
// there. Replay fills it. Loading the previous file and then replaying the
// block log would apply every account twice.
func NewDiskState(path string) (*diskState, error) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reset account file %s: %w", path, err)
	}
	_ = os.Remove(path + ".tmp")
	return &diskState{path: path, dirty: map[string]diskRec{}}, nil
}

// openDiskState opens an account file a flush has already written. A missing
// file is an empty ledger, not an error: nothing has been flushed yet.
func openDiskState(path string) (*diskState, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return &diskState{path: path, dirty: map[string]diskRec{}}, nil
	}
	if err != nil {
		return nil, err
	}
	base, err := readDiskBase(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &diskState{path: path, base: base, dirty: map[string]diskRec{}, live: int(base.count)}, nil
}

func readDiskBase(f *os.File) (*diskBase, error) {
	var hdr [diskStateHeader]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return nil, fmt.Errorf("account file header: %w", err)
	}
	if string(hdr[:len(diskStateMagic)]) != diskStateMagic {
		return nil, fmt.Errorf("account file: unrecognized magic")
	}
	ver := binary.BigEndian.Uint32(hdr[8:12])
	if ver != diskStateVersion {
		return nil, fmt.Errorf("account file: version %d, this build reads %d", ver, diskStateVersion)
	}
	return &diskBase{
		f:        f,
		count:    binary.BigEndian.Uint64(hdr[12:20]),
		dataOff:  int64(binary.BigEndian.Uint64(hdr[20:28])),
		indexOff: int64(binary.BigEndian.Uint64(hdr[28:36])),
	}, nil
}

func (s *diskState) dirtyCount() int { return len(s.dirty) }

func (s *diskState) Get(addr string) Account {
	acc, ok := s.Lookup(addr)
	if !ok {
		return Account{}
	}
	return acc
}

func (s *diskState) Lookup(addr string) (Account, bool) {
	if rec, ok := s.dirty[addr]; ok {
		if rec.tomb {
			return Account{}, false
		}
		return cloneAccount(rec.acc), true
	}
	if s.base == nil || s.base.count == 0 {
		return Account{}, false
	}
	acc, ok := s.base.mustFind(addr)
	return acc, ok
}

func (s *diskState) Put(addr string, acc Account) {
	_, exists := s.lookupExists(addr)
	s.dirty[addr] = diskRec{acc: cloneAccount(acc)}
	if !exists {
		s.live++
	}
}

func (s *diskState) Delete(addr string) {
	if _, exists := s.lookupExists(addr); !exists {
		return
	}
	s.live--
	if s.base != nil && s.base.has(addr) {
		s.dirty[addr] = diskRec{tomb: true}
		return
	}
	// The key lived only in the overlay. Dropping the entry is the delete;
	// a tombstone would hide nothing and would still be copied by Snapshot.
	delete(s.dirty, addr)
}

func (s *diskState) Len() int { return s.live }

func (s *diskState) Range(fn func(addr string, acc Account) bool) {
	// Range has no error to return. A file that cannot be read mid-walk would
	// hand the caller a partial ledger that hashes to a wrong state root, so
	// stop loudly instead.
	if err := s.merge(func(addr string, acc Account) bool {
		return fn(addr, cloneAccount(acc))
	}); err != nil {
		panic(err)
	}
}

func (s *diskState) Snapshot() StateDB {
	d := make(map[string]diskRec, len(s.dirty))
	for k, v := range s.dirty {
		v.acc = cloneAccount(v.acc)
		d[k] = v
	}
	return &diskState{path: s.path, base: s.base, dirty: d, live: s.live}
}

// lookupExists reports whether addr is in the logical ledger. It does not copy.
func (s *diskState) lookupExists(addr string) (Account, bool) {
	if rec, ok := s.dirty[addr]; ok {
		if rec.tomb {
			return Account{}, false
		}
		return rec.acc, true
	}
	if s.base == nil {
		return Account{}, false
	}
	return s.base.mustFind(addr)
}

// merge walks the file and the overlay in address order. Tombstones suppress
// the file's copy. fn returning false stops the walk. A read error from the
// file is returned, not treated as the end of it: Flush writes what merge
// yields, and a short walk would be a ledger with accounts missing.
func (s *diskState) merge(fn func(string, Account) bool) error {
	keys := make([]string, 0, len(s.dirty))
	for k := range s.dirty {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var (
		bi  int
		bok bool
		bk  string
		ba  Account
		err error
	)
	bok, err = s.advanceBase(&bk, &ba, 0)
	if err != nil {
		return err
	}
	di := 0
	for bok || di < len(keys) {
		switch {
		case di < len(keys) && (!bok || keys[di] < bk):
			rec := s.dirty[keys[di]]
			if !rec.tomb && !fn(keys[di], rec.acc) {
				return nil
			}
			di++
		case di < len(keys) && keys[di] == bk:
			rec := s.dirty[keys[di]]
			if !rec.tomb && !fn(keys[di], rec.acc) {
				return nil
			}
			di++
			bi++
			if bok, err = s.advanceBase(&bk, &ba, bi); err != nil {
				return err
			}
		default:
			if !fn(bk, ba) {
				return nil
			}
			bi++
			if bok, err = s.advanceBase(&bk, &ba, bi); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *diskState) advanceBase(key *string, acc *Account, i int) (bool, error) {
	if s.base == nil || i >= int(s.base.count) {
		return false, nil
	}
	k, a, err := s.base.recordAt(i)
	if err != nil {
		return false, fmt.Errorf("account file record %d: %w", i, err)
	}
	*key, *acc = k, a
	return true, nil
}

// Flush writes a new sorted file from the file plus the overlay and swaps it
// in. The overlay is cleared, so afterwards RAM holds no accounts.
//
// Call it on the canonical state only, with no snapshot of it still in use:
// the previous file descriptor is closed, and a snapshot that shared it would
// start failing its reads. Blockchain flushes under its own write lock, after
// speculative copies have been discarded or adopted.
func (s *diskState) Flush() error {
	if len(s.dirty) == 0 {
		return nil
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("account flush: %w", err)
	}
	fail := func(err error) error {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if _, err := f.Write(make([]byte, diskStateHeader)); err != nil {
		return fail(err)
	}
	off := int64(diskStateHeader)
	offsets := make([]int64, 0, s.live)
	var nerr error
	if err := s.merge(func(addr string, acc Account) bool {
		offsets = append(offsets, off)
		n, err := writeAccountRecord(f, addr, acc)
		if err != nil {
			nerr = err
			return false
		}
		off += int64(n)
		return true
	}); err != nil {
		return fail(err)
	}
	if nerr != nil {
		return fail(nerr)
	}
	indexOff := off
	var ob [8]byte
	for _, o := range offsets {
		binary.BigEndian.PutUint64(ob[:], uint64(o))
		if _, err := f.Write(ob[:]); err != nil {
			return fail(err)
		}
	}
	var hdr [diskStateHeader]byte
	copy(hdr[:], diskStateMagic)
	binary.BigEndian.PutUint32(hdr[8:12], diskStateVersion)
	binary.BigEndian.PutUint64(hdr[12:20], uint64(len(offsets)))
	binary.BigEndian.PutUint64(hdr[20:28], diskStateHeader)
	binary.BigEndian.PutUint64(hdr[28:36], uint64(indexOff))
	if _, err := f.WriteAt(hdr[:], 0); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("account flush: %w", err)
	}
	nf, err := os.Open(s.path)
	if err != nil {
		return err
	}
	base, err := readDiskBase(nf)
	if err != nil {
		nf.Close()
		return err
	}
	old := s.base
	s.base = base
	s.dirty = map[string]diskRec{}
	s.live = len(offsets)
	if old != nil && old.f != nil {
		_ = old.f.Close()
	}
	return nil
}

func writeAccountRecord(w io.Writer, addr string, acc Account) (int, error) {
	if len(addr) > 65535 {
		return 0, fmt.Errorf("account key is %d bytes", len(addr))
	}
	val, err := json.Marshal(acc)
	if err != nil {
		return 0, err
	}
	var hdr [6]byte
	binary.BigEndian.PutUint16(hdr[:2], uint16(len(addr)))
	binary.BigEndian.PutUint32(hdr[2:], uint32(len(val)))
	n := 0
	if _, err := w.Write(hdr[:2]); err != nil {
		return 0, err
	}
	n += 2
	if _, err := w.Write([]byte(addr)); err != nil {
		return 0, err
	}
	n += len(addr)
	if _, err := w.Write(hdr[2:]); err != nil {
		return 0, err
	}
	n += 4
	if _, err := w.Write(val); err != nil {
		return 0, err
	}
	n += len(val)
	return n, nil
}

func (b *diskBase) has(addr string) bool {
	_, ok := b.mustFind(addr)
	return ok
}

// mustFind is find for the StateDB methods, which have no error to return.
//
// A read error used to come back as "no such account". That is the one answer
// that is never safe: a funded account reads as empty, so a valid block fails
// validation and the node splits from the network — or, worse, an existing
// account is re-created at nonce 0, and every transaction it ever signed
// becomes valid again. Like Range, a ledger that cannot be read stops loudly.
func (b *diskBase) mustFind(addr string) (Account, bool) {
	if b.count == 0 {
		return Account{}, false
	}
	if b.f == nil {
		panic("account file read after the chain was closed")
	}
	acc, ok, err := b.find(addr)
	if err != nil {
		panic(fmt.Errorf("account file read failed for %s: %w", addr, err))
	}
	return acc, ok
}

func (b *diskBase) find(addr string) (Account, bool, error) {
	lo, hi := 0, int(b.count)
	for lo < hi {
		mid := (lo + hi) / 2
		k, err := b.keyAt(mid)
		if err != nil {
			return Account{}, false, err
		}
		if k < addr {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo >= int(b.count) {
		return Account{}, false, nil
	}
	k, acc, err := b.recordAt(lo)
	if err != nil || k != addr {
		return Account{}, false, err
	}
	return acc, true, nil
}

func (b *diskBase) keyAt(i int) (string, error) {
	off, err := b.offsetAt(i)
	if err != nil {
		return "", err
	}
	var ln [2]byte
	if _, err := b.f.ReadAt(ln[:], off); err != nil {
		return "", err
	}
	n := int(binary.BigEndian.Uint16(ln[:]))
	buf := make([]byte, n)
	if _, err := b.f.ReadAt(buf, off+2); err != nil {
		return "", err
	}
	return string(buf), nil
}

func (b *diskBase) recordAt(i int) (string, Account, error) {
	off, err := b.offsetAt(i)
	if err != nil {
		return "", Account{}, err
	}
	var ln [2]byte
	if _, err := b.f.ReadAt(ln[:], off); err != nil {
		return "", Account{}, err
	}
	kl := int(binary.BigEndian.Uint16(ln[:]))
	key := make([]byte, kl)
	if _, err := b.f.ReadAt(key, off+2); err != nil {
		return "", Account{}, err
	}
	var vl [4]byte
	if _, err := b.f.ReadAt(vl[:], off+2+int64(kl)); err != nil {
		return "", Account{}, err
	}
	n := int(binary.BigEndian.Uint32(vl[:]))
	val := make([]byte, n)
	if _, err := b.f.ReadAt(val, off+2+int64(kl)+4); err != nil {
		return "", Account{}, err
	}
	var acc Account
	if err := json.Unmarshal(val, &acc); err != nil {
		return "", Account{}, err
	}
	return string(key), acc, nil
}

func (b *diskBase) offsetAt(i int) (int64, error) {
	var buf [8]byte
	if _, err := b.f.ReadAt(buf[:], b.indexOff+int64(i)*8); err != nil {
		return 0, err
	}
	return int64(binary.BigEndian.Uint64(buf[:])), nil
}

// cloneAccount copies an account, including its asset map, so a later mutation
// of the caller's value cannot change the one the ledger stored. The undo log
// depends on that: it records the account as it was, and a shared map would
// make the record follow the write.
func cloneAccount(a Account) Account {
	if a.Assets == nil {
		return a
	}
	m := make(map[string]uint64, len(a.Assets))
	for k, v := range a.Assets {
		m[k] = v
	}
	a.Assets = m
	return a
}
