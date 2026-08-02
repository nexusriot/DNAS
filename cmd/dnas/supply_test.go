package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/core"
)

func TestPrintSupply(t *testing.T) {
	var buf bytes.Buffer
	printSupply(&buf, core.Supply{
		Height:         7,
		Minted:         350 * core.Coin,
		Burned:         core.Coin / 2,
		Circulating:    349*core.Coin + core.Coin/2,
		Accounts:       3,
		Subsidy:        50 * core.Coin,
		NextHalving:    core.HalvingInterval,
		Consistent:     true,
		MintedFmt:      core.FormatAmount(350 * core.Coin),
		BurnedFmt:      core.FormatAmount(core.Coin / 2),
		CirculatingFmt: core.FormatAmount(349*core.Coin + core.Coin/2),
		SubsidyFmt:     core.FormatAmount(50 * core.Coin),
	})
	out := buf.String()
	for _, want := range []string{"supply at height 7", "minted", "burned", "circulating", "held by 3 account(s)", "conservation  ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// A broken conservation identity must be reported loudly, not formatted away.
func TestPrintSupplyFlagsInconsistency(t *testing.T) {
	var buf bytes.Buffer
	printSupply(&buf, core.Supply{
		Height:         2,
		Minted:         100 * core.Coin,
		Burned:         0,
		Circulating:    99 * core.Coin,
		Consistent:     false,
		MintedFmt:      core.FormatAmount(100 * core.Coin),
		BurnedFmt:      core.FormatAmount(0),
		CirculatingFmt: core.FormatAmount(99 * core.Coin),
		SubsidyFmt:     core.FormatAmount(50 * core.Coin),
	})
	if !strings.Contains(buf.String(), "BROKEN") {
		t.Errorf("inconsistent supply not flagged:\n%s", buf.String())
	}
}
