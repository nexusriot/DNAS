package core

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// An atomic reorg.
//
// The block log used to truncate to the fork and then append the winning
// suffix. Once the truncate landed there was no old chain left to roll back
// to, so a failure partway poisoned the store: loud, and unrecoverable.
//
// The sequence is now three steps, and the main log is not touched until the
// third:
//
//  1. Write the winning suffix to a side file and fsync it.
//  2. Publish a commit record (temp file, fsync, rename) naming that suffix.
//  3. Apply the suffix to the main log.
//
// A crash before the commit record leaves the main log as it was; the torn
// side file is discarded on the next open. A crash after the commit record
// is finished on the next open from the side file, whether or not the apply
// had started — truncate-and-append is idempotent once the suffix is known.
// A commit record that does not match its side file is the one case that
// still refuses to guess: the store will not open, and the error names
// `dnas db verify`.

const reorgMagic = "DNASREOG"

type reorgCommit struct {
	fork  uint64
	count uint32
	sum   [32]byte
}

func reorgSidePath(storePath string) string   { return storePath + ".reorg" }
func reorgCommitPath(storePath string) string { return storePath + ".reorg.commit" }

// commitReorg publishes suffix as the chain above fork and applies it to the
// log. The main log is unchanged if this returns an error before the apply;
// an error from the apply poisons the store, and the commit record is left
// in place so the next open can finish the job.
func (s *blockStore) commitReorg(fork uint64, suffix []Block) error {
	s.mu.Lock()
	err := s.errPoisonedLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	sum, err := writeReorgSide(s.path, fork, suffix)
	if err != nil {
		return err
	}
	if err := writeReorgCommit(s.path, fork, uint32(len(suffix)), sum); err != nil {
		// The rename may have landed before the failure (a failed directory
		// fsync, say). A commit record left behind for a reorg memory never
		// adopted would make the next open switch the log to it — and truncate
		// every block appended in the meantime. Withdraw it; if even that fails,
		// refuse further writes rather than let the log run ahead of a record
		// that contradicts it.
		if rerr := removeReorgFiles(s.path); rerr != nil {
			s.poison(fmt.Errorf("withdraw failed reorg commit: %w", rerr))
		}
		return err
	}
	if err := s.applyReorgSuffix(fork, suffix); err != nil {
		s.poison(err)
		return fmt.Errorf("persist reorg: %w", err)
	}
	// The log is switched, so this is success whatever the cleanup does: a
	// record that outlives it is re-checked on the next open and found applied
	// (see reorgApplied), even if more blocks have been appended by then.
	_ = removeReorgFiles(s.path)
	return nil
}

func (s *blockStore) applyReorgSuffix(fork uint64, suffix []Block) error {
	if err := s.truncateAfter(fork); err != nil {
		return err
	}
	for _, b := range suffix {
		if err := s.append(b); err != nil {
			return err
		}
	}
	return nil
}

func writeReorgSide(storePath string, fork uint64, suffix []Block) ([32]byte, error) {
	var zero [32]byte
	tmp := storePath + ".reorg.tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return zero, err
	}
	fail := func(err error) ([32]byte, error) {
		f.Close()
		_ = os.Remove(tmp)
		return zero, err
	}
	// Payload is hashed, then the file is magic || sum || payload, so a reader
	// can check the body against the front of the file before trusting it.
	var payload []byte
	payload = binary.BigEndian.AppendUint64(payload, fork)
	payload = binary.BigEndian.AppendUint32(payload, uint32(len(suffix)))
	for _, b := range suffix {
		data, err := encodeStoredBlock(b)
		if err != nil {
			return fail(err)
		}
		payload = binary.BigEndian.AppendUint32(payload, uint32(len(data)))
		payload = append(payload, data...)
	}
	sum := sha256.Sum256(payload)
	if _, err := f.Write([]byte(reorgMagic)); err != nil {
		return fail(err)
	}
	if _, err := f.Write(sum[:]); err != nil {
		return fail(err)
	}
	if _, err := f.Write(payload); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return zero, err
	}
	if err := os.Rename(tmp, reorgSidePath(storePath)); err != nil {
		_ = os.Remove(tmp)
		return zero, err
	}
	// The side file must be durable before the commit record names it.
	if err := syncDir(filepath.Dir(storePath)); err != nil {
		return zero, err
	}
	return sum, nil
}

func writeReorgCommit(storePath string, fork uint64, count uint32, sum [32]byte) error {
	tmp := storePath + ".reorg.commit.tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	var buf []byte
	buf = append(buf, []byte(reorgMagic)...)
	buf = binary.BigEndian.AppendUint64(buf, fork)
	buf = binary.BigEndian.AppendUint32(buf, count)
	buf = append(buf, sum[:]...)
	if _, err := f.Write(buf); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, reorgCommitPath(storePath)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// The rename is the commit. Until the directory entry is durable, a power
	// loss can drop it after the apply has already truncated the main log —
	// the one outcome this file exists to prevent.
	return syncDir(filepath.Dir(storePath))
}

// syncDir fsyncs a directory so the renames in it survive a power loss.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// recoverReorg finishes or discards a reorg the last process did not. It is
// called before the log is read, so the blocks openStore returns are the
// chain the commit record named.
func recoverReorg(storePath string) error {
	commit, ok, err := readReorgCommit(storePath)
	if err != nil {
		return err
	}
	if !ok {
		_ = os.Remove(reorgSidePath(storePath))
		_ = os.Remove(storePath + ".reorg.tmp")
		return nil
	}
	fork, suffix, err := readReorgSide(storePath, commit)
	if errors.Is(err, os.ErrNotExist) {
		// A commit record with no side file. The side file is made durable
		// BEFORE the record is written, so the only way to get here is a cleanup
		// that removed the side file and crashed before removing the record (the
		// order an older build used). The reorg was applied before cleanup began;
		// if the log is long enough to hold it, drop the stale record.
		s, blocks, oerr := openStoreFile(storePath)
		if oerr != nil {
			return oerr
		}
		s.close()
		if uint64(len(blocks)) >= commit.fork+1+uint64(commit.count) {
			return removeReorgFiles(storePath)
		}
	}
	if err != nil {
		return fmt.Errorf("reorg commit record does not match its side file (%w); "+
			"the main log was not switched on the strength of a record that does not "+
			"check — restore the chain from a copy and run `dnas db verify`", err)
	}
	s, blocks, err := openStoreFile(storePath)
	if err != nil {
		return err
	}
	defer s.close()
	if reorgApplied(blocks, fork, suffix) {
		return removeReorgFiles(storePath)
	}
	if err := s.applyReorgSuffix(fork, suffix); err != nil {
		return fmt.Errorf("finish persisted reorg: %w", err)
	}
	return removeReorgFiles(storePath)
}

// reorgApplied reports whether the log already holds suffix directly above
// fork. Blocks appended AFTER the suffix do not make it "not applied": a node
// that switched branches and kept mining extends the log past the suffix, and
// treating that as unapplied would truncate those blocks on the next open.
func reorgApplied(blocks []Block, fork uint64, suffix []Block) bool {
	want := int(fork) + 1 + len(suffix)
	if len(blocks) < want {
		return false
	}
	for i, b := range suffix {
		if blocks[int(fork)+1+i].Hash != b.Hash {
			return false
		}
	}
	return true
}

func readReorgCommit(storePath string) (reorgCommit, bool, error) {
	var c reorgCommit
	data, err := os.ReadFile(reorgCommitPath(storePath))
	if os.IsNotExist(err) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	// magic(8) + fork(8) + count(4) + sum(32)
	if len(data) != 8+8+4+32 || string(data[:8]) != reorgMagic {
		return c, false, errors.New("torn or corrupt reorg commit record")
	}
	c.fork = binary.BigEndian.Uint64(data[8:16])
	c.count = binary.BigEndian.Uint32(data[16:20])
	copy(c.sum[:], data[20:52])
	return c, true, nil
}

func readReorgSide(storePath string, commit reorgCommit) (uint64, []Block, error) {
	f, err := os.Open(reorgSidePath(storePath))
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()
	hdr := make([]byte, len(reorgMagic)+32)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return 0, nil, err
	}
	if string(hdr[:len(reorgMagic)]) != reorgMagic {
		return 0, nil, errors.New("side file has the wrong magic")
	}
	payload, err := io.ReadAll(f)
	if err != nil {
		return 0, nil, err
	}
	sum := sha256.Sum256(payload)
	var fileSum [32]byte
	copy(fileSum[:], hdr[len(reorgMagic):])
	if fileSum != sum || sum != commit.sum {
		return 0, nil, errors.New("side file checksum mismatch")
	}
	if len(payload) < 12 {
		return 0, nil, errors.New("side file is truncated")
	}
	fork := binary.BigEndian.Uint64(payload[:8])
	count := binary.BigEndian.Uint32(payload[8:12])
	if fork != commit.fork || count != commit.count {
		return 0, nil, errors.New("side file does not match the commit record")
	}
	rest := payload[12:]
	suffix := make([]Block, 0, count)
	for i := uint32(0); i < count; i++ {
		if len(rest) < 4 {
			return 0, nil, errors.New("side file ended inside a block")
		}
		n := binary.BigEndian.Uint32(rest[:4])
		rest = rest[4:]
		if uint64(n) > maxStoredBlockBytes || int(n) > len(rest) {
			return 0, nil, errors.New("side file block length is invalid")
		}
		b, err := decodeStoredBlock(rest[:n])
		if err != nil {
			return 0, nil, err
		}
		suffix = append(suffix, b)
		rest = rest[n:]
	}
	if len(rest) != 0 {
		return 0, nil, errors.New("side file has trailing bytes")
	}
	return fork, suffix, nil
}

// removeReorgFiles retires a reorg's files. The COMMIT RECORD goes first and
// is made durable before the side file is touched: a record without its side
// file is the one leftover recovery cannot check, while a side file without a
// record is simply discarded on the next open.
func removeReorgFiles(storePath string) error {
	if err := os.Remove(reorgCommitPath(storePath)); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(storePath + ".reorg.commit.tmp")
	if err := syncDir(filepath.Dir(storePath)); err != nil {
		return err
	}
	_ = os.Remove(reorgSidePath(storePath))
	_ = os.Remove(storePath + ".reorg.tmp")
	return nil
}
