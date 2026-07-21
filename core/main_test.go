package core

import (
	"os"
	"testing"
)

// TestMain holds difficulty fixed (no retargeting) for the whole core suite so
// mining-based tests stay instant. difficulty_test re-enables retargeting where it
// needs to exercise the LWMA (see withRetarget).
func TestMain(m *testing.M) {
	NoRetarget = true
	os.Exit(m.Run())
}
