package api_test

import (
	"os"
	"testing"

	"github.com/nexusriot/DNAS/core"
)

// TestMain holds difficulty fixed so blocks mined in the API tests are instant.
func TestMain(m *testing.M) {
	core.NoRetarget = true
	os.Exit(m.Run())
}
