package main

import (
	"flag"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/nexusriot/DNAS/core"
)

// runMiner is a standalone external miner that talks only to a node's HTTP API:
// it fetches a block template, searches locally for a winning nonce, and submits
// the mined block. Mining is fully decoupled from the node, so hashpower can live
// on a separate machine.
//
//	dnas miner -api URL -address ADDR [-once] [-shares] [-longpoll=false]
//
// By default it LONG POLLS: the template request does not answer until the tip
// moves, so the miner starts on a fresh candidate the moment the old one dies
// instead of hashing a dead template until its next poll.
//
// With -shares it also submits hashes that clear the node's easier SHARE target
// but not the block target. Those are worth nothing on-chain — they are proof
// that work is being done, which is what lets a pool pay for it — and a share
// that turns out to clear the real target is submitted as the block it is.
func runMiner(args []string) {
	fs := flag.NewFlagSet("miner", flag.ExitOnError)
	api := fs.String("api", "localhost:8080", "node HTTP API address")
	addr := fs.String("address", "", "address to pay block rewards to (required)")
	once := fs.Bool("once", false, "mine a single block, then exit")
	shares := fs.Bool("shares", false, "also submit shares (hashes meeting the node's easier share target)")
	longpoll := fs.Bool("longpoll", true, "wait on the template request until the tip changes")
	_ = fs.Parse(args)
	if *addr == "" {
		fmt.Println("miner: -address is required (where to pay rewards)")
		return
	}
	base := ensureHTTP(*api)
	// A miner recomputes the candidate's merkle root, and a transaction's hash is
	// network-bound (the network id is part of its encoding) — so a miner on the
	// wrong network computes a different root and every block and share it submits
	// is rejected as malformed. Take the network from the node.
	adoptNetwork(base)
	fmt.Printf("mining to %s via %s\n", *addr, base)

	var lastTip string
	for {
		tmpl, err := fetchTemplate(base, *addr, *longpoll, lastTip)
		if err != nil {
			fmt.Println("template:", err)
			time.Sleep(2 * time.Second)
			continue
		}
		if *shares {
			fmt.Printf("mining block %d  block diff %.2f  share diff %.2f\n",
				tmpl.Index, core.TargetDifficulty(tmpl.Bits), core.TargetDifficulty(tmpl.ShareBits))
		}
		lastTip = tmpl.PrevHash

		mined, ok := mineTemplate(base, tmpl, *shares)
		if !ok {
			continue // the tip advanced under us; fetch a fresh template
		}
		if err := submitWinner(base, mined, *shares); err != nil {
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

// submitWinner sends a block the miner found. In shares mode it goes to
// /submitshare, which accepts it as a block AND records it in the pool ledger —
// a winning hash is a share too, and a pool that did not count it would
// under-credit the miner that found it. Otherwise it goes straight to
// /submitblock, as it always has.
func submitWinner(base string, b core.Block, shares bool) error {
	if !shares {
		return postJSON(base+"/submitblock", b)
	}
	var res struct {
		Accepted bool `json:"accepted"`
		Block    bool `json:"block"`
	}
	if err := postJSONInto(base+"/submitshare", b, &res); err != nil {
		return err
	}
	if !res.Block {
		return fmt.Errorf("the node accepted the hash as a share but not as a block")
	}
	return nil
}

// blockTemplate is a candidate block plus the node's share target. The block's
// fields are inlined by the API, so this decodes both in one go.
type blockTemplate struct {
	core.Block
	ShareBits   uint32 `json:"share_bits"`
	ShareFactor uint32 `json:"share_factor"`
}

// fetchTemplate asks for a candidate block. When long polling, `prevTip` is the
// tip the miner has already worked on: the node holds the request until the tip
// moves off it, so the answer is always a template worth hashing. A prevTip the
// node has already passed is answered immediately, so this can never park.
func fetchTemplate(base, addr string, longpoll bool, prevTip string) (blockTemplate, error) {
	url := fmt.Sprintf("%s/blocktemplate?address=%s", base, addr)
	var tmpl blockTemplate
	// The first request of a run has no previous tip to wait past, so it is an
	// ordinary (immediate) fetch; every one after it can hang.
	if !longpoll || prevTip == "" {
		return tmpl, getJSON(url, &tmpl)
	}
	url += fmt.Sprintf("&longpoll=1&prev=%s&timeout=%d", prevTip, int(longPollTimeout.Seconds()))
	return tmpl, getJSONVia(longPollHTTP, url, &tmpl)
}

// longPollHTTP outlives a long poll. The shared client's timeout is tuned for
// ordinary requests and would cut every long poll short.
var longPollHTTP = &http.Client{Timeout: longPollTimeout + 15*time.Second}

// longPollTimeout is how long the miner lets a template request hang before
// asking again. Well under any sensible proxy or firewall idle timeout.
const longPollTimeout = 30 * time.Second

// mineTemplate searches for a winning nonce for tmpl, aborting (ok=false) if the
// node's tip reaches the template's height first (someone else mined it). The tip
// is polled on a background ticker so the hash loop's abort check stays cheap.
//
// With shares on, every hash that clears the share target is submitted as it is
// found, so the pool sees a steady stream of proof-of-work rather than silence
// punctuated by the occasional block.
func mineTemplate(base string, tmpl blockTemplate, shares bool) (core.Block, bool) {
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
	if !shares || tmpl.ShareBits == 0 {
		return core.Mine(tmpl.Block, superseded.Load)
	}
	return mineWithShares(base, tmpl, superseded.Load)
}

// mineWithShares is the hash loop with share reporting: it walks nonces exactly
// as core.Mine does, and submits every candidate that clears the share target.
// A candidate that clears the BLOCK target is returned to the caller, which
// submits it as a block — the share submission of the same hash is skipped, so
// the node is not told about it twice.
func mineWithShares(base string, tmpl blockTemplate, abort func() bool) (core.Block, bool) {
	b := tmpl.Block
	b.MerkleRoot = core.MerkleRoot(b.Transactions)
	submitted, rejected := 0, 0
	for {
		if abort() {
			if submitted > 0 || rejected > 0 {
				fmt.Printf("  (tip moved; %d share(s) submitted, %d rejected, for block %d)\n",
					submitted, rejected, b.Index)
			}
			return b, false
		}
		b.Hash = b.ComputeHash()
		if b.HasValidPoW() {
			return b, true
		}
		if core.MeetsShareTarget(b.Hash, tmpl.ShareBits) {
			if err := postJSON(base+"/submitshare", b); err != nil {
				// A rejected share means the node and this miner disagree about
				// something structural; silence would leave a miner hashing for
				// nothing, so say it once and keep going.
				if rejected == 0 {
					fmt.Println("  share rejected:", err)
				}
				rejected++
			} else {
				submitted++
			}
		}
		b.Nonce++
	}
}

// currentHeight fetches the node's current chain height.
func currentHeight(base string) (uint64, error) {
	var info struct {
		Height uint64 `json:"height"`
	}
	err := getJSON(base+"/info", &info)
	return info.Height, err
}
