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

var (
	upgradesMu sync.RWMutex
	upgrades   = map[string]uint64{}
)

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
