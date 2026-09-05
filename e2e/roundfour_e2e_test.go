//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The features of this round, driven through the real binary against a real
// node. Every one of them was reachable only in theory before: consensus
// supported it and no client could get at it.

// Spending FROM a multisig account. Consensus has checked M-of-N signatures
// from the start and four surfaces would derive an address for you; nothing
// could move the coin back out, so a funded 2-of-3 address was a hole to put
// money in.
func TestMultisigSpendEndToEnd(t *testing.T) {
	n := startNode(t, nodeOpts{name: "multisig"})
	fundNode(t, n)

	// Three members, and the address their script hashes to.
	var keys []string
	for _, name := range []string{"m1.json", "m2.json", "m3.json"} {
		newWallet(t, n.dir, name)
		keys = append(keys, strings.TrimSpace(n.cli("wallet", "pubkey", "-o", name)))
	}
	members := strings.Join(keys, ",")
	addr := strings.TrimSpace(n.cli("multisig", "address", "-threshold", "2", "-pubkeys", members))
	if !strings.HasPrefix(addr, "dnas") {
		t.Fatalf("multisig address = %q", addr)
	}

	// Fund it, then sweep it back out to a fresh address.
	n.send(addr, 5*Coin, testFee)
	n.generate(1)
	if bal := n.balance(addr); bal != 5*Coin {
		t.Fatalf("the multisig account holds %d, want %d", bal, 5*Coin)
	}
	dest := newWallet(t, n.dir, "dest.json")

	out := n.cli("multisig", "propose", "-api", n.apiAddr, "-threshold", "2",
		"-pubkeys", members, "-to", dest, "-amount", "all", "-o", "spend.json")
	mustContain(t, out, "needs 2 signature(s)", "propose")

	// One signature is not enough, and the tool says so rather than letting the
	// node answer with a bare rejection.
	early := n.cliAllowFail("multisig", "submit", "-in", "spend.json", "-api", n.apiAddr)
	if !strings.Contains(early.out, "not ready to submit") {
		t.Fatalf("submitting an unsigned spend: %s", early.out)
	}

	mustContain(t, n.cli("multisig", "sign", "-wallet", "m1.json", "-in", "spend.json"),
		"1 of 2 signature(s)", "first signature")
	// The same member signing twice is refused: consensus needs M DISTINCT
	// members, so a doubly-signed file is not redundant, it is unusable.
	dup := n.cliAllowFail("multisig", "sign", "-wallet", "m1.json", "-in", "spend.json")
	if !strings.Contains(dup.out, "already signed") {
		t.Fatalf("a duplicate signature was accepted: %s", dup.out)
	}
	// So is a stranger's.
	newWallet(t, n.dir, "stranger.json")
	stranger := n.cliAllowFail("multisig", "sign", "-wallet", "stranger.json", "-in", "spend.json")
	if !strings.Contains(stranger.out, "not one of the") {
		t.Fatalf("a non-member signed: %s", stranger.out)
	}

	mustContain(t, n.cli("multisig", "sign", "-wallet", "m3.json", "-in", "spend.json"),
		"complete", "second signature")
	inspect := n.cli("multisig", "inspect", "-in", "spend.json")
	mustContain(t, inspect, "2 of 2 required", "inspect")
	mustContain(t, inspect, "ready to submit", "inspect")

	mustContain(t, n.cli("multisig", "submit", "-in", "spend.json", "-api", n.apiAddr),
		"submitted", "submit")
	n.generate(1)
	if bal := n.balance(addr); bal != 0 {
		t.Fatalf("the multisig account still holds %d after the sweep", bal)
	}
	if bal := n.balance(dest); bal == 0 {
		t.Fatal("the recipient was not paid")
	}
}

// A 2-of-3 escrow, built on that spending path: the coin moves only when two of
// the three roles agree, and the arbiter alone can do nothing.
func TestEscrowReleaseEndToEnd(t *testing.T) {
	n := startNode(t, nodeOpts{name: "escrow"})
	fundNode(t, n)

	roles := map[string]string{}
	for _, role := range []string{"buyer", "seller", "arbiter"} {
		newWallet(t, n.dir, role+".json")
		roles[role] = strings.TrimSpace(n.cli("wallet", "pubkey", "-o", role+".json"))
	}
	out := n.cli("escrow", "new", "-api", n.apiAddr, "-buyer", roles["buyer"],
		"-seller", roles["seller"], "-arbiter", roles["arbiter"],
		"-terms", "one bicycle", "-o", "escrow.json")
	mustContain(t, out, "2-of-3", "escrow new")

	// One party holding two roles would be a 1-of-2 wearing a 2-of-3's clothes.
	shared := n.cliAllowFail("escrow", "new", "-api", n.apiAddr, "-buyer", roles["buyer"],
		"-seller", roles["buyer"], "-arbiter", roles["arbiter"], "-o", "bad.json")
	if !strings.Contains(shared.out, "share a key") {
		t.Fatalf("an escrow with a shared role was accepted: %s", shared.out)
	}

	var esc struct {
		Address string `json:"address"`
	}
	readJSONFile(t, filepath.Join(n.dir, "escrow.json"), &esc)
	n.send(esc.Address, 3*Coin, testFee)
	n.generate(1)
	mustContain(t, n.cli("escrow", "show", "-api", n.apiAddr, "-in", "escrow.json"),
		"3.00000000", "escrow show")

	// Release pays the seller, and needs two signatures to do it.
	mustContain(t, n.cli("escrow", "release", "-api", n.apiAddr, "-in", "escrow.json", "-o", "payout.json"),
		"release to the seller", "escrow release")
	n.cli("multisig", "sign", "-wallet", "buyer.json", "-in", "payout.json")
	// The arbiter alone plus the buyer is a valid pairing; the buyer alone is not.
	alone := n.cliAllowFail("multisig", "submit", "-in", "payout.json", "-api", n.apiAddr)
	if !strings.Contains(alone.out, "not ready to submit") {
		t.Fatalf("one signature released the escrow: %s", alone.out)
	}
	n.cli("multisig", "sign", "-wallet", "arbiter.json", "-in", "payout.json")
	mustContain(t, n.cli("multisig", "submit", "-in", "payout.json", "-api", n.apiAddr),
		"submitted", "escrow payout")
	n.generate(1)

	sellerAddr := strings.TrimSpace(n.cli("wallet", "address", "-o", "seller.json"))
	if bal := n.balance(sellerAddr); bal == 0 {
		t.Fatal("the seller was not paid")
	}
}

// Anchoring a file: the chain as a timestamp service, which the memo field made
// reachable.
func TestAnchorEndToEnd(t *testing.T) {
	n := startNode(t, nodeOpts{name: "anchor"})
	fundNode(t, n)

	path := filepath.Join(n.dir, "contract.txt")
	if err := os.WriteFile(path, []byte("the terms, as agreed"), 0o644); err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimSpace(n.cli("anchor", "hash", "-file", "contract.txt"))
	if len(digest) != 64 {
		t.Fatalf("anchor hash printed %q", digest)
	}

	out := n.cli("anchor", "add", "-api", n.apiAddr, "-file", "contract.txt", "-key", "wallet.json")
	mustContain(t, out, digest, "anchor add")
	mustContain(t, out, "wrote contract.txt.dnasanchor", "anchor add")

	// Before it is mined there is nothing to prove.
	pending := n.cliAllowFail("anchor", "verify", "-api", n.apiAddr, "-file", "contract.txt")
	if !strings.Contains(pending.out, "NOT PROVEN") {
		t.Fatalf("an unmined anchor verified: %s", pending.out)
	}
	n.generate(2)
	proven := n.cli("anchor", "verify", "-api", n.apiAddr, "-file", "contract.txt")
	mustContain(t, proven, "existed before block", "anchor verify")
	mustContain(t, proven, "proof-of-work headers", "anchor verify")

	// Change one byte and the claim collapses, which is the whole point.
	if err := os.WriteFile(path, []byte("the terms, as agreed (amended)"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed := n.cliAllowFail("anchor", "verify", "-api", n.apiAddr, "-file", "contract.txt")
	if !strings.Contains(changed.out, "does not match its receipt") {
		t.Fatalf("a changed file still verified: %s", changed.out)
	}
	if changed.err == nil {
		t.Fatal("a failed anchor verification exited 0")
	}
}

// An invoice, from asking to be paid to verifying that you were.
func TestInvoiceLifecycleEndToEnd(t *testing.T) {
	n := startNode(t, nodeOpts{name: "invoice"})
	fundNode(t, n)
	newWallet(t, n.dir, "shop.json")

	out := n.cli("invoice", "new", "-api", n.apiAddr, "-amount", "1.25",
		"-memo", "two coffees", "-key", "shop.json", "-o", "inv.json")
	mustContain(t, out, "dnas:", "the payment uri")
	mustContain(t, out, "1.25000000", "invoice new")

	// Unpaid, and it says so with a non-zero status so a script can wait on it.
	unpaid := n.cliAllowFail("invoice", "watch", "-api", n.apiAddr, "-in", "inv.json")
	mustContain(t, unpaid.out, "unpaid", "invoice watch")
	if unpaid.err == nil {
		t.Fatal("watching an unpaid invoice exited 0")
	}

	var inv struct {
		Address string `json:"address"`
		Amount  uint64 `json:"amount"`
	}
	readJSONFile(t, filepath.Join(n.dir, "inv.json"), &inv)
	n.send(inv.Address, inv.Amount, testFee)
	n.generate(1)

	// One confirmation is not settlement: the tip is the block most likely to be
	// replaced, and a merchant shipping on it has been paid reversibly.
	shallow := n.cliAllowFail("invoice", "watch", "-api", n.apiAddr, "-in", "inv.json")
	if !strings.Contains(shallow.out, "not yet") {
		t.Fatalf("a 1-confirmation payment was reported settled: %s", shallow.out)
	}
	n.generate(2)
	paid := n.cli("invoice", "watch", "-api", n.apiAddr, "-in", "inv.json")
	mustContain(t, paid, "PAID", "invoice watch")
	mustContain(t, paid, "verified against a proof-of-work chain", "invoice watch")
}

// Signing a message: proving control of an address without spending from it,
// and the domain separation that keeps such a signature from being a transfer.
func TestWalletMessageSigningEndToEnd(t *testing.T) {
	n := startNode(t, nodeOpts{name: "signmsg"})
	addr := newWallet(t, n.dir, "signer.json")

	n.cli("wallet", "sign", "-o", "signer.json", "-m", "I control this address", "-out", "sig.json")
	mustContain(t, n.cli("wallet", "verify", "-in", "sig.json", "-address", addr),
		"valid signature", "wallet verify")

	// The wrong address, a changed message, and a forged claim all fail.
	other := newWallet(t, n.dir, "other.json")
	wrong := n.cliAllowFail("wallet", "verify", "-in", "sig.json", "-address", other)
	if !strings.Contains(wrong.out, "BAD") || wrong.err == nil {
		t.Fatalf("verifying against the wrong address: %s", wrong.out)
	}
	changed := n.cliAllowFail("wallet", "verify", "-in", "sig.json", "-m", "I control everything")
	if !strings.Contains(changed.out, "BAD") {
		t.Fatalf("a changed message verified: %s", changed.out)
	}
}

// Rotating a key file's passphrase, which encryption at rest had no way to do:
// a passphrase that had been somewhere it should not could never be replaced.
func TestWalletPassphraseRotationEndToEnd(t *testing.T) {
	n := startNode(t, nodeOpts{name: "rotate"})
	addr := newWallet(t, n.dir, "rot.json")

	// Plaintext → encrypted.
	out := n.cliEnv([]string{"DNAS_NEW_WALLET_PASSPHRASE=first"}, "wallet", "passphrase", "-o", "rot.json")
	mustContain(t, out, "re-encrypted", "rotation")
	// The address must survive, or the coin is gone.
	if got := strings.TrimSpace(n.cliEnv([]string{"DNAS_WALLET_PASSPHRASE=first"},
		"wallet", "address", "-o", "rot.json")); got != addr {
		t.Fatalf("the key changed: %s, want %s", got, addr)
	}
	// Without the passphrase it says what is actually wrong.
	locked := n.cliAllowFail("wallet", "address", "-o", "rot.json")
	if !strings.Contains(locked.out, "encrypted") {
		t.Fatalf("opening an encrypted key file said: %s", locked.out)
	}
	// Encrypted → re-encrypted → plaintext.
	n.cliEnv([]string{"DNAS_WALLET_PASSPHRASE=first", "DNAS_NEW_WALLET_PASSPHRASE=second"},
		"wallet", "passphrase", "-o", "rot.json")
	mustContain(t, n.cliEnv([]string{"DNAS_WALLET_PASSPHRASE=second"},
		"wallet", "passphrase", "-o", "rot.json", "-remove"), "in the clear", "decryption")
	if got := strings.TrimSpace(n.cli("wallet", "address", "-o", "rot.json")); got != addr {
		t.Fatalf("the key changed across the whole cycle: %s, want %s", got, addr)
	}
}

// Backing up what a re-sync cannot replace.
func TestBackupEndToEnd(t *testing.T) {
	n := startNode(t, nodeOpts{name: "backup"})
	n.generate(1)
	env := []string{"DNAS_BACKUP_PASSPHRASE=hunter2"}

	out := n.cliEnv(env, "backup", "save", "-api", n.apiAddr, "-o", "bundle.json")
	mustContain(t, out, "wallet.json", "backup save")
	mustContain(t, out, "nodekey.json", "backup save")
	// The chain is deliberately not in it: it is public and re-syncs.
	if strings.Contains(out, "chain.db") {
		t.Fatalf("the backup includes the chain:\n%s", out)
	}
	mustContain(t, n.cliEnv(env, "backup", "list", "-in", "bundle.json"), "file(s)", "backup list")

	// A wrong passphrase reads nothing, and a restore refuses to clobber.
	wrong := n.cliAllowFail("backup", "list", "-in", "bundle.json")
	if wrong.err == nil {
		t.Fatal("listing a bundle without the passphrase exited 0")
	}
	clash := n.cliEnvAllowFail(env, "backup", "restore", "-in", "bundle.json")
	if !strings.Contains(clash.out, "already exist") {
		t.Fatalf("a restore over live files: %s", clash.out)
	}
	if err := os.MkdirAll(filepath.Join(n.dir, "restored"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustContain(t, n.cliEnv(env, "backup", "restore", "-in", "bundle.json", "-d", "restored"),
		"restored wallet.json", "backup restore")
}

// Inspecting a transaction before submitting it, and the reasons a valid one
// still would not be accepted.
func TestTxInspectEndToEnd(t *testing.T) {
	n := startNode(t, nodeOpts{name: "txinspect"})
	fundNode(t, n)
	dest := newWallet(t, n.dir, "dest.json")
	hash := n.send(dest, Coin, testFee)
	n.generate(1)

	out := n.cli("tx", "inspect", "-api", n.apiAddr, "-hash", hash)
	mustContain(t, out, "coin transfer", "tx inspect")
	mustContain(t, out, "single signature", "tx inspect")
	mustContain(t, out, "well-formed and correctly authorized", "tx inspect")
	mustContain(t, n.cli("tx", "verify", "-api", n.apiAddr, "-hash", hash), "valid", "tx verify")

	// A half-signed multisig file is what somebody will actually point this at.
	var keys []string
	for _, name := range []string{"a.json", "b.json"} {
		newWallet(t, n.dir, name)
		keys = append(keys, strings.TrimSpace(n.cli("wallet", "pubkey", "-o", name)))
	}
	members := strings.Join(keys, ",")
	addr := strings.TrimSpace(n.cli("multisig", "address", "-threshold", "2", "-pubkeys", members))
	n.send(addr, 2*Coin, testFee)
	n.generate(1)
	n.cli("multisig", "propose", "-api", n.apiAddr, "-threshold", "2", "-pubkeys", members,
		"-to", dest, "-amount", "all", "-o", "half.json")
	n.cli("multisig", "sign", "-wallet", "a.json", "-in", "half.json")

	half := n.cliAllowFail("tx", "inspect", "-api", n.apiAddr, "-in", "half.json")
	mustContain(t, half.out, "2-of-2 multisig", "inspect a multisig file")
	mustContain(t, half.out, "1 of 2 required signatures", "inspect a multisig file")
	if half.err == nil {
		t.Fatal("inspecting a transaction that would be refused exited 0")
	}
}

// Asset metadata: a balance of `tok3f2a…` meant nothing until the chain could
// say what that is.
func TestAssetRegistryEndToEnd(t *testing.T) {
	n := startNode(t, nodeOpts{name: "assets"})
	fundNode(t, n)

	mustContain(t, n.cli("assets", "-api", n.apiAddr), "no issued assets", "an empty registry")
	out := n.cli("spv", "-api", n.apiAddr, "wallet", "-f", "aw.json", "-key", "wallet.json",
		"issue", "GOLD", "1000")
	mustContain(t, out, "asset id", "issue")
	n.generate(1)

	list := n.cli("assets", "-api", n.apiAddr)
	mustContain(t, list, "GOLD", "asset list")
	var assets []struct {
		ID     string `json:"id"`
		Ticker string `json:"ticker"`
		Supply uint64 `json:"supply"`
	}
	n.getJSON("/assets", &assets)
	if len(assets) != 1 || assets[0].Ticker != "GOLD" || assets[0].Supply != 1000 {
		t.Fatalf("/assets = %+v", assets)
	}
	show := n.cli("assets", "show", assets[0].ID, "-api", n.apiAddr)
	mustContain(t, show, "supply  1000", "asset show")
	mustContain(t, show, "100.0%", "asset show reports the holders")
	// An unknown id is refused rather than printed as a blank asset.
	unknown := n.cliAllowFail("assets", "show", "toknotreal", "-api", n.apiAddr)
	if unknown.err == nil {
		t.Fatal("showing an unknown asset exited 0")
	}
}

// The light wallet's height window and memo: consensus fields that no client
// could set.
func TestSendOptionsEndToEnd(t *testing.T) {
	n := startNode(t, nodeOpts{name: "sendopts"})
	fundNode(t, n)
	dest := newWallet(t, n.dir, "dest.json")

	out := n.cli("spv", "-api", n.apiAddr, "wallet", "-f", "sw.json", "-key", "wallet.json",
		"-memo", "rent", "-expire-in", "50", "-lock-for", "1", "send", dest, "1")
	mustContain(t, out, `memo "rent"`, "send options")
	mustContain(t, out, "expires after height", "send options")

	var pending []struct {
		Memo      string `json:"memo"`
		Expiry    uint64 `json:"expiry"`
		LockUntil uint64 `json:"lock_until"`
	}
	n.getJSON("/mempool", &pending)
	if len(pending) != 1 || pending[0].Memo != "rent" || pending[0].Expiry == 0 || pending[0].LockUntil == 0 {
		t.Fatalf("/mempool = %+v", pending)
	}

	// An expiry already in the past is refused before anything is signed.
	past := n.cli("spv", "-api", n.apiAddr, "wallet", "-f", "sw.json", "-key", "wallet.json",
		"-expiry", "1", "send", dest, "1")
	mustContain(t, past, "can never be mined", "a past expiry")
	// So is a memo over the consensus limit.
	long := n.cli("spv", "-api", n.apiAddr, "wallet", "-f", "sw.json", "-key", "wallet.json",
		"-memo", strings.Repeat("x", 300), "send", dest, "1")
	mustContain(t, long, "the limit is", "an oversized memo")
}

// A pruning node, and the honesty it requires: a client must be able to tell
// "not in the chain" from "not visible from here".
//
// The pruning itself is unit-tested at every layer (core keeps the state and the
// headers, the API answers 410 rather than 404, the light client scans only what
// it was served). What only the binary can show is the wiring: the flag, the
// floor it is raised to, and the fields /info publishes so a client can ask
// elsewhere. It deliberately does NOT mine past the keep window — that needs 132
// blocks, and regtest cannot mint them in one go: block timestamps advance a
// second each and MaxFutureDrift stops a node roughly 120 blocks in.
func TestPruningNodeReportsWhatItHolds(t *testing.T) {
	n := startNode(t, nodeOpts{name: "prune", extra: []string{"-prune", "5"}})
	// -prune 5 is raised to the minimum, which is above the reorg depth: a node
	// that cannot reorg is not a node.
	mustContain(t, n.logs(), "pruning enabled", "the prune log line")
	mustContain(t, n.logs(), "prune raised to the minimum", "an unsafe keep is raised")

	n.generate(3)
	var info struct {
		Pruned     bool   `json:"pruned"`
		PruneKeep  uint64 `json:"prune_keep"`
		BodyHeight uint64 `json:"body_height"`
		FilterBase uint64 `json:"filter_base"`
	}
	n.getJSON("/info", &info)
	if !info.Pruned {
		t.Fatal("/info does not report that this node prunes")
	}
	if info.PruneKeep < 132 {
		t.Fatalf("prune_keep = %d, want the raised minimum", info.PruneKeep)
	}
	// Nothing is deep enough to have been dropped yet, so everything is still
	// served — the difference from a non-pruning node is only what /info says.
	if info.BodyHeight != 1 || info.FilterBase != 0 {
		t.Fatalf("body_height = %d, filter_base = %d before anything was pruned", info.BodyHeight, info.FilterBase)
	}
	if code, _ := n.get("/block/1"); code != 200 {
		t.Fatalf("a block inside the keep window returned %d", code)
	}
	// -printconfig shows it, so an operator can check before starting.
	mustContain(t, n.cli("node", "-printconfig", "-prune", "500", "-db", "pc.db"),
		"keep 500 recent bodies", "printconfig")
	// What a node does once bodies HAVE been dropped — 410 rather than 404, no
	// empty filters, a cached filter-header chain — is covered where it can be
	// driven without minting 132 blocks: api.TestPrunedNodeDistinguishesGoneFromMissing
	// and core's prune_test.go.
}

// The console: the only way to look at a node whose API is unreachable.
func TestConsoleAnswersTheReadCommands(t *testing.T) {
	n := startNode(t, nodeOpts{name: "console"})
	n.generate(1)

	script := "help\ninfo\nbalance\nassets\nsupply\nhealth\nreorgs\nprune\nwebhooks\nmine\nquit\n"
	// -console forces the prompt on: a pipe is not a terminal, so without it the
	// console is unreachable to anything but a human at a keyboard.
	out := n.cliStdin(script, "node", "-console", "-network", "regtest",
		"-db", "console.db", "-api", ":0", "-listen", ":0", "-wallet", "console-wallet.json")
	for _, want := range []string{"minted", "no assets have been issued", "NOT READY",
		"0 reorg(s)", "not pruning", "no webhooks configured", "mining is false"} {
		if !strings.Contains(out, want) {
			t.Errorf("the console output is missing %q\n%s", want, out)
		}
	}
}

// readJSONFile decodes a file the CLI wrote.
func readJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
