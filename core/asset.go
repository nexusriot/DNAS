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
