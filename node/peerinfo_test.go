package node

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// The node's network identity must NOT be its wallet key: the identity public
// key goes to every peer, and an address is a hash of a public key, so sharing
// them hands every peer the address holding the node's coin.
func TestIdentityIsSeparateFromTheWallet(t *testing.T) {
	w, _ := wallet.New()
	id, _ := wallet.New()

	shared := New(Config{ListenAddr: ":0"}, core.NewBlockchain(), core.NewMempool(), w)
	if !shared.IdentityIsWallet() {
		t.Fatal("a node given no identity should fall back to the wallet and admit it")
	}
	// Anyone holding the identity key can derive the wallet address — the leak.
	derived, err := wallet.AddressFromPubKeyHex(shared.IdentityKey())
	if err != nil {
		t.Fatal(err)
	}
	if derived != w.Address() {
		t.Fatal("the fallback identity should be the wallet key (that is the point of the warning)")
	}

	separate := New(Config{ListenAddr: ":0", Identity: id}, core.NewBlockchain(), core.NewMempool(), w)
	if separate.IdentityIsWallet() {
		t.Fatal("a node given its own identity still reports sharing the wallet key")
	}
	if separate.IdentityKey() != id.PublicKeyHex() {
		t.Fatal("the node is not using the identity it was given")
	}
	leaked, err := wallet.AddressFromPubKeyHex(separate.IdentityKey())
	if err != nil {
		t.Fatal(err)
	}
	if leaked == w.Address() {
		t.Fatal("the identity key still derives the wallet address")
	}
}

func TestLoadOrCreateIdentityPersists(t *testing.T) {
	path := t.TempDir() + "/nodekey.json"
	first, created, err := LoadOrCreateIdentity(path, "")
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	again, created, err := LoadOrCreateIdentity(path, "")
	if err != nil || created {
		t.Fatalf("reload: created=%v err=%v", created, err)
	}
	if first.PublicKeyHex() != again.PublicKeyHex() {
		t.Fatal("the identity changed across restarts, so peers would not recognize the node")
	}
}

func TestPeersReportsConnectionDetail(t *testing.T) {
	n, _, _ := testNode(t)
	// Two hand-built peers standing in for live connections: the fields Peers
	// reports are all set at handshake time.
	inbound := &peer{addr: "10.0.0.2:3000", ip: "10.0.0.2", id: "peer-in", version: 2,
		caps: map[string]bool{CapMempool: true, CapDandelion: true}, inbound: true}
	outbound := &peer{addr: "10.0.0.1:3000", ip: "10.0.0.1", id: "peer-out", version: 2,
		caps: map[string]bool{CapMempool: true}}
	n.addPeer(inbound)
	n.addPeer(outbound)
	n.bans.add("peer-in", banBadHeaders)

	got := n.Peers()
	if len(got) != 2 {
		t.Fatalf("Peers() = %d entries, want 2", len(got))
	}
	// Sorted by address, so the outbound 10.0.0.1 comes first.
	if got[0].Addr != "10.0.0.1:3000" || got[0].Inbound {
		t.Fatalf("first entry = %+v, want the outbound 10.0.0.1", got[0])
	}
	if got[1].Addr != "10.0.0.2:3000" || !got[1].Inbound {
		t.Fatalf("second entry = %+v, want the inbound 10.0.0.2", got[1])
	}
	if got[1].BanScore != banBadHeaders {
		t.Fatalf("ban score = %d, want %d", got[1].BanScore, banBadHeaders)
	}
	if len(got[1].Caps) != 2 || got[1].Caps[0] != CapDandelion {
		t.Fatalf("caps = %v, want them listed and sorted", got[1].Caps)
	}
	if got[0].Version != 2 || got[0].Identity != "peer-out" {
		t.Fatalf("version/identity missing: %+v", got[0])
	}
}

func TestBansListedAndCleared(t *testing.T) {
	n, _, _ := testNode(t)
	n.bans.add("noisy", banThreshold) // over the line
	n.bans.add("scored", banStalling) // under it

	entries := n.Bans()
	if len(entries) != 2 {
		t.Fatalf("Bans() = %d entries, want 2", len(entries))
	}
	if entries[0].Key != "noisy" || !entries[0].Banned {
		t.Fatalf("worst entry = %+v, want the banned one first", entries[0])
	}
	// A key under the threshold must still be listed: seeing a peer approach the
	// limit is most of the value of scoring.
	if entries[1].Key != "scored" || entries[1].Banned {
		t.Fatalf("second entry = %+v, want a listed-but-not-banned key", entries[1])
	}
	if n.BanThreshold() != banThreshold {
		t.Fatalf("BanThreshold() = %d, want %d", n.BanThreshold(), banThreshold)
	}

	if err := n.Unban("noisy"); err != nil {
		t.Fatalf("unban: %v", err)
	}
	if n.bans.banned("noisy") {
		t.Fatal("the key is still banned after being cleared")
	}
	// Clearing must zero the score, not merely dip below the threshold — one more
	// infraction would otherwise re-ban immediately.
	if score := n.bans.scoreOf("noisy"); score != 0 {
		t.Fatalf("score after unban = %d, want 0", score)
	}
	// Unbanning something that was never scored is an error, so "done" never
	// silently means "there was nothing there".
	if err := n.Unban("never-seen"); err == nil {
		t.Fatal("unbanning an unknown key reported success")
	}
	if err := n.Unban(""); err == nil {
		t.Fatal("unbanning an empty key reported success")
	}
}

func TestAddAndDropPeerValidate(t *testing.T) {
	n, _, _ := testNode(t)
	if err := n.AddPeer(""); err == nil {
		t.Fatal("an empty address was accepted")
	}
	if err := n.AddPeer(n.cfg.AdvertiseAddr); err == nil {
		t.Fatal("the node agreed to dial itself")
	}
	if _, err := n.DropPeer("nobody"); err == nil {
		t.Fatal("dropping an unknown peer reported success")
	}
	if _, err := n.DropPeer(""); err == nil {
		t.Fatal("dropping an empty match reported success")
	}
}

// A node must not stay connected to itself. Peers cannot be told apart by
// address — a node advertising ":3000" is reached as "localhost:3000" — so the
// check is on identity, and the address is remembered so the dial loop stops.
func TestSelfConnectionIsRefusedAndRemembered(t *testing.T) {
	n, _, _ := testNode(t)
	if n.book.isSelf("localhost:3000") {
		t.Fatal("an address is marked self before anything happened")
	}
	n.book.note("localhost:3000")
	if !n.book.shouldDial("localhost:3000") {
		t.Fatal("a fresh address should be dialable")
	}

	n.book.noteSelf("localhost:3000")
	if !n.book.isSelf("localhost:3000") {
		t.Fatal("the alias was not recorded")
	}
	if n.book.shouldDial("localhost:3000") {
		t.Fatal("the node would dial its own address again")
	}
	// It must also stop gossiping the alias, or peers will dial us twice.
	n.book.note("localhost:3000")
	for _, a := range n.book.all() {
		if a == "localhost:3000" {
			t.Fatal("a known-self address is still gossiped")
		}
	}
}

func TestBlocksBehindTracksTheBestKnownHeight(t *testing.T) {
	n, _, _ := testNode(t)
	if n.BlocksBehind() != 0 {
		t.Fatalf("a node nobody has told anything should be caught up, got %d", n.BlocksBehind())
	}
	n.noteBestHeight(10)
	if got := n.BlocksBehind(); got != 10 {
		t.Fatalf("BlocksBehind = %d, want 10", got)
	}
	if _, err := n.Generate(3); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got := n.BlocksBehind(); got != 7 {
		t.Fatalf("BlocksBehind after 3 blocks = %d, want 7", got)
	}
}

func TestLogLevels(t *testing.T) {
	prev := LogLevel()
	t.Cleanup(func() { SetLogLevel(prev) })

	for _, name := range []string{"error", "warn", "info", "debug", "INFO"} {
		if _, err := ParseLevel(name); err != nil {
			t.Errorf("ParseLevel(%q): %v", name, err)
		}
	}
	if _, err := ParseLevel("chatty"); err == nil {
		t.Fatal("a misspelled level was accepted, so a node would run at a verbosity nobody asked for")
	}

	SetLogLevel(LevelWarn)
	if enabled(LevelInfo) {
		t.Fatal("info should be suppressed at warn")
	}
	if !enabled(LevelError) || !enabled(LevelWarn) {
		t.Fatal("warn and error must survive at warn")
	}
	SetLogLevel(LevelDebug)
	if !enabled(LevelDebug) {
		t.Fatal("debug should be enabled at debug")
	}
	if got := LevelWarn.String(); got != "warn" {
		t.Fatalf("LevelWarn.String() = %q", got)
	}
	if got := Level(99).String(); got != "info" {
		t.Fatalf("an out-of-range level should read as info, got %q", got)
	}
}

func TestReorgHistoryRecordsAdoptedSwitches(t *testing.T) {
	n, _, _ := testNode(t)
	if rep := n.Reorgs(); rep.Total != 0 || len(rep.Reorgs) != 0 {
		t.Fatalf("a fresh node reports %d reorgs", rep.Total)
	}
	if rep := n.Reorgs(); rep.MaxDepth != core.MaxReorgDepth || rep.Capacity != reorgHistoryCapacity {
		t.Fatalf("report is missing its limits: %+v", rep)
	}

	// Mine a couple of blocks, then record a switch that discarded one of them.
	if _, err := n.Generate(2); err != nil {
		t.Fatalf("generate: %v", err)
	}
	discarded, ok := n.chain.BlockAt(2)
	if !ok {
		t.Fatal("no block at height 2")
	}
	n.noteReorg([]core.Block{discarded}, 1)

	rep := n.Reorgs()
	if rep.Total != 1 || len(rep.Reorgs) != 1 {
		t.Fatalf("report = %+v, want one reorg", rep)
	}
	r := rep.Reorgs[0]
	if r.Depth != 1 || r.ForkHeight != 1 || r.OldTip != discarded.Hash || r.Requeued != 1 {
		t.Fatalf("reorg = %+v", r)
	}
	if rep.Deepest != 1 {
		t.Fatalf("deepest = %d, want 1", rep.Deepest)
	}
	if !strings.Contains(r.At, "T") {
		t.Fatalf("timestamp %q is not RFC3339", r.At)
	}

	// An extension discards nothing and is not a reorg.
	n.noteReorg(nil, 0)
	if n.Reorgs().Total != 1 {
		t.Fatal("a plain extension was recorded as a reorg")
	}
}

// The ring is bounded: a node under a reorg storm must not grow memory with it.
func TestReorgHistoryIsBounded(t *testing.T) {
	n, _, _ := testNode(t)
	if _, err := n.Generate(2); err != nil {
		t.Fatalf("generate: %v", err)
	}
	b, _ := n.chain.BlockAt(2)
	for i := 0; i < reorgHistoryCapacity+10; i++ {
		n.noteReorg([]core.Block{b}, 0)
	}
	rep := n.Reorgs()
	if rep.Kept != reorgHistoryCapacity {
		t.Fatalf("kept %d reorgs, want the cap of %d", rep.Kept, reorgHistoryCapacity)
	}
	if rep.Total != uint64(reorgHistoryCapacity+10) {
		t.Fatalf("total = %d, want every one counted even though the ring forgot some", rep.Total)
	}
}
