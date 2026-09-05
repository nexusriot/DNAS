package main

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// describeTx is what `inspect` prints and what `verify` exits on, so it has to
// separate three different things that all look like "bad" in JSON: malformed,
// unauthorized, and perfectly valid but unminable.
func TestDescribeTxSeparatesMalformedFromUnauthorized(t *testing.T) {
	w, _ := wallet.New()
	dest, _ := wallet.New()

	good := core.Transaction{From: w.Address(), To: dest.Address(), Amount: core.Coin,
		Fee: core.DefaultMinRelayFee * 1000, Nonce: 0}
	if err := good.Sign(w); err != nil {
		t.Fatal(err)
	}
	r := describeTx(good, 10)
	if !r.ok() {
		t.Fatalf("a valid transaction reported sanity=%v sig=%v", r.SanityErr, r.SigErr)
	}
	if len(r.Warnings) != 0 {
		t.Fatalf("a valid transaction warned: %v", r.Warnings)
	}
	if r.Auth != "single signature" || !strings.Contains(r.Kind, "coin transfer") {
		t.Fatalf("kind/auth = %q / %q", r.Kind, r.Auth)
	}
	if r.FeeRate == 0 || r.Size == 0 {
		t.Fatalf("size %d, fee rate %d", r.Size, r.FeeRate)
	}

	// Unsigned: well-formed enough for the sanity rules, and not authorized.
	unsigned := good
	unsigned.Signature = ""
	if r := describeTx(unsigned, 10); r.SigErr == nil {
		t.Fatal("an unsigned transaction was reported as authorized")
	}
	// Malformed: no recipient. Both problems are reported, not just the first —
	// chasing them one at a time hides which kind of file you actually have.
	broken := core.Transaction{From: w.Address(), Fee: 1, Nonce: 0}
	r = describeTx(broken, 10)
	if r.SanityErr == nil {
		t.Fatal("a transaction with no recipient passed the sanity rules")
	}
	if r.SigErr == nil {
		t.Fatal("an unsigned malformed transaction was reported as authorized")
	}
}

// Valid, signed, and still not minable: the two cases that are invisible in the
// JSON and are the reason this command exists.
func TestDescribeTxWarnsAboutTheHeightWindow(t *testing.T) {
	w, _ := wallet.New()
	dest, _ := wallet.New()
	base := core.Transaction{From: w.Address(), To: dest.Address(), Amount: core.Coin,
		Fee: core.DefaultMinRelayFee * 1000}

	expired := base
	expired.Expiry = 5
	if err := expired.Sign(w); err != nil {
		t.Fatal(err)
	}
	r := describeTx(expired, 10) // next block would be 11, past the expiry
	if !r.ok() {
		t.Fatalf("an expired transaction is still well-formed and signed: %v / %v", r.SanityErr, r.SigErr)
	}
	if !warnsAbout(r, "expired") {
		t.Fatalf("no expiry warning: %v", r.Warnings)
	}
	// Below its expiry there is nothing to warn about.
	if r := describeTx(expired, 3); warnsAbout(r, "expired") {
		t.Fatalf("warned about an expiry that has not passed: %v", r.Warnings)
	}

	locked := base
	locked.LockUntil = 100
	if err := locked.Sign(w); err != nil {
		t.Fatal(err)
	}
	if r := describeTx(locked, 10); !warnsAbout(r, "not yet valid") {
		t.Fatalf("no lock warning: %v", r.Warnings)
	}
	// With no height (offline), the window cannot be judged and must not be
	// guessed at: an offline check that invented a verdict would be worse than
	// none.
	if r := describeTx(locked, 0); len(r.Warnings) != 0 {
		t.Fatalf("offline check warned anyway: %v", r.Warnings)
	}

	// A fee under the relay floor is the third silent rejection.
	cheap := base
	cheap.Fee = 1
	if err := cheap.Sign(w); err != nil {
		t.Fatal(err)
	}
	if r := describeTx(cheap, 10); !warnsAbout(r, "relay floor") {
		t.Fatalf("no fee-rate warning: %v", r.Warnings)
	}
}

// A half-signed multisig file is the most common thing anyone will point this at,
// since collecting the signatures takes several stops.
func TestDescribeTxReportsMultisigProgress(t *testing.T) {
	ws, keys := members(t, 3)
	dest, _ := wallet.New()
	tx, err := buildMultisigSpend(2, keys, dest.Address(), core.Coin, core.DefaultMinRelayFee*2000, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	r := describeTx(tx, 10)
	if !strings.Contains(r.Auth, "2-of-3") {
		t.Fatalf("auth = %q", r.Auth)
	}
	if !warnsAbout(r, "0 of 2") {
		t.Fatalf("no progress warning on an unsigned spend: %v", r.Warnings)
	}
	if err := addMemberSignature(&tx, ws[0]); err != nil {
		t.Fatal(err)
	}
	if r := describeTx(tx, 10); !warnsAbout(r, "1 of 2") {
		t.Fatalf("no progress warning at one signature: %v", r.Warnings)
	}
	if err := addMemberSignature(&tx, ws[1]); err != nil {
		t.Fatal(err)
	}
	r = describeTx(tx, 10)
	if !r.ok() {
		t.Fatalf("a complete spend is not ok: %v / %v", r.SanityErr, r.SigErr)
	}
	if len(r.Warnings) != 0 {
		t.Fatalf("a complete spend warned: %v", r.Warnings)
	}
}

func TestTxKindNamesEveryForm(t *testing.T) {
	w, _ := wallet.New()
	dest, _ := wallet.New()
	for _, tc := range []struct {
		want string
		tx   core.Transaction
	}{
		{"coinbase", core.NewCoinbase(w.Address(), core.Coin)},
		{"asset issuance", core.Transaction{From: w.Address(), Issue: &core.AssetIssue{Ticker: "GOLD", Supply: 10}}},
		{"asset transfer", core.Transaction{From: w.Address(), To: dest.Address(), AssetID: "abc", Amount: 5}},
		{"2 recipients", core.Transaction{From: w.Address(), Outputs: []core.Output{
			{To: dest.Address(), Amount: 1}, {To: w.Address(), Amount: 2}}}},
		{"coin transfer", core.Transaction{From: w.Address(), To: dest.Address(), Amount: core.Coin}},
	} {
		if got := txKind(tc.tx); !strings.Contains(got, tc.want) {
			t.Errorf("txKind = %q, want something containing %q", got, tc.want)
		}
	}
}

func TestTxAuthNamesEveryScript(t *testing.T) {
	w, _ := wallet.New()
	for _, tc := range []struct {
		want string
		tx   core.Transaction
	}{
		{"not signed", core.NewCoinbase(w.Address(), core.Coin)},
		{"multisig", core.Transaction{Multisig: &core.MultisigScript{Threshold: 2,
			PubKeys: []string{"a", "b", "c"}}}},
		{"HTLC claim", core.Transaction{HTLC: &core.HTLCScript{Timeout: 5}, Preimage: "aa"}},
		{"HTLC refund", core.Transaction{HTLC: &core.HTLCScript{Timeout: 5}}},
		{"vault", core.Transaction{Vault: &core.VaultScript{Unlock: 9}}},
		{"single signature", core.Transaction{From: w.Address(), To: w.Address()}},
	} {
		if got := txAuth(tc.tx); !strings.Contains(got, tc.want) {
			t.Errorf("txAuth = %q, want something containing %q", got, tc.want)
		}
	}
}

func TestLoadTxArgRefusesAnAmbiguousRequest(t *testing.T) {
	if _, _, _, err := loadTxArg("http://127.0.0.1:1", "a.json", "abc"); err == nil {
		t.Fatal("-in and -hash were both accepted")
	}
	if _, _, _, err := loadTxArg("http://127.0.0.1:1", "", ""); err == nil {
		t.Fatal("a request naming no transaction was accepted")
	}
}

// warnsAbout reports whether any warning mentions the given text.
func warnsAbout(r txReport, text string) bool {
	for _, w := range r.Warnings {
		if strings.Contains(w, text) {
			return true
		}
	}
	return false
}
