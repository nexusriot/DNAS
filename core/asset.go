package core

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// DNAS supports lightweight native assets ("tokens") alongside the base coin. An
// account can hold a balance of any number of assets in addition to its coin
// balance; assets are transferred like coin (fees are always paid in coin) and
// their balances are committed in the state root, so a light client can prove an
// asset balance exactly as it proves a coin balance.

// MaxTickerLen bounds an asset's human ticker. Tickers are validated to a small
// safe charset so they can't inject the '|' delimiter used in signing bytes.
const MaxTickerLen = 8

// MaxAssetSupply caps an asset's issued supply. Kept well under 2^63 so that
// asset arithmetic (which converts through int64 for signed deltas) can never
// overflow, since an asset's total is conserved across all accounts.
const MaxAssetSupply uint64 = 1 << 62

// AssetIssue mints a new asset. It is carried on an otherwise-ordinary
// transaction: the issuer (From) pays the coin fee and is credited Supply units
// of a brand-new asset whose id is derived from (issuer, ticker, nonce), so the
// same issuer can mint distinct assets and ids never collide.
type AssetIssue struct {
	Ticker string `json:"ticker"`
	Supply uint64 `json:"supply"`
}

// AssetID deterministically derives a new asset's id from its issuer, ticker and
// the issuing transaction's nonce. Binding the nonce makes every issuance unique.
func AssetID(issuer, ticker string, nonce uint64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", issuer, ticker, nonce)))
	return "tok" + hex.EncodeToString(h[:16])
}

// validTicker reports whether a ticker is a short alphanumeric string (so it is
// human-readable and cannot contain the '|' signing-bytes delimiter).
func validTicker(t string) error {
	if len(t) == 0 || len(t) > MaxTickerLen {
		return fmt.Errorf("ticker must be 1..%d characters", MaxTickerLen)
	}
	for _, r := range t {
		ok := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !ok {
			return errors.New("ticker must be alphanumeric")
		}
	}
	return nil
}

// withAssetDelta returns a copy of the account with delta applied to asset id,
// copying the Assets map (copy-on-write) so shared references and undo snapshots
// are never mutated in place. A resulting zero balance drops the entry. The
// caller must ensure a debit does not underflow.
func (a Account) withAssetDelta(id string, delta int64) Account {
	m := make(map[string]uint64, len(a.Assets)+1)
	for k, v := range a.Assets {
		m[k] = v
	}
	m[id] = uint64(int64(m[id]) + delta)
	if m[id] == 0 {
		delete(m, id)
	}
	if len(m) == 0 {
		m = nil
	}
	a.Assets = m
	return a
}

// Asset management operations.
//
// Issuance fixes a supply and then the issuer has no further say: the units
// exist, and that is the whole lifecycle. Real tokens are rarely like that —
// most want to expand supply later, and all of them want a way to destroy units
// that should no longer exist (a redemption, a bridge withdrawal, a mistake).
//
// The interesting part is AUTHORITY. "Only the issuer may mint" needs consensus
// to know who issued an asset, and the obvious way to arrange that is a registry
// lookup. But the asset registry (assetindex.go) is DERIVED state: it is rebuilt
// by walking the chain and is not committed in any header, so a validation rule
// that read it would be a consensus rule depending on something no block commits.
//
// The id is the answer instead. An asset id is sha256(issuer | ticker | nonce),
// so a transaction that names its ticker and its issuing nonce PROVES the sender
// is the issuer by reproducing the id — no lookup, nothing to trust, and it
// cannot be forged without a preimage attack on sha256. The authority check is
// therefore one hash, computed from data the transaction itself carries.

// Asset operations.
const (
	AssetOpMint = "mint"
	AssetOpBurn = "burn"
)

// AssetOp is a management operation on an existing asset. It travels on a
// transaction whose AssetID names the asset; Ticker and IssueNonce reproduce the
// id from the sender, which is what proves the sender issued it.
type AssetOp struct {
	Op     string `json:"op"`
	Amount uint64 `json:"amount"`
	// Ticker and IssueNonce are the issuance preimage: AssetID(From, Ticker,
	// IssueNonce) must equal the transaction's AssetID.
	Ticker     string `json:"ticker"`
	IssueNonce uint64 `json:"issue_nonce"`
}

// KnownAssetOp reports whether op is an operation this build understands.
func KnownAssetOp(op string) bool {
	return op == AssetOpMint || op == AssetOpBurn
}

// Validate checks an operation's shape, independent of any chain state.
func (o *AssetOp) Validate() error {
	if o == nil {
		return errors.New("no asset operation")
	}
	if !KnownAssetOp(o.Op) {
		return fmt.Errorf("unknown asset operation %q (mint | burn)", o.Op)
	}
	if o.Amount == 0 || o.Amount > MaxAssetSupply {
		return fmt.Errorf("asset operation amount must be in 1..%d", MaxAssetSupply)
	}
	return validTicker(o.Ticker)
}

// AuthorizedBy reports whether `issuer` is the account that issued `assetID`,
// by reproducing the id from the operation's stated preimage.
func (o *AssetOp) AuthorizedBy(issuer, assetID string) bool {
	if o == nil {
		return false
	}
	return AssetID(issuer, o.Ticker, o.IssueNonce) == assetID
}

// IsAssetOp reports whether this transaction manages an existing asset.
func (t Transaction) IsAssetOp() bool { return t.AssetOp != nil }
