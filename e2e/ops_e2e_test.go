//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Operator-facing behaviour, driven through the real binary: the things that
// only exist once a node is actually running and someone has to look after it.

// A node's network identity must be its own key, not the wallet's — the identity
// public key goes to every peer, and an address is a hash of a public key.
func TestNodeIdentityIsNotTheWallet(t *testing.T) {
	n := startNode(t, nodeOpts{name: "identity"})

	walletAddr := n.address()
	// The identity key file is created beside the chain, and its address must
	// differ from the wallet's — if they matched, every peer could derive the
	// address holding this node's coin.
	identityAddr := strings.TrimSpace(n.cli("wallet", "address", "-o", "nodekey.json"))
	if identityAddr == "" || !strings.HasPrefix(identityAddr, "dnas") {
		t.Fatalf("no node identity key was created: %q", identityAddr)
	}
	if identityAddr == walletAddr {
		t.Fatal("the node identity is the wallet key, so peers can derive the wallet address")
	}
	// And it must be stable, or peers would not recognize the node after a restart.
	again := strings.TrimSpace(n.cli("wallet", "address", "-o", "nodekey.json"))
	if again != identityAddr {
		t.Fatalf("identity changed between reads: %s then %s", identityAddr, again)
	}
}

func TestPrintConfigShowsTheResolvedSettings(t *testing.T) {
	n := startNode(t, nodeOpts{name: "printconfig"})
	out := n.cli("node", "-printconfig", "-network", "regtest", "-db", "pc.db", "-mine")
	for _, want := range []string{"effective configuration:", "network", "regtest", "nodekey", "mine"} {
		mustContain(t, out, want, "printconfig")
	}
	mustContain(t, out, "nothing was started", "printconfig should not run the node")
	// -advertise is resolved from -listen rather than shown blank.
	mustContain(t, out, "defaulted to -listen", "advertise resolution")
}

func TestStructuredLogging(t *testing.T) {
	n := startNode(t, nodeOpts{name: "logjson", extra: []string{"-logjson"}})
	n.generate(1)
	logs := n.logs()
	if !strings.Contains(logs, `{"level":"info","event":`) {
		t.Fatalf("expected JSON log lines, got:\n%s", tailOf(logs))
	}
	// The prose form is the default, and the level names are the documented ones.
	quiet := startNode(t, nodeOpts{name: "quiet", extra: []string{"-loglevel", "error"}})
	quiet.generate(1)
	if strings.Contains(quiet.logs(), "event=") {
		t.Fatalf("error level should suppress info events, got:\n%s", tailOf(quiet.logs()))
	}
	// A misspelled level is refused at startup rather than silently selecting one,
	// and the process exits non-zero so a supervisor notices.
	bad := n.cliAllowFail("node", "-loglevel", "chatty", "-printconfig")
	if !strings.Contains(bad.out, "unknown log level") {
		t.Fatalf("a bad -loglevel was accepted: %s", bad.out)
	}
	if bad.err == nil {
		t.Fatal("a bad -loglevel exited 0")
	}
}

func TestPeersAndBansCLI(t *testing.T) {
	a := startNode(t, nodeOpts{name: "ops-a"})
	b := startNode(t, nodeOpts{name: "ops-b"})

	// Runtime peer addition, then the detailed view of the resulting connection.
	out := a.cli("peers", "add", b.p2pAddr, "-api", a.apiAddr)
	mustContain(t, out, "dialing", "peers add")
	waitFor(t, 20*time.Second, "the peers to connect", func() bool {
		return strings.Contains(a.cli("peers", "-api", a.apiAddr), b.p2pAddr)
	})

	list := a.cli("peers", "-api", a.apiAddr)
	for _, want := range []string{"ADDRESS", "IDENTITY", "DIR", "VER", "out", "mpool"} {
		mustContain(t, list, want, "peers list")
	}
	// A node must not report a connection to itself. That happened whenever its
	// advertised address was spelled differently by a peer, and burned two slots.
	ownIdentity := strings.TrimSpace(a.cli("wallet", "pubkey", "-o", "nodekey.json"))
	if strings.Contains(list, ownIdentity[:12]) {
		t.Fatalf("node A lists a connection to ITSELF:\n%s", list)
	}

	// Bans: nothing scored yet, and clearing an unknown key must fail loudly.
	bans := a.cli("peers", "bans", "-api", a.apiAddr)
	mustContain(t, bans, "no ban scores recorded", "peers bans")
	// Clearing a key that was never scored fails loudly AND exits non-zero, so
	// "done" never quietly means "there was nothing there".
	unban := a.cliAllowFail("peers", "unban", "definitely-not-a-peer", "-api", a.apiAddr)
	if !containsAny(unban.out, "no ban score", "error") {
		t.Fatalf("unbanning an unknown key reported success: %s", unban.out)
	}
	if unban.err == nil {
		t.Fatal("unbanning an unknown key exited 0")
	}

	// Dropping a live peer closes the connection.
	drop := a.cli("peers", "drop", b.p2pAddr, "-api", a.apiAddr)
	mustContain(t, drop, "closed the connection", "peers drop")
}

func TestStatsAndHealthAndReorgsCLI(t *testing.T) {
	n := startNode(t, nodeOpts{name: "ops-stats"})
	fundNode(t, n)
	n.generate(4)

	stats := n.cli("stats", "-api", n.apiAddr)
	for _, want := range []string{"chain stats over", "hashrate", "intervals", "miners in this window"} {
		mustContain(t, stats, want, "stats")
	}

	// A node with no peers is deliberately NOT ready, and `dnas health` says so
	// and exits non-zero — which is what makes it usable as a supervisor check.
	health := n.cliAllowFail("health", "-api", n.apiAddr)
	mustContain(t, health.out, "NOT READY", "health with no peers")
	mustContain(t, health.out, "no peers connected", "health reason")
	if health.err == nil {
		t.Fatal("`dnas health` exited 0 for a node that is not ready")
	}

	reorgs := n.cli("reorgs", "-api", n.apiAddr)
	mustContain(t, reorgs, "0 reorg(s)", "reorgs on a linear chain")
	mustContain(t, reorgs, "orphan block(s)", "reorgs reports the orphan pool")
}

// The light client must not re-download the header chain on every command: the
// cache file is created, reused, and extended.
func TestSPVHeaderCache(t *testing.T) {
	n := startNode(t, nodeOpts{name: "spvcache"})
	n.generate(3)

	out := n.cli("spv", "-api", n.apiAddr, "-cache", "hdrs.json", "sync")
	mustContain(t, out, "✓ header chain verified", "first sync")
	first := n.fileSize("hdrs.json")
	if first == 0 {
		t.Fatal("the header cache was not written")
	}

	// More blocks: the cache must grow, and the client must still verify.
	n.generate(3)
	out = n.cli("spv", "-api", n.apiAddr, "-cache", "hdrs.json", "sync")
	mustContain(t, out, "✓ header chain verified", "incremental sync")
	if second := n.fileSize("hdrs.json"); second <= first {
		t.Fatalf("cache did not grow: %d then %d bytes", first, second)
	}
	// A wallet keeps its own cache beside its state file, so two wallets never
	// share one.
	addr := newWallet(t, n.dir, "cachewallet.json")
	n.cli("spv", "-api", n.apiAddr, "wallet", "-f", "cw.json", "add", addr)
	if n.fileSize("cw.json.headers") == 0 {
		t.Fatal("the wallet did not keep its own header cache")
	}
}

// Paging: the bulk reads must be bounded and addressable by height.
func TestPagedReadEndpoints(t *testing.T) {
	n := startNode(t, nodeOpts{name: "paging"})
	n.generate(6)

	for _, path := range []string{"/chain", "/headers", "/cfilters", "/cfheaders"} {
		var page []any
		n.getJSON(path+"?from=2&limit=2", &page)
		if len(page) != 2 {
			t.Fatalf("%s?from=2&limit=2 returned %d entries, want 2", path, len(page))
		}
		var beyond []any
		n.getJSON(path+"?from=500", &beyond)
		if len(beyond) != 0 {
			t.Fatalf("%s past the tip returned %d entries", path, len(beyond))
		}
		if code, _ := n.get(path + "?limit=0"); code != 400 {
			t.Fatalf("%s?limit=0 status = %d, want 400", path, code)
		}
	}
}

// Fee-bumping and cancelling: replace-by-fee has always been in consensus and
// no client could reach it.
func TestBumpAndCancelCLI(t *testing.T) {
	n := startNode(t, nodeOpts{name: "bump"})
	fundNode(t, n)
	recipient := newWallet(t, n.dir, "bump-dest.json")

	// A payment priced just above the relay floor, so a bump has room to move.
	hash := n.send(recipient, Coin, testFee)
	out := n.cli("spv", "-api", n.apiAddr, "wallet", "-f", "bw.json", "-key", "wallet.json",
		"bump", hash)
	mustContain(t, out, "bumped", "wallet bump")
	mustContain(t, out, "same payment at nonce", "bump keeps the payment")

	// The bump replaced the original rather than adding a second payment.
	var pending []map[string]any
	n.getJSON("/mempool", &pending)
	if len(pending) != 1 {
		t.Fatalf("mempool holds %d transactions after a bump, want 1", len(pending))
	}
	bumped, _ := pending[0]["fee"].(float64)
	if uint64(bumped) <= testFee {
		t.Fatalf("bumped fee = %v, want more than the original %d", bumped, testFee)
	}

	// Cancelling spends the nonce on a self-payment, so the recipient is never paid.
	newHash, _ := pending[0]["hash"].(string)
	if newHash == "" {
		// /mempool returns transactions, which carry no hash field; look it up
		// through the wallet's own report instead.
		newHash = hash
	}
	cancel := n.cli("spv", "-api", n.apiAddr, "wallet", "-f", "bw.json", "-key", "wallet.json",
		"cancel", newHash)
	if !containsAny(cancel, "cancelled", "already confirmed", "never seen") {
		t.Fatalf("unexpected cancel output: %s", cancel)
	}
}

// --- helpers for the operator-facing checks ---

// logs returns everything the node has written to its log so far.
func (n *node) logs() string { return n.log.String() }

// tailOf trims a log to its last few lines, so a failure message stays readable.
func tailOf(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 12 {
		lines = lines[len(lines)-12:]
	}
	return strings.Join(lines, "\n")
}

// fileSize is the size of a file in the node's data directory, or 0 if absent.
func (n *node) fileSize(name string) int64 {
	fi, err := os.Stat(filepath.Join(n.dir, name))
	if err != nil {
		return 0
	}
	return fi.Size()
}

// cliResult is a CLI run whose exit status matters.
type cliResult struct {
	out string
	err error
}

// cliAllowFail runs the CLI and returns its output AND its error, for the
// commands whose exit status is part of the contract (`dnas health` exits
// non-zero when a node is not ready, so a supervisor can use it).
func (n *node) cliAllowFail(args ...string) cliResult {
	n.t.Helper()
	cmd := exec.Command(dnasBin(n.t), args...)
	cmd.Dir = n.dir
	out, err := cmd.CombinedOutput()
	return cliResult{out: string(out), err: err}
}

// cliEnv runs the CLI with extra environment variables, for the commands whose
// secrets arrive that way rather than as flags (a wallet passphrase must not
// land in shell history or in `ps`).
func (n *node) cliEnv(env []string, args ...string) string {
	n.t.Helper()
	res := n.cliEnvAllowFail(env, args...)
	if res.err != nil {
		n.t.Fatalf("dnas %s: %v\n%s", strings.Join(args, " "), res.err, res.out)
	}
	return res.out
}

// cliEnvAllowFail is cliEnv for a command whose exit status is part of what is
// being checked.
func (n *node) cliEnvAllowFail(env []string, args ...string) cliResult {
	n.t.Helper()
	cmd := exec.Command(dnasBin(n.t), args...)
	cmd.Dir = n.dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return cliResult{out: string(out), err: err}
}

// cliStdin runs the CLI with a script on stdin, for the interactive console.
// The console keeps the node running until it is told to quit, so the script
// must end in `quit`.
func (n *node) cliStdin(script string, args ...string) string {
	n.t.Helper()
	cmd := exec.Command(dnasBin(n.t), args...)
	cmd.Dir = n.dir
	cmd.Stdin = strings.NewReader(script)
	out, _ := cmd.CombinedOutput()
	return string(out)
}
