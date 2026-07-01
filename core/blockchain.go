package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
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
	work   *big.Int // cumulative proof-of-work of blocks
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
	}
}

// --- read accessors (locked) ----------------------------------------------

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

// --- mutation (locked) ------------------------------------------------------

// AddBlock validates a block against the current tip and, if valid, appends it
// and updates account state atomically.
func (bc *Blockchain) AddBlock(block Block) error {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	newState, err := validateAndApply(bc.blocks, bc.state, block)
	if err != nil {
		return err
	}
	bc.blocks = append(bc.blocks, block)
	bc.state = newState
	bc.work.Add(bc.work, BlockWork(block.Difficulty))
	return nil
}

// ReplaceChain adopts an incoming chain if it carries strictly more cumulative
// work than ours and is fully valid from genesis. Returns whether it was
// adopted. Equal-or-less work keeps our chain (no flapping on ties).
func (bc *Blockchain) ReplaceChain(incoming []Block) (bool, error) {
	if len(incoming) == 0 {
		return false, errors.New("empty chain")
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()

	if ChainWork(incoming).Cmp(bc.work) <= 0 {
		return false, nil
	}
	if incoming[0].Hash != GenesisBlock().Hash {
		return false, errors.New("genesis mismatch")
	}

	blocks := []Block{incoming[0]}
	state := map[string]Account{}
	for i := 1; i < len(incoming); i++ {
		newState, err := validateAndApply(blocks, state, incoming[i])
		if err != nil {
			return false, fmt.Errorf("block %d: %w", i, err)
		}
		blocks = append(blocks, incoming[i])
		state = newState
	}
	bc.blocks = blocks
	bc.state = state
	bc.work = ChainWork(blocks)
	return true, nil
}

// --- persistence ------------------------------------------------------------

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

// Load reads a chain from disk and revalidates it end to end.
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

// --- validation core --------------------------------------------------------

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

// validateAndApply fully validates `block` as the successor to blocks[len-1]
// against `prevState`, returning the new state on success. It does not mutate
// its inputs, so callers can use it for both AddBlock and chain replacement.
func validateAndApply(blocks []Block, prevState map[string]Account, block Block) (map[string]Account, error) {
	prev := blocks[len(blocks)-1]
	height := block.Index

	if block.Index != prev.Index+1 {
		return nil, fmt.Errorf("bad index %d after %d", block.Index, prev.Index)
	}
	if block.PrevHash != prev.Hash {
		return nil, errors.New("prev hash mismatch")
	}
	if block.Timestamp <= prev.Timestamp {
		return nil, errors.New("timestamp not increasing")
	}
	if block.Timestamp > time.Now().Unix()+MaxFutureDrift {
		return nil, errors.New("timestamp too far in the future")
	}
	if block.Difficulty != expectedDifficulty(blocks, height) {
		return nil, fmt.Errorf("wrong difficulty %d", block.Difficulty)
	}
	if !block.HasValidPoW() {
		return nil, errors.New("invalid proof of work")
	}
	if MerkleRoot(block.Transactions) != block.MerkleRoot {
		return nil, errors.New("merkle root mismatch")
	}
	if len(block.Transactions) == 0 {
		return nil, errors.New("block has no coinbase transaction")
	}
	if len(block.Transactions) > MaxBlockTxs+1 {
		return nil, errors.New("too many transactions")
	}

	coinbase := block.Transactions[0]
	if !coinbase.IsCoinbase() {
		return nil, errors.New("first transaction must be coinbase")
	}
	if coinbase.To == "" {
		return nil, errors.New("coinbase has no recipient")
	}

	state := cloneState(prevState)
	seen := make(map[string]bool)
	var fees uint64
	for i := 1; i < len(block.Transactions); i++ {
		tx := block.Transactions[i]
		if tx.IsCoinbase() {
			return nil, errors.New("only one coinbase transaction allowed")
		}
		if tx.IsExpiredAt(height) {
			return nil, fmt.Errorf("tx %d expired (expiry %d < height %d)", i, tx.Expiry, height)
		}
		h := tx.Hash()
		if seen[h] {
			return nil, errors.New("duplicate transaction in block")
		}
		seen[h] = true
		if err := applyTx(state, tx); err != nil {
			return nil, fmt.Errorf("tx %d: %w", i, err)
		}
		fees += tx.Fee
	}

	reward := BlockReward(height)
	if coinbase.Amount != reward+fees {
		return nil, fmt.Errorf("bad coinbase amount: got %d, want %d (reward %d + fees %d)",
			coinbase.Amount, reward+fees, reward, fees)
	}
	credit(state, coinbase.To, coinbase.Amount)
	return state, nil
}

func cloneState(s map[string]Account) map[string]Account {
	c := make(map[string]Account, len(s))
	for k, v := range s {
		c[k] = v
	}
	return c
}

func credit(state map[string]Account, addr string, amount uint64) {
	acc := state[addr]
	acc.Balance += amount // overflow-guarded by caller/consensus scale
	state[addr] = acc
}

// applyTx validates a single signed transfer against state and applies it.
func applyTx(state map[string]Account, tx Transaction) error {
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
	if sender.Balance < total {
		return fmt.Errorf("insufficient balance for %s: have %d, need %d", tx.From, sender.Balance, total)
	}
	sender.Balance -= total
	sender.Nonce++
	state[tx.From] = sender
	credit(state, tx.To, tx.Amount)
	return nil
}
