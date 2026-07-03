package core

import "testing"

func TestParseAmount(t *testing.T) {
	cases := []struct {
		in    string
		units uint64
	}{
		{"0", 0},
		{"1", Coin},
		{"1.5", Coin + Coin/2},
		{"10", 10 * Coin},
		{"0.00000001", 1},
		{"  2.25  ", 2*Coin + Coin/4},
	}
	for _, c := range cases {
		got, err := ParseAmount(c.in)
		if err != nil {
			t.Errorf("ParseAmount(%q) error: %v", c.in, err)
			continue
		}
		if got != c.units {
			t.Errorf("ParseAmount(%q) = %d, want %d", c.in, got, c.units)
		}
	}
	for _, bad := range []string{"abc", "1.x", ""} {
		if _, err := ParseAmount(bad); err == nil {
			t.Errorf("ParseAmount(%q) should have failed", bad)
		}
	}
}

func TestParseFormatRoundTrip(t *testing.T) {
	if got := FormatAmount(Coin + Coin/2); got != "1.50000000 DNAS" {
		t.Errorf("FormatAmount = %q, want %q", got, "1.50000000 DNAS")
	}
	for _, u := range []uint64{0, 1, Coin, 12345678901234} {
		s := FormatAmount(u)
		back, err := ParseAmount(s[:len(s)-len(" "+Ticker)])
		if err != nil {
			t.Fatalf("re-parse %q: %v", s, err)
		}
		if back != u {
			t.Errorf("round trip %d -> %q -> %d", u, s, back)
		}
	}
}

func TestBlockRewardHalving(t *testing.T) {
	if got := BlockReward(0); got != InitialBlockReward {
		t.Errorf("reward(0) = %d, want %d", got, InitialBlockReward)
	}
	if got := BlockReward(HalvingInterval); got != InitialBlockReward/2 {
		t.Errorf("reward at first halving = %d, want %d", got, InitialBlockReward/2)
	}
	if got := BlockReward(2 * HalvingInterval); got != InitialBlockReward/4 {
		t.Errorf("reward at second halving = %d, want %d", got, InitialBlockReward/4)
	}
	if got := BlockReward(64 * HalvingInterval); got != 0 {
		t.Errorf("reward after 64 halvings = %d, want 0", got)
	}
}
