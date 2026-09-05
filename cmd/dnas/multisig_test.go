package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// members returns n fresh wallets and their public keys.
func members(t *testing.T, n int) ([]*wallet.Wallet, []string) {
	t.Helper()
	ws := make([]*wallet.Wallet, n)
	keys := make([]string, n)
	for i := range ws {
		w, err := wallet.New()
		if err != nil {
			t.Fatal(err)
		}
		ws[i], keys[i] = w, w.PublicKeyHex()
	}
	return ws, keys
}

// The whole point: a funded multisig account must be spendable. Consensus has
// always accepted an M-of-N spend; until `dnas multisig` there was no way to
// build one, so coin sent to a multisig address stayed there.
func TestMultisigSpendVerifiesAtTheThreshold(t *testing.T) {
	ws, keys := members(t, 3)
	dest, _ := wallet.New()

	tx, err := buildMultisigSpend(2, keys, dest.Address(), 5*core.Coin, 1000, 7, "", "rent")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	addr, _ := wallet.MultisigAddress(2, keys)
	if tx.From != addr {
		t.Fatalf("from = %s, want the multisig address %s", tx.From, addr)
	}
	if err := tx.VerifySignature(); err == nil {
		t.Fatal("an unsigned spend verified")
	}

	// One signature is not enough — that is what the threshold means.
	if err := addMemberSignature(&tx, ws[0]); err != nil {
		t.Fatalf("first signature: %v", err)
	}
	if err := tx.VerifySignature(); err == nil {
		t.Fatal("a 1-of-2 spend verified")
	}
	// The second one completes it.
	if err := addMemberSignature(&tx, ws[2]); err != nil {
		t.Fatalf("second signature: %v", err)
	}
	if err := tx.VerifySignature(); err != nil {
		t.Fatalf("a 2-of-3 spend with two member signatures does not verify: %v", err)
	}
	if err := core.CheckTxSanity(tx); err != nil {
		t.Fatalf("sanity: %v", err)
	}
}

// The two mistakes a signing tool must catch before the node does, because at
// the node they both surface as "not enough signatures" rather than as what
// actually went wrong.
func TestMultisigSigningRefusesStrangersAndDoubleSigning(t *testing.T) {
	ws, keys := members(t, 3)
	dest, _ := wallet.New()
	tx, err := buildMultisigSpend(2, keys, dest.Address(), core.Coin, 1000, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}

	stranger, _ := wallet.New()
	if err := addMemberSignature(&tx, stranger); err == nil {
		t.Fatal("a non-member was allowed to sign")
	}
	if len(tx.Signatures) != 0 {
		t.Fatalf("a refused signature was still appended: %d present", len(tx.Signatures))
	}

	if err := addMemberSignature(&tx, ws[1]); err != nil {
		t.Fatal(err)
	}
	// Consensus counts DISTINCT members, and rejects any signature matching no
	// unused one — so the same member signing twice does not make an invalid file,
	// it makes an unusable one.
	if err := addMemberSignature(&tx, ws[1]); err == nil {
		t.Fatal("the same member signed twice")
	}
	if len(tx.Signatures) != 1 {
		t.Fatalf("the duplicate was appended anyway: %d signatures", len(tx.Signatures))
	}

	// And a file with no script at all is not a multisig spend.
	plain := core.Transaction{From: "dnasx", To: dest.Address(), Amount: 1}
	if err := addMemberSignature(&plain, ws[0]); err == nil {
		t.Fatal("a transaction with no multisig script was signed as one")
	}
}

// Every signature commits to the amount, the recipient and the nonce, so a
// half-signed file is not a bearer instrument: a later signer can agree to the
// same transfer or refuse, but cannot redirect it.
func TestMultisigFileCannotBeRedirectedAfterSigning(t *testing.T) {
	ws, keys := members(t, 2)
	dest, _ := wallet.New()
	thief, _ := wallet.New()

	tx, err := buildMultisigSpend(2, keys, dest.Address(), 5*core.Coin, 1000, 3, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range ws {
		if err := addMemberSignature(&tx, w); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.VerifySignature(); err != nil {
		t.Fatalf("the complete spend does not verify: %v", err)
	}
	for _, tamper := range []func(*core.Transaction){
		func(x *core.Transaction) { x.To = thief.Address() },
		func(x *core.Transaction) { x.Amount *= 2 },
		func(x *core.Transaction) { x.Nonce++ },
		func(x *core.Transaction) { x.Fee *= 100 },
		func(x *core.Transaction) { x.Memo = "changed" },
	} {
		altered := tx
		tamper(&altered)
		if err := altered.VerifySignature(); err == nil {
			t.Fatalf("a tampered spend still verified: %+v", altered)
		}
	}
	// Swapping the script for one the tamperer controls does not help either: the
	// address is a hash of the script, so it no longer names the funded account.
	_, otherKeys := members(t, 2)
	hijacked := tx
	hijacked.Multisig = &core.MultisigScript{Threshold: 2, PubKeys: otherKeys}
	if err := hijacked.VerifySignature(); err == nil {
		t.Fatal("a spend whose script was replaced verified")
	}
}

// The member key order must not change the address, and the file must store one
// canonical order so two members can compare their copies byte for byte.
func TestMultisigSpendIsCanonicalRegardlessOfKeyOrder(t *testing.T) {
	_, keys := members(t, 3)
	dest, _ := wallet.New()

	a, err := buildMultisigSpend(2, keys, dest.Address(), core.Coin, 1000, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	shuffled := []string{keys[2], keys[0], keys[1]}
	b, err := buildMultisigSpend(2, shuffled, dest.Address(), core.Coin, 1000, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.From != b.From {
		t.Fatalf("key order changed the address: %s vs %s", a.From, b.From)
	}
	if a.Hash() != b.Hash() {
		t.Fatal("two members' copies of the same spend are not byte-identical")
	}
}

func TestMultisigSpendRejectsBadArguments(t *testing.T) {
	_, keys := members(t, 3)
	dest, _ := wallet.New()
	for _, tc := range []struct {
		name      string
		threshold int
		keys      []string
		to        string
	}{
		{"no recipient", 2, keys, ""},
		{"no members", 2, nil, dest.Address()},
		{"threshold above the member count", 4, keys, dest.Address()},
		{"threshold of zero", 0, keys, dest.Address()},
	} {
		if _, err := buildMultisigSpend(tc.threshold, tc.keys, tc.to, core.Coin, 1000, 0, "", ""); err == nil {
			t.Errorf("%s was accepted", tc.name)
		}
	}
}

// The spend file records its network because the members sign OFFLINE. Without
// it, a member whose CLI defaults to another chain signs a different message,
// and the mistake only shows up at submission with the coin still stuck.
func TestMultisigSpendFileCarriesItsNetwork(t *testing.T) {
	restore := core.NetworkName()
	defer core.SetNetwork(restore)
	if err := core.SetNetwork("regtest"); err != nil {
		t.Fatal(err)
	}

	ws, keys := members(t, 2)
	dest, _ := wallet.New()
	tx, err := buildMultisigSpend(2, keys, dest.Address(), core.Coin, 1000, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := addMemberSignature(&tx, ws[0]); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "spend.json")
	if err := writeSpend(path, tx); err != nil {
		t.Fatal(err)
	}

	// A second member's tool starts up on the default network, as an offline
	// machine would. Reading the file must move it to the right chain, so the
	// signature it then adds is over the same message as the first.
	if err := core.SetNetwork("mainnet"); err != nil {
		t.Fatal(err)
	}
	back, err := readSpend(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if core.NetworkName() != "regtest" {
		t.Fatalf("reading the file left this process on %s", core.NetworkName())
	}
	if err := addMemberSignature(&back, ws[1]); err != nil {
		t.Fatal(err)
	}
	if err := back.VerifySignature(); err != nil {
		t.Fatalf("signatures made on two machines do not agree: %v", err)
	}

	// A file naming a chain this build does not know is refused rather than
	// silently signed on whatever is loaded.
	bad := filepath.Join(t.TempDir(), "bad.json")
	writeJSON(t, bad, multisigFile{Version: multisigFileVersion, Network: "someothernet", Tx: tx})
	if _, err := readSpend(bad); err == nil {
		t.Fatal("a spend for an unknown network was accepted")
	}
	// So is one from a format version this build does not understand, and one
	// that holds no transaction at all.
	future := filepath.Join(t.TempDir(), "future.json")
	writeJSON(t, future, multisigFile{Version: multisigFileVersion + 1, Network: "regtest", Tx: tx})
	if _, err := readSpend(future); err == nil {
		t.Fatal("a spend in an unknown format version was accepted")
	}
	empty := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(empty, []byte(`{"version":1,"network":"regtest"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSpend(empty); err == nil {
		t.Fatal("a file with no transaction was accepted")
	}
}

func TestParseMembersAcceptsTheUsualSeparators(t *testing.T) {
	want := []string{"aa", "bb", "cc"}
	for _, in := range []string{"aa,bb,cc", "aa, bb, cc", "aa bb cc", "aa,bb\ncc", " aa , bb ,, cc "} {
		got := parseMembers(in)
		if len(got) != len(want) {
			t.Fatalf("%q parsed to %v", in, got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%q parsed to %v", in, got)
			}
		}
	}
	if got := parseMembers("   "); len(got) != 0 {
		t.Fatalf("blank input parsed to %v", got)
	}
}
