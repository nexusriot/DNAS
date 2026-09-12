package core

import "sync"

// Consensus upgrades let a rule change activate at a fixed block height, so the
// network adopts new rules on a coordinated flag-day instead of an uncoordinated
// hard fork. Activation heights are configuration (like checkpoints, §finality):
// set identically on every node at startup via SetUpgradeHeight; an unset upgrade
// is never active. A validation rule guards itself with IsUpgradeActive(name,
// blockHeight), so blocks below the activation height keep the old rule and blocks
// at/after it enforce the new one.
//
// Height activation is the mechanism every rule here ultimately runs on, but it
// is no longer the only way a height gets set. A rule may instead be put to a
// miner vote: a BIP9 deployment claims a bit of the block header's Version field,
// and once a window of blocks signals it the chain itself fixes the activation
// height and installs it here (see versionbits.go). Validation is unchanged
// either way — a rule asks IsUpgradeActive and never learns which route set the
// number.

// UpgradeDustLimit, once active, rejects coin transfers below DustThreshold — a
// worked example of a height-activated consensus rule.
const UpgradeDustLimit = "dustlimit"

// UpgradeMultiOutput, once active, allows multi-recipient transfers
// (Transaction.Outputs). Until then a transaction carrying outputs is rejected,
// so the whole network starts accepting them at the same height instead of some
// nodes treating a block as valid while others reject it. Because the outputs are
// encoded only when present (see codec.go), enabling the rule changes no existing
// transaction's id and no stored chain's validity.
const UpgradeMultiOutput = "multioutput"

// UpgradeVault, once active, allows spends authorized by a time-delayed vault
// script (Transaction.Vault). Until then a transaction carrying one is rejected,
// so no node accepts a block the rest of the network would refuse. Vault-
// authorized spends encode only when the script is present (see codec.go), so
// scheduling this changes no existing transaction id and no stored chain.
const UpgradeVault = "vault"

// UpgradeFeeSponsor, once active, allows a third party to pay a transaction's
// fee (Transaction.FeePayer). It is height-activated for the same reason as the
// others: the rule changes who must be able to afford the fee, so every node has
// to start applying it at the same block.
const UpgradeFeeSponsor = "feesponsor"

// UpgradeCheckedAddresses, once active, requires every address a transaction
// names — sender, recipient, each multi-output recipient, and the fee payer — to
// be a well-formed, checksummed DNAS address.
//
// Until this activates, consensus checks only that addresses are not absurdly
// long. Every client validates recipients before signing, but that is a
// convention, not a rule: a buggy or malicious client can put anything in `To`,
// and the coin lands on a state key nobody holds the private key for. Burned,
// permanently, with no way to tell it from a deliberate burn.
//
// It is height-activated like the others because it is a tightening: blocks
// below the activation height keep the old rule, so an existing chain still
// replays. What it CANNOT do is recover coin already sent to a malformed
// address — those state entries stay where they are.
const UpgradeCheckedAddresses = "checkedaddresses"

// UpgradeUniqueCoinbase, once active, requires a block's coinbase to carry the
// block's own height in its Nonce field (Bitcoin's BIP34).
//
// Without it a coinbase commits only (recipient, amount), so two blocks paying
// the same miner the same subsidy produce the SAME txid. The transaction index
// works around the duplication by resolving to the first occurrence, but the
// consequence is real: an inclusion proof for such a coinbase can only point at
// one of the blocks, and "which block paid this" has no answer.
//
// Binding the height makes every coinbase distinct, because no two blocks in a
// chain share a height. It is height-activated because it is a consensus change
// in both directions — below the activation a coinbase Nonce must be 0, at and
// above it must equal the height — so an existing chain replays unchanged.
const UpgradeUniqueCoinbase = "uniquecoinbase"

// UpgradeAssetOps, once active, allows an asset's issuer to mint more of it or
// to burn units it holds (Transaction.AssetOp).
//
// Until it activates, an asset's supply is fixed at issuance and nothing can
// change it. That is a real restriction rather than an oversight — a holder of a
// fixed-supply asset knows the issuer cannot dilute them — so turning it on is a
// change to what every holder is trusting, and it gets a flag day like the rest.
const UpgradeAssetOps = "assetops"

var (
	upgradesMu sync.RWMutex
	upgrades   = map[string]uint64{}
)

// knownUpgrades is every upgrade this build understands. An operator scheduling
// one by name is told immediately if it is misspelled, rather than running with a
// rule that silently never activates — which on a network where the others did
// activate means being forked off it.
var knownUpgrades = []string{UpgradeDustLimit, UpgradeMultiOutput, UpgradeVault,
	UpgradeFeeSponsor, UpgradeCheckedAddresses, UpgradeUniqueCoinbase, UpgradeAssetOps}

// Upgrades lists the upgrade names this build understands.
func Upgrades() []string { return append([]string(nil), knownUpgrades...) }

// KnownUpgrade reports whether name is an upgrade this build understands.
func KnownUpgrade(name string) bool {
	for _, u := range knownUpgrades {
		if u == name {
			return true
		}
	}
	return false
}

// SetUpgradeHeight schedules an upgrade to activate at the given block height.
// Call it at startup, before syncing, with the same values on every node.
func SetUpgradeHeight(name string, height uint64) {
	upgradesMu.Lock()
	defer upgradesMu.Unlock()
	upgrades[name] = height
}

// ClearUpgradeHeight unschedules an upgrade, so it is never active again until
// something reschedules it. A miner vote that a reorg has undone withdraws its
// activation height this way (see versionbits.go).
func ClearUpgradeHeight(name string) {
	upgradesMu.Lock()
	defer upgradesMu.Unlock()
	delete(upgrades, name)
}

// ClearUpgrades removes all scheduled upgrades (used by tests).
func ClearUpgrades() {
	upgradesMu.Lock()
	defer upgradesMu.Unlock()
	upgrades = map[string]uint64{}
}

// IsUpgradeActive reports whether the named upgrade is in force at blockHeight
// (i.e. blockHeight ≥ its scheduled activation height). An unscheduled upgrade is
// never active.
func IsUpgradeActive(name string, blockHeight uint64) bool {
	upgradesMu.RLock()
	defer upgradesMu.RUnlock()
	h, ok := upgrades[name]
	return ok && blockHeight >= h
}
