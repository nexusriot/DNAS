package main

import (
	"flag"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/nexusriot/DNAS/core"
)

// runMiner is a standalone external miner that talks only to a node's HTTP API:
// it fetches a block template, searches locally for a winning nonce, and submits
// the mined block. Mining is fully decoupled from the node, so hashpower can live
// on a separate machine.
//
//	dnas miner -api URL -address ADDR [-once]
func runMiner(args []string) {
	fs := flag.NewFlagSet("miner", flag.ExitOnError)
	api := fs.String("api", "localhost:8080", "node HTTP API address")
	addr := fs.String("address", "", "address to pay block rewards to (required)")
	once := fs.Bool("once", false, "mine a single block, then exit")
	_ = fs.Parse(args)
	if *addr == "" {
		fmt.Println("miner: -address is required (where to pay rewards)")
		return
	}
	base := ensureHTTP(*api)
	fmt.Printf("mining to %s via %s\n", *addr, base)

	for {
		var tmpl core.Block
		if err := getJSON(fmt.Sprintf("%s/blocktemplate?address=%s", base, *addr), &tmpl); err != nil {
			fmt.Println("template:", err)
			time.Sleep(2 * time.Second)
			continue
		}

		mined, ok := mineTemplate(base, tmpl)
		if !ok {
			continue // the tip advanced under us; fetch a fresh template
		}
		if err := postJSON(base+"/submitblock", mined); err != nil {
			fmt.Println("submit rejected (tip likely moved):", err)
			continue
		}
		fmt.Printf("✓ mined + submitted block %d  diff %.2f  %s\n",
			mined.Index, core.TargetDifficulty(mined.Bits), mined.Hash[:12])
		if *once {
			return
		}
	}
}

// mineTemplate searches for a winning nonce for tmpl, aborting (ok=false) if the
// node's tip reaches the template's height first (someone else mined it). The tip
// is polled on a background ticker so the hash loop's abort check stays cheap.
func mineTemplate(base string, tmpl core.Block) (core.Block, bool) {
	var superseded atomic.Bool
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if h, err := currentHeight(base); err == nil && h >= tmpl.Index {
					superseded.Store(true)
					return
				}
			}
		}
	}()
	return core.Mine(tmpl, superseded.Load)
}

// currentHeight fetches the node's current chain height.
func currentHeight(base string) (uint64, error) {
	var info struct {
		Height uint64 `json:"height"`
	}
	err := getJSON(base+"/info", &info)
	return info.Height, err
}
