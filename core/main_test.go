package core

import (
	"os"
	"testing"
)

// upgradeBaseline is the schedule TestMain installed: empty by default, and
// every upgrade at height 0 under the all-upgrades profile. Tests restore THIS
// rather than an empty table (see restoreUpgrades), so one test taking control
// of the schedule does not silently switch the profile off for everything that
// runs after it.
var upgradeBaseline map[string]uint64

// TestMain holds difficulty fixed (no retargeting) for the whole core suite so
// mining-based tests stay instant. difficulty_test re-enables retargeting where it
// needs to exercise the LWMA (see withRetarget).
//
// It also installs the UPGRADE PROFILE. Consensus upgrades are normally
// unscheduled, and each test that wants one switches on exactly that one — so
// the rules were only ever exercised one at a time. That is how a rule that is
// correct alone and wrong in combination survives: `checkedaddresses` demanded
// a recipient address of every transaction, which made every asset operation
// and every name operation permanently invalid, and no test ever had those two
// upgrades on together to notice.
//
// Set DNAS_TEST_UPGRADES=all to run the suite with every upgrade active from
// height 0 (`make test-upgrades`). It is a second pass rather than the default
// because the default must keep proving the pre-activation behaviour an
// existing chain replays under.
func TestMain(m *testing.M) {
	NoRetarget = true
	if os.Getenv("DNAS_TEST_UPGRADES") == "all" {
		upgradeBaseline = map[string]uint64{}
		for _, u := range Upgrades() {
			upgradeBaseline[u] = profileActivation(u)
		}
	}
	restoreUpgrades()
	os.Exit(m.Run())
}

// profileActivation is the height the all-upgrades profile schedules an upgrade
// at: 0 for almost everything, and 1 for the two that add a field to the HEADER
// PREIMAGE.
//
// Those two cannot activate at genesis. Genesis is a constant — its hash is the
// network's identity, pinned in the consensus vectors and checked by every node
// that opens a store — and a commitment active at height 0 is inside its
// preimage, so switching one on there does not exercise the rule, it defines a
// different chain. Height 1 puts both commitments in force for every block
// anyone mines while leaving the axiom alone.
func profileActivation(name string) uint64 {
	switch name {
	case UpgradeFilterCommit, UpgradeHistoryRoot:
		return 1
	default:
		return 0
	}
}

// restoreUpgrades puts the schedule back to the profile's baseline. It is what
// a test's cleanup runs instead of ClearUpgrades: clearing is right for the
// default profile (whose baseline is empty) and wrong for the all-upgrades one,
// where it would turn the profile off for every later test in the package.
func restoreUpgrades() {
	ClearUpgrades()
	for name, height := range upgradeBaseline {
		SetUpgradeHeight(name, height)
	}
}

// withoutUpgrade switches one upgrade OFF for the duration of a test, putting
// the profile baseline back afterwards.
//
// It is for a test whose SUBJECT is the rule below an activation height —
// "dust is allowed before the upgrade", "a vault spend is refused before the
// upgrade". Such a test is not fragile under the all-upgrades profile, it is
// contradicted by it, and pinning the one upgrade it is about is better than
// skipping: the test then means the same thing under both profiles instead of
// quietly not running under one.
func withoutUpgrade(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		ClearUpgradeHeight(name)
		n := name
		t.Cleanup(func() { restoreUpgrade(n) })
	}
}

// withDefaultUpgrades pins the whole schedule to "nothing activated", for the
// golden-corpus tests: a consensus vector file is generated under one schedule,
// so comparing this build against it only means anything under that schedule.
func withDefaultUpgrades(t *testing.T) {
	t.Helper()
	ClearUpgrades()
	t.Cleanup(restoreUpgrades)
}

// restoreUpgrade puts one upgrade back to its baseline, for a test that
// scheduled just that one.
func restoreUpgrade(name string) {
	if height, ok := upgradeBaseline[name]; ok {
		SetUpgradeHeight(name, height)
		return
	}
	ClearUpgradeHeight(name)
}
