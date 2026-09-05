package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/nexusriot/DNAS/core"
)

// `dnas peers` and `dnas stats`: the operator's view of a running node.
//
// Both exist because the information was already there and unreachable. A node
// knew the version, identity, direction, age and ban score of every connection
// and reported a list of address strings; it knew every ban score and exposed
// none of them; and it reported its difficulty without ever saying what that
// implied about hashrate or block timing.

// runPeers implements `dnas peers [-api URL] [subcommand]`:
//
//	dnas peers                        list connections in detail
//	dnas peers bans                   show scored and banned keys
//	dnas peers unban <key>            clear a ban score
//	dnas peers add <host:port>        dial a peer now
//	dnas peers drop <addr|identity>   close a connection
func runPeers(args []string) {
	// These subcommands take a positional argument (`unban <key>`, `add <addr>`),
	// and Go's flag package stops parsing at the first positional — so a `-api`
	// written after it would be silently ignored and the command would hit the
	// DEFAULT node. Sending an `unban` to the wrong node is not a mistake worth
	// allowing quietly, so -api is pulled out from wherever it appears.
	apiAddr, rest := extractFlag(args, "api", "localhost:8080")
	base := ensureHTTP(apiAddr)

	cmd := "list"
	if len(rest) > 0 {
		cmd = rest[0]
	}
	switch cmd {
	case "list":
		peersList(base)
	case "bans":
		bansList(base)
	case "unban":
		if len(rest) < 2 {
			fmt.Println("usage: dnas peers unban <identity-or-ip>")
			return
		}
		postAndReport(base+"/unban", map[string]string{"key": rest[1]},
			fmt.Sprintf("cleared the ban score for %s", rest[1]))
	case "add":
		if len(rest) < 2 {
			fmt.Println("usage: dnas peers add <host:port>")
			return
		}
		postAndReport(base+"/addpeer", map[string]string{"addr": rest[1]},
			fmt.Sprintf("dialing %s", rest[1]))
	case "drop":
		if len(rest) < 2 {
			fmt.Println("usage: dnas peers drop <address-or-identity>")
			return
		}
		postAndReport(base+"/droppeer", map[string]string{"peer": rest[1]},
			fmt.Sprintf("closed the connection to %s (an outbound peer will be redialed)", rest[1]))
	default:
		fmt.Println("unknown peers command:", cmd, "(list | bans | unban | add | drop)")
	}
}

// peerRow mirrors one entry of GET /peers.
type peerRow struct {
	Addr      string   `json:"addr"`
	IP        string   `json:"ip"`
	Identity  string   `json:"identity"`
	Version   int      `json:"version"`
	Caps      []string `json:"caps"`
	Inbound   bool     `json:"inbound"`
	BanScore  int      `json:"ban_score"`
	Connected string   `json:"connected"`
	Syncing   bool     `json:"syncing"`
}

func peersList(base string) {
	var peers []peerRow
	if err := getJSON(base+"/peers", &peers); err != nil {
		log.Fatalf("peers: %v", err)
	}
	if len(peers) == 0 {
		fmt.Println("no peers connected")
		return
	}
	fmt.Printf("%-22s %-12s %-4s %-4s %-8s %-6s %-5s %s\n",
		"ADDRESS", "IDENTITY", "DIR", "VER", "UP", "SCORE", "SYNC", "CAPS")
	for _, p := range peers {
		dir := "out"
		if p.Inbound {
			dir = "in"
		}
		addr := p.Addr
		if addr == "" {
			addr = p.IP + " (no hello yet)"
		}
		sync := ""
		if p.Syncing {
			sync = "yes"
		}
		fmt.Printf("%-22s %-12s %-4s %-4d %-8s %-6d %-5s %s\n",
			addr, short(p.Identity), dir, p.Version, p.Connected, p.BanScore, sync,
			strings.Join(p.Caps, ","))
	}
}

func bansList(base string) {
	var res struct {
		Threshold int `json:"threshold"`
		Entries   []struct {
			Key    string `json:"key"`
			Score  int    `json:"score"`
			Banned bool   `json:"banned"`
		} `json:"entries"`
	}
	if err := getJSON(base+"/bans", &res); err != nil {
		log.Fatalf("bans: %v", err)
	}
	if len(res.Entries) == 0 {
		fmt.Println("no ban scores recorded")
		return
	}
	fmt.Printf("ban threshold: %d points\n\n", res.Threshold)
	fmt.Printf("%-8s %-7s %s\n", "SCORE", "STATE", "KEY (identity or IP)")
	for _, e := range res.Entries {
		state := "scored"
		if e.Banned {
			state = "BANNED"
		}
		fmt.Printf("%-8d %-7s %s\n", e.Score, state, e.Key)
	}
	fmt.Println("\nclear one with: dnas peers unban <key>")
}

// postAndReport POSTs a body and prints either the node's error or a success line.
func postAndReport(url string, body any, success string) {
	if err := postJSON(url, body); err != nil {
		log.Fatal(err)
	}
	fmt.Println(success)
}

// runStats implements `dnas stats [-api URL] [-window N]`: what the chain's
// numbers imply — estimated hashrate, block timing against the target, fee flow,
// and who has been mining.
func runStats(args []string) {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	window := fs.Int("window", 0, "blocks to cover (default 144)")
	_ = fs.Parse(args)

	url := ensureHTTP(*apiAddr) + "/chainstats"
	if *window > 0 {
		url += fmt.Sprintf("?window=%d", *window)
	}
	var st core.ChainStats
	if err := getJSON(url, &st); err != nil {
		log.Fatalf("stats: %v", err)
	}

	fmt.Printf("chain stats over %d block(s), heights %d-%d (%ds)\n\n",
		st.Window, st.FromHeight, st.ToHeight, st.Seconds)
	fmt.Printf("  hashrate     %s (estimated from work over time)\n", orDash(st.HashrateFmt))
	fmt.Printf("  intervals    min %ds  median %ds  mean %ds  max %ds   (target %ds)\n",
		st.MinInterval, st.MedianInterval, st.MeanInterval, st.MaxInterval, st.TargetInterval)
	fmt.Printf("  difficulty   %.2f - %.2f\n", st.MinDifficulty, st.MaxDifficulty)
	fmt.Println()
	fmt.Printf("  txs          %d in %d bytes (fullest block %d%% of the limit)\n", st.Txs, st.Bytes, st.FullestPct)
	fmt.Printf("  fees         %s total: %s burned, %s to miners\n", st.FeesFmt, st.BurnedFmt, st.TipsFmt)
	if st.MeanFeeRate > 0 {
		fmt.Printf("  fee rate     %d base units per byte (mean)\n", st.MeanFeeRate)
	}
	if len(st.Miners) > 0 {
		fmt.Println("\n  miners in this window:")
		for _, m := range st.Miners {
			fmt.Printf("    %-52s %3d block(s)  %d%%\n", m.Address, m.Blocks, m.Percent)
		}
	}
}

// orDash renders an empty measurement as a dash rather than blank, so a reader
// can tell "not measurable" from "missing".
func orDash(s string) string {
	if s == "" {
		return "— (no elapsed time in the window)"
	}
	return s
}

// runReorgs implements `dnas reorgs [-api URL]`: the chain switches this node
// has lived through, which the event stream announces once and then forgets.
func runReorgs(args []string) {
	fs := flag.NewFlagSet("reorgs", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	_ = fs.Parse(args)

	var rep struct {
		Total    uint64 `json:"total"`
		Deepest  int    `json:"deepest"`
		Kept     int    `json:"kept"`
		Capacity int    `json:"capacity"`
		Orphans  int    `json:"orphans"`
		MaxDepth int    `json:"max_depth"`
		Reorgs   []struct {
			At         string `json:"at"`
			Height     uint64 `json:"height"`
			ForkHeight uint64 `json:"fork_height"`
			Depth      int    `json:"depth"`
			Adopted    int    `json:"adopted"`
			OldTip     string `json:"old_tip"`
			NewTip     string `json:"new_tip"`
			Requeued   int    `json:"requeued"`
			DroppedTxs int    `json:"dropped_txs"`
		} `json:"reorgs"`
	}
	if err := getJSON(ensureHTTP(*apiAddr)+"/reorgs", &rep); err != nil {
		log.Fatalf("reorgs: %v", err)
	}
	fmt.Printf("%d reorg(s) since this node started; deepest %d block(s) (the limit is %d)\n",
		rep.Total, rep.Deepest, rep.MaxDepth)
	fmt.Printf("%d orphan block(s) currently parked awaiting a parent\n", rep.Orphans)
	if len(rep.Reorgs) == 0 {
		fmt.Println("\nno reorgs recorded")
		return
	}
	fmt.Printf("\nshowing %d of them (a ring of %d is kept), newest first:\n\n", rep.Kept, rep.Capacity)
	for _, r := range rep.Reorgs {
		fmt.Printf("  %s  forked at %d, discarded %d, adopted %d -> height %d\n",
			r.At, r.ForkHeight, r.Depth, r.Adopted, r.Height)
		fmt.Printf("      %s -> %s\n", short(r.OldTip), short(r.NewTip))
		if r.Requeued > 0 || r.DroppedTxs > 0 {
			fmt.Printf("      %d transaction(s) re-queued, %d dropped\n", r.Requeued, r.DroppedTxs)
		}
	}
}

// runHealth implements `dnas health [-api URL]`, and exits non-zero when the
// node is not ready — so it works as a supervisor or CI check, not just a
// readout. /info always answers 200, which is why it cannot serve this purpose.
func runHealth(args []string) {
	fs := flag.NewFlagSet("health", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	_ = fs.Parse(args)

	var res struct {
		OK           bool     `json:"ok"`
		Network      string   `json:"network"`
		Height       uint64   `json:"height"`
		TipAge       string   `json:"tip_age"`
		Peers        int      `json:"peers"`
		BlocksBehind uint64   `json:"blocks_behind"`
		Mempool      int      `json:"mempool"`
		Reasons      []string `json:"reasons"`
	}
	// An unhealthy node answers 503 WITH the reasons, so the body is decoded
	// whatever the status and only a genuine transport/parse failure is fatal.
	if _, err := getJSONAny(ensureHTTP(*apiAddr)+"/health", &res); err != nil {
		fmt.Println("unreachable:", err)
		exitCode(1)
		return
	}
	state := "READY"
	if !res.OK {
		state = "NOT READY"
	}
	fmt.Printf("%s  network=%s height=%d tip-age=%s peers=%d behind=%d mempool=%d\n",
		state, res.Network, res.Height, res.TipAge, res.Peers, res.BlocksBehind, res.Mempool)
	for _, r := range res.Reasons {
		fmt.Printf("  - %s\n", r)
	}
	if !res.OK {
		exitCode(1)
	}
}

// exitCode ends the process with a status, so `dnas health` is usable in a
// supervisor check or a CI step rather than only readable by a person.
func exitCode(code int) { os.Exit(code) }

// extractFlag pulls `-name value`, `--name value`, `-name=value` or
// `--name=value` out of args from any position, returning its value (or def)
// and the arguments that remain. It exists for the subcommands that mix flags
// and positionals, where the standard parser would ignore a trailing flag.
func extractFlag(args []string, name, def string) (string, []string) {
	value := def
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-"+name || a == "--"+name:
			if i+1 < len(args) {
				value = args[i+1]
				i++ // consume the value
			}
		case strings.HasPrefix(a, "-"+name+"="):
			value = strings.TrimPrefix(a, "-"+name+"=")
		case strings.HasPrefix(a, "--"+name+"="):
			value = strings.TrimPrefix(a, "--"+name+"=")
		default:
			rest = append(rest, a)
		}
	}
	return value, rest
}
