package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// Pruning: running a node without keeping every block body in memory.
//
// A Blockchain holds its blocks in a slice, so a long chain costs its whole size
// in RAM — at the relay limit that is roughly a megabyte per block, and a
// hundred thousand blocks is not a machine anybody has. Everything a node needs
// to VALIDATE the next block is in the account state, the headers, and the last
// few blocks: the state is the running total, the headers carry linkage,
// difficulty and median-time-past, and only a reorg reaches back into bodies.
//
// So a pruning node keeps the state and the headers for all time, and bodies
// only for the most recent PruneKeep blocks. Below that, blocks become
// header-only placeholders — exactly the representation a fast-synced node
// already uses below its snapshot (see snapshot.go), which is why nothing in
// validation, supply accounting or maturity needs to change.
//
// What a pruned node gives up is the ability to SERVE what it no longer holds:
// old block bodies, inclusion proofs for old transactions, and compact filters
// for old blocks. That last one is the trap — a filter built from a missing body
// is a valid, EMPTY filter, and an empty filter is a proof of ABSENCE. Serving
// those would tell a light client that its address is provably not in a block
// the node simply cannot read any more. So the filter for a body-less block is
// reported as unavailable rather than built (see BlockFilterAt), and a node
// publishes the height its bodies start at so clients can tell "not there" from
// "not known".

// MinPruneKeep is the smallest number of recent block bodies a pruning node may
// keep. It is above MaxReorgDepth because a reorg replays the bodies it
// disconnects: pruning inside the range a reorg can reach would make the node
// unable to follow a chain it must follow. The margin on top is for the
// difficulty window and median-time-past, which read headers rather than bodies
// but are the sort of thing that grows.
const MinPruneKeep = MaxReorgDepth + 32

// EnablePruning turns on body pruning, keeping the most recent `keep` bodies. A
// keep below MinPruneKeep is raised to it rather than accepted: a node that
// cannot reorg is not a node, and silently honouring an unsafe value would make
// that failure appear later, as an inability to follow the chain.
//
// It prunes what already qualifies, then stays on as blocks connect.
func (bc *Blockchain) EnablePruning(keep uint64) uint64 {
	if keep < MinPruneKeep {
		keep = MinPruneKeep
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()
	bc.pruneKeep = keep
	bc.pruneLocked()
	return keep
}

// PruneKeep reports how many recent bodies this node keeps, or 0 when it keeps
// everything.
func (bc *Blockchain) PruneKeep() uint64 {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.pruneKeep
}

// pruneLocked drops the bodies of blocks deeper than pruneKeep. bc.mu held for
// writing; a no-op when pruning is off.
func (bc *Blockchain) pruneLocked() {
	if bc.pruneKeep == 0 {
		return
	}
	tip := bc.blocks[len(bc.blocks)-1].Index
	if tip < bc.pruneKeep {
		return
	}
	// Everything strictly below this height loses its body.
	cutoff := tip - bc.pruneKeep
	for i := uint64(0); i < cutoff && i < uint64(len(bc.blocks)); i++ {
		b := bc.blocks[i]
		if b.IsPlaceholder() {
			continue // already pruned, or below a fast-sync snapshot
		}
		// The transaction index and the address index point into a body that is
		// about to go, so their entries for it are dropped: a lookup that returned
		// a location holding nothing would be worse than a miss.
		//
		// The ASSET registry is deliberately kept. An asset issued at height 5 still
		// exists and is still held; what is lost is the ability to fetch the
		// transaction that issued it, not the fact that it happened.
		bc.forgetBodyLocked(b)
		bc.blocks[i] = blockFromHeader(b.Header())
		bc.undos[i] = nil // no reorg may reach here (see MinPruneKeep)
		bc.pruned++
	}
	bc.maybeCompactStoreLocked(cutoff)
}

// storeCompactInterval is how many newly-pruned heights accumulate before the
// on-disk log is rewritten to match. Compaction is a whole-file rewrite, so
// doing it per block would make pruning cost O(chain) every block; batching
// makes it amortized. The cost of waiting is only that the file is briefly
// larger than memory, which is the state it was permanently in before.
const storeCompactInterval = 128

// maybeCompactStoreLocked MARKS the store as wanting a rewrite. It does not do
// one, and that is the whole point: the rewrite is O(whole store), and running
// it here — inside the chain's write lock, on the block-application path —
// froze the miner, every API read and every peer handler for its duration
// (measured: 49ms on a 198 KB store, growing with the file). A node drives the
// actual work off the hot path; see CompactStore. bc.mu held.
func (bc *Blockchain) maybeCompactStoreLocked(cutoff uint64) {
	if bc.store == nil || bc.storePath == "" {
		return
	}
	if cutoff < bc.storeCompactedTo+storeCompactInterval {
		return
	}
	bc.compactionDue = true
}

// CompactionDue reports whether pruning has advanced far enough that the store
// is worth rewriting. It is polled rather than pushed, so the decision of WHEN
// to spend the I/O stays with whoever owns the process.
func (bc *Blockchain) CompactionDue() bool {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.compactionDue && bc.store != nil && bc.storePath != ""
}

// forgetBodyLocked removes the index entries that point into a block's body,
// leaving derived facts that outlive the body (the asset registry) in place.
// bc.mu held for writing.
func (bc *Blockchain) forgetBodyLocked(b Block) {
	for _, tx := range b.Transactions {
		h := tx.Hash()
		if loc, ok := bc.txIndex[h]; ok && loc.Height == b.Index {
			delete(bc.txIndex, h)
		}
	}
	bc.unindexBlockAddressesLocked(b)
}

// BodyHeight is the lowest height whose block body this node still has. It is
// what a client needs in order to tell "your transaction is not in the chain"
// from "I cannot see that far back".
//
// It starts the search at 1 because genesis carries no transactions on any node:
// counting it would make every pruned node claim to hold bodies from 0. So an
// unpruned chain answers 1, and a pruned one answers wherever its window starts.
func (bc *Blockchain) BodyHeight() uint64 {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.bodyHeightLocked()
}

// bodyHeightLocked is BodyHeight without the lock. bc.mu held.
func (bc *Blockchain) bodyHeightLocked() uint64 {
	for i := 1; i < len(bc.blocks); i++ {
		if !bc.blocks[i].IsPlaceholder() {
			return uint64(i)
		}
	}
	// No bodies above genesis: a chain of only genesis, or a freshly fast-synced
	// node before it has extended.
	return uint64(len(bc.blocks))
}

// HasBody reports whether the node still holds the body of the block at height.
func (bc *Blockchain) HasBody(height uint64) bool {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if height >= uint64(len(bc.blocks)) {
		return false
	}
	return !bc.blocks[height].IsPlaceholder()
}

// PrunedCount is how many bodies this node has discarded, for reporting.
func (bc *Blockchain) PrunedCount() uint64 {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.pruned
}

// ErrPrunedBody is returned when a caller asks for something that needs a body
// this node no longer has. It is a distinct error because the right answer for a
// client is to ask another node, not to conclude the data does not exist.
var ErrPrunedBody = errors.New("this node has pruned that block's body")

// BlockBodyAt returns a block body, or ErrPrunedBody when it has been pruned.
// Callers that must distinguish "no such height" from "pruned" use this rather
// than BlockAt, which returns the placeholder.
func (bc *Blockchain) BlockBodyAt(height uint64) (Block, error) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if height >= uint64(len(bc.blocks)) {
		return Block{}, fmt.Errorf("no block at height %d", height)
	}
	b := bc.blocks[height]
	if b.IsPlaceholder() {
		return Block{}, fmt.Errorf("%w (height %d; bodies start at %d)", ErrPrunedBody, height, bc.bodyHeightLocked())
	}
	return b, nil
}

// StoreStats reports what the on-disk log costs and how much pruning has
// reclaimed. Without this, "the store is compacted now" is a claim rather than
// something an operator can check — and on a chain of empty blocks the saving is
// genuinely near zero, which is worth being able to see rather than assume.
type StoreStats struct {
	Bytes       int64  `json:"bytes"`        // current on-disk size
	BytesSaved  int64  `json:"bytes_saved"`  // reclaimed by compaction since start
	CompactedTo uint64 `json:"compacted_to"` // highest height whose record is header-only
	Error       string `json:"error,omitempty"`
}

// StoreStats returns the on-disk log's statistics (zero when in-memory only).
func (bc *Blockchain) StoreStats() StoreStats {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if bc.store == nil {
		return StoreStats{}
	}
	st := StoreStats{
		Bytes:       bc.store.sizeBytes(),
		BytesSaved:  bc.storeBytesSaved,
		CompactedTo: bc.storeCompactedTo,
	}
	if bc.storeCompactErr != nil {
		st.Error = bc.storeCompactErr.Error()
	}
	return st
}

// CompactStore rewrites the on-disk log to match the pruned in-memory chain,
// doing the expensive part with NO lock held.
//
// Three phases:
//
//  1. Under a READ lock, snapshot the chain and derive the state sidecar's
//     contents. Cheap: a slice copy and a walk back through the undo logs.
//  2. With NO lock, write the sidecar and stage the new log in a temp file.
//     This is the O(store) part, and nothing blocks while it runs.
//  3. Under the WRITE lock, check the snapshot is still valid (a reorg may have
//     rewritten history underneath us), append whatever blocks were committed
//     while phase 2 ran, and rename the staged file into place.
//
// If a reorg invalidated the snapshot the staged file is discarded and the
// request stays pending, so the next call simply retries against the new chain.
func (bc *Blockchain) CompactStore() error {
	// Phase 1: snapshot under a read lock.
	bc.mu.RLock()
	if bc.store == nil || bc.storePath == "" {
		bc.mu.RUnlock()
		return errors.New("this chain has no on-disk store")
	}
	path := bc.storePath
	blocks := make([]Block, len(bc.blocks))
	copy(blocks, bc.blocks)
	snapshot, snapErr := bc.stateSnapshotLocked()
	bc.mu.RUnlock()
	if snapErr != nil {
		bc.noteCompactErr(snapErr)
		return snapErr
	}

	// Phase 2: no lock held. The sidecar is written FIRST — a crash between the
	// two then leaves a complete store plus a harmless extra file, where the
	// reverse would leave a pruned store with nothing to replay from.
	if snapshot != nil {
		if err := writeStateSnapshotFile(path, *snapshot); err != nil {
			bc.noteCompactErr(err)
			return err
		}
	}
	offsets, size, err := writeCompacted(path, blocks)
	if err != nil {
		bc.noteCompactErr(err)
		return err
	}

	// Phase 3: swap under the write lock.
	bc.mu.Lock()
	defer bc.mu.Unlock()
	if bc.store == nil {
		discardCompaction(path)
		return errors.New("the store was closed while compacting")
	}
	// Did history change under us? The staged file mirrors `blocks`, so it is
	// only usable if the live chain still begins with exactly that prefix.
	if len(bc.blocks) < len(blocks) ||
		bc.blocks[len(blocks)-1].Hash != blocks[len(blocks)-1].Hash {
		discardCompaction(path)
		return errors.New("a reorg replaced the chain while compacting; will retry")
	}
	tail := bc.blocks[len(blocks):]
	before := bc.store.sizeBytes()
	if err := bc.store.adoptCompacted(path, offsets, size, tail); err != nil {
		bc.storeCompactErr = err
		return err
	}
	bc.storeBytesSaved += before - bc.store.sizeBytes()
	bc.storeCompactedTo = bc.bodyHeightLocked()
	bc.storeCompactErr = nil
	bc.compactionDue = false
	return nil
}

// noteCompactErr records a compaction failure for reporting. A failure is not
// fatal: the chain in memory is correct and authoritative, and an oversized
// file is a disk problem rather than a correctness one.
func (bc *Blockchain) noteCompactErr(err error) {
	bc.mu.Lock()
	bc.storeCompactErr = err
	bc.mu.Unlock()
}

// statePath is the sidecar holding the account state at the prune cutoff.
func statePath(storePath string) string { return storePath + ".state" }

// stateSnapshotLocked derives the account state as of the highest PRUNED
// height. It only reads chain state, so it runs under a READ lock and the
// caller writes the file afterwards with nothing held. nil means nothing is
// pruned and no sidecar is needed. bc.mu held (read is enough).
//
// This sidecar is the half of store pruning that is not optional. Dropping a
// block's body from disk drops the transactions that produced the balances, so
// a restart has the header chain and no way to recompute state from it — the
// store would shrink and then fail to load, which is worse than not pruning at
// all. A reopen replays FROM this, exactly as a fast-synced node does, and it
// is verifiable the same way: its accounts must hash to the state root
// committed in a proof-of-work-checked header.
func (bc *Blockchain) stateSnapshotLocked() (*Snapshot, error) {
	body := bc.bodyHeightLocked()
	if body == 0 || body > uint64(len(bc.blocks)) {
		return nil, nil // nothing pruned: a plain reopen can replay everything
	}
	at := body - 1 // the highest height whose body is gone
	state := cloneState(bc.state)
	for i := len(bc.blocks) - 1; i > int(at); i-- {
		applyUndo(state, bc.undos[i])
	}
	return &Snapshot{Height: at, Header: bc.blocks[at].Header(), Accounts: state}, nil
}

// writeStateSnapshotFile writes a sidecar beside the store, atomically. Holds
// no chain lock: the snapshot it is given is already an immutable copy.
func writeStateSnapshotFile(storePath string, snap Snapshot) error {
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state snapshot: %w", err)
	}
	tmp := statePath(storePath) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write state snapshot: %w", err)
	}
	return os.Rename(tmp, statePath(storePath))
}

// readStateSnapshot loads the sidecar written beside a pruned store.
func readStateSnapshot(storePath string) (Snapshot, bool, error) {
	data, err := os.ReadFile(statePath(storePath))
	if os.IsNotExist(err) {
		return Snapshot{}, false, nil
	}
	if err != nil {
		return Snapshot{}, false, err
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return Snapshot{}, false, fmt.Errorf("parse state snapshot: %w", err)
	}
	return s, true, nil
}

// firstPlaceholder reports the index of the first header-only block above
// genesis, and whether the chain has any. A store whose early records are
// placeholders was pruned and needs its snapshot to reopen.
func firstPlaceholder(blocks []Block) (int, bool) {
	for i := 1; i < len(blocks); i++ {
		if blocks[i].IsPlaceholder() {
			return i, true
		}
	}
	return 0, false
}
