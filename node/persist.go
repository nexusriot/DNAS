package node

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"

	"github.com/nexusriot/DNAS/core"
)

// Files holding a node's soft state alongside its block store. Unlike the chain
// (which is authoritative and re-syncable from peers), these are conveniences
// that let a node keep its known peers, ban scores, and pending transactions
// across a restart instead of starting cold each time.
const (
	peersFile   = "peers.json"
	bansFile    = "bans.json"
	mempoolFile = "mempool.json"
)

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
		}
	}
	var bans map[string]int
	if readJSONFile(n.statePath(bansFile), &bans) == nil && len(bans) > 0 {
		n.bans.restore(bans)
		log.Printf("restored %d ban score(s)", len(bans))
	}
	var txs []core.Transaction
	if readJSONFile(n.statePath(mempoolFile), &txs) == nil {
		restored := 0
		for _, tx := range txs {
			if added, err := n.mempool.Add(tx); err == nil && added {
				restored++
			}
		}
		if restored > 0 {
			log.Printf("restored %d mempool transaction(s)", restored)
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
	writeJSONFile(n.statePath(bansFile), n.bans.snapshot())
	writeJSONFile(n.statePath(mempoolFile), n.mempool.All())
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
