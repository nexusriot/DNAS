package core

import "sort"

// What an asset actually IS, as opposed to a balance of it.
//
// An asset id is derived from (issuer, ticker, nonce) and is what balances are
// keyed by, so an account holding one shows `tok3f2a…: 500` and nothing else:
// not the ticker, not who issued it, not how much exists. The derivation is
// one-way, so the id cannot be unpacked — and there was nowhere to look it up.
// Two issuers can both mint "GOLD", which is fine (the ids differ) but makes
// the ticker alone useless as an identifier; only the id/issuer pair says which
// GOLD a balance is.
//
// So the chain keeps a registry: one entry per issuance, built as blocks connect
// and rolled back with them. It is derived state, rebuildable by walking the
// chain, and it is small — bounded by the number of issuances, not by the number
// of transfers — so unlike the address index it is always on.

// AssetInfo describes one issued asset.
type AssetInfo struct {
	ID     string `json:"id"`
	Ticker string `json:"ticker"`
	Issuer string `json:"issuer"`
	// Supply is the asset's CURRENT total: what was issued, plus everything its
	// issuer has minted since, minus everything it has burned (see asset.go). It
	// was fixed at issuance before UpgradeAssetOps existed, and on a chain where
	// that upgrade never activates it still is.
	Supply uint64 `json:"supply"`
	// Issued is the supply the original issuance created, so a holder can see how
	// far the total has moved from it.
	Issued uint64 `json:"issued"`
	Height uint64 `json:"height"` // the block that issued it
	TxHash string `json:"tx"`
}

// indexBlockAssetsLocked records any issuance in b. bc.mu held for writing.
func (bc *Blockchain) indexBlockAssetsLocked(b Block) {
	for _, tx := range b.Transactions {
		if !tx.IsIssue() {
			continue
		}
		id := AssetID(tx.From, tx.Issue.Ticker, tx.Nonce)
		if bc.assets == nil {
			bc.assets = map[string]AssetInfo{}
		}
		// An id already present would mean two issuances derived the same id, which
		// the nonce binding makes impossible on one chain. Keeping the first is the
		// same rule the transaction index follows for duplicate txids.
		if _, exists := bc.assets[id]; exists {
			continue
		}
		bc.assets[id] = AssetInfo{
			ID: id, Ticker: tx.Issue.Ticker, Issuer: tx.From,
			Supply: tx.Issue.Supply, Issued: tx.Issue.Supply,
			Height: b.Index, TxHash: tx.Hash(),
		}
	}
	// Mints and burns move the total the registry reports. Applied after the
	// issuances above so a block that issues an asset and immediately mints more
	// of it lands in the right order.
	for _, tx := range b.Transactions {
		bc.applyAssetOpToIndexLocked(tx, false)
	}
}

// applyAssetOpToIndexLocked moves an asset's recorded total by one management
// operation, or undoes it when reversing. bc.mu held for writing.
func (bc *Blockchain) applyAssetOpToIndexLocked(tx Transaction, reverse bool) {
	if !tx.IsAssetOp() {
		return
	}
	info, ok := bc.assets[tx.AssetID]
	if !ok {
		return // an operation on an asset this node has no record of
	}
	mint := tx.AssetOp.Op == AssetOpMint
	if reverse {
		mint = !mint
	}
	if mint {
		info.Supply += tx.AssetOp.Amount
	} else if info.Supply >= tx.AssetOp.Amount {
		info.Supply -= tx.AssetOp.Amount
	}
	bc.assets[tx.AssetID] = info
}

// unindexBlockAssetsLocked forgets the assets issued by a block being
// disconnected. An asset whose issuance is no longer on the chain does not
// exist: the balances went with it, and leaving the entry would advertise a
// token nothing holds. bc.mu held for writing.
func (bc *Blockchain) unindexBlockAssetsLocked(b Block) {
	// Operations first, then issuances: undoing in the opposite order to indexing
	// keeps a block that issued and then minted from leaving a stale total behind.
	for _, tx := range b.Transactions {
		bc.applyAssetOpToIndexLocked(tx, true)
	}
	for _, tx := range b.Transactions {
		if !tx.IsIssue() {
			continue
		}
		id := AssetID(tx.From, tx.Issue.Ticker, tx.Nonce)
		if info, ok := bc.assets[id]; ok && info.Height == b.Index {
			delete(bc.assets, id)
		}
	}
}

// buildAssetIndex builds the registry for a whole chain, for a Blockchain
// assembled other than by connecting blocks one at a time (a snapshot restore).
//
// On a fast-synced chain the bodies below the snapshot are absent, so their
// issuances cannot be recovered — the balances are in the snapshot's state but
// the descriptions are not. That is a gap in what the node can describe, not in
// what it can validate, and it closes as the chain is walked from a full peer.
func buildAssetIndex(blocks []Block) map[string]AssetInfo {
	idx := map[string]AssetInfo{}
	for _, b := range blocks {
		for _, tx := range b.Transactions {
			if !tx.IsIssue() {
				continue
			}
			id := AssetID(tx.From, tx.Issue.Ticker, tx.Nonce)
			if _, exists := idx[id]; exists {
				continue
			}
			idx[id] = AssetInfo{
				ID: id, Ticker: tx.Issue.Ticker, Issuer: tx.From, Supply: tx.Issue.Supply,
				Height: b.Index, TxHash: tx.Hash(),
			}
		}
	}
	return idx
}

// Asset returns what is known about one asset id.
func (bc *Blockchain) Asset(id string) (AssetInfo, bool) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	info, ok := bc.assets[id]
	return info, ok
}

// Assets lists every asset the chain has issued, oldest first (and by id within
// a block, so the order is stable rather than map-random).
func (bc *Blockchain) Assets() []AssetInfo {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	out := make([]AssetInfo, 0, len(bc.assets))
	for _, info := range bc.assets {
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Height != out[j].Height {
			return out[i].Height < out[j].Height
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// AssetsByTicker returns every asset issued under a ticker. It is a LIST, not a
// single asset, because a ticker is not an identifier: anyone may issue "GOLD",
// and a client that resolved a ticker to one asset would be choosing which
// issuer its user meant.
func (bc *Blockchain) AssetsByTicker(ticker string) []AssetInfo {
	var out []AssetInfo
	for _, info := range bc.Assets() {
		if info.Ticker == ticker {
			out = append(out, info)
		}
	}
	return out
}

// AssetHolders reports how much of an asset each account holds, largest first.
// The sum is the asset's whole supply: an asset's total is conserved, so this is
// also the check that the ledger has not lost any of it.
func (bc *Blockchain) AssetHolders(id string) []AssetHolder {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	var out []AssetHolder
	for addr, acc := range bc.state {
		if amount := acc.Assets[id]; amount > 0 {
			out = append(out, AssetHolder{Address: addr, Amount: amount})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Amount != out[j].Amount {
			return out[i].Amount > out[j].Amount
		}
		return out[i].Address < out[j].Address
	})
	return out
}

// AssetHolder is one account's balance of an asset.
type AssetHolder struct {
	Address string `json:"address"`
	Amount  uint64 `json:"amount"`
}
