package core

import (
	"errors"
	"fmt"
)

// Store inspection and bulk import/export, for operators rather than consensus.
//
// A chain file is otherwise opaque: the only thing that can read it is a running
// node, which either starts or does not, and says little about why. These give
// the same answers without one — what is in the file, whether it replays, and
// how to get a chain in or out of it.

// StoreInfo is a summary of a chain store as it sits on disk.
type StoreInfo struct {
	Path       string `json:"path"`
	Bytes      int64  `json:"bytes"`
	Records    int    `json:"records"`
	Height     uint64 `json:"height"`
	Tip        string `json:"tip"`
	GenesisOK  bool   `json:"genesis_ok"`
	Network    string `json:"network"`
	Truncated  bool   `json:"truncated"` // a torn trailing record was dropped on open
	TotalTxs   int    `json:"total_txs"` // non-coinbase transactions across the store
	AvgTxBlock string `json:"avg_txs"`   // human-readable average, for a quick sense of load
	Empty      bool   `json:"is_empty"`  // no records at all (a fresh file)
	Mismatch   string `json:"mismatch"`  // set when the genesis is from another network
}

// StoreStat reads a chain store and summarizes it WITHOUT validating it: no
// proof of work is checked and no state is replayed, so it answers even for a
// store a node refuses to open.
//
// It opens the file READ-ONLY (readStore, not openStore). Inspection must not
// modify what it inspects: a node taking ownership of its store repairs a torn
// trailing record, and doing that from a tool would destroy a block a running
// node had just appended. A torn record is therefore reported, not repaired.
func StoreStat(path string) (StoreInfo, error) {
	blocks, intact, total, err := readStore(path)
	if err != nil {
		return StoreInfo{}, err
	}
	info := StoreInfo{
		Path:      path,
		Bytes:     intact,
		Records:   len(blocks),
		Network:   NetworkName(),
		Truncated: intact != total,
		Empty:     len(blocks) == 0,
	}
	if len(blocks) == 0 {
		return info, nil
	}
	tip := blocks[len(blocks)-1]
	info.Height = tip.Index
	info.Tip = tip.Hash
	info.GenesisOK = blocks[0].Hash == GenesisBlock().Hash
	if !info.GenesisOK {
		info.Mismatch = fmt.Sprintf("genesis %s does not match this network's %s",
			blocks[0].Hash, GenesisBlock().Hash)
	}
	for _, b := range blocks {
		if len(b.Transactions) > 1 {
			info.TotalTxs += len(b.Transactions) - 1
		}
	}
	info.AvgTxBlock = fmt.Sprintf("%.2f", float64(info.TotalTxs)/float64(len(blocks)))
	return info, nil
}

// VerifyReport is the outcome of a full store verification.
type VerifyReport struct {
	Path    string `json:"path"`
	Blocks  int    `json:"blocks"`
	Height  uint64 `json:"height"`
	Tip     string `json:"tip"`
	OK      bool   `json:"ok"`
	BadAt   int    `json:"bad_at"` // index of the first block that failed (-1 if none)
	Problem string `json:"problem,omitempty"`
}

// VerifyStore replays a chain store through full validation — every signature,
// every state root, every retarget — and reports the first block that fails.
// This is what a node does at startup, minus the starting: it answers "would
// this chain load, and if not, where does it break?".
//
// Like StoreStat it reads the file read-only and replays into a throwaway
// in-memory chain, so verifying never touches the store it is verifying.
func VerifyStore(path string) (VerifyReport, error) {
	blocks, _, _, err := readStore(path)
	if err != nil {
		return VerifyReport{}, err
	}

	rep := VerifyReport{Path: path, Blocks: len(blocks), BadAt: -1}
	if len(blocks) == 0 {
		rep.Problem = "store is empty"
		return rep, nil
	}
	if blocks[0].Hash != GenesisBlock().Hash {
		rep.BadAt = 0
		rep.Problem = "genesis block is not this network's genesis"
		return rep, nil
	}
	bc := NewBlockchain()
	for i := 1; i < len(blocks); i++ {
		if err := bc.AddBlock(blocks[i]); err != nil {
			rep.BadAt = i
			rep.Problem = err.Error()
			rep.Height = bc.Height()
			rep.Tip = bc.Tip().Hash
			return rep, nil
		}
	}
	rep.OK = true
	rep.Height = bc.Height()
	rep.Tip = bc.Tip().Hash
	return rep, nil
}

// ExportStore writes a chain store out as the portable JSON chain file Load
// reads (the same format Save produces), so a chain can be moved between nodes
// or archived without shipping the append-only log. The store is validated on
// the way out — there is no point exporting a chain that will not import.
//
// Unlike StoreStat and VerifyStore this one OPENS the store (Open), so the node
// that owns it must be stopped first. There is no lock file to enforce that.
func ExportStore(path, out string) (int, error) {
	rep, err := VerifyStore(path)
	if err != nil {
		return 0, err
	}
	if !rep.OK {
		return 0, fmt.Errorf("refusing to export an invalid chain: block %d: %s", rep.BadAt, rep.Problem)
	}
	bc, err := Open(path)
	if err != nil {
		return 0, err
	}
	defer bc.Close()
	if err := bc.Save(out); err != nil {
		return 0, err
	}
	return bc.Len(), nil
}

// ImportStore loads a portable JSON chain file and writes it into a chain store
// at `path`, which must not already hold blocks beyond genesis — importing over
// a populated store would be a reorg with no fork choice behind it, so it is
// refused rather than guessed at. Every block is fully validated on the way in.
func ImportStore(in, path string) (int, error) {
	src, err := Load(in)
	if err != nil {
		return 0, err
	}
	dst, err := Open(path)
	if err != nil {
		return 0, err
	}
	defer dst.Close()
	if dst.Len() > 1 {
		return 0, errors.New("destination store already holds blocks (import into a fresh one)")
	}
	blocks := src.Blocks()
	for i := 1; i < len(blocks); i++ {
		if err := dst.AddBlock(blocks[i]); err != nil {
			return i - 1, fmt.Errorf("import block %d: %w", i, err)
		}
	}
	return len(blocks) - 1, nil
}
