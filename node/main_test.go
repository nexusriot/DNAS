package node

import (
	"os"
	"testing"

	"github.com/nexusriot/DNAS/core"
)

// TestMain holds difficulty fixed so the nodes mined in tests produce blocks
// instantly (mainnet leaves retargeting on; regtest also disables it).
func TestMain(m *testing.M) {
	core.NoRetarget = true
	os.Exit(m.Run())
}
