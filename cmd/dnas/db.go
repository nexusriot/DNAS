package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/nexusriot/DNAS/core"
)

// runDB implements `dnas db ...`: the operator's view of a chain store, without
// a running node.
//
//	dnas db info   [-db FILE]                  what is in the file
//	dnas db verify [-db FILE]                  replay it through full validation
//	dnas db export [-db FILE] -o FILE.json     write a portable chain file
//	dnas db import [-db FILE] -in FILE.json    load one into a fresh store
//
// Every subcommand takes -network, because the store's validity depends on it:
// a testnet chain is not a valid mainnet chain and the genesis hash says so.
func runDB(args []string) {
	if len(args) == 0 {
		fmt.Println(`usage: dnas db <cmd> [-db FILE] [-network NAME]
  info                     summarize the store (no validation)
  verify                   replay every block through full validation
  export -o FILE.json      write the chain out as a portable JSON file
  import -in FILE.json     load a portable chain file into a fresh store`)
		return
	}
	fs := flag.NewFlagSet("db", flag.ExitOnError)
	dbPath := fs.String("db", "chain.db", "blockchain append-only store file")
	network := fs.String("network", core.MainNet, "network the store belongs to")
	out := fs.String("o", "", "output file for `export`")
	in := fs.String("in", "", "input file for `import`")
	_ = fs.Parse(args[1:])

	if err := core.SetNetwork(*network); err != nil {
		log.Fatal(err)
	}

	switch args[0] {
	case "info":
		dbInfo(*dbPath)
	case "verify":
		dbVerify(*dbPath)
	case "export":
		if *out == "" {
			log.Fatal("db export: -o FILE.json is required")
		}
		dbExport(*dbPath, *out)
	case "import":
		if *in == "" {
			log.Fatal("db import: -in FILE.json is required")
		}
		dbImport(*in, *dbPath)
	default:
		fmt.Println("unknown db command:", args[0], "(info | verify | export | import)")
	}
}

func dbInfo(path string) {
	info, err := core.StoreStat(path)
	if err != nil {
		log.Fatalf("db info: %v", err)
	}
	fmt.Printf("store    %s\n", info.Path)
	fmt.Printf("network  %s\n", info.Network)
	fmt.Printf("size     %s (%d record(s))\n", humanBytes(info.Bytes), info.Records)
	if info.Empty {
		fmt.Println("state    empty (a fresh store)")
		return
	}
	fmt.Printf("height   %d\n", info.Height)
	fmt.Printf("tip      %s\n", info.Tip)
	fmt.Printf("txs      %d non-coinbase (%s per block)\n", info.TotalTxs, info.AvgTxBlock)
	if info.Truncated {
		fmt.Println("repaired a torn trailing record (a crash mid-append)")
	}
	if info.GenesisOK {
		fmt.Println("genesis  ok")
	} else {
		fmt.Printf("genesis  MISMATCH: %s\n", info.Mismatch)
		fmt.Println("         (is this store from another network? try -network)")
	}
}

func dbVerify(path string) {
	rep, err := core.VerifyStore(path)
	if err != nil {
		log.Fatalf("db verify: %v", err)
	}
	if rep.OK {
		fmt.Printf("ok: %d block(s) replayed, height %d, tip %s\n", rep.Blocks, rep.Height, rep.Tip)
		return
	}
	fmt.Printf("FAILED at block %d of %d: %s\n", rep.BadAt, rep.Blocks, rep.Problem)
	if rep.BadAt > 0 {
		fmt.Printf("the store is good up to height %d (%s)\n", rep.Height, rep.Tip)
	}
	os.Exit(1)
}

func dbExport(path, out string) {
	n, err := core.ExportStore(path, out)
	if err != nil {
		log.Fatalf("db export: %v", err)
	}
	fmt.Printf("exported %d block(s) to %s\n", n, out)
}

func dbImport(in, path string) {
	n, err := core.ImportStore(in, path)
	if err != nil {
		log.Fatalf("db import: %v", err)
	}
	fmt.Printf("imported %d block(s) from %s into %s\n", n, in, path)
}

// humanBytes renders a byte count in the largest unit that keeps it readable.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
