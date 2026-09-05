package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/node"
	"github.com/nexusriot/DNAS/wallet"
)

// The operator's console.
//
// A node started in a terminal drops into this prompt, and it is the only way to
// look at a node with no HTTP API reachable — which is exactly the situation
// where somebody most wants to look. It had six commands (send, balance,
// address, info, peers, mempool) while the node grew a couple of dozen surfaces
// around it: assets, supply, health, reorgs, hashrate, webhooks, pruning,
// regtest generation, a mempool with contents rather than a count.
//
// So this is the same set the API serves, read directly from the node in
// process — no HTTP, no token, no listener. Where the CLI's own commands take
// flags, the console takes key=value words, which is the smallest thing that
// stays readable when a send needs a memo and a deadline:
//
//	send dnasabc… 2.5 fee=0.001 expiry=+20 memo="two coffees"
//
// The commands are a table rather than a switch so `help` cannot drift from what
// actually exists — the help text IS the table.

// replCmd is one console command.
type replCmd struct {
	name  string
	usage string
	help  string
	run   func(n *node.Node, out io.Writer, args []string)
}

// replCommands is the console's whole vocabulary.
var replCommands = []replCmd{
	{"send", "send <to> <amount> [fee=A] [expiry=N|+N] [lock=N|+N] [memo=TEXT]",
		"pay from this node's wallet", replSend},
	{"balance", "balance [address]", "coin, assets and nonce of an account", replBalance},
	{"address", "address", "this node's wallet address", replAddress},
	{"info", "info", "height, difficulty, tip, mempool, peers", replInfo},
	{"peers", "peers", "connected peers in detail", replPeers},
	{"mempool", "mempool [list]", "pending transactions", replMempool},
	{"tx", "tx <hash>", "look one transaction up, confirmed or pending", replTx},
	{"assets", "assets [id]", "issued native assets, or one in detail", replAssets},
	{"supply", "supply", "minted, burned and circulating coin", replSupply},
	{"stats", "stats [window]", "hashrate and block intervals", replStats},
	{"health", "health", "whether this node is usable, and why not", replHealth},
	{"reorgs", "reorgs", "the reorganizations this node has lived through", replReorgs},
	{"prune", "prune", "what block bodies this node still holds", replPrune},
	{"webhooks", "webhooks", "webhook delivery counters", replWebhooks},
	{"mine", "mine [on|off]", "show or toggle mining", replMine},
	{"generate", "generate [n]", "mine n blocks immediately (regtest only)", replGenerate},
}

func repl(n *node.Node) { replLoop(n, os.Stdin, os.Stdout) }

// replLoop is the console over explicit streams, so it can be driven by a test
// rather than by a terminal.
func replLoop(n *node.Node, in io.Reader, out io.Writer) {
	fmt.Fprintln(out, "type `help` for the commands, `quit` to leave (the node keeps running until you stop it)")
	sc := bufio.NewScanner(in)
	for {
		fmt.Fprint(out, "dnas> ")
		if !sc.Scan() {
			return
		}
		fields := splitConsoleLine(sc.Text())
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "help", "?":
			replHelp(out)
			continue
		case "quit", "exit":
			return
		}
		cmd, ok := lookupReplCmd(fields[0])
		if !ok {
			fmt.Fprintf(out, "unknown command: %s (try `help`)\n", fields[0])
			continue
		}
		cmd.run(n, out, fields[1:])
	}
}

func lookupReplCmd(name string) (replCmd, bool) {
	for _, c := range replCommands {
		if c.name == name {
			return c, true
		}
	}
	return replCmd{}, false
}

func replHelp(out io.Writer) {
	for _, c := range replCommands {
		fmt.Fprintf(out, "  %-58s %s\n", c.usage, c.help)
	}
	fmt.Fprintln(out, "  help | quit")
}

// splitConsoleLine splits a line into words, keeping a quoted run together so a
// memo can contain spaces. Without this, `memo="two coffees"` would arrive as
// two words and the memo would silently lose everything after the first.
func splitConsoleLine(line string) []string {
	var (
		fields  []string
		current strings.Builder
		inQuote rune
	)
	flush := func() {
		if current.Len() > 0 {
			fields = append(fields, current.String())
			current.Reset()
		}
	}
	for _, r := range line {
		switch {
		case inQuote != 0:
			if r == inQuote {
				inQuote = 0
				continue
			}
			current.WriteRune(r)
		case r == '"' || r == '\'':
			inQuote = r
		case r == ' ' || r == '\t':
			flush()
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return fields
}

// consoleOpts are the key=value words on a command line.
type consoleOpts map[string]string

// parseConsoleOpts splits arguments into positional words and key=value options.
func parseConsoleOpts(args []string) ([]string, consoleOpts) {
	var positional []string
	opts := consoleOpts{}
	for _, a := range args {
		if key, value, ok := strings.Cut(a, "="); ok && key != "" {
			opts[key] = value
			continue
		}
		positional = append(positional, a)
	}
	return positional, opts
}

// height resolves a height option, accepting an absolute value or a "+N" offset
// from the tip — which is what an operator means by "expires in twenty blocks".
func (o consoleOpts) height(key string, tip uint64) (uint64, error) {
	raw, ok := o[key]
	if !ok || raw == "" {
		return 0, nil
	}
	if strings.HasPrefix(raw, "+") {
		delta, err := strconv.ParseUint(raw[1:], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("bad %s=%s", key, raw)
		}
		return tip + delta, nil
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad %s=%s", key, raw)
	}
	return value, nil
}

// amount resolves a coin-amount option.
func (o consoleOpts) amount(key string) (uint64, error) {
	raw, ok := o[key]
	if !ok || raw == "" {
		return 0, nil
	}
	return core.ParseAmount(raw)
}

func replSend(n *node.Node, out io.Writer, args []string) {
	pos, opts := parseConsoleOpts(args)
	if len(pos) < 2 {
		fmt.Fprintln(out, "usage: send <to> <amount> [fee=A] [expiry=N|+N] [lock=N|+N] [memo=TEXT]")
		return
	}
	if err := wallet.ValidateAddress(pos[0]); err != nil {
		fmt.Fprintln(out, "invalid recipient:", err)
		return
	}
	amount, err := core.ParseAmount(pos[1])
	if err != nil {
		fmt.Fprintln(out, err)
		return
	}
	// The third positional stays supported: `send addr 1 0.001` is what this
	// console has always accepted, and breaking it would be a gratuitous change
	// to the one interface people have muscle memory for.
	fee, err := opts.amount("fee")
	if err != nil {
		fmt.Fprintln(out, "bad fee:", err)
		return
	}
	if fee == 0 && len(pos) > 2 {
		if fee, err = core.ParseAmount(pos[2]); err != nil {
			fmt.Fprintln(out, "bad fee:", err)
			return
		}
	}
	tip := n.Chain().Height()
	expiry, err := opts.height("expiry", tip)
	if err != nil {
		fmt.Fprintln(out, err)
		return
	}
	if expiry == 0 && len(pos) > 3 { // the old fourth positional
		if expiry, err = strconv.ParseUint(pos[3], 10, 64); err != nil {
			fmt.Fprintln(out, "bad expiry height:", err)
			return
		}
	}
	lock, err := opts.height("lock", tip)
	if err != nil {
		fmt.Fprintln(out, err)
		return
	}

	w := n.Wallet()
	tx := core.Transaction{
		From: w.Address(), To: pos[0], Amount: amount, Fee: fee,
		Nonce: n.NextNonce(w.Address()), Expiry: expiry, LockUntil: lock, Memo: opts["memo"],
	}
	if err := tx.Sign(w); err != nil {
		fmt.Fprintln(out, err)
		return
	}
	if err := n.SubmitTx(tx); err != nil {
		fmt.Fprintln(out, "rejected:", err)
		return
	}
	fmt.Fprintf(out, "submitted %s  %s to %s (fee %s, nonce %d)\n",
		short(tx.Hash()), core.FormatAmount(amount), short(pos[0]), core.FormatAmount(fee), tx.Nonce)
	if lock != 0 || expiry != 0 {
		fmt.Fprintf(out, "  valid in heights %s\n", describeWindow(lock, expiry))
	}
}

// describeWindow renders a transaction's height window.
func describeWindow(lock, expiry uint64) string {
	switch {
	case lock != 0 && expiry != 0:
		return fmt.Sprintf("%d..%d", lock, expiry)
	case lock != 0:
		return fmt.Sprintf("%d and above", lock)
	case expiry != 0:
		return fmt.Sprintf("up to %d", expiry)
	}
	return "any"
}

func replBalance(n *node.Node, out io.Writer, args []string) {
	addr := n.Wallet().Address()
	if len(args) > 0 {
		addr = args[0]
	}
	acc := n.Chain().Account(addr)
	fmt.Fprintf(out, "%s\n  balance %s  nonce %d\n", addr, core.FormatAmount(acc.Balance), acc.Nonce)
	// Assets were invisible here, which made an account holding them look empty.
	for _, id := range sortedAssetIDs(acc.Assets) {
		label := short(id)
		if info, ok := n.Chain().Asset(id); ok {
			label = fmt.Sprintf("%s (%s)", info.Ticker, short(id))
		}
		fmt.Fprintf(out, "  %d units of %s\n", acc.Assets[id], label)
	}
}

// sortedAssetIDs orders an account's asset ids, so two reads print the same way.
func sortedAssetIDs(assets map[string]uint64) []string {
	ids := make([]string, 0, len(assets))
	for id := range assets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func replAddress(n *node.Node, out io.Writer, _ []string) {
	fmt.Fprintln(out, n.Wallet().Address())
}

func replInfo(n *node.Node, out io.Writer, _ []string) {
	tip := n.Chain().Tip()
	fmt.Fprintf(out, "network   %s\n", core.NetworkName())
	fmt.Fprintf(out, "height    %d  tip %s\n", tip.Index, short(tip.Hash))
	fmt.Fprintf(out, "difficulty %.2f (next)  base fee %d/byte\n",
		core.TargetDifficulty(n.Chain().NextBits()), n.Chain().NextBaseFee())
	fmt.Fprintf(out, "mempool   %d tx, %d bytes (min relay %d/byte)\n",
		n.Mempool().Size(), n.Mempool().Bytes(), n.Mempool().MinFee())
	fmt.Fprintf(out, "peers     %d  mining %v\n", len(n.PeerAddrs()), n.Mining())
	fmt.Fprintf(out, "identity  %s\n", short(n.IdentityKey()))
}

func replPeers(n *node.Node, out io.Writer, _ []string) {
	peers := n.Peers()
	if len(peers) == 0 {
		fmt.Fprintln(out, "no peers connected")
		return
	}
	fmt.Fprintf(out, "%-24s %-14s %-4s %-4s %-8s %s\n", "ADDRESS", "IDENTITY", "DIR", "VER", "UP", "SYNCING")
	for _, p := range peers {
		dir := "out"
		if p.Inbound {
			dir = "in"
		}
		fmt.Fprintf(out, "%-24s %-14s %-4s %-4d %-8s %v\n",
			p.Addr, short(p.Identity), dir, p.Version, p.Connected, p.Syncing)
	}
}

func replMempool(n *node.Node, out io.Writer, args []string) {
	mp := n.Mempool()
	fmt.Fprintf(out, "%d pending, %d bytes of %d (min relay %d/byte)\n",
		mp.Size(), mp.Bytes(), mp.MaxBytes(), mp.MinFee())
	if len(args) == 0 || args[0] != "list" {
		if mp.Size() > 0 {
			fmt.Fprintln(out, "  (`mempool list` to see them)")
		}
		return
	}
	for _, tx := range mp.All() {
		fmt.Fprintf(out, "  %s  %s → %s  %s  fee %s (%d bytes)\n",
			short(tx.Hash()), short(tx.From), short(tx.To), core.FormatAmount(tx.Amount),
			core.FormatAmount(tx.Fee), tx.Size())
	}
}

func replTx(n *node.Node, out io.Writer, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(out, "usage: tx <hash>")
		return
	}
	hash := args[0]
	if tx, loc, ok := n.Chain().FindTx(hash); ok {
		fmt.Fprintf(out, "confirmed in block %d (position %d), %d confirmation(s)\n",
			loc.Height, loc.Index, n.Chain().Height()-loc.Height+1)
		replPrintTx(out, tx)
		return
	}
	if tx, ok := n.Mempool().Get(hash); ok {
		fmt.Fprintln(out, "pending in the mempool")
		replPrintTx(out, tx)
		return
	}
	fmt.Fprintln(out, "not found: neither confirmed nor pending on this node")
	if n.Chain().BodyHeight() > 1 {
		fmt.Fprintf(out, "  (this node has pruned the bodies below height %d, so it cannot see that far back)\n",
			n.Chain().BodyHeight())
	}
}

func replPrintTx(out io.Writer, tx core.Transaction) {
	fmt.Fprintf(out, "  from   %s\n", tx.From)
	if tx.IsMultiOutput() {
		for _, o := range tx.Outputs {
			fmt.Fprintf(out, "  → %s  %s\n", o.To, core.FormatAmount(o.Amount))
		}
	} else {
		fmt.Fprintf(out, "  to     %s\n", tx.To)
		fmt.Fprintf(out, "  amount %s\n", core.FormatAmount(tx.Amount))
	}
	fmt.Fprintf(out, "  fee    %s  nonce %d  size %d bytes\n", core.FormatAmount(tx.Fee), tx.Nonce, tx.Size())
	if tx.Memo != "" {
		fmt.Fprintf(out, "  memo   %q\n", tx.Memo)
	}
	if tx.LockUntil != 0 || tx.Expiry != 0 {
		fmt.Fprintf(out, "  window heights %s\n", describeWindow(tx.LockUntil, tx.Expiry))
	}
}

func replAssets(n *node.Node, out io.Writer, args []string) {
	if len(args) > 0 {
		info, ok := n.Chain().Asset(args[0])
		if !ok {
			fmt.Fprintln(out, "no such asset on this chain")
			return
		}
		fmt.Fprintf(out, "%s  %s\n  issuer %s\n  supply %d, issued in block %d\n",
			info.Ticker, info.ID, info.Issuer, info.Supply, info.Height)
		for _, h := range n.Chain().AssetHolders(info.ID) {
			fmt.Fprintf(out, "  %s  %d\n", short(h.Address), h.Amount)
		}
		return
	}
	list := n.Chain().Assets()
	if len(list) == 0 {
		fmt.Fprintln(out, "no assets have been issued on this chain")
		return
	}
	for _, a := range list {
		fmt.Fprintf(out, "  %-8s %s  supply %d  issuer %s\n", a.Ticker, short(a.ID), a.Supply, short(a.Issuer))
	}
}

func replSupply(n *node.Node, out io.Writer, _ []string) {
	s := n.Chain().Supply()
	fmt.Fprintf(out, "minted      %s\nburned      %s\ncirculating %s\naccounts    %d\n",
		s.MintedFmt, s.BurnedFmt, s.CirculatingFmt, s.Accounts)
	fmt.Fprintf(out, "next block mints %s; the subsidy halves at height %d\n", s.SubsidyFmt, s.NextHalving)
	if !s.Consistent {
		fmt.Fprintln(out, "WARNING: minted − burned does not equal the circulating total")
	}
}

func replStats(n *node.Node, out io.Writer, args []string) {
	window := 0
	if len(args) > 0 {
		if v, err := strconv.Atoi(args[0]); err == nil {
			window = v
		}
	}
	s := n.Chain().Stats(window)
	if s.Window < 2 {
		fmt.Fprintln(out, "not enough blocks yet to measure an interval")
		return
	}
	fmt.Fprintf(out, "over blocks %d..%d (%d blocks, %ds)\n", s.FromHeight, s.ToHeight, s.Window, s.Seconds)
	fmt.Fprintf(out, "  hashrate %s\n", s.HashrateFmt)
	fmt.Fprintf(out, "  intervals: median %ds against a %ds target\n", s.MedianInterval, core.TargetBlockTime)
}

func replHealth(n *node.Node, out io.Writer, _ []string) {
	// The same judgement /health makes, so a console answer and a supervisor's
	// answer cannot differ.
	behind := n.BlocksBehind()
	fmt.Fprintf(out, "height %d, %d block(s) behind the best peer, %d peer(s)\n",
		n.Chain().Height(), behind, len(n.PeerAddrs()))
	var reasons []string
	if len(n.PeerAddrs()) == 0 {
		reasons = append(reasons, "no peers connected")
	}
	if behind > 0 {
		reasons = append(reasons, fmt.Sprintf("%d block(s) behind", behind))
	}
	if len(reasons) == 0 {
		fmt.Fprintln(out, "READY")
		return
	}
	fmt.Fprintln(out, "NOT READY:", strings.Join(reasons, "; "))
}

func replReorgs(n *node.Node, out io.Writer, _ []string) {
	r := n.Reorgs()
	fmt.Fprintf(out, "%d reorg(s), deepest %d (the consensus limit is %d)\n", r.Total, r.Deepest, r.MaxDepth)
	fmt.Fprintf(out, "%d orphan block(s) buffered\n", r.Orphans)
	for _, e := range r.Reorgs {
		fmt.Fprintf(out, "  height %d: dropped %d, adopted %d, %d tx re-queued (%s)\n",
			e.Height, e.Depth, e.Adopted, e.Requeued, e.At)
	}
}

func replPrune(n *node.Node, out io.Writer, _ []string) {
	bc := n.Chain()
	if bc.PruneKeep() == 0 && bc.BodyHeight() <= 1 {
		if bc.Height() == 0 {
			fmt.Fprintln(out, "not pruning: the chain is at genesis")
			return
		}
		fmt.Fprintf(out, "not pruning: every body from height 1 to %d is held\n", bc.Height())
		return
	}
	fmt.Fprintf(out, "bodies from height %d to %d (%d discarded, keeping %d)\n",
		bc.BodyHeight(), bc.Height(), bc.PrunedCount(), bc.PruneKeep())
	fmt.Fprintf(out, "filter headers from height %d\n", bc.FilterHeaderBase())
	fmt.Fprintln(out, "older bodies, their inclusion proofs and their filters cannot be served from here")
}

func replWebhooks(n *node.Node, out io.Writer, _ []string) {
	if !n.WebhooksEnabled() {
		fmt.Fprintln(out, "no webhooks configured (start the node with -webhook URL)")
		return
	}
	s := n.WebhookStats()
	fmt.Fprintf(out, "%d url(s): %d sent, %d failed, %d dropped, %d queued\n",
		s.URLs, s.Sent, s.Failed, s.Dropped, s.Queued)
}

func replMine(n *node.Node, out io.Writer, args []string) {
	if len(args) == 0 {
		fmt.Fprintf(out, "mining is %v\n", n.Mining())
		return
	}
	switch args[0] {
	case "on", "true", "1":
		n.SetMining(true)
	case "off", "false", "0":
		n.SetMining(false)
	default:
		fmt.Fprintln(out, "usage: mine [on|off]")
		return
	}
	fmt.Fprintf(out, "mining is now %v\n", n.Mining())
}

func replGenerate(n *node.Node, out io.Writer, args []string) {
	count := 1
	if len(args) > 0 {
		if v, err := strconv.Atoi(args[0]); err == nil {
			count = v
		}
	}
	hashes, err := n.Generate(count)
	if err != nil {
		fmt.Fprintln(out, err)
		return
	}
	fmt.Fprintf(out, "mined %d block(s), tip now %d\n", len(hashes), n.Chain().Height())
}
