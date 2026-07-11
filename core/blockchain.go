package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sort"
	"sync"
	"time"
)

// Account is the state tracked per address: a spendable balance and a nonce
// (the next expected transaction sequence number, which stops replay).
type Account struct {
	Balance uint64 `json:"balance"`
	Nonce   uint64 `json:"nonce"`
}

// Blockchain is a thread-safe chain of blocks plus the account state derived by
// replaying every transaction. All exported methods take the lock, so it is
// safe to share one *Blockchain across the miner, P2P handlers and API.
type Blockchain struct {
	mu     sync.RWMutex
	blocks []Block
	state  map[string]Account
	work   *big.Int      // cumulative proof-of-work of blocks
	undos  [][]undoEntry // undos[i] reverts blocks[i]'s state changes (undos[0] is nil)
	store  *blockStore   // append-only persistence (nil = in-memory only)
}

// undoEntry records an account's prior value so a block's effect can be
// reversed during a reorg without replaying the chain from genesis.
type undoEntry struct {
	addr    string
	prev    Account
	existed bool
}

// GenesisBlock is fixed and identical on every node; without this, two fresh
// nodes would compute different genesis hashes and never agree on a chain.
func GenesisBlock() Block {
	b := Block{
		Index:      0,
		Timestamp:  GenesisTimestamp,
		PrevHash:   GenesisPrevHash,
		Difficulty: GenesisDifficulty,
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
	return &Blockchain{
		blocks: []Block{genesis},
		state:  map[string]Account{},
		work:   BlockWork(genesis.Difficulty),
		undos:  [][]undoEntry{nil}, // genesis has no undo (it is never rolled back)
	}
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

// NextDifficulty is the difficulty the next mined block must satisfy.
func (bc *Blockchain) NextDifficulty() int {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return expectedDifficulty(bc.blocks, uint64(len(bc.blocks)))
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
// proof. The bool is false if the transaction is not in the chain.
func (bc *Blockchain) FindTxProof(txHash string) (TxProof, bool) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	tip := bc.blocks[len(bc.blocks)-1].Index
	for _, b := range bc.blocks {
		for i, tx := range b.Transactions {
			if tx.Hash() == txHash {
				proof, _ := MerkleProof(b.Transactions, i)
				return TxProof{
					Found:         true,
					BlockIndex:    b.Index,
					BlockHash:     b.Hash,
					MerkleRoot:    b.MerkleRoot,
					Confirmations: tip - b.Index + 1,
					Tx:            tx,
					Proof:         proof,
				}, true
			}
		}
	}
	return TxProof{Found: false}, false
}

// AddBlock validates a block against the current tip and, if valid, appends it
// and updates account state in place. Application is incremental (no full-state
// clone): the block's changes are applied directly and an undo log is recorded
// so a later reorg can reverse them. On any validation error the state is left
// unchanged.
func (bc *Blockchain) AddBlock(block Block) error {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	undo, err := applyBlock(bc.state, bc.blocks, block)
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
	bc.work.Add(bc.work, BlockWork(block.Difficulty))
	return nil
}

// ReplaceChain adopts an incoming chain when it wins the fork-choice rule and is
// fully valid from genesis. Returns whether it was adopted. The rule is:
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
func (bc *Blockchain) ReplaceChain(incoming []Block) (bool, error) {
	if len(incoming) == 0 {
		return false, errors.New("empty chain")
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()
	if incoming[0].Hash != GenesisBlock().Hash {
		return false, errors.New("genesis mismatch")
	}
	fork := commonPrefix(bc.blocks, incoming) - 1 // last shared block (>= 0)
	return bc.reorgLocked(fork, incoming[fork+1:])
}

// ReorgFrom replaces the blocks above forkHeight with the given suffix (which
// must build on our block at forkHeight), adopting it if it wins the fork-choice
// rule. Block-locator sync uses this to transfer only the divergent suffix
// rather than a whole competing chain. Callers must not hold bc.mu.
func (bc *Blockchain) ReorgFrom(forkHeight uint64, suffix []Block) (bool, error) {
	if len(suffix) == 0 {
		return false, nil
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()
	if forkHeight >= uint64(len(bc.blocks)) {
		return false, errors.New("fork height beyond tip")
	}
	return bc.reorgLocked(int(forkHeight), suffix)
}

// reorgLocked replaces the blocks above `fork` with `suffix`, applying the
// fork-choice rule (most work; ties broken by the smaller tip hash). It
// validates the suffix on a rolled-back copy of state — so a bad suffix cannot
// corrupt the live chain — then persists and commits atomically. bc.mu held.
func (bc *Blockchain) reorgLocked(fork int, suffix []Block) (bool, error) {
	// Candidate cumulative work = shared prefix + suffix.
	candWork := ChainWork(bc.blocks[:fork+1])
	for _, b := range suffix {
		candWork.Add(candWork, BlockWork(b.Difficulty))
	}
	switch candWork.Cmp(bc.work) {
	case -1: // less work
		return false, nil
	case 0: // equal work: adopt only if the candidate tip hash is smaller
		candTip := bc.blocks[fork].Hash
		if len(suffix) > 0 {
			candTip = suffix[len(suffix)-1].Hash
		}
		if candTip >= bc.blocks[len(bc.blocks)-1].Hash {
			return false, nil
		}
	}

	state := cloneState(bc.state)
	for i := len(bc.blocks) - 1; i > fork; i-- {
		applyUndo(state, bc.undos[i])
	}
	blocks := append([]Block(nil), bc.blocks[:fork+1]...)
	undos := append([][]undoEntry(nil), bc.undos[:fork+1]...)
	for i, b := range suffix {
		undo, err := applyBlock(state, blocks, b)
		if err != nil {
			return false, fmt.Errorf("block %d: %w", fork+1+i, err)
		}
		blocks = append(blocks, b)
		undos = append(undos, undo)
	}

	// Persist (truncate to the fork, append the new suffix) before committing in
	// memory, so disk and memory stay consistent.
	if bc.store != nil {
		if err := bc.store.truncateAfter(uint64(fork)); err != nil {
			return false, fmt.Errorf("persist reorg: %w", err)
		}
		for i := fork + 1; i < len(blocks); i++ {
			if err := bc.store.append(blocks[i]); err != nil {
				return false, fmt.Errorf("persist reorg: %w", err)
			}
		}
	}

	bc.blocks = blocks
	bc.state = state
	bc.undos = undos
	bc.work = candWork
	return true, nil
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
		for i := 1; i < len(blocks); i++ {
			if err := bc.AddBlock(blocks[i]); err != nil {
				store.close()
				return nil, fmt.Errorf("replay block %d: %w", i, err)
			}
		}
	}
	bc.store = store // future writes now persist
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
	ok, err := bc.ReplaceChain(blocks)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("failed to load chain")
	}
	return bc, nil
}

// expectedDifficulty deterministically derives the required difficulty for the
// block at `height`, given the chain up to height-1. Difficulty only changes on
// RetargetInterval boundaries, adjusting toward TargetBlockTime.
func expectedDifficulty(blocks []Block, height uint64) int {
	if height == 0 {
		return GenesisDifficulty
	}
	prevDiff := blocks[height-1].Difficulty
	if height%RetargetInterval != 0 {
		return prevDiff
	}
	window := RetargetInterval
	actual := blocks[height-1].Timestamp - blocks[height-window].Timestamp
	expected := int64(window) * TargetBlockTime

	diff := prevDiff
	switch {
	case actual < expected/2:
		diff++
	case actual > expected*2:
		diff--
	}
	if diff < MinDifficulty {
		diff = MinDifficulty
	}
	if diff > MaxDifficulty {
		diff = MaxDifficulty
	}
	return diff
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
		cb := blocks[i].Transactions[0]
		if cb.IsCoinbase() && cb.To == addr {
			sum += cb.Amount
		}
	}
	return sum
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
func applyBlock(state map[string]Account, blocks []Block, block Block) ([]undoEntry, error) {
	if err := validateBlockStructure(blocks, block); err != nil {
		return nil, err
	}
	undo, err := applyTxsAndCoinbase(state, blocks, block)
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
	if block.Timestamp > time.Now().Unix()+MaxFutureDrift {
		return errors.New("timestamp too far in the future")
	}
	if block.Difficulty != expectedDifficulty(blocks, block.Index) {
		return fmt.Errorf("wrong difficulty %d", block.Difficulty)
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
	coinbase := block.Transactions[0]
	if !coinbase.IsCoinbase() {
		return errors.New("first transaction must be coinbase")
	}
	if coinbase.To == "" {
		return errors.New("coinbase has no recipient")
	}
	return nil
}

// applyTxsAndCoinbase applies a structurally-valid block's transactions and
// coinbase to `state` in place, returning an undo log. It performs the
// state-dependent checks (nonces, balances, coinbase amount) but NOT the
// structural ones (see validateBlockStructure) — so it can also be run on a
// state copy to compute a candidate block's resulting state root before mining.
func applyTxsAndCoinbase(state map[string]Account, blocks []Block, block Block) ([]undoEntry, error) {
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
	var tips uint64
	for i := 1; i < len(block.Transactions); i++ {
		tx := block.Transactions[i]
		if tx.IsCoinbase() {
			return fail(errors.New("only one coinbase transaction allowed"))
		}
		if tx.IsExpiredAt(height) {
			return fail(fmt.Errorf("tx %d expired (expiry %d < height %d)", i, tx.Expiry, height))
		}
		if tx.IsLockedAt(height) {
			return fail(fmt.Errorf("tx %d not yet valid (lock_until %d > height %d)", i, tx.LockUntil, height))
		}
		if tx.HTLCRefundNotReady(height) {
			return fail(fmt.Errorf("tx %d htlc refund before timeout (timeout %d > height %d)", i, tx.HTLC.Timeout, height))
		}
		if tx.Fee < baseFee {
			return fail(fmt.Errorf("tx %d fee %d below base fee %d", i, tx.Fee, baseFee))
		}
		if len(tx.Memo) > MaxMemoBytes {
			return fail(fmt.Errorf("tx %d memo too long (%d > %d)", i, len(tx.Memo), MaxMemoBytes))
		}
		h := tx.Hash()
		if seen[h] {
			return fail(errors.New("duplicate transaction in block"))
		}
		seen[h] = true
		reserve := immatureCoinbase(blocks, height, tx.From)
		if err := applyTxTo(state, tx, reserve, set); err != nil {
			return fail(fmt.Errorf("tx %d: %w", i, err))
		}
		tips += tx.Fee - baseFee // base fee is burned; miner keeps only the tip
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
	if _, err := applyTxsAndCoinbase(s, bc.blocks, candidate); err != nil {
		return "", err
	}
	return stateRoot(s), nil
}

// applyTxTo validates a single signed transfer against state and applies it via
// set (which records undo information). reserve is the sender's immature
// coinbase amount that must remain unspent (coinbase maturity).
func applyTxTo(state map[string]Account, tx Transaction, reserve uint64, set func(string, Account)) error {
	if err := tx.VerifySignature(); err != nil {
		return err
	}
	if tx.Amount == 0 && tx.Fee == 0 {
		return errors.New("empty transfer")
	}
	total := tx.Amount + tx.Fee
	if total < tx.Amount {
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

	recip := state[tx.To]
	if recip.Balance+tx.Amount < recip.Balance {
		return errors.New("recipient balance overflow")
	}
	recip.Balance += tx.Amount
	set(tx.To, recip)
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
