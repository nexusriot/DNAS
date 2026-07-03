package core

import (
	"fmt"
	"strconv"
	"strings"
)

// Consensus and monetary parameters. Every node MUST agree on these for the
// network to converge, so they live in one place and are baked into validation.
const (
	// Ticker is the human-facing symbol.
	Ticker = "DNAS"

	// Coin is the number of indivisible base units in one DNAS (like satoshis
	// per bitcoin). All amounts on the wire and in state are integers of base
	// units to avoid floating-point rounding bugs in consensus.
	Coin uint64 = 100_000_000

	// InitialBlockReward is the coinbase subsidy for the first halving epoch.
	InitialBlockReward uint64 = 50 * Coin
	// HalvingInterval is how many blocks between reward halvings.
	HalvingInterval uint64 = 210_000

	// Proof-of-work: difficulty is the number of leading hex "0"s required in a
	// block hash. Bounded so a toy network can never get stuck on an
	// unreachable target or spin uselessly on a trivial one.
	GenesisDifficulty = 4
	MinDifficulty     = 3
	MaxDifficulty     = 5

	// TargetBlockTime is the desired seconds between blocks; RetargetInterval is
	// how often difficulty is recalculated. Both feed the deterministic
	// retarget rule so all nodes derive the same next difficulty.
	TargetBlockTime  int64  = 5
	RetargetInterval uint64 = 10

	// CoinbaseMaturity is how many blocks a coinbase reward must age before the
	// miner can spend it. This protects against spending a reward that a reorg
	// later removes. (Bitcoin uses 100; kept small here for a lively devnet.)
	CoinbaseMaturity = 3

	// MaxBlockTxs caps non-coinbase transactions per block.
	MaxBlockTxs = 1000
	// MaxMemoBytes caps the optional per-transaction memo.
	MaxMemoBytes = 256

	// DefaultMinRelayFee is the base per-transaction fee a node asks for before it
	// will relay/queue a transaction. It is *relay policy*, not a consensus rule:
	// a node with a lower floor will still accept the transaction in a mined block.
	// The effective floor rises above this base as the mempool fills (see
	// Mempool.MinFee). 0.0001 DNAS keeps a devnet cheap while still deterring spam.
	DefaultMinRelayFee uint64 = Coin / 10_000
	// MaxFutureDrift is how many seconds ahead of local time a block may claim
	// before we reject it (loosely; late blocks are accepted once time passes).
	MaxFutureDrift int64 = 120

	// The genesis block is fixed so every node computes an identical hash and
	// can therefore agree on the same chain. 2025-01-01T00:00:00Z.
	GenesisTimestamp int64  = 1735689600
	GenesisPrevHash  string = "0"
)

// BlockReward returns the coinbase subsidy at a given height, halving every
// HalvingInterval blocks until it reaches zero.
func BlockReward(height uint64) uint64 {
	halvings := height / HalvingInterval
	if halvings >= 64 {
		return 0
	}
	return InitialBlockReward >> halvings
}

// FormatAmount renders base units as a decimal DNAS string with the ticker.
func FormatAmount(units uint64) string {
	return fmt.Sprintf("%d.%08d %s", units/Coin, units%Coin, Ticker)
}

// ParseAmount parses a decimal DNAS string (e.g. "1.5") into base units.
func ParseAmount(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	parts := strings.SplitN(s, ".", 2)
	whole, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amount %q: %w", s, err)
	}
	var frac uint64
	if len(parts) == 2 && parts[1] != "" {
		f := parts[1]
		if len(f) > 8 {
			f = f[:8]
		}
		for len(f) < 8 {
			f += "0"
		}
		frac, err = strconv.ParseUint(f, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid fraction in %q: %w", s, err)
		}
	}
	return whole*Coin + frac, nil
}
