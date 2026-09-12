package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sort"
	"sync"
)

// Account is the state tracked per address: a spendable coin balance, a nonce
// (the next expected transaction sequence number, which stops replay), and any
// native asset balances the address holds (asset id -> amount). Assets is
// omitempty, so a coin-only account serializes and hashes exactly as before.
type Account struct {
	Balance uint64            `json:"balance"`
	Nonce   uint64            `json:"nonce"`
	Assets  map[string]uint64 `json:"assets,omitempty"`
}

// Blockchain is a thread-safe chain of blocks plus the account state derived by
// replaying every transaction. All exported methods take the lock, so it is
// safe to share one *Blockchain across the miner, P2P handlers and API.
type Blockchain struct {
	mu        sync.RWMutex
	blocks    []Block
	state     map[string]Account
	work      *big.Int      // cumulative proof-of-work of blocks
	undos     [][]undoEntry // undos[i] reverts blocks[i]'s state changes (undos[0] is nil)
	store     *blockStore   // append-only persistence (nil = in-memory only)
	storePath string        // where that store lives, so it can be compacted in place
	// storeCompactedTo is the highest height whose stored record is already
	// header-only. Compaction rewrites the whole file, so it is amortized rather
	// than run on every block (see maybeCompactStoreLocked).
	storeCompactedTo uint64
	// compactionDue is set when pruning has advanced far enough that the log is
	// worth rewriting. It is a FLAG rather than the work itself: the rewrite is
	// O(whole store) and must not run on the block-application path.
	compactionDue   bool
	storeCompactErr error            // last compaction failure, reported via PruneInfo
	storeBytesSaved int64            // bytes reclaimed by compaction so far
	txIndex         map[string]TxLoc // txid -> where it is confirmed (see txindex.go)
	// addrIndex maps an address to every transaction that touched it. Optional
	// (nil = disabled, the default) because its size is unbounded by the chain —
	// see addrindex.go.
	addrIndex map[string][]TxLoc
	// assets describes every asset the chain has issued (id -> ticker, issuer,
	// supply). Always on: it is bounded by the number of issuances, and without it
	// an asset balance is an opaque id (see assetindex.go).
	assets map[string]AssetInfo
	burned uint64 // cumulative base fee burned by connected blocks (see supply.go)
	// pruneKeep is how many recent block bodies to keep in memory; 0 keeps all of
	// them (the default). pruned counts the bodies discarded so far. See prune.go.
	pruneKeep uint64
	pruned    uint64
	// filterHeaders is the BIP157-style filter-header chain, cached as blocks
	// connect and covering heights filterBase..tip. It is cached rather than
	// recomputed because it is a running hash over block BODIES: once a body is
	// pruned its filter cannot be rebuilt, and folding over what remains would
	// produce a chain that disagrees with every other node's (see cfilter.go).
	filterHeaders []string
	filterBase    uint64
	// sigCache remembers which transactions have already had their authorization
	// verified, so a payment's signature is not re-checked when the block carrying
	// it arrives (see valcache.go). Shared with the mempool by the node.
	sigCache *ValidationCache
}

// ValidationCache returns the chain's signature-verification cache, so the
// mempool can share it: a transaction verified on admission is then free to
// apply when its block arrives.
func (bc *Blockchain) ValidationCache() *ValidationCache { return bc.sigCache }

// undoEntry records an account's prior value so a block's effect can be
// reversed during a reorg without replaying the chain from genesis.
type undoEntry struct {
	addr    string
	prev    Account
	existed bool
}

// GenesisBlock is fixed and identical on every node of a network; without this,
// two fresh nodes would compute different genesis hashes and never agree on a
// chain. It differs BETWEEN networks (the network id is bound into PrevHash, see
// network.go), so a testnet or regtest chain can never be mistaken for the real
// one — on mainnet the id is empty and the genesis hash is unchanged.
func GenesisBlock() Block {
	b := Block{
		Index:     0,
		Timestamp: GenesisTimestamp,
		PrevHash:  genesisPrevHash(),
		Bits:      GenesisBits,
	}
	b.MerkleRoot = MerkleRoot(b.Transactions)
	b.StateRoot = stateRoot(map[string]Account{}) // empty state at genesis
	b.BaseFee = InitialBaseFee
	b.Hash = b.ComputeHash()
	return b
}

// NewBlockchain returns a chain containing only the genesis block.
func NewBlockchain() *Blockchain {
	genesis := GenesisBlock()
	bc := &Blockchain{
		filterHeaders: FilterHeaderChain([]BlockFilter{BuildBlockFilter(genesis)}),
		blocks:        []Block{genesis},
		state:         map[string]Account{},
		work:          BlockWork(genesis.Bits),
		undos:         [][]undoEntry{nil}, // genesis has no undo (it is never rolled back)
		txIndex:       map[string]TxLoc{}, // genesis carries no transactions
		assets:        map[string]AssetInfo{},
		sigCache:      NewValidationCache(DefaultValidationCacheSize),
	}
	bc.refreshDeploymentsLocked()
	return bc
}

func (bc *Blockchain) Tip() Block {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.blocks[len(bc.blocks)-1]
}

func (bc *Blockchain) Height() uint64 {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.blocks[len(bc.blocks)-1].Index
}

func (bc *Blockchain) Len() int {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return len(bc.blocks)
}

// Blocks returns a copy of the full chain, safe to hand to callers.
func (bc *Blockchain) Blocks() []Block {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	out := make([]Block, len(bc.blocks))
	copy(out, bc.blocks)
	return out
}

func (bc *Blockchain) Account(addr string) Account {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.state[addr]
}

func (bc *Blockchain) Balance(addr string) uint64 { return bc.Account(addr).Balance }

// SpendableBalance is the balance minus any immature coinbase that cannot yet be
// spent in the next block.
func (bc *Blockchain) SpendableBalance(addr string) uint64 {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	bal := bc.state[addr].Balance
	imm := immatureCoinbase(bc.blocks, uint64(len(bc.blocks)), addr)
	if imm >= bal {
		return 0
	}
	return bal - imm
}

// Work returns the chain's cumulative proof-of-work.
func (bc *Blockchain) Work() *big.Int {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return new(big.Int).Set(bc.work)
}

// NextBits is the compact proof-of-work target the next mined block must satisfy.
func (bc *Blockchain) NextBits() uint32 {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return expectedBits(bc.blocks, uint64(len(bc.blocks)))
}

// Headers returns the header of every block, in order — enough for a light
// (SPV) client to verify proof-of-work and the hash chain.
func (bc *Blockchain) Headers() []Header {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	out := make([]Header, len(bc.blocks))
	for i, b := range bc.blocks {
		out[i] = b.Header()
	}
	return out
}

// HeaderAt returns the header at the given height.
func (bc *Blockchain) HeaderAt(height uint64) (Header, bool) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if height >= uint64(len(bc.blocks)) {
		return Header{}, false
	}
	return bc.blocks[height].Header(), true
}

// HeadersFrom returns up to max headers starting at the given height, for
// headers-first sync (a peer catches up by fetching headers in batches before
// downloading bodies).
func (bc *Blockchain) HeadersFrom(from uint64, max int) []Header {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if from >= uint64(len(bc.blocks)) || max <= 0 {
		return nil
	}
	end := from + uint64(max)
	if end > uint64(len(bc.blocks)) {
		end = uint64(len(bc.blocks))
	}
	out := make([]Header, 0, end-from)
	for i := from; i < end; i++ {
		out = append(out, bc.blocks[i].Header())
	}
	return out
}

// BlocksRange returns the block bodies in [from, to] (inclusive), capped at max,
// for ranged block download during sync.
func (bc *Blockchain) BlocksRange(from, to uint64, max int) []Block {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if from >= uint64(len(bc.blocks)) || to < from || max <= 0 {
		return nil
	}
	if to >= uint64(len(bc.blocks)) {
		to = uint64(len(bc.blocks)) - 1
	}
	if to-from+1 > uint64(max) {
		to = from + uint64(max) - 1
	}
	out := make([]Block, 0, to-from+1)
	for i := from; i <= to; i++ {
		out = append(out, bc.blocks[i])
	}
	return out
}

// BlockAt returns the block body at the given height.
func (bc *Blockchain) BlockAt(height uint64) (Block, bool) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if height >= uint64(len(bc.blocks)) {
		return Block{}, false
	}
	return bc.blocks[height], true
}

// TxProof is a transaction-inclusion proof for a light client: the block it was
// mined into, that block's merkle root, and the merkle path from the
// transaction up to the root. A client verifies it with VerifyMerkleProof
// against the merkle root in the (already PoW-verified) block header.
type TxProof struct {
	Found         bool              `json:"found"`
	BlockIndex    uint64            `json:"block_index"`
	BlockHash     string            `json:"block_hash"`
	MerkleRoot    string            `json:"merkle_root"`
	Confirmations uint64            `json:"confirmations"`
	Tx            Transaction       `json:"tx"`
	Proof         []MerkleProofStep `json:"proof"`
}

// FindTxProof locates a confirmed transaction by hash and builds its inclusion
// proof. The bool is false if the transaction is not in the chain. The lookup is
// a single index hit (see txindex.go), not a scan of every block body.
func (bc *Blockchain) FindTxProof(txHash string) (TxProof, bool) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	tx, loc, ok := bc.findTxLocked(txHash)
	if !ok {
		return TxProof{Found: false}, false
	}
	b := bc.blocks[loc.Height]
	proof, _ := MerkleProof(b.Transactions, loc.Index)
	return TxProof{
		Found:         true,
		BlockIndex:    b.Index,
		BlockHash:     b.Hash,
		MerkleRoot:    b.MerkleRoot,
		Confirmations: bc.blocks[len(bc.blocks)-1].Index - b.Index + 1,
		Tx:            tx,
		Proof:         proof,
	}, true
}

// AddBlock validates a block against the current tip and, if valid, appends it
// and updates account state in place. Application is incremental (no full-state
// clone): the block's changes are applied directly and an undo log is recorded
// so a later reorg can reverse them. On any validation error the state is left
// unchanged.
func (bc *Blockchain) AddBlock(block Block) error {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	undo, err := applyBlock(bc.state, bc.blocks, block, bc.sigCache)
	if err != nil {
		return err
	}
	if bc.store != nil {
		if err := bc.store.append(block); err != nil {
			applyUndo(bc.state, undo) // keep memory consistent with disk
			return fmt.Errorf("persist block: %w", err)
		}
	}
	bc.blocks = append(bc.blocks, block)
	bc.undos = append(bc.undos, undo)
	// A miner vote may have closed a window with this block, which fixes (or
	// withdraws) an activation height the NEXT block is validated against.
	bc.refreshDeploymentsLocked()
	bc.work.Add(bc.work, BlockWork(block.Bits))
	bc.indexBlock(block)
	bc.burned += blockBurned(block)
	bc.extendFilterHeadersLocked(block)
	bc.pruneLocked() // a no-op unless this node prunes (see prune.go)
	return nil
}

// ReplaceChain adopts an incoming chain when it wins the fork-choice rule and is
// fully valid from genesis. It returns whether it was adopted and, if so, the
// blocks the switch discarded (see ReorgFrom). The rule is:
//
//  1. more cumulative work wins;
//  2. on equal work, the chain with the lexicographically smaller tip hash wins.
//
// Rule 2 is a deterministic tie-break: because block hashes are effectively
// random, every node independently agrees on the same canonical chain, so
// equal-work forks (common here since difficulty is usually constant) converge
// immediately instead of lingering until one side happens to extend. It is also
// monotonic — a node only ever switches toward a smaller tip hash — so ties
// cannot cause it to flap back and forth.
func (bc *Blockchain) ReplaceChain(incoming []Block) (bool, []Block, error) {
	if len(incoming) == 0 {
		return false, nil, errors.New("empty chain")
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()
	if incoming[0].Hash != GenesisBlock().Hash {
		return false, nil, errors.New("genesis mismatch")
	}
	fork := commonPrefix(bc.blocks, incoming) - 1 // last shared block (>= 0)
	return bc.reorgLocked(fork, incoming[fork+1:])
}

// ReorgFrom replaces the blocks above forkHeight with the given suffix (which
// must build on our block at forkHeight), adopting it if it wins the fork-choice
// rule. Block-locator sync uses this to transfer only the divergent suffix
// rather than a whole competing chain. Callers must not hold bc.mu.
//
// When a reorg is adopted it also returns the blocks it discarded, oldest first.
// Their transactions are not invalid — they simply lost the race — so the caller
// (the node) returns the still-spendable ones to the mempool rather than letting
// confirmed payments vanish with the losing branch.
func (bc *Blockchain) ReorgFrom(forkHeight uint64, suffix []Block) (bool, []Block, error) {
	if len(suffix) == 0 {
		return false, nil, nil
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()
	if forkHeight >= uint64(len(bc.blocks)) {
		return false, nil, errors.New("fork height beyond tip")
	}
	return bc.reorgLocked(int(forkHeight), suffix)
}

// ReorgRefusedError is returned when the finality guards refuse a reorg that
// fork choice would otherwise have adopted: it reaches below a checkpoint, or it
// would discard more than MaxReorgDepth committed blocks.
//
// It is a distinct type rather than a string because the two failures mean
// opposite things to an operator. An *invalid* chain means a peer sent garbage,
// which is that peer's problem. A *refused* reorg means this node has just
// declined to follow what may well be the network's real chain — the guard did
// its job, and the node may now be permanently diverged, because the same guard
// will refuse the same switch forever. That is the single most consequential
// thing a node can do quietly, and until this type existed it did exactly that:
// the error was discarded at the call site and counted nowhere.
type ReorgRefusedError struct {
	Depth      int    // blocks the reorg wanted to discard
	ForkHeight uint64 // last block the two chains shared
	Limit      int    // MaxReorgDepth, when the depth guard refused it
	Checkpoint uint64 // checkpoint height, when the checkpoint guard refused it
}

func (e *ReorgRefusedError) Error() string {
	if e.Limit > 0 {
		return fmt.Sprintf("reorg too deep: would discard %d blocks (max %d)", e.Depth, e.Limit)
	}
	return fmt.Sprintf("reorg would discard the checkpointed block at height %d", e.Checkpoint)
}

// Reason is a short machine-ish label for reporting.
func (e *ReorgRefusedError) Reason() string {
	if e.Limit > 0 {
		return "too_deep"
	}
	return "below_checkpoint"
}

// AsReorgRefused reports whether err is a finality refusal, and which one.
func AsReorgRefused(err error) (*ReorgRefusedError, bool) {
	var e *ReorgRefusedError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// reorgLocked replaces the blocks above `fork` with `suffix`, applying the
// fork-choice rule (most work; ties broken by the smaller tip hash). It
// validates the suffix on a rolled-back copy of state — so a bad suffix cannot
// corrupt the live chain — then persists and commits atomically. On success it
// returns the discarded blocks, oldest first. bc.mu held.
func (bc *Blockchain) reorgLocked(fork int, suffix []Block) (bool, []Block, error) {
	// Finality guards, checked before fork choice so a deep or checkpoint-violating
	// reorg is refused regardless of how much work it claims:
	//   - never roll back a block at or below the highest checkpoint;
	//   - never discard more than MaxReorgDepth already-committed blocks.
	// Neither affects initial sync or forward extension (fork == our tip, so
	// nothing is discarded).
	if hc := highestCheckpoint(); uint64(fork) < hc {
		return false, nil, &ReorgRefusedError{
			Depth: len(bc.blocks) - 1 - fork, ForkHeight: uint64(fork), Checkpoint: hc,
		}
	}
	if removed := len(bc.blocks) - 1 - fork; removed > MaxReorgDepth {
		return false, nil, &ReorgRefusedError{
			Depth: removed, ForkHeight: uint64(fork), Limit: MaxReorgDepth,
		}
	}

	// Candidate cumulative work = shared prefix + suffix.
	candWork := ChainWork(bc.blocks[:fork+1])
	for _, b := range suffix {
		candWork.Add(candWork, BlockWork(b.Bits))
	}
	switch candWork.Cmp(bc.work) {
	case -1: // less work
		return false, nil, nil
	case 0: // equal work: adopt only if the candidate tip hash is smaller
		candTip := bc.blocks[fork].Hash
		if len(suffix) > 0 {
			candTip = suffix[len(suffix)-1].Hash
		}
		if candTip >= bc.blocks[len(bc.blocks)-1].Hash {
			return false, nil, nil
		}
	}

	state := cloneState(bc.state)
	for i := len(bc.blocks) - 1; i > fork; i-- {
		applyUndo(state, bc.undos[i])
	}
	blocks := append([]Block(nil), bc.blocks[:fork+1]...)
	undos := append([][]undoEntry(nil), bc.undos[:fork+1]...)
	for i, b := range suffix {
		undo, err := applyBlock(state, blocks, b, bc.sigCache)
		if err != nil {
			return false, nil, fmt.Errorf("block %d: %w", fork+1+i, err)
		}
		blocks = append(blocks, b)
		undos = append(undos, undo)
	}

	// Persist (truncate to the fork, append the new suffix) before committing in
	// memory, so disk and memory stay consistent.
	//
	// This sequence is the one place a write cannot be rolled back: once the
	// truncate lands, the log no longer holds the losing suffix, so a failure
	// partway through leaves disk with a prefix of a chain this node is not yet
	// running. There is nothing to undo to — so instead of returning an error and
	// carrying on (which would append the old chain's continuation on top of the
	// new suffix and leave a log that will not replay), the store is poisoned and
	// refuses every later write. The node stays up and readable; it just cannot
	// pretend its disk is still authoritative.
	if bc.store != nil {
		if err := bc.store.truncateAfter(uint64(fork)); err != nil {
			bc.store.poison(err)
			return false, nil, fmt.Errorf("persist reorg: %w", err)
		}
		for i := fork + 1; i < len(blocks); i++ {
			if err := bc.store.append(blocks[i]); err != nil {
				bc.store.poison(err)
				return false, nil, fmt.Errorf("persist reorg: %w", err)
			}
		}
	}

	// Committed: swap the chain in and bring the derived indexes with it. Blocks
	// are unindexed from the tip down and the new suffix indexed from the fork up,
	// which keeps the "first occurrence wins" rule of the transaction index intact.
	disconnected := append([]Block(nil), bc.blocks[fork+1:]...)
	for i := len(bc.blocks) - 1; i > fork; i-- {
		bc.unindexBlock(bc.blocks[i])
		bc.burned -= blockBurned(bc.blocks[i])
	}
	bc.blocks = blocks
	bc.refreshDeploymentsLocked() // a reorg can undo a lock-in (see versionbits.go)
	bc.state = state
	bc.undos = undos
	bc.work = candWork
	bc.truncateFilterHeadersLocked(uint64(fork))
	for i := fork + 1; i < len(blocks); i++ {
		bc.indexBlock(blocks[i])
		bc.burned += blockBurned(blocks[i])
		bc.extendFilterHeadersLocked(blocks[i])
	}
	// A reorg can advance the tip by many blocks at once, which moves the prune
	// cutoff with it.
	bc.pruneLocked()
	return true, disconnected, nil
}

// Locator returns block hashes from the tip backwards at exponentially growing
// steps (dense near the tip, sparse toward genesis, always including it). A peer
// finds the most recent hash it also has to identify the fork point cheaply.
func (bc *Blockchain) Locator() []string {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	var loc []string
	step := 1
	for i := len(bc.blocks) - 1; i > 0; i -= step {
		loc = append(loc, bc.blocks[i].Hash)
		if len(loc) > 10 {
			step *= 2
		}
	}
	loc = append(loc, bc.blocks[0].Hash) // genesis is always common
	return loc
}

// LocatorFork returns the height of the most recent block whose hash appears in
// the locator (the fork point). Genesis is always shared, so this is well-defined.
func (bc *Blockchain) LocatorFork(locator []string) uint64 {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	have := make(map[string]uint64, len(bc.blocks))
	for i, b := range bc.blocks {
		have[b.Hash] = uint64(i)
	}
	for _, h := range locator { // ordered tip -> genesis: first match is highest
		if height, ok := have[h]; ok {
			return height
		}
	}
	return 0
}

// commonPrefix returns the number of leading blocks a and b share (by hash).
// Both chains start at the same genesis, so this is always >= 1.
func commonPrefix(a, b []Block) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i].Hash == b[i].Hash {
		i++
	}
	return i
}

// ValidateHeaderChain checks that headers form a contiguous, proof-of-work-valid
// sequence whose first header builds on (prevHash, prevIndex). It is a cheap
// pre-filter for headers-first sync — run before downloading bodies — and does
// not verify the difficulty retarget (bodies are fully validated by AddBlock).
func ValidateHeaderChain(headers []Header, prevHash string, prevIndex uint64) error {
	ph, pi := prevHash, prevIndex
	for i, h := range headers {
		if h.Index != pi+1 {
			return fmt.Errorf("header %d: bad index %d after %d", i, h.Index, pi)
		}
		if h.PrevHash != ph {
			return fmt.Errorf("header %d: prev hash mismatch", i)
		}
		if !h.HasValidPoW() {
			return fmt.Errorf("header %d: invalid proof of work", i)
		}
		ph, pi = h.Hash, h.Index
	}
	return nil
}

// Save writes the chain to a JSON file.
func (bc *Blockchain) Save(path string) error {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	data, err := json.MarshalIndent(bc.blocks, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Open returns a blockchain backed by an append-only block log at path,
// replaying and validating whatever is stored. Once open, AddBlock appends
// incrementally (O(1) on disk) and reorgs truncate+append — no whole-file
// rewrite. A fresh file is initialized with the genesis block.
func Open(path string) (*Blockchain, error) {
	store, blocks, err := openStore(path)
	if err != nil {
		return nil, err
	}
	bc := NewBlockchain() // in-memory genesis; store stays nil during replay
	if len(blocks) == 0 {
		if err := store.append(GenesisBlock()); err != nil {
			store.close()
			return nil, err
		}
	} else {
		if blocks[0].Hash != GenesisBlock().Hash {
			store.close()
			return nil, errors.New("genesis mismatch (incompatible store)")
		}
		// A pruned store holds header-only records for the heights whose bodies
		// were dropped. Those cannot be replayed — the transactions that produced
		// the balances are gone — so state comes from the snapshot written beside
		// the store, verified against the state root in a header the chain's own
		// proof of work covers. This is the same bootstrap a fast-synced node
		// does, from a local file rather than a peer.
		if from, pruned := firstPlaceholder(blocks); pruned {
			seeded, err := openPrunedStore(path, blocks, from)
			if err != nil {
				store.close()
				return nil, err
			}
			bc = seeded
		} else {
			for i := 1; i < len(blocks); i++ {
				if err := bc.AddBlock(blocks[i]); err != nil {
					store.close()
					return nil, fmt.Errorf("replay block %d: %w", i, err)
				}
			}
		}
	}
	bc.store = store // future writes now persist
	bc.storePath = path
	return bc, nil
}

// openPrunedStore rebuilds a chain whose early bodies were pruned from disk,
// using the state snapshot written alongside it.
func openPrunedStore(path string, blocks []Block, firstPruned int) (*Blockchain, error) {
	snap, ok, err := readStateSnapshot(path)
	if err != nil {
		return nil, fmt.Errorf("pruned store at %s: %w", path, err)
	}
	if !ok {
		return nil, fmt.Errorf(
			"store %s is pruned (block %d has no body) but its state snapshot %s is missing; "+
				"the balances cannot be rebuilt from headers alone — restore the snapshot, "+
				"or re-sync from a peer",
			path, firstPruned, statePath(path))
	}
	if snap.Height >= uint64(len(blocks)) {
		return nil, fmt.Errorf("state snapshot claims height %d but the store holds %d blocks",
			snap.Height, len(blocks))
	}
	// Every height at or below the snapshot must be covered by it: a body-less
	// block above the snapshot could never be replayed.
	for i := int(snap.Height) + 1; i < len(blocks); i++ {
		if blocks[i].IsPlaceholder() {
			return nil, fmt.Errorf(
				"block %d has no body but sits above the state snapshot at height %d; "+
					"this store cannot be replayed", i, snap.Height)
		}
	}
	headers := make([]Header, snap.Height+1)
	for i := range headers {
		headers[i] = blocks[i].Header()
	}
	// NewFromSnapshot re-derives the state root from the accounts and checks it
	// against the header, so a tampered or stale snapshot is caught here rather
	// than becoming this node's idea of everyone's balances.
	bc, err := NewFromSnapshot(snap, headers)
	if err != nil {
		return nil, fmt.Errorf("state snapshot for %s: %w", path, err)
	}
	for i := int(snap.Height) + 1; i < len(blocks); i++ {
		if err := bc.AddBlock(blocks[i]); err != nil {
			return nil, fmt.Errorf("replay block %d above the snapshot: %w", i, err)
		}
	}
	// The reopened chain is pruned by construction; remember how far, so the next
	// compaction does not redo work it has already done.
	bc.storeCompactedTo = snap.Height + 1
	return bc, nil
}

// Close releases the backing store, if any.
func (bc *Blockchain) Close() error {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	if bc.store != nil {
		err := bc.store.close()
		bc.store = nil
		return err
	}
	return nil
}

// Load reads a chain from a JSON snapshot (as written by Save) and revalidates
// it end to end. This is the import/export format; runtime persistence uses Open.
func Load(path string) (*Blockchain, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var blocks []Block
	if err := json.Unmarshal(data, &blocks); err != nil {
		return nil, err
	}
	if len(blocks) == 0 {
		return nil, errors.New("empty chain file")
	}
	if blocks[0].Hash != GenesisBlock().Hash {
		return nil, errors.New("genesis mismatch (incompatible chain file)")
	}
	bc := NewBlockchain()
	if len(blocks) == 1 {
		return bc, nil // just genesis
	}
	ok, _, err := bc.ReplaceChain(blocks)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("failed to load chain")
	}
	return bc, nil
}

// expectedBits deterministically derives the required proof-of-work target for
// the block at `height` using an LWMA-1 (linearly-weighted moving average)
// retarget over the last `lwmaWindow` blocks. Unlike a step retarget it adjusts
// every block and weights recent blocks more, so it tracks the target block time
// smoothly. Per-block solve times are clamped to [1, 6·TargetBlockTime], which
// also neutralises the huge first gap from the far-past genesis timestamp (it no
// longer collapses the target on the first window).
//
// The target is clamped to PowLimit on the EASY side only — there is no hard
// ceiling on difficulty, so it rises without bound to match whatever hashpower
// shows up (this is what gives proof of work its economic security: rewriting
// history must redo that work). When NoRetarget is set (regtest and the test
// suite, like Bitcoin's fPowNoRetargeting) it instead holds the genesis target so
// blocks stay instant; a real network leaves it off.
func expectedBits(blocks []Block, height uint64) uint32 {
	if NoRetarget || height <= lwmaWindow {
		return GenesisBits
	}
	N := lwmaWindow
	maxSolve := 6 * TargetBlockTime
	var weightedSolve int64
	sumTarget := new(big.Int)
	for i := 1; i <= N; i++ {
		cur := height - uint64(N) - 1 + uint64(i) // window blocks: height-N .. height-1
		st := blocks[cur].Timestamp - blocks[cur-1].Timestamp
		if st < 1 {
			st = 1
		} else if st > maxSolve {
			st = maxSolve
		}
		weightedSolve += int64(i) * st
		sumTarget.Add(sumTarget, CompactToBig(blocks[cur].Bits))
	}
	// nextTarget = avgTarget · weightedSolve / (T · N(N+1)/2). If blocks arrive at
	// exactly TargetBlockTime the two factors cancel and the target is unchanged;
	// faster blocks shrink it (harder, without bound), slower blocks grow it (easier).
	avgTarget := new(big.Int).Div(sumTarget, big.NewInt(int64(N)))
	next := new(big.Int).Mul(avgTarget, big.NewInt(weightedSolve))
	denom := big.NewInt(TargetBlockTime * int64(N) * int64(N+1) / 2)
	next.Div(next, denom)

	if next.Sign() <= 0 {
		next = big.NewInt(1) // never zero/negative (would be an unsatisfiable target)
	}
	if next.Cmp(PowLimit) > 0 {
		next = new(big.Int).Set(PowLimit) // easiest allowed; no hard cap on the difficulty side
	}
	return BigToCompact(next)
}

// expectedBaseFee deterministically derives the base fee for the block at
// `height` from its parent's fullness (EIP-1559 style): if the parent held more
// than BaseFeeTargetTxs transactions the fee rises, if fewer it falls, each by at
// most 1/BaseFeeMaxChangeDenominator. It is clamped to MinBaseFee so it can never
// reach zero and can always recover. Every node derives the same value.
func expectedBaseFee(blocks []Block, height uint64) uint64 {
	if height == 0 {
		return InitialBaseFee
	}
	parent := blocks[height-1]
	base := parent.BaseFee
	if base < MinBaseFee {
		base = MinBaseFee
	}
	parentTxs := 0
	if len(parent.Transactions) > 0 {
		parentTxs = len(parent.Transactions) - 1 // exclude the coinbase
	}
	target := BaseFeeTargetTxs
	switch {
	case parentTxs == target:
		return base
	case parentTxs > target:
		delta := base * uint64(parentTxs-target) / uint64(target) / BaseFeeMaxChangeDenominator
		if delta == 0 {
			delta = 1 // always move at least one unit when off-target
		}
		return base + delta
	default: // parentTxs < target
		delta := base * uint64(target-parentTxs) / uint64(target) / BaseFeeMaxChangeDenominator
		if delta >= base || base-delta < MinBaseFee {
			return MinBaseFee
		}
		return base - delta
	}
}

// NextBaseFee is the base fee the next mined block must commit to.
func (bc *Blockchain) NextBaseFee() uint64 {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return expectedBaseFee(bc.blocks, uint64(len(bc.blocks)))
}

// immatureCoinbase sums the coinbase amounts paid to addr in blocks that have
// not yet matured for a spend at the given height: a coinbase mined at height C
// is spendable only once height >= C + CoinbaseMaturity. Computed from block
// history, so it needs no extra state and reverses naturally on reorg.
func immatureCoinbase(blocks []Block, height uint64, addr string) uint64 {
	var start uint64 = 1
	if height > CoinbaseMaturity {
		start = height - CoinbaseMaturity + 1
	}
	var sum uint64
	for i := start; i < height && i < uint64(len(blocks)); i++ {
		if len(blocks[i].Transactions) == 0 {
			continue // pruned/placeholder block (below a snapshot): already final, so mature
		}
		cb := blocks[i].Transactions[0]
		if cb.IsCoinbase() && cb.To == addr {
			sum += cb.Amount
		}
	}
	return sum
}

// blockWeight is the total serialized size of a block's non-coinbase
// transactions — the metered quantity bounded by MaxBlockBytes and priced by the
// per-byte base fee.
func blockWeight(b Block) int {
	w := 0
	for i := 1; i < len(b.Transactions); i++ {
		w += b.Transactions[i].Size()
	}
	return w
}

// medianTimePast returns the median timestamp of the last up-to-11 blocks. A new
// block must be strictly greater than this (not merely greater than its parent),
// which bounds how far a miner can backdate a block to game the retarget while
// still tolerating small out-of-order timestamps.
func medianTimePast(blocks []Block) int64 {
	k := 11
	if len(blocks) < k {
		k = len(blocks)
	}
	ts := make([]int64, k)
	for i := 0; i < k; i++ {
		ts[i] = blocks[len(blocks)-1-i].Timestamp
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
	return ts[k/2]
}

// applyBlock fully validates `block` as the successor to blocks[len-1] and, if
// valid, applies it to `state` in place, returning an undo log that reverses it.
// On any error the state is rolled back so it is left exactly as it was. It also
// verifies the committed state root: the post-block account state must hash to
// block.StateRoot.
func applyBlock(state map[string]Account, blocks []Block, block Block, cache *ValidationCache) ([]undoEntry, error) {
	if err := validateBlockStructure(blocks, block); err != nil {
		return nil, err
	}
	// Warm the signature cache across all cores before the serial pass below, so
	// initial sync is not bound to one core verifying one signature at a time.
	cache.verifyAllAuthorized(block.Transactions)
	undo, err := applyTxsAndCoinbase(state, blocks, block, cache)
	if err != nil {
		return nil, err
	}
	if root := stateRoot(state); block.StateRoot != root {
		applyUndo(state, undo)
		return nil, fmt.Errorf("state root mismatch: got %q, want %q", block.StateRoot, root)
	}
	return undo, nil
}

// validateBlockStructure checks everything about a block that does not depend on
// account state: linkage, timestamps, difficulty, proof of work, the merkle root,
// and the coinbase's shape. It is separated from state application so the miner
// can compute the resulting state root (NextStateRoot) before a block is mined.
func validateBlockStructure(blocks []Block, block Block) error {
	prev := blocks[len(blocks)-1]
	if block.Index != prev.Index+1 {
		return fmt.Errorf("bad index %d after %d", block.Index, prev.Index)
	}
	if block.PrevHash != prev.Hash {
		return errors.New("prev hash mismatch")
	}
	if block.Timestamp <= medianTimePast(blocks) {
		return errors.New("timestamp not after median-time-past")
	}
	// NetworkTime, not time.Now: a node whose own clock is skewed would
	// otherwise reject every block its peers produce (see nettime.go).
	if block.Timestamp > NetworkTime()+MaxFutureDrift {
		return errors.New("timestamp too far in the future")
	}
	if cp, ok := checkpointAt(block.Index); ok && block.Hash != cp {
		return fmt.Errorf("block %d violates checkpoint (hash does not match the pinned value)", block.Index)
	}
	if block.Bits != expectedBits(blocks, block.Index) {
		return fmt.Errorf("wrong pow target: bits %#x, want %#x", block.Bits, expectedBits(blocks, block.Index))
	}
	if block.BaseFee != expectedBaseFee(blocks, block.Index) {
		return fmt.Errorf("wrong base fee %d, want %d", block.BaseFee, expectedBaseFee(blocks, block.Index))
	}
	if !block.HasValidPoW() {
		return errors.New("invalid proof of work")
	}
	if MerkleRoot(block.Transactions) != block.MerkleRoot {
		return errors.New("merkle root mismatch")
	}
	if len(block.Transactions) == 0 {
		return errors.New("block has no coinbase transaction")
	}
	if len(block.Transactions) > MaxBlockTxs+1 {
		return errors.New("too many transactions")
	}
	if w := blockWeight(block); w > MaxBlockBytes {
		return fmt.Errorf("block too large: %d transaction bytes (max %d)", w, MaxBlockBytes)
	}
	// Bytes are not the only scarce resource a block spends: signature
	// verification is, and one multisig spend can cost as much of it as hundreds
	// of ordinary payments. Without a budget a block could take longer to check
	// than to produce, which every node on the network pays for (Bitcoin meters
	// the same thing as sigops).
	if ops := BlockVerifyOps(block.Transactions); ops > MaxBlockVerifyOps {
		return fmt.Errorf("block too expensive to verify: %d signature operations (max %d)", ops, MaxBlockVerifyOps)
	}
	coinbase := block.Transactions[0]
	if !coinbase.IsCoinbase() {
		return errors.New("first transaction must be coinbase")
	}
	if coinbase.To == "" {
		return errors.New("coinbase has no recipient")
	}
	return validateCoinbaseShape(coinbase, block.Index)
}

// validateCoinbaseShape pins the coinbase to the one form it is allowed to take:
// a recipient and an amount, nothing else. The coinbase is exempt from the
// per-byte base fee and from MaxBlockBytes (both meter the paid transactions), so
// its size needs its own cap or a miner could bloat every node's storage for
// free. The unused fields must be empty for the same reason a transaction may
// carry only one kind of authorization — they are committed by the block hash but
// mean nothing, so leaving them free only invites divergence between
// implementations. Every coinbase this code has ever produced (NewCoinbase sets
// exactly From/To/Amount) satisfies this.
func validateCoinbaseShape(cb Transaction, height uint64) error {
	if n := cb.Size(); n > MaxCoinbaseBytes {
		return fmt.Errorf("coinbase too large: %d bytes (max %d)", n, MaxCoinbaseBytes)
	}
	if len(cb.To) > MaxAddressBytes {
		return fmt.Errorf("coinbase recipient too long (%d > %d)", len(cb.To), MaxAddressBytes)
	}
	if cb.Fee != 0 || cb.Expiry != 0 || cb.LockUntil != 0 {
		return errors.New("coinbase must not set fee, expiry or lock_until")
	}
	// The Nonce field is where the block height is bound once BIP34 is active; it
	// is the only field of the coinbase whose legal value depends on the block
	// carrying it (see UpgradeUniqueCoinbase).
	if IsUpgradeActive(UpgradeUniqueCoinbase, height) {
		if cb.Nonce != height {
			return fmt.Errorf("coinbase nonce %d must equal the block height %d", cb.Nonce, height)
		}
	} else if cb.Nonce != 0 {
		return errors.New("coinbase must not set nonce")
	}
	if cb.AssetID != "" || cb.Issue != nil || cb.AssetOp != nil {
		return errors.New("coinbase must not carry an asset")
	}
	if cb.PubKey != "" || cb.Signature != "" || len(cb.Signatures) > 0 {
		return errors.New("coinbase must not carry signatures")
	}
	if cb.Multisig != nil || cb.HTLC != nil || cb.Vault != nil || cb.Preimage != "" {
		return errors.New("coinbase must not carry an authorization script")
	}
	if cb.FeePayer != "" || cb.FeePayerPubKey != "" || cb.FeePayerSig != "" {
		return errors.New("coinbase must not carry a fee sponsor (it pays no fee)")
	}
	if len(cb.Memo) > MaxMemoBytes {
		return fmt.Errorf("coinbase memo too long (%d > %d)", len(cb.Memo), MaxMemoBytes)
	}
	return nil
}

// TxRejection identifies which transaction in a candidate block consensus
// refused, and why. Returning the position rather than just a message is what
// lets a miner recover: if a mempool transaction turns out to be unmineable,
// every block built on it is invalid, so the miner must be able to find and drop
// the offender instead of rebuilding the same doomed candidate forever.
type TxRejection struct {
	Index int // position in the block's transaction list
	Err   error
}

func (e *TxRejection) Error() string { return fmt.Sprintf("tx %d: %v", e.Index, e.Err) }
func (e *TxRejection) Unwrap() error { return e.Err }

// checkTxAtHeight applies the consensus rules that depend on the height a
// transaction is being included at, as opposed to the context-free ones in
// CheckTxSanity. Mempool.Select mirrors these for the block it is building, so
// the miner never selects a transaction its own rules would reject.
func checkTxAtHeight(tx Transaction, height uint64) error {
	if tx.IsExpiredAt(height) {
		return fmt.Errorf("expired (expiry %d < height %d)", tx.Expiry, height)
	}
	if tx.IsLockedAt(height) {
		return fmt.Errorf("not yet valid (lock_until %d > height %d)", tx.LockUntil, height)
	}
	if tx.HTLCRefundNotReady(height) {
		return fmt.Errorf("htlc refund before timeout (timeout %d > height %d)", tx.HTLC.Timeout, height)
	}
	if tx.VaultHotNotReady(height) {
		return fmt.Errorf("vault hot-key spend before unlock (unlock %d > height %d)", tx.Vault.Unlock, height)
	}
	// Height-activated rule (consensus upgrade): vault-authorized spends.
	if tx.IsVault() && !IsUpgradeActive(UpgradeVault, height) {
		return errors.New("vault spends are not active at this height")
	}
	// Height-activated rule (consensus upgrade): minting and burning an existing
	// asset. Before this, a supply is fixed at issuance and holders can rely on
	// that, so switching it on changes what they are trusting.
	if tx.IsAssetOp() && !IsUpgradeActive(UpgradeAssetOps, height) {
		return errors.New("asset mint/burn operations are not active at this height")
	}
	// Height-activated rule (consensus upgrade): fee sponsorship.
	if tx.IsSponsored() && !IsUpgradeActive(UpgradeFeeSponsor, height) {
		return errors.New("fee sponsorship is not active at this height")
	}
	// Height-activated rule (consensus upgrade): multi-recipient transfers are
	// only valid from their activation height, so the whole network starts
	// accepting them together rather than splitting over whether a block is valid.
	if tx.IsMultiOutput() && !IsUpgradeActive(UpgradeMultiOutput, height) {
		return errors.New("multi-output transfers are not active at this height")
	}
	// Height-activated rule (consensus upgrade): every address must be a
	// well-formed, checksummed one. This is the only rule that stops a client bug
	// from burning coin to a typo — client-side validation is a convention, and
	// consensus has never enforced it (see UpgradeCheckedAddresses).
	if IsUpgradeActive(UpgradeCheckedAddresses, height) {
		if err := checkTxAddresses(tx); err != nil {
			return err
		}
	}
	// Height-activated rule (consensus upgrade): once UpgradeDustLimit is in
	// force, coin transfers below DustThreshold are rejected. Off until an
	// activation height is scheduled, and never applies to asset/issue txs.
	if IsUpgradeActive(UpgradeDustLimit, height) && !tx.IsIssue() && !tx.IsAssetTransfer() {
		for _, amount := range tx.coinAmounts() {
			if amount > 0 && amount < DustThreshold {
				return fmt.Errorf("dust output: amount %d below dust threshold %d", amount, DustThreshold)
			}
		}
	}
	return nil
}

// applyTxsAndCoinbase applies a structurally-valid block's transactions and
// coinbase to `state` in place, returning an undo log. It performs the
// state-dependent checks (nonces, balances, coinbase amount) but NOT the
// structural ones (see validateBlockStructure) — so it can also be run on a
// state copy to compute a candidate block's resulting state root before mining.
func applyTxsAndCoinbase(state map[string]Account, blocks []Block, block Block, cache *ValidationCache) ([]undoEntry, error) {
	height := block.Index
	coinbase := block.Transactions[0]

	// set records the pre-change value before every mutation, so the undo log
	// (applied in reverse) restores the exact pre-block state.
	var undo []undoEntry
	set := func(addr string, acc Account) {
		prevAcc, existed := state[addr]
		undo = append(undo, undoEntry{addr: addr, prev: prevAcc, existed: existed})
		state[addr] = acc
	}
	fail := func(err error) ([]undoEntry, error) {
		applyUndo(state, undo)
		return nil, err
	}

	baseFee := block.BaseFee
	seen := make(map[string]bool)
	var tips, burned uint64
	for i := 1; i < len(block.Transactions); i++ {
		tx := block.Transactions[i]
		if tx.IsCoinbase() {
			return fail(&TxRejection{Index: i, Err: errors.New("only one coinbase transaction allowed")})
		}
		if err := CheckTxSanity(tx); err != nil {
			return fail(&TxRejection{Index: i, Err: err})
		}
		if err := checkTxAtHeight(tx, height); err != nil {
			return fail(&TxRejection{Index: i, Err: err})
		}
		minFee := BaseFeeFor(tx, baseFee)
		if tx.Fee < minFee {
			return fail(&TxRejection{Index: i, Err: fmt.Errorf("fee %d below per-byte base fee %d (%d bytes × %d)", tx.Fee, minFee, tx.Size(), baseFee)})
		}
		h := tx.Hash()
		if seen[h] {
			return fail(&TxRejection{Index: i, Err: errors.New("duplicate transaction in block")})
		}
		seen[h] = true
		reserve := immatureCoinbase(blocks, height, tx.From)
		// A fee sponsor spends coin too, so its own immature coinbase is reserved
		// exactly as the sender's is — otherwise a miner could pay everyone's fees
		// out of a reward a reorg may yet take away.
		var payerReserve uint64
		if tx.IsSponsored() {
			payerReserve = immatureCoinbase(blocks, height, tx.FeePayer)
		}
		if err := applyTxTo(state, tx, reserve, payerReserve, set, cache); err != nil {
			return fail(&TxRejection{Index: i, Err: err})
		}
		tips += tx.Fee - minFee // base fee × size is burned; miner keeps only the tip
		burned += minFee
	}

	// Miner is paid the subsidy plus tips; the base-fee portion of every fee is
	// burned (never credited), permanently reducing the money supply.
	reward := BlockReward(height)
	if coinbase.Amount != reward+tips {
		return fail(fmt.Errorf("bad coinbase amount: got %d, want %d (reward %d + tips %d)",
			coinbase.Amount, reward+tips, reward, tips))
	}
	acc := state[coinbase.To]
	if acc.Balance+coinbase.Amount < acc.Balance {
		return fail(errors.New("coinbase amount overflow"))
	}
	acc.Balance += coinbase.Amount
	set(coinbase.To, acc)

	// Everything above checks that each transaction is individually legal. This
	// checks the block as a whole: that applying it created exactly the subsidy
	// and destroyed exactly the base fees, and moved no asset into or out of
	// existence. An arithmetic slip anywhere in the application path shows up
	// here as a rejected block rather than as silent inflation (see
	// conservation.go).
	if err := checkConservation(state, undo, block.Transactions, reward, burned); err != nil {
		return fail(err)
	}
	return undo, nil
}

// NextStateRoot computes the state root a candidate block would commit to, by
// applying its transactions to a copy of current state. The miner calls it to
// fill block.StateRoot before mining (the root is part of the proof-of-work
// commitment). Errors if the candidate's transactions don't apply cleanly.
func (bc *Blockchain) NextStateRoot(candidate Block) (string, error) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	s := cloneState(bc.state)
	if _, err := applyTxsAndCoinbase(s, bc.blocks, candidate, bc.sigCache); err != nil {
		return "", err
	}
	return stateRoot(s), nil
}

// applyTxTo validates a single signed transfer against state and applies it via
// set (which records undo information). reserve is the sender's immature
// coinbase amount that must remain unspent (coinbase maturity).
func applyTxTo(state map[string]Account, tx Transaction, reserve, payerReserve uint64, set func(string, Account), cache *ValidationCache) error {
	if err := cache.Verify(tx); err != nil {
		return err
	}
	if tx.IsIssue() {
		return applyIssue(state, tx, reserve, payerReserve, set)
	}
	if tx.IsAssetOp() {
		return applyAssetOp(state, tx, reserve, payerReserve, set)
	}
	if tx.IsAssetTransfer() {
		return applyAssetTransfer(state, tx, reserve, payerReserve, set)
	}
	out, ok := tx.TotalOut()
	if !ok {
		return errors.New("outputs overflow")
	}
	if out == 0 && tx.Fee == 0 {
		return errors.New("empty transfer")
	}
	total := out + senderFee(tx)
	if total < out {
		return errors.New("amount+fee overflow")
	}
	sender := state[tx.From]
	if tx.Nonce != sender.Nonce {
		return fmt.Errorf("bad nonce for %s: got %d, want %d", tx.From, tx.Nonce, sender.Nonce)
	}
	need := total + reserve
	if need < total {
		return errors.New("amount+fee+reserve overflow")
	}
	if sender.Balance < need {
		return fmt.Errorf("insufficient spendable balance for %s: have %d, need %d (%d immature)", tx.From, sender.Balance, total, reserve)
	}
	sender.Balance -= total
	sender.Nonce++
	set(tx.From, sender)
	if err := chargeSponsor(state, tx, payerReserve, set); err != nil {
		return err
	}

	// Credit each recipient in turn, reading state back every time so a repeated
	// recipient accumulates and a transaction paying its own sender nets correctly.
	for _, o := range tx.outputs() {
		recip := state[o.To]
		if recip.Balance+o.Amount < recip.Balance {
			return errors.New("recipient balance overflow")
		}
		recip.Balance += o.Amount
		set(o.To, recip)
	}
	return nil
}

// applyIssue mints a new asset to the sender: it pays the coin fee (respecting
// coinbase maturity via reserve) and is credited Supply units of a fresh asset
// whose id is bound to (issuer, ticker, nonce).
func applyIssue(state map[string]Account, tx Transaction, reserve, payerReserve uint64, set func(string, Account)) error {
	if err := validTicker(tx.Issue.Ticker); err != nil {
		return err
	}
	if tx.Issue.Supply == 0 || tx.Issue.Supply > MaxAssetSupply {
		return fmt.Errorf("asset supply must be in 1..%d", MaxAssetSupply)
	}
	sender := state[tx.From]
	if tx.Nonce != sender.Nonce {
		return fmt.Errorf("bad nonce for %s: got %d, want %d", tx.From, tx.Nonce, sender.Nonce)
	}
	fee := senderFee(tx)
	need := fee + reserve
	if need < fee {
		return errors.New("fee+reserve overflow")
	}
	if sender.Balance < need {
		return fmt.Errorf("insufficient spendable balance for %s: have %d, need fee %d (%d immature)", tx.From, sender.Balance, fee, reserve)
	}
	sender.Balance -= fee
	sender.Nonce++
	sender = sender.withAssetDelta(AssetID(tx.From, tx.Issue.Ticker, tx.Nonce), int64(tx.Issue.Supply))
	set(tx.From, sender)
	return chargeSponsor(state, tx, payerReserve, set)
}

// applyAssetOp mints or burns units of an asset the sender issued.
//
// Authority is checked by reproducing the asset id from the sender and the
// operation's stated ticker and issuing nonce: an id that matches can only have
// been produced by that issuer (see asset.go). Nothing is looked up, so this is
// a pure function of the transaction and the sender's account, exactly like
// every other rule here.
func applyAssetOp(state map[string]Account, tx Transaction, reserve, payerReserve uint64, set func(string, Account)) error {
	op := tx.AssetOp
	if err := op.Validate(); err != nil {
		return err
	}
	if !op.AuthorizedBy(tx.From, tx.AssetID) {
		return fmt.Errorf("%s did not issue asset %s (the stated ticker and nonce do not derive it)", tx.From, tx.AssetID)
	}

	sender := state[tx.From]
	if tx.Nonce != sender.Nonce {
		return fmt.Errorf("bad nonce for %s: got %d, want %d", tx.From, tx.Nonce, sender.Nonce)
	}
	fee := senderFee(tx)
	need := fee + reserve
	if need < fee {
		return errors.New("fee+reserve overflow")
	}
	if sender.Balance < need {
		return fmt.Errorf("insufficient coin for fee for %s: have %d, need %d (%d immature)", tx.From, sender.Balance, fee, reserve)
	}

	held := sender.Assets[tx.AssetID]
	var delta int64
	switch op.Op {
	case AssetOpMint:
		// The cap is on the issuer's holding, which is the whole supply for a mint:
		// nothing else can create units, so this is where the total is bounded and
		// where the signed arithmetic elsewhere is kept from overflowing.
		if held+op.Amount < held || held+op.Amount > MaxAssetSupply {
			return fmt.Errorf("minting %d would take asset %s past the %d cap", op.Amount, tx.AssetID, MaxAssetSupply)
		}
		delta = int64(op.Amount)
	case AssetOpBurn:
		// An issuer may only burn what it HOLDS. Burning someone else's units
		// would be confiscation, which is a different feature and not this one.
		if held < op.Amount {
			return fmt.Errorf("cannot burn %d of asset %s: the issuer holds %d", op.Amount, tx.AssetID, held)
		}
		delta = -int64(op.Amount)
	default:
		return fmt.Errorf("unknown asset operation %q", op.Op)
	}

	sender.Balance -= fee
	sender.Nonce++
	sender = sender.withAssetDelta(tx.AssetID, delta)
	set(tx.From, sender)
	return chargeSponsor(state, tx, payerReserve, set)
}

// applyAssetTransfer moves tx.Amount of asset tx.AssetID from sender to
// recipient. The fee is paid in coin (respecting coinbase maturity); the asset
// amount is checked against the sender's asset balance. Supplies are capped
// (MaxAssetSupply) so the arithmetic can't overflow.
func applyAssetTransfer(state map[string]Account, tx Transaction, reserve, payerReserve uint64, set func(string, Account)) error {
	if tx.Amount == 0 {
		return errors.New("empty asset transfer")
	}
	sender := state[tx.From]
	if tx.Nonce != sender.Nonce {
		return fmt.Errorf("bad nonce for %s: got %d, want %d", tx.From, tx.Nonce, sender.Nonce)
	}
	fee := senderFee(tx)
	need := fee + reserve
	if need < fee {
		return errors.New("fee+reserve overflow")
	}
	if sender.Balance < need {
		return fmt.Errorf("insufficient coin for fee for %s: have %d, need %d (%d immature)", tx.From, sender.Balance, fee, reserve)
	}
	if sender.Assets[tx.AssetID] < tx.Amount {
		return fmt.Errorf("insufficient asset for %s: have %d, need %d", tx.From, sender.Assets[tx.AssetID], tx.Amount)
	}
	sender.Balance -= fee
	sender.Nonce++
	sender = sender.withAssetDelta(tx.AssetID, -int64(tx.Amount))
	set(tx.From, sender)
	if err := chargeSponsor(state, tx, payerReserve, set); err != nil {
		return err
	}

	recip := state[tx.To] // reflects the sender update when From == To
	recip = recip.withAssetDelta(tx.AssetID, int64(tx.Amount))
	set(tx.To, recip)
	return nil
}

// senderFee is the part of a transaction's fee that comes out of the SENDER's
// balance: all of it normally, none of it when a sponsor pays (see
// chargeSponsor). The fee itself is unchanged either way — the base-fee portion
// is still burned and the tip still goes to the miner; only who is debited moves.
func senderFee(tx Transaction) uint64 {
	if tx.IsSponsored() {
		return 0
	}
	return tx.Fee
}

// chargeSponsor debits a sponsored transaction's fee from the fee payer, subject
// to the payer's own coinbase-maturity reserve. It is a no-op for an unsponsored
// transaction.
//
// The sponsor's NONCE is deliberately untouched: it is not the sponsor's
// transaction. Replay is already impossible because the sponsor's signature
// covers the sender and the sender's nonce, which the ledger consumes exactly
// once — so a sponsorship is spent along with the transfer it paid for. Leaving
// the nonce alone also means sponsoring does not disturb transactions the payer
// has of its own in flight.
func chargeSponsor(state map[string]Account, tx Transaction, reserve uint64, set func(string, Account)) error {
	if !tx.IsSponsored() {
		return nil
	}
	payer := state[tx.FeePayer]
	need := tx.Fee + reserve
	if need < tx.Fee {
		return errors.New("fee+reserve overflow")
	}
	if payer.Balance < need {
		return fmt.Errorf("fee sponsor %s cannot cover the fee: have %d, need %d (%d immature)",
			tx.FeePayer, payer.Balance, tx.Fee, reserve)
	}
	payer.Balance -= tx.Fee
	set(tx.FeePayer, payer)
	return nil
}

// applyUndo reverses a block's state changes by restoring recorded prior values
// in reverse order.
func applyUndo(state map[string]Account, undo []undoEntry) {
	for i := len(undo) - 1; i >= 0; i-- {
		u := undo[i]
		if u.existed {
			state[u.addr] = u.prev
		} else {
			delete(state, u.addr)
		}
	}
}

func cloneState(s map[string]Account) map[string]Account {
	c := make(map[string]Account, len(s))
	for k, v := range s {
		c[k] = v
	}
	return c
}
