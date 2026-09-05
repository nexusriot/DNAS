package main

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/node"
	"github.com/nexusriot/DNAS/wallet"
)

// The console is the only way to look at a node whose HTTP API is unreachable,
// which is exactly the situation where somebody most wants to look. It had six
// commands while the node grew a couple of dozen surfaces around it, so what is
// worth testing is that the vocabulary is complete and self-describing.
func TestREPLVocabularyIsCompleteAndDocumented(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range replCommands {
		if c.name == "" || c.usage == "" || c.help == "" || c.run == nil {
			t.Fatalf("command %+v is missing something", c)
		}
		if seen[c.name] {
			t.Fatalf("command %q is registered twice", c.name)
		}
		seen[c.name] = true
		// The usage line must start with the command, or `help` describes
		// something other than what the user has to type.
		if !strings.HasPrefix(c.usage, c.name) {
			t.Errorf("usage %q does not start with %q", c.usage, c.name)
		}
	}
	// The surfaces the node grew and the console could not reach.
	for _, want := range []string{"send", "balance", "address", "info", "peers", "mempool",
		"tx", "assets", "supply", "stats", "health", "reorgs", "prune", "webhooks", "mine", "generate"} {
		if !seen[want] {
			t.Errorf("the console has no %q command", want)
		}
	}
	if _, ok := lookupReplCmd("nonsense"); ok {
		t.Fatal("an unknown command resolved")
	}
}

// A memo with a space in it was the reason quoting had to exist: without it the
// memo silently loses everything after the first word.
func TestSplitConsoleLineKeepsQuotedRuns(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`send dnasx 1.5`, []string{"send", "dnasx", "1.5"}},
		{`send dnasx 1 memo="two coffees"`, []string{"send", "dnasx", "1", "memo=two coffees"}},
		{`send dnasx 1 memo='two coffees'`, []string{"send", "dnasx", "1", "memo=two coffees"}},
		{`  spaced   out  `, []string{"spaced", "out"}},
		{``, nil},
		{`   `, nil},
		{`memo="a b c" fee=0.001`, []string{"memo=a b c", "fee=0.001"}},
	}
	for _, tc := range cases {
		got := splitConsoleLine(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("split(%q) = %v, want %v", tc.in, got, tc.want)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Fatalf("split(%q) = %v, want %v", tc.in, got, tc.want)
			}
		}
	}
}

func TestConsoleOptsParseKeysAndHeights(t *testing.T) {
	pos, opts := parseConsoleOpts([]string{"dnasx", "1.5", "fee=0.001", "expiry=+20", "memo=rent"})
	if len(pos) != 2 || pos[0] != "dnasx" || pos[1] != "1.5" {
		t.Fatalf("positional = %v", pos)
	}
	if opts["memo"] != "rent" {
		t.Fatalf("memo = %q", opts["memo"])
	}
	fee, err := opts.amount("fee")
	if err != nil || fee != 100_000 {
		t.Fatalf("fee = %d, %v", fee, err)
	}
	// "+N" is what an operator means by "expires in twenty blocks", and it has to
	// resolve against the tip before the transaction is signed.
	h, err := opts.height("expiry", 500)
	if err != nil || h != 520 {
		t.Fatalf("expiry = %d, %v", h, err)
	}
	// An absolute value passes through.
	_, abs := parseConsoleOpts([]string{"lock=900"})
	if h, err := abs.height("lock", 500); err != nil || h != 900 {
		t.Fatalf("lock = %d, %v", h, err)
	}
	// An absent option is zero, not an error: that is how "no deadline" is said.
	if h, err := opts.height("lock", 500); err != nil || h != 0 {
		t.Fatalf("absent lock = %d, %v", h, err)
	}
	if a, err := opts.amount("nothing"); err != nil || a != 0 {
		t.Fatalf("absent amount = %d, %v", a, err)
	}
	// Nonsense must be refused rather than silently treated as zero, which would
	// send a payment with no deadline when one was asked for.
	_, bad := parseConsoleOpts([]string{"expiry=soon", "fee=lots"})
	if _, err := bad.height("expiry", 1); err == nil {
		t.Fatal("expiry=soon was accepted")
	}
	if _, err := bad.amount("fee"); err == nil {
		t.Fatal("fee=lots was accepted")
	}
}

func TestDescribeWindowReadsBothWays(t *testing.T) {
	for _, tc := range []struct {
		lock, expiry uint64
		want         string
	}{
		{0, 0, "any"},
		{100, 0, "100 and above"},
		{0, 200, "up to 200"},
		{100, 200, "100..200"},
	} {
		if got := describeWindow(tc.lock, tc.expiry); got != tc.want {
			t.Errorf("describeWindow(%d, %d) = %q, want %q", tc.lock, tc.expiry, got, tc.want)
		}
	}
}

// Driving the console for real: every read command must answer without a node
// that has peers, an API, or a chain beyond genesis — a console that only works
// on a healthy node is useless exactly when it is needed.
func TestREPLAnswersEveryReadCommand(t *testing.T) {
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	n := node.New(node.Config{ListenAddr: "127.0.0.1:0", Regtest: true},
		core.NewBlockchain(), core.NewMempool(), w)
	defer n.Shutdown()

	script := strings.Join([]string{
		"help",
		"address",
		"info",
		"balance",
		"peers",
		"mempool",
		"mempool list",
		"tx deadbeef",
		"assets",
		"supply",
		"stats",
		"health",
		"reorgs",
		"prune",
		"webhooks",
		"mine",
		"nonsense",
		"",
		"quit",
	}, "\n") + "\n"

	var out strings.Builder
	replLoop(n, strings.NewReader(script), &out)
	got := out.String()

	for _, want := range []string{
		w.Address(),                  // address
		"no peers connected",         // peers
		"0 pending",                  // mempool
		"not found",                  // tx
		"no assets have been issued", // assets
		"minted",                     // supply
		"NOT READY",                  // health (no peers)
		"0 reorg(s)",                 // reorgs
		"not pruning",                // prune
		"no webhooks configured",     // webhooks
		"mining is false",            // mine
		"unknown command: nonsense",  // the error path
		"send <to> <amount>",         // help
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the console output is missing %q\n---\n%s", want, got)
		}
	}
	// Every command must have printed something: a silent answer is
	// indistinguishable from a command that does not exist.
	if strings.Count(got, "dnas> ") < len(replCommands) {
		t.Errorf("only %d prompts for %d commands", strings.Count(got, "dnas> "), len(replCommands))
	}
}

// A send from the console, including the options that had no way in.
func TestREPLSendCarriesTheOptions(t *testing.T) {
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	chain := core.NewBlockchain()
	mp := core.NewMempoolWithPolicy(core.DefaultMempoolSize, core.DefaultMinRelayFee)
	n := node.New(node.Config{ListenAddr: "127.0.0.1:0", Regtest: true}, chain, mp, w)
	defer n.Shutdown()
	// Fund the node's own wallet and mature it, or the mempool rightly refuses.
	if _, err := n.Generate(core.CoinbaseMaturity + 1); err != nil {
		t.Fatalf("fund: %v", err)
	}

	dest, _ := wallet.New()
	fee := core.FormatAmount(core.DefaultMinRelayFee * 2000)
	fee = strings.TrimSuffix(fee, " "+core.Ticker)
	var out strings.Builder
	replLoop(n, strings.NewReader(strings.Join([]string{
		"send " + dest.Address() + " 1 fee=" + fee + " expiry=+50 lock=+1 memo=\"two coffees\"",
		"mempool list",
		"quit",
	}, "\n")+"\n"), &out)

	got := out.String()
	if !strings.Contains(got, "submitted") {
		t.Fatalf("the send was refused:\n%s", got)
	}
	pending := mp.All()
	if len(pending) != 1 {
		t.Fatalf("mempool holds %d transactions", len(pending))
	}
	tx := pending[0]
	if tx.Memo != "two coffees" {
		t.Fatalf("memo = %q — the quoted words were lost", tx.Memo)
	}
	tip := chain.Height()
	if tx.Expiry != tip+50 || tx.LockUntil != tip+1 {
		t.Fatalf("window = %d..%d, want %d..%d", tx.LockUntil, tx.Expiry, tip+1, tip+50)
	}
	// And the console must show it, memo and window included.
	if !strings.Contains(got, "valid in heights") {
		t.Errorf("the send did not report the window it attached:\n%s", got)
	}
}
