package core

import "testing"

func TestParseAmountRejectsWhatItCannotRepresent(t *testing.T) {
	good := map[string]uint64{
		"1":            Coin,
		"1.5":          Coin + Coin/2,
		"0.00000001":   1,
		"2.1000000000": 2*Coin + Coin/10, // trailing zeros past 8 places are harmless
		"0.000000019":  1,                // a 9th decimal truncates (pinned by the consensus vectors)
		" 3 ":          3 * Coin,
		"184467440737": 184467440737 * Coin,
	}
	for in, want := range good {
		got, err := ParseAmount(in)
		if err != nil || got != want {
			t.Errorf("ParseAmount(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{
		"184467440738",   // whole*Coin wraps uint64: used to parse as 0.904 DNAS
		"99999999999999", // far past it
		"1.00000000junk", // junk after the eighth decimal used to be cut off unseen
		"1.000000001x",   // junk past the eighth decimal, after digits
		"1.5x",
		"-1",
		"",
		"abc",
	} {
		if got, err := ParseAmount(in); err == nil {
			t.Errorf("ParseAmount(%q) = %d, want an error", in, got)
		}
	}
}
