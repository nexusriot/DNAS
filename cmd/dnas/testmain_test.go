package main

import (
	"os"
	"testing"

	"github.com/nexusriot/DNAS/core"
)

// TestMain holds difficulty fixed so blocks mined by the CLI tests are instant.
func TestMain(m *testing.M) {
	core.NoRetarget = true
	os.Exit(m.Run())
}
