package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
)

// `dnas doctor` — one command that runs the checks an operator would otherwise
// have to know to run by hand.
//
// Everything here is already visible somewhere: /info, /health, /metrics and
// /reorgs between them expose every number below. What they do not do is say
// which numbers MATTER, or what a bad one means. That knowledge has been
// accumulating in this project's prose — the /16-versus-ASN caveat in the
// address manager, the reason a refused reorg is the most consequential thing a
// node can do quietly, why a fast-synced node answers 410 for old filters — and
// prose is not something you can run before going to bed.
//
// So each check below is: a thing that goes wrong, how to see it, and what to do.
// The exit status is 0 when nothing is wrong, 1 when something is, so it is
// usable from cron and not only by a person reading it.

// severity orders the findings and decides the exit status.
type severity int

const (
	sevOK severity = iota
	sevNote
	sevWarn
	sevFail
)

func (s severity) label() string {
	switch s {
	case sevFail:
		return "FAIL"
	case sevWarn:
		return "WARN"
	case sevNote:
		return "NOTE"
	default:
		return "ok"
	}
}

// finding is one check's verdict. Fix is the thing to actually do about it, and
// is what separates this from another status dump.
type finding struct {
	Check string
	Sev   severity
	Msg   string
	Fix   string
}

// doctorInfo is the subset of /info the checks read.
type doctorInfo struct {
	Network      string   `json:"network"`
	Height       uint64   `json:"height"`
	Tip          string   `json:"tip"`
	Peers        []string `json:"peers"`
	Mining       bool     `json:"mining"`
	AddressIndex bool     `json:"address_index"`
	BodyHeight   uint64   `json:"body_height"`
	FilterBase   uint64   `json:"filter_base"`
	Pruned       bool     `json:"pruned"`
	PruneKeep    uint64   `json:"prune_keep"`
	Addrs        struct {
		New            int `json:"new"`
		Tried          int `json:"tried"`
		Groups         int `json:"groups"`
		LiveGroups     int `json:"live_groups"`
		MaxPerGroup    int `json:"max_outbound_per_group"`
		OutboundDialed int `json:"outbound_dialed"`
	} `json:"addrs"`
	Store struct {
		Bytes      int64  `json:"bytes"`
		BytesSaved int64  `json:"bytes_saved"`
		Error      string `json:"error"`
	} `json:"store"`
}

type doctorHealth struct {
	OK           bool     `json:"ok"`
	TipAge       string   `json:"tip_age"`
	BlocksBehind uint64   `json:"blocks_behind"`
	Reasons      []string `json:"reasons"`
}

type doctorReorgs struct {
	Total          uint64 `json:"total"`
	Deepest        int    `json:"deepest"`
	Orphans        int    `json:"orphans"`
	MaxDepth       int    `json:"max_depth"`
	Refused        uint64 `json:"refused"`
	RefusedDeepest int    `json:"refused_deepest"`
	RefusedAt      string `json:"refused_at"`
	RefusedWhy     string `json:"refused_why"`
}

type doctorPeer struct {
	Addr       string `json:"addr"`
	TimeOffset int64  `json:"time_offset"`
}

func runDoctor(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	quiet := fs.Bool("quiet", false, "print only findings, not the checks that passed")
	_ = fs.Parse(args)

	base := ensureHTTP(*apiAddr)
	var info doctorInfo
	if err := getJSON(base+"/info", &info); err != nil {
		fmt.Printf("FAIL  node unreachable at %s: %v\n", *apiAddr, err)
		fmt.Println("      fix: check the node is running and -api points at it")
		exitCode(1)
		return
	}

	var health doctorHealth
	// An unhealthy node answers 503 WITH its reasons, so the status is not an error.
	_, _ = getJSONAny(base+"/health", &health)
	var reorgs doctorReorgs
	_ = getJSON(base+"/reorgs", &reorgs)
	var peers []doctorPeer
	_ = getJSON(base+"/peers", &peers)

	findings := diagnose(info, health, reorgs, peers, os.Getenv("DNAS_API_TOKEN") != "", *apiAddr)

	worst := sevOK
	for _, f := range findings {
		if f.Sev > worst {
			worst = f.Sev
		}
		if f.Sev == sevOK && *quiet {
			continue
		}
		fmt.Printf("%-5s %-18s %s\n", f.Sev.label(), f.Check, f.Msg)
		if f.Fix != "" && f.Sev != sevOK {
			fmt.Printf("      %-18s fix: %s\n", "", f.Fix)
		}
	}
	fmt.Printf("\n%s on %s at height %d\n", summaryLine(worst), info.Network, info.Height)
	if worst >= sevWarn {
		exitCode(1)
	}
}

func summaryLine(worst severity) string {
	switch worst {
	case sevFail:
		return "PROBLEMS FOUND"
	case sevWarn:
		return "warnings"
	case sevNote:
		return "healthy, with notes"
	default:
		return "healthy"
	}
}

// diagnose is the whole checklist, separated from printing and from HTTP so it
// can be tested against constructed states rather than a live node.
func diagnose(info doctorInfo, health doctorHealth, reorgs doctorReorgs,
	peers []doctorPeer, tokenSet bool, apiAddr string) []finding {

	var out []finding
	add := func(check string, sev severity, msg, fix string) {
		out = append(out, finding{Check: check, Sev: sev, Msg: msg, Fix: fix})
	}

	// A refused reorg first, because nothing else here would notice it. A diverged
	// node has peers, a fresh tip, and by its own reckoning is not behind — and the
	// same guard refuses the same switch every time, so it will not recover on its
	// own.
	switch {
	case reorgs.Refused > 0:
		add("fork-choice", sevFail,
			fmt.Sprintf("%d reorg(s) REFUSED (deepest %d, last %s: %s) — this node may have stopped following the network's chain",
				reorgs.Refused, reorgs.RefusedDeepest, orDash(reorgs.RefusedAt), orDash(reorgs.RefusedWhy)),
			"compare the tip with another node; if they disagree, resync this one from a trusted store")
	default:
		add("fork-choice", sevOK, "no refused reorgs", "")
	}

	// Peers, and then the part a peer COUNT cannot tell you.
	switch n := len(info.Peers); {
	case n == 0:
		add("peers", sevFail, "no peers connected — this node sees no network at all",
			"check -peers/-dnsseeds and that the listen port is reachable")
	case n < 3:
		add("peers", sevWarn, fmt.Sprintf("only %d peer(s)", n),
			"a node with few peers is cheap to eclipse; add seeds or open the listen port")
	default:
		add("peers", sevOK, fmt.Sprintf("%d peers connected", n), "")
	}

	// Outbound diversity. This is the check the /16-versus-ASN caveat exists for:
	// eight connections into one operator's range is one connection as far as an
	// eclipse is concerned.
	if outbound := info.Addrs.OutboundDialed; outbound > 0 {
		groups := info.Addrs.LiveGroups
		switch {
		case groups <= 1 && outbound > 1:
			add("peer-diversity", sevWarn,
				fmt.Sprintf("%d outbound connections all in one address group", outbound),
				"one operator may hold all of them; add peers in unrelated networks")
		case groups < 3:
			add("peer-diversity", sevNote,
				fmt.Sprintf("%d outbound connections across %d address group(s)", outbound, groups),
				"grouping is by /16, not by ASN, so this bounds an eclipse rather than preventing it")
		default:
			add("peer-diversity", sevOK,
				fmt.Sprintf("%d outbound across %d groups", outbound, groups), "")
		}
	}

	// The clock. A skewed clock is not a local inconvenience: a node an hour out
	// rejects blocks the network produces, or mines on a tip its peers refuse.
	if worst := worstOffset(peers); len(peers) > 0 {
		switch {
		case abs64(worst) > 60:
			add("clock", sevWarn,
				fmt.Sprintf("peers report this machine's clock is off by up to %ds", worst),
				"fix the system clock (the node compensates for validation, but only within bounds)")
		case abs64(worst) > 5:
			add("clock", sevNote, fmt.Sprintf("clock offset up to %ds against peers", worst), "")
		default:
			add("clock", sevOK, "clock agrees with peers", "")
		}
	}

	// Liveness, deferred to /health so the two can never disagree.
	if health.OK {
		add("liveness", sevOK, fmt.Sprintf("ready; tip is %s old", orDash(health.TipAge)), "")
	} else {
		add("liveness", sevWarn,
			fmt.Sprintf("not ready: %s", strings.Join(health.Reasons, "; ")),
			"see `dnas health` for the same detail with an exit status")
	}
	if health.BlocksBehind > 0 {
		add("sync", sevWarn, fmt.Sprintf("%d block(s) behind the best height a peer has advertised", health.BlocksBehind),
			"give it time; if it does not close, check the fork-choice finding above")
	}

	// What this node can serve. A fast-synced or pruned node answers 410 below its
	// body height, which looks to a light client exactly like a broken node unless
	// someone knows to expect it.
	if info.FilterBase > info.BodyHeight+1 {
		add("light-clients", sevNote,
			fmt.Sprintf("filters start at height %d but bodies at %d — this node cannot serve filters below that",
				info.FilterBase, info.BodyHeight),
			"expected on a fast-synced node; a light client should use a peer with the full history")
	}
	if info.Pruned {
		add("pruning", sevNote,
			fmt.Sprintf("pruned: keeping %d block bodies, %d bytes on disk (%d reclaimed)",
				info.PruneKeep, info.Store.Bytes, info.Store.BytesSaved),
			"a pruned store is not fully re-verifiable; `dnas db verify` says which heights it could check")
	}

	// Orphans waiting for a parent are normal in ones and a symptom in dozens.
	if reorgs.Orphans > 8 {
		add("orphans", sevWarn, fmt.Sprintf("%d blocks parked waiting for a parent", reorgs.Orphans),
			"this node is missing an ancestor its peers have; check connectivity and let it resync")
	}

	// The API. An open write API on a non-loopback address is the one finding here
	// that is somebody else's problem to exploit.
	if !tokenSet && !isLoopback(apiAddr) {
		add("api-auth", sevFail,
			fmt.Sprintf("the API is bound to %s with no token: anyone who can reach it can spend this node's wallet", apiAddr),
			"set DNAS_API_TOKEN, or bind the API to 127.0.0.1")
	} else if !tokenSet {
		add("api-auth", sevNote, "no API token set (loopback only)", "")
	} else {
		add("api-auth", sevOK, "write endpoints require a token", "")
	}

	if info.Network == "mainnet" {
		add("network", sevNote, "running on mainnet — DNAS is a learning project, not money",
			"do not point it at the internet")
	}
	return out
}

// worstOffset is the largest clock disagreement any peer reports, signed, so the
// direction is visible rather than only the magnitude.
func worstOffset(peers []doctorPeer) int64 {
	var worst int64
	for _, p := range peers {
		if abs64(p.TimeOffset) > abs64(worst) {
			worst = p.TimeOffset
		}
	}
	return worst
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// isLoopback reports whether an API address is only reachable from this machine.
//
// It splits with net.SplitHostPort rather than on the first colon, because an
// IPv6 address is full of colons: "[::1]:8080" cut at the first one yields "[",
// which matches nothing and would report the most loopback address there is as
// world-reachable — turning the api-auth check into a false alarm exactly where
// it should be silent.
func isLoopback(addr string) bool {
	host := strings.TrimPrefix(strings.TrimPrefix(addr, "http://"), "https://")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	// A bare host with no port and no parseable IP: only an explicit localhost
	// counts, since anything else may resolve anywhere.
	return false
}
