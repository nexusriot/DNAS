package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/nexusriot/DNAS/core"
)

// runSupply implements `dnas supply [-api URL]`: what a node believes about coin
// issuance. Unlike a balance query this is chain-wide — how much has ever been
// minted, how much the burned base fee has destroyed, and whether the two still
// add up to what accounts actually hold.
func runSupply(args []string) {
	fs := flag.NewFlagSet("supply", flag.ExitOnError)
	api := fs.String("api", "localhost:8080", "node HTTP API address")
	_ = fs.Parse(args)

	var s core.Supply
	if err := getJSON(ensureHTTP(*api)+"/supply", &s); err != nil {
		fmt.Println("error:", err)
		return
	}
	printSupply(os.Stdout, s)
}

// printSupply renders a supply report.
func printSupply(w io.Writer, s core.Supply) {
	fmt.Fprintf(w, "supply at height %d\n", s.Height)
	fmt.Fprintf(w, "  minted        %22s   block subsidies\n", s.MintedFmt)
	fmt.Fprintf(w, "  burned        %22s   base fee, destroyed\n", s.BurnedFmt)
	fmt.Fprintf(w, "  circulating   %22s   held by %d account(s)\n", s.CirculatingFmt, s.Accounts)
	fmt.Fprintf(w, "  next subsidy  %22s   halves at height %d\n", s.SubsidyFmt, s.NextHalving)
	if s.Consistent {
		fmt.Fprintln(w, "  conservation  ok (minted - burned == circulating)")
	} else {
		fmt.Fprintf(w, "  conservation  BROKEN: minted - burned = %s, accounts hold %s\n",
			core.FormatAmount(s.Minted-s.Burned), s.CirculatingFmt)
	}
}
