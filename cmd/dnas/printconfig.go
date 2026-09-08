package main

import (
	"fmt"
	"strings"

	"github.com/nexusriot/DNAS/core"
)

// `-printconfig`: what the node would actually run with.
//
// A node's settings arrive from two places — a JSON config file and the command
// line, with flags overriding the file — and several of them are then adjusted
// by the code: -regtest rewrites the network, a network supplies a default
// netkey, an unset -nodekey resolves to a path beside the chain, and zero values
// mean "use the default" rather than zero. So the effective configuration is not
// something you can read off either input, which makes "why is it doing that?"
// unnecessarily hard to answer.
//
// This prints the merged, resolved result and exits without touching the chain
// or the network, so it is safe to run against a live data directory.

// effectiveConfig is the resolved settings, in the order an operator reads them.
type effectiveConfig struct {
	Network     string
	Listen      string
	Advertise   string
	API         string
	Peers       []string
	DNSSeeds    []string
	NetKey      string
	MaxPeers    int
	Wallet      string
	DB          string
	NodeKey     string
	Mine        bool
	Regtest     bool
	Dandelion   bool
	AddrIndex   bool
	Faucet      bool
	ShareFactor int
	MempoolMax  int
	MinRelayFee int
	APIRate     int
	APIBurst    int
	Prune       int
	Webhooks    []string
	LogLevel    string
	LogJSON     bool
	Checkpoints []string
	Upgrades    []string
	APIAuth     bool
}

func printEffectiveConfig(c effectiveConfig) {
	row := func(k string, v any) { fmt.Printf("  %-14s %v\n", k, v) }
	list := func(v []string) string {
		if len(v) == 0 {
			return "(none)"
		}
		return strings.Join(v, ", ")
	}
	// The advertised address defaults to the listen address inside node.New, so
	// resolve it here too rather than printing an empty field.
	advertise := c.Advertise
	if advertise == "" {
		advertise = c.Listen + "  (defaulted to -listen)"
	}
	netkey := "(open / permissionless)"
	if c.NetKey != "" {
		netkey = "set (private network)"
	}
	shareFactor := fmt.Sprint(c.ShareFactor)
	if c.ShareFactor == 0 {
		shareFactor = fmt.Sprintf("%d  (default)", core.DefaultShareFactor)
	}

	fmt.Println("effective configuration:")
	row("network", c.Network)
	row("listen", c.Listen)
	row("advertise", advertise)
	row("api", c.API)
	row("peers", list(c.Peers))
	row("dnsseeds", list(c.DNSSeeds))
	row("netkey", netkey)
	row("maxpeers", c.MaxPeers)
	fmt.Println()
	row("wallet", c.Wallet)
	row("db", c.DB)
	row("nodekey", c.NodeKey)
	fmt.Println()
	row("mine", c.Mine)
	row("regtest", c.Regtest)
	row("dandelion", c.Dandelion)
	row("addrindex", c.AddrIndex)
	row("faucet", c.Faucet)
	row("sharefactor", shareFactor)
	row("mempool", c.MempoolMax)
	row("minrelayfee", fmt.Sprintf("%d /byte", c.MinRelayFee))
	if c.APIRate <= 0 {
		row("api rate", "unlimited")
	} else {
		row("api rate", fmt.Sprintf("%d/s per client, burst %d", c.APIRate, c.APIBurst))
	}
	fmt.Println()
	row("loglevel", c.LogLevel)
	row("logjson", c.LogJSON)
	row("api auth", authState(c.APIAuth))
	if c.Prune > 0 {
		row("prune", fmt.Sprintf("keep %d recent bodies", c.Prune))
	} else {
		row("prune", "off (all bodies kept)")
	}
	row("webhooks", list(c.Webhooks))
	row("checkpoints", list(c.Checkpoints))
	row("upgrades", list(c.Upgrades))
	fmt.Println()
	fmt.Println("nothing was started; drop -printconfig to run the node.")
}

// authState describes API authentication in terms of what it means rather than
// as a bare boolean, since it comes from an environment variable and not a flag.
func authState(on bool) string {
	if on {
		return "on (DNAS_API_TOKEN is set; writes need a bearer token)"
	}
	return "off (DNAS_API_TOKEN unset; the whole API is open)"
}
