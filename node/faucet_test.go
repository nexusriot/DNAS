package node

import (
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// faucetNode returns a funded node with the faucet switched on, running on a
// network that permits one.
func faucetNode(t *testing.T, cfg Config) (*Node, *wallet.Wallet) {
	t.Helper()
	prev, prevRetarget := core.NetworkName(), core.NoRetarget
	if err := core.SetNetwork(core.RegTest); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = core.SetNetwork(prev)
		core.NoRetarget = prevRetarget
	})
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ListenAddr = ":0"
	cfg.Faucet = true
	n := New(cfg, core.NewBlockchain(), core.NewMempool(), w)
	if _, err := n.Generate(core.CoinbaseMaturity + 1); err != nil {
		t.Fatalf("fund faucet: %v", err)
	}
	t.Cleanup(n.Shutdown)
	return n, w
}

func TestFaucetPays(t *testing.T) {
	n, _ := faucetNode(t, Config{})
	if !n.FaucetEnabled() {
		t.Fatal("the faucet should be enabled on regtest when the operator asks for it")
	}
	recipient, _ := wallet.New()
	tx, err := n.Faucet(recipient.Address())
	if err != nil {
		t.Fatalf("faucet: %v", err)
	}
	if tx.To != recipient.Address() || tx.Amount != n.FaucetAmount() {
		t.Fatalf("payout = %d to %s, want %d to the recipient", tx.Amount, tx.To, n.FaucetAmount())
	}
	if _, ok := n.Mempool().Get(tx.Hash()); !ok {
		t.Fatal("the faucet payment was not broadcast")
	}
	if _, err := n.Generate(1); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got := n.Chain().Balance(recipient.Address()); got != n.FaucetAmount() {
		t.Fatalf("recipient balance = %d, want %d", got, n.FaucetAmount())
	}
}

// The faucet is a property of the NETWORK, not of the node's configuration: no
// flag can make a real chain give coin away.
func TestFaucetRefusedOnMainnet(t *testing.T) {
	n, _ := faucetNode(t, Config{})
	prev, prevRetarget := core.NetworkName(), core.NoRetarget
	if err := core.SetNetwork(core.MainNet); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = core.SetNetwork(prev)
		core.NoRetarget = prevRetarget
	}()
	if n.FaucetEnabled() {
		t.Fatal("the faucet reports itself enabled on mainnet")
	}
	recipient, _ := wallet.New()
	_, err := n.Faucet(recipient.Address())
	if err == nil || !strings.Contains(err.Error(), "not available on mainnet") {
		t.Fatalf("expected a mainnet refusal, got %v", err)
	}
}

func TestFaucetDisabledUnlessAskedFor(t *testing.T) {
	n, _ := faucetNode(t, Config{})
	n.cfg.Faucet = false
	recipient, _ := wallet.New()
	if _, err := n.Faucet(recipient.Address()); err == nil {
		t.Fatal("a node with the faucet switched off paid out")
	}
}

func TestFaucetCooldownPerAddressAndRequester(t *testing.T) {
	n, _ := faucetNode(t, Config{FaucetCooldown: time.Hour})
	first, _ := wallet.New()
	second, _ := wallet.New()

	if _, err := n.FaucetFor(first.Address(), "10.0.0.1"); err != nil {
		t.Fatalf("first payout: %v", err)
	}
	// Same address, different requester: refused on the address's cooldown.
	if _, err := n.FaucetFor(first.Address(), "10.0.0.2"); err == nil {
		t.Fatal("the same address was paid twice inside the cooldown")
	}
	// Different address, same requester: refused on the requester's cooldown.
	if _, err := n.FaucetFor(second.Address(), "10.0.0.1"); err == nil {
		t.Fatal("the same requester drained the faucet through a second address")
	}
	// Different address and requester: allowed.
	if _, err := n.FaucetFor(second.Address(), "10.0.0.9"); err != nil {
		t.Fatalf("an unrelated request was refused: %v", err)
	}
}

// A refused request must not extend anyone's cooldown, or one blocked address
// could keep pushing an innocent requester's window forward.
func TestFaucetRefusalDoesNotStampCooldowns(t *testing.T) {
	n, _ := faucetNode(t, Config{FaucetCooldown: time.Hour})
	addr, _ := wallet.New()
	fresh, _ := wallet.New()
	if _, err := n.FaucetFor(addr.Address(), "10.0.0.1"); err != nil {
		t.Fatalf("first payout: %v", err)
	}
	if _, err := n.FaucetFor(addr.Address(), "10.0.0.2"); err == nil {
		t.Fatal("expected the address cooldown to refuse this")
	}
	// 10.0.0.2 was never paid, so it must still be eligible.
	if _, err := n.FaucetFor(fresh.Address(), "10.0.0.2"); err != nil {
		t.Fatalf("a requester refused on someone else's cooldown was stamped anyway: %v", err)
	}
}

func TestFaucetRejectsBadAddressAndItself(t *testing.T) {
	n, w := faucetNode(t, Config{})
	if _, err := n.Faucet("not-an-address"); err == nil {
		t.Fatal("the faucet accepted a malformed address")
	}
	if _, err := n.Faucet(w.Address()); err == nil {
		t.Fatal("the faucet paid itself")
	}
}

func TestFaucetRefusesWhenDry(t *testing.T) {
	prev, prevRetarget := core.NetworkName(), core.NoRetarget
	if err := core.SetNetwork(core.RegTest); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = core.SetNetwork(prev)
		core.NoRetarget = prevRetarget
	}()
	w, _ := wallet.New()
	n := New(Config{ListenAddr: ":0", Faucet: true}, core.NewBlockchain(), core.NewMempool(), w)
	t.Cleanup(n.Shutdown)
	recipient, _ := wallet.New()
	_, err := n.Faucet(recipient.Address())
	if err == nil || !strings.Contains(err.Error(), "dry") {
		t.Fatalf("expected a dry-faucet refusal, got %v", err)
	}
}

// Regression: the faucet must price its fee against the node's RELAY FLOOR as
// well as the consensus base fee. The two move independently — the base fee
// decays toward MinBaseFee on an idle chain while the floor stays at its
// configured base — so budgeting off the base fee alone produced a payout the
// node's own mempool refused to relay.
func TestFaucetFeeClearsTheRelayFloor(t *testing.T) {
	prev, prevRetarget := core.NetworkName(), core.NoRetarget
	if err := core.SetNetwork(core.RegTest); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = core.SetNetwork(prev)
		core.NoRetarget = prevRetarget
	}()
	w, _ := wallet.New()
	// A pool with the production relay floor, which is what a real node runs.
	mp := core.NewMempoolWithPolicy(core.DefaultMempoolSize, core.DefaultMinRelayFee)
	n := New(Config{ListenAddr: ":0", Faucet: true}, core.NewBlockchain(), mp, w)
	t.Cleanup(n.Shutdown)
	if _, err := n.Generate(core.CoinbaseMaturity + 2); err != nil {
		t.Fatalf("fund faucet: %v", err)
	}

	recipient, _ := wallet.New()
	tx, err := n.Faucet(recipient.Address())
	if err != nil {
		t.Fatalf("faucet: %v", err)
	}
	if floor := mp.MinFee(); tx.Fee < floor*uint64(tx.Size()) {
		t.Fatalf("faucet fee %d is below the relay floor %d/byte × %d bytes", tx.Fee, floor, tx.Size())
	}
	if _, ok := mp.Get(tx.Hash()); !ok {
		t.Fatal("the faucet payment was refused by the node's own mempool")
	}
}
