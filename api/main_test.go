package api_test

import (
	"os"
	"testing"

	"github.com/nexusriot/DNAS/core"
)

// TestMain holds difficulty fixed so blocks mined in the API tests are instant.
func TestMain(m *testing.M) {
	core.NoRetarget = true
	// The all-upgrades profile, as in core (see core/main_test.go): a second pass
	// with every consensus rule in force, so they are exercised in combination
	// rather than one at a time.
	if os.Getenv("DNAS_TEST_UPGRADES") == "all" {
		for _, u := range core.Upgrades() {
			core.SetUpgradeHeight(u, upgradeProfileHeight(u))
		}
	}
	os.Exit(m.Run())
}

// upgradeProfileHeight mirrors core's profile: everything at height 0 except
// the two header commitments, which cannot activate at genesis without
// redefining its hash — and so the network's identity.
func upgradeProfileHeight(name string) uint64 {
	switch name {
	case core.UpgradeFilterCommit, core.UpgradeHistoryRoot:
		return 1
	default:
		return 0
	}
}
