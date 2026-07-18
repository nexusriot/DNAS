package core

import "sync"

// Checkpoints pin known-good block hashes at specific heights. They add a
// finality layer on top of most-work fork choice: a block at a checkpointed
// height must carry the pinned hash, and no reorg may discard a checkpointed
// block. Genesis is always an implicit checkpoint.
//
// On a public chain these would be shipped in a release (hashes of deeply-buried
// blocks) so a fresh or lagging node cannot be fed a bogus deep history, and an
// attacker cannot rewrite settled blocks even with more work. On this toy the map
// starts empty (an ephemeral dev chain has no stable hashes to pin); operators
// add entries with AddCheckpoint at startup, before the node begins syncing.
var (
	checkpointsMu sync.RWMutex
	checkpoints   = map[uint64]string{}
)

// AddCheckpoint pins the block at height to hash. Call it at startup, before the
// node starts syncing; it is safe for concurrent readers but is intended as
// configuration, not a runtime control.
func AddCheckpoint(height uint64, hash string) {
	checkpointsMu.Lock()
	defer checkpointsMu.Unlock()
	checkpoints[height] = hash
}

// ClearCheckpoints removes all operator-added checkpoints (genesis remains an
// implicit checkpoint). Used by tests.
func ClearCheckpoints() {
	checkpointsMu.Lock()
	defer checkpointsMu.Unlock()
	checkpoints = map[uint64]string{}
}

// checkpointAt returns the pinned hash for a height, if any. Height 0 is always
// the deterministic genesis hash.
func checkpointAt(height uint64) (string, bool) {
	if height == 0 {
		return GenesisBlock().Hash, true
	}
	checkpointsMu.RLock()
	defer checkpointsMu.RUnlock()
	h, ok := checkpoints[height]
	return h, ok
}

// highestCheckpoint returns the greatest checkpointed height (0 if only the
// implicit genesis checkpoint is set). A reorg may not fork below it.
func highestCheckpoint() uint64 {
	checkpointsMu.RLock()
	defer checkpointsMu.RUnlock()
	var max uint64
	for h := range checkpoints {
		if h > max {
			max = h
		}
	}
	return max
}
