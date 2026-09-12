package node

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"

	"github.com/nexusriot/DNAS/core"
	"time"
)

// Files holding a node's soft state alongside its block store. Unlike the chain
// (which is authoritative and re-syncable from peers), these are conveniences
// that let a node keep its known peers, ban scores, and pending transactions
// across a restart instead of starting cold each time.
const (
	peersFile   = "peers.json"
	addrsFile   = "addrs.json"
	bansFile    = "bans.json"
	mempoolFile = "mempool.json"
	// The three in-memory rings that used to die with the process. A restart is
	// exactly when their contents matter most: the reorg log is the record of the
	// incident you are restarting because of, and the share ledger and PPLNS
	// window are a pool's accounting — losing them means miners are not paid for
	// work they actually did.
	sharesFile = "shares.json"
	poolFile   = "pool.json"
	reorgsFile = "reorgs.json"
)

// poolState is the pool's accounting as it is written to disk.
type poolState struct {
	Submitted uint64        `json:"submitted"`
	Accepted  uint64        `json:"accepted"`
	Stale     uint64        `json:"stale"`
	Blocks    uint64        `json:"blocks"`
	Miners    []MinerShares `json:"miners"`
	Window    []PoolShare   `json:"window"`
}

func (n *Node) statePath(name string) string { return filepath.Join(n.cfg.StateDir, name) }

// loadState restores persisted peer/ban/mempool state from the state directory
// if one is configured. Missing files are normal (a first run) and ignored.
func (n *Node) loadState() {
	if n.cfg.StateDir == "" {
		return
	}
	var peers []string
	if readJSONFile(n.statePath(peersFile), &peers) == nil {
		for _, a := range peers {
			n.book.note(a)
			n.addrs.Add(a, "peers.json")
		}
	}
	// The address manager's own table, which peers.json cannot express: it says
	// which addresses actually completed a handshake. That `tried` set is this
	// node's hard-won evidence about who is real, and starting cold without it
	// means trusting gossip again on every restart. peers.json is still read
	// above so a store written by an older build still loads.
	var addrs []addrEntry
	if readJSONFile(n.statePath(addrsFile), &addrs) == nil && len(addrs) > 0 {
		n.addrs.Restore(addrs)
		newCount, triedCount := n.addrs.Size()
		Infof("restored known addresses", "new", newCount, "tried", triedCount)
	}
	var bans map[string]int
	if readJSONFile(n.statePath(bansFile), &bans) == nil && len(bans) > 0 {
		n.bans.restore(bans)
		Infof("restored ban scores", "count", len(bans))
	}
	var pool poolState
	if readJSONFile(n.statePath(sharesFile), &pool) == nil {
		n.shares.restore(pool)
		if len(pool.Miners) > 0 || pool.Accepted > 0 {
			Infof("restored share ledger", "miners", len(pool.Miners), "accepted", pool.Accepted)
		}
	}
	var window []PoolShare
	if readJSONFile(n.statePath(poolFile), &window) == nil && len(window) > 0 {
		n.window.restore(window)
		Infof("restored pool payout window", "shares", len(window))
	}
	var reorgs []Reorg
	if readJSONFile(n.statePath(reorgsFile), &reorgs) == nil && len(reorgs) > 0 {
		n.reorgs.restore(reorgs)
		Infof("restored reorg history", "count", len(reorgs))
	}
	var txs []core.Transaction
	if readJSONFile(n.statePath(mempoolFile), &txs) == nil {
		// Restore each sender's transactions in nonce order: the mempool refuses a
		// nonce that would leave a gap, and the saved file is in map-iteration order,
		// so an unsorted replay would drop most of a sender's queue.
		sort.Slice(txs, func(i, j int) bool {
			if txs[i].From != txs[j].From {
				return txs[i].From < txs[j].From
			}
			return txs[i].Nonce < txs[j].Nonce
		})
		restored := 0
		for _, tx := range txs {
			if added, err := n.mempool.Add(tx); err == nil && added {
				restored++
			}
		}
		if restored > 0 {
			Infof("restored mempool transactions", "count", restored)
		}
	}
}

// saveState persists peer/ban/mempool state to the state directory if configured.
// It is called on graceful shutdown so the next start resumes from where it left
// off (a hard kill may lose the latest state, which re-syncs from peers anyway).
func (n *Node) saveState() {
	if n.cfg.StateDir == "" {
		return
	}
	writeJSONFile(n.statePath(peersFile), n.book.all())
	writeJSONFile(n.statePath(addrsFile), n.addrs.Snapshot())
	writeJSONFile(n.statePath(bansFile), n.bans.snapshot())
	writeJSONFile(n.statePath(mempoolFile), n.mempool.All())
	writeJSONFile(n.statePath(sharesFile), n.shares.snapshot())
	writeJSONFile(n.statePath(poolFile), n.window.snapshot())
	writeJSONFile(n.statePath(reorgsFile), n.reorgs.snapshot())
}

func readJSONFile(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// writeJSONFile writes v as pretty JSON via a temp file + rename, so a crash
// mid-write can't leave a truncated file.
func writeJSONFile(path string, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("persist %s: %v", filepath.Base(path), err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("persist %s: %v", filepath.Base(path), err)
	}
}

// compactInterval is how often the node checks whether the block store wants
// rewriting. Slow, because the answer is almost always no and the work is
// I/O-heavy when it is yes.
const compactInterval = 30 * time.Second

// compactLoop rewrites the block store off the block-application path.
//
// Pruning marks the store as due (core.Blockchain.CompactionDue); the rewrite
// itself is O(whole store), so doing it inline froze the node — the miner, every
// API read and every peer handler — for its duration. Here it runs on its own
// schedule, and core does the expensive phase with no chain lock held.
func (n *Node) compactLoop() {
	t := time.NewTicker(compactInterval)
	defer t.Stop()
	for {
		select {
		case <-n.quit:
			return
		case <-t.C:
			if !n.chain.CompactionDue() {
				continue
			}
			before := n.chain.StoreStats().Bytes
			if err := n.chain.CompactStore(); err != nil {
				// Not fatal: memory is authoritative and an oversized file is a
				// disk problem. A reorg mid-rewrite lands here too, and simply
				// leaves the request pending for the next tick.
				Debugf("store compaction deferred", "err", err)
				continue
			}
			st := n.chain.StoreStats()
			Infof("block store compacted",
				"bytes", st.Bytes, "reclaimed", before-st.Bytes, "bodies_from", n.chain.BodyHeight())
		}
	}
}
