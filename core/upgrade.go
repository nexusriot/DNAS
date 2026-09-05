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
// This is height (flag-day) activation. Miner version-bit *signaling* (BIP9)
// would additionally require a version field in the block header and is left as
// future work; height activation is the mechanism DNAS uses.

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

var (
	upgradesMu sync.RWMutex
	upgrades   = map[string]uint64{}
)

// knownUpgrades is every upgrade this build understands. An operator scheduling
// one by name is told immediately if it is misspelled, rather than running with a
// rule that silently never activates — which on a network where the others did
// activate means being forked off it.
var knownUpgrades = []string{UpgradeDustLimit, UpgradeMultiOutput, UpgradeVault, UpgradeFeeSponsor}

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
