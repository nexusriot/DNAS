package core

// Coin supply accounting. Two independent quantities are tracked so they can be
// checked against each other rather than derived from one another:
//
//   - MINTED is a pure function of height: the sum of every block subsidy up to
//     the tip. Nothing else creates coin.
//   - BURNED is accumulated from block bodies as they connect: every transaction
//     pays base fee × its size, and that portion is destroyed rather than paid to
//     the miner (tips, the fee above it, are only moved from sender to miner).
//
// CIRCULATING is then read straight out of the account state. The identity
//
//	minted − burned == circulating
//
// must hold for every valid chain, so Supply reports it as `Consistent`: a false
// there means an accounting bug has silently inflated or destroyed coin.

// CumulativeSubsidy is the total coin minted by block subsidies from height 1
// through `height` inclusive. Genesis (height 0) mints nothing. It walks halving
// epochs rather than blocks, so it is O(halvings) regardless of chain length.
func CumulativeSubsidy(height uint64) uint64 {
	var total uint64
	for h := uint64(1); h <= height; {
		reward := BlockReward(h)
		if reward == 0 {
			break // subsidy has halved away to nothing; nothing further is minted
		}
		epochEnd := (h/HalvingInterval + 1) * HalvingInterval // first height of the next epoch
		if epochEnd > height+1 {
			epochEnd = height + 1
		}
		total += reward * (epochEnd - h)
		h = epochEnd
	}
	return total
}

// blockBurned is the coin a block destroys: the mandatory per-byte base fee of
// every non-coinbase transaction it carries. A header-only placeholder block
// (a pruned body below a snapshot) burns nothing here — its burn is folded into
// the seed value NewFromSnapshot derives from the verified state.
func blockBurned(b Block) uint64 {
	var burned uint64
	for i := 1; i < len(b.Transactions); i++ {
		burned += BaseFeeFor(b.Transactions[i], b.BaseFee)
	}
	return burned
}

// totalBalance sums the coin held by every account. Assets are a separate ledger
// and never count toward coin supply.
func totalBalance(state map[string]Account) uint64 {
	var total uint64
	for _, acc := range state {
		total += acc.Balance
	}
	return total
}

// Supply is the coin accounting at a chain tip: how much has ever been minted,
// how much has been burned by base fees, and how much is actually held in
// accounts. Amounts are base units; the *_fmt fields are the same values
// rendered for display.
type Supply struct {
	Height      uint64 `json:"height"`
	Minted      uint64 `json:"minted"`
	Burned      uint64 `json:"burned"`
	Circulating uint64 `json:"circulating"`
	Accounts    int    `json:"accounts"`
	Subsidy     uint64 `json:"subsidy"`      // subsidy the next block will mint
	NextHalving uint64 `json:"next_halving"` // height at which the subsidy next halves
	Consistent  bool   `json:"consistent"`   // minted − burned == circulating

	MintedFmt      string `json:"minted_fmt"`
	BurnedFmt      string `json:"burned_fmt"`
	CirculatingFmt string `json:"circulating_fmt"`
	SubsidyFmt     string `json:"subsidy_fmt"`
}

// Supply returns the coin accounting for the current tip.
func (bc *Blockchain) Supply() Supply {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	height := bc.blocks[len(bc.blocks)-1].Index
	minted := CumulativeSubsidy(height)
	circulating := totalBalance(bc.state)
	next := height + 1
	s := Supply{
		Height:      height,
		Minted:      minted,
		Burned:      bc.burned,
		Circulating: circulating,
		Accounts:    len(bc.state),
		Subsidy:     BlockReward(next),
		NextHalving: (next/HalvingInterval + 1) * HalvingInterval,
		Consistent:  minted >= bc.burned && minted-bc.burned == circulating,
	}
	s.MintedFmt = FormatAmount(s.Minted)
	s.BurnedFmt = FormatAmount(s.Burned)
	s.CirculatingFmt = FormatAmount(s.Circulating)
	s.SubsidyFmt = FormatAmount(s.Subsidy)
	return s
}

// Burned is the cumulative base fee this chain has destroyed.
func (bc *Blockchain) Burned() uint64 {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.burned
}
