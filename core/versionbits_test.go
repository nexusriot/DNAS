package core

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// headersSignaling builds a synthetic header chain of the given length where
// `vote(height)` decides whether that height signals `bit`. Only Index and
// Version matter to the state machine, which is the point: a deployment's
// progress must depend on nothing but the votes already mined.
func headersSignaling(n int, bit uint8, vote func(uint64) bool) []Header {
	hs := make([]Header, n)
	for i := range hs {
		hs[i] = Header{Index: uint64(i)}
		if vote(uint64(i)) {
			hs[i].Version = SignalVersion(bit)
		}
	}
	return hs
}

func TestDeploymentValidateRejectsImpossibleTerms(t *testing.T) {
	base := Deployment{Name: UpgradeUniqueCoinbase, Bit: 1, Start: 10, Timeout: 100, Window: 10, Threshold: 8}
	if err := base.Validate(); err != nil {
		t.Fatalf("a sane deployment was rejected: %v", err)
	}

	cases := map[string]Deployment{
		"unknown upgrade":      {Name: "nosuchrule", Bit: 1, Start: 10, Timeout: 100, Window: 10, Threshold: 8},
		"bit out of range":     {Name: UpgradeUniqueCoinbase, Bit: 29, Start: 10, Timeout: 100, Window: 10, Threshold: 8},
		"zero window":          {Name: UpgradeUniqueCoinbase, Bit: 1, Start: 10, Timeout: 100, Window: 0, Threshold: 8},
		"unreachable quorum":   {Name: UpgradeUniqueCoinbase, Bit: 1, Start: 10, Timeout: 100, Window: 10, Threshold: 11},
		"timeout before start": {Name: UpgradeUniqueCoinbase, Bit: 1, Start: 10, Timeout: 12, Window: 10, Threshold: 8},
	}
	for name, d := range cases {
		if err := d.Validate(); err == nil {
			t.Errorf("%s: accepted, want rejected", name)
		}
	}
}

// A bit only counts as a vote when the version's top bits mark it as signaling.
// Otherwise an old miner emitting version 0 — or any future use of the high bits
// — would be counted as supporting changes it has never heard of.
func TestSignalsRequiresTopBits(t *testing.T) {
	if (Header{Version: 1 << 3}).Signals(3) {
		t.Error("a bare bit with no signaling marker was counted as a vote")
	}
	if !(Header{Version: SignalVersion(3)}).Signals(3) {
		t.Error("a properly marked vote was not counted")
	}
	if (Header{Version: SignalVersion(3)}).Signals(4) {
		t.Error("signaling bit 3 was read as a vote for bit 4")
	}
	if (Header{Version: SignalVersion()}).Signals(0) {
		t.Error("a signaling version that votes for nothing was read as a vote")
	}
	if (Header{Version: SignalVersion(3)}).Signals(MaxDeploymentBit + 1) {
		t.Error("an out-of-range bit was answered rather than refused")
	}
}

// The full BIP9 path: quiet, then a window that meets the threshold locks it in,
// and it goes active one whole window later — never in the window that voted, so
// every node gets the same warning before the rule changes.
func TestDeploymentLocksInThenActivatesAWindowLater(t *testing.T) {
	d := Deployment{Name: UpgradeUniqueCoinbase, Bit: 2, Start: 10, Timeout: 1000, Window: 10, Threshold: 8}
	// Heights 10..19 are the first window and vote unanimously.
	vote := func(h uint64) bool { return h >= 10 && h < 20 }

	// Mid-vote: still merely started, with no activation height yet.
	st := evaluateDeployment(headersSignaling(20, d.Bit, vote), d)
	if st.State != DeploymentStarted {
		t.Fatalf("during the first window state = %q, want %q", st.State, DeploymentStarted)
	}
	if st.Activation != 0 {
		t.Errorf("activation %d announced before the window closed", st.Activation)
	}

	// The window has closed: locked in, activating at 30.
	st = evaluateDeployment(headersSignaling(21, d.Bit, vote), d)
	if st.State != DeploymentLockedIn {
		t.Fatalf("after a unanimous window state = %q, want %q", st.State, DeploymentLockedIn)
	}
	if st.Activation != 30 {
		t.Errorf("activation = %d, want 30 (one window after lock-in at 20)", st.Activation)
	}

	// Still only locked in at height 29 — the whole warning window must elapse.
	if st := evaluateDeployment(headersSignaling(30, d.Bit, vote), d); st.State != DeploymentLockedIn {
		t.Errorf("at height 29 state = %q, want %q", st.State, DeploymentLockedIn)
	}
	st = evaluateDeployment(headersSignaling(31, d.Bit, vote), d)
	if st.State != DeploymentActive {
		t.Fatalf("at height 30 state = %q, want %q", st.State, DeploymentActive)
	}
	if st.Activation != 30 {
		t.Errorf("activation = %d, want 30", st.Activation)
	}
}

// Votes are counted per window, not cumulatively: a deployment that is popular
// but never popular enough in any single window must not creep over the line.
func TestDeploymentNeedsThresholdWithinOneWindow(t *testing.T) {
	d := Deployment{Name: UpgradeUniqueCoinbase, Bit: 2, Start: 0, Timeout: 1000, Window: 10, Threshold: 8}
	// Seven of every ten — short of the threshold, forever.
	vote := func(h uint64) bool { return h%10 < 7 }
	if st := evaluateDeployment(headersSignaling(200, d.Bit, vote), d); st.State != DeploymentStarted {
		t.Fatalf("70%% support for 20 windows reached %q, want %q", st.State, DeploymentStarted)
	}
}

func TestDeploymentFailsAtTimeout(t *testing.T) {
	d := Deployment{Name: UpgradeUniqueCoinbase, Bit: 2, Start: 0, Timeout: 50, Window: 10, Threshold: 8}
	never := func(uint64) bool { return false }
	st := evaluateDeployment(headersSignaling(80, d.Bit, never), d)
	if st.State != DeploymentFailed {
		t.Fatalf("state = %q, want %q", st.State, DeploymentFailed)
	}
	// Terminal: further blocks, even unanimous ones, cannot revive it.
	late := func(h uint64) bool { return h >= 60 }
	if st := evaluateDeployment(headersSignaling(200, d.Bit, late), d); st.State != DeploymentFailed {
		t.Fatalf("a failed deployment revived to %q", st.State)
	}
}

func TestRegisterDeploymentRefusesAContestedBit(t *testing.T) {
	t.Cleanup(ClearDeployments)
	ClearDeployments()
	first := Deployment{Name: UpgradeUniqueCoinbase, Bit: 4, Start: 0, Timeout: 100, Window: 10, Threshold: 8}
	if err := RegisterDeployment(first); err != nil {
		t.Fatal(err)
	}
	clash := Deployment{Name: UpgradeVault, Bit: 4, Start: 0, Timeout: 100, Window: 10, Threshold: 8}
	err := RegisterDeployment(clash)
	if err == nil {
		t.Fatal("two deployments were allowed to share a bit, so one vote would count for both")
	}
	if !strings.Contains(err.Error(), "already claimed") {
		t.Fatalf("unhelpful error: %v", err)
	}
	// Re-registering the SAME name is how terms are corrected before launch.
	if err := RegisterDeployment(first); err != nil {
		t.Fatalf("re-registering the same deployment failed: %v", err)
	}
}

// The end-to-end claim: a chain whose miners signal actually turns the rule on,
// through the same upgrade table every validation rule already reads.
func TestMinerVoteActivatesTheRuleOnChain(t *testing.T) {
	t.Cleanup(func() { ClearDeployments(); ClearUpgrades() })
	ClearDeployments()
	ClearUpgrades()

	d := Deployment{Name: UpgradeUniqueCoinbase, Bit: 5, Start: 1, Timeout: 1000, Window: 3, Threshold: 3}
	if err := RegisterDeployment(d); err != nil {
		t.Fatal(err)
	}

	bc := NewBlockchain()
	miner, _ := wallet.New()
	// Heights 1..3 vote; lock-in is decided at 4 and activation lands at 7.
	for h := uint64(1); h <= 6; h++ {
		mustAdd(t, bc, mineVersioned(t, bc, miner.Address(), SignalVersion(d.Bit), nil))
	}

	st := bc.DeploymentStatuses()
	if len(st) != 1 {
		t.Fatalf("got %d deployment statuses, want 1", len(st))
	}
	if st[0].Activation != 7 {
		t.Fatalf("activation = %d, want 7 (state %q)", st[0].Activation, st[0].State)
	}
	if !IsUpgradeActive(UpgradeUniqueCoinbase, 7) {
		t.Fatal("the vote carried but the upgrade table was not updated")
	}
	if IsUpgradeActive(UpgradeUniqueCoinbase, 6) {
		t.Fatal("the rule reached back below its activation height")
	}

	// And the rule really is enforced from height 7: a coinbase without the
	// height bound into it is now invalid, where it was mandatory before.
	stale := mineVersioned(t, bc, miner.Address(), SignalVersion(d.Bit), nil)
	stale.Transactions[0].Nonce = 0
	stale.MerkleRoot = MerkleRoot(stale.Transactions)
	stale.StateRoot, _ = bc.NextStateRoot(stale)
	stale, _ = Mine(stale, nil)
	if err := bc.AddBlock(stale); err == nil {
		t.Fatal("a pre-BIP34 coinbase was accepted at the activation height")
	}
	mustAdd(t, bc, mineVersioned(t, bc, miner.Address(), SignalVersion(d.Bit), nil))
	if got := bc.Tip().Transactions[0].Nonce; got != 7 {
		t.Fatalf("coinbase nonce at height 7 = %d, want 7", got)
	}
}

// A lock-in that only ever existed on a losing branch must not survive the reorg
// that discards it: the winning chain never cast those votes, so a node that
// kept the rule on would be validating against a chain nobody else is on.
func TestReorgWithdrawsALockIn(t *testing.T) {
	t.Cleanup(func() { ClearDeployments(); ClearUpgrades() })
	ClearDeployments()
	ClearUpgrades()

	d := Deployment{Name: UpgradeUniqueCoinbase, Bit: 6, Start: 1, Timeout: 1000, Window: 3, Threshold: 3}
	if err := RegisterDeployment(d); err != nil {
		t.Fatal(err)
	}

	bc := NewBlockchain()
	miner, _ := wallet.New()
	for h := uint64(1); h <= 4; h++ {
		mustAdd(t, bc, mineVersioned(t, bc, miner.Address(), SignalVersion(d.Bit), nil))
	}
	if !IsUpgradeActive(UpgradeUniqueCoinbase, 7) {
		t.Fatal("expected the vote to have locked in on the original branch")
	}

	// A longer branch from genesis whose miners never voted.
	rival := NewBlockchain()
	for h := uint64(1); h <= 6; h++ {
		mustAdd(t, rival, mineVersioned(t, rival, miner.Address(), 0, nil))
	}
	replaced, _, err := bc.ReplaceChain(rival.Blocks())
	if err != nil {
		t.Fatal(err)
	}
	if !replaced {
		t.Fatal("the longer chain was not adopted, so the reorg path went untested")
	}
	if IsUpgradeActive(UpgradeUniqueCoinbase, 7) {
		t.Fatal("a lock-in from a discarded branch survived the reorg")
	}
	if st := bc.DeploymentStatuses(); st[0].State != DeploymentStarted {
		t.Fatalf("after the reorg state = %q, want %q", st[0].State, DeploymentStarted)
	}
}

// BIP34's actual purpose: two blocks paying the same miner the same subsidy no
// longer share a transaction id, so "which block paid this" has one answer.
func TestUniqueCoinbaseGivesDistinctTxids(t *testing.T) {
	t.Cleanup(ClearUpgrades)
	ClearUpgrades()

	dup := NewCoinbase("addr", 50*Coin)
	if dup.Hash() != NewCoinbase("addr", 50*Coin).Hash() {
		t.Fatal("two identical coinbases hashed differently, so this test proves nothing")
	}

	SetUpgradeHeight(UpgradeUniqueCoinbase, 1)
	a := NewCoinbaseAt("addr", 50*Coin, 7)
	b := NewCoinbaseAt("addr", 50*Coin, 8)
	if a.Hash() == b.Hash() {
		t.Fatal("coinbases at different heights still share a txid")
	}
	// Below the activation the coinbase is byte-for-byte what it always was, so
	// an existing chain replays to the same hashes.
	if NewCoinbaseAt("addr", 50*Coin, 0).Hash() != dup.Hash() {
		t.Fatal("the rule changed a coinbase below its activation height")
	}
}

func TestCoinbaseShapeChecksHeightBinding(t *testing.T) {
	t.Cleanup(ClearUpgrades)
	ClearUpgrades()

	// Inactive: a nonce is forbidden, as it always was.
	if err := validateCoinbaseShape(Transaction{From: CoinbaseSender, To: "a", Nonce: 5}, 5); err == nil {
		t.Error("a coinbase nonce was accepted below the activation height")
	}

	SetUpgradeHeight(UpgradeUniqueCoinbase, 5)
	if err := validateCoinbaseShape(Transaction{From: CoinbaseSender, To: "a", Nonce: 5}, 5); err != nil {
		t.Errorf("a correctly bound coinbase was rejected: %v", err)
	}
	if err := validateCoinbaseShape(Transaction{From: CoinbaseSender, To: "a", Nonce: 0}, 5); err == nil {
		t.Error("an unbound coinbase was accepted at the activation height")
	}
	if err := validateCoinbaseShape(Transaction{From: CoinbaseSender, To: "a", Nonce: 4}, 5); err == nil {
		t.Error("a coinbase bound to the wrong height was accepted")
	}
}
