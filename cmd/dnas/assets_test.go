package main

import (
	"testing"

	"github.com/nexusriot/DNAS/core"
)

// A ticker is not an identifier — anyone may issue "GOLD" — and that is the one
// thing about an asset list that can mislead, so the command says it out loud.
func TestDuplicateTickersAreReported(t *testing.T) {
	list := []core.AssetInfo{
		{ID: "tok1", Ticker: "GOLD", Issuer: "dnasa"},
		{ID: "tok2", Ticker: "GOLD", Issuer: "dnasb"},
		{ID: "tok3", Ticker: "SILVER", Issuer: "dnasa"},
		{ID: "tok4", Ticker: "COPPER", Issuer: "dnasa"},
		{ID: "tok5", Ticker: "COPPER", Issuer: "dnasc"},
	}
	got := duplicateTickers(list)
	if len(got) != 2 || got[0] != "COPPER" || got[1] != "GOLD" {
		t.Fatalf("duplicateTickers = %v, want [COPPER GOLD]", got)
	}
	// The same issuer minting the same ticker twice is NOT the ambiguity this
	// warns about: both are theirs, and the ids still differ.
	same := []core.AssetInfo{
		{ID: "tok1", Ticker: "GOLD", Issuer: "dnasa"},
		{ID: "tok2", Ticker: "GOLD", Issuer: "dnasa"},
	}
	if got := duplicateTickers(same); len(got) != 0 {
		t.Fatalf("one issuer's two GOLDs were reported as ambiguous: %v", got)
	}
	if got := duplicateTickers(nil); len(got) != 0 {
		t.Fatalf("an empty list reported %v", got)
	}
}
