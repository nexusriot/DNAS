package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// roleKeys returns three distinct wallets for buyer, seller and arbiter.
func roleKeys(t *testing.T) (buyer, seller, arbiter *wallet.Wallet) {
	t.Helper()
	ws, _ := members(t, 3)
	return ws[0], ws[1], ws[2]
}

func TestNewEscrowIsA2of3OverTheThreeRoles(t *testing.T) {
	buyer, seller, arbiter := roleKeys(t)
	e, err := newEscrow(buyer.PublicKeyHex(), seller.PublicKeyHex(), arbiter.PublicKeyHex(), "one bicycle")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	want, _ := wallet.MultisigAddress(2, []string{buyer.PublicKeyHex(), seller.PublicKeyHex(), arbiter.PublicKeyHex()})
	if e.Address != want {
		t.Fatalf("address = %s, want %s", e.Address, want)
	}
	// The payee of each direction is the role's own address, and the arbiter is
	// never one: its only power is to break a tie.
	if got, err := e.payee(roleSeller); err != nil || got != seller.Address() {
		t.Fatalf("release pays %s (%v), want the seller %s", got, err, seller.Address())
	}
	if got, err := e.payee(roleBuyer); err != nil || got != buyer.Address() {
		t.Fatalf("refund pays %s (%v), want the buyer %s", got, err, buyer.Address())
	}
}

// The mistake that silently removes the protection: one party holding two of the
// three keys is a 1-of-2 wearing a 2-of-3's clothes, and can move the coin alone.
func TestNewEscrowRefusesASharedRole(t *testing.T) {
	buyer, seller, arbiter := roleKeys(t)
	b, s, a := buyer.PublicKeyHex(), seller.PublicKeyHex(), arbiter.PublicKeyHex()
	for _, tc := range []struct {
		name    string
		b, s, a string
	}{
		{"buyer is also the seller", b, b, a},
		{"buyer is also the arbiter", b, s, b},
		{"seller is also the arbiter", b, s, s},
		{"no buyer", "", s, a},
		{"no seller", b, "", a},
		{"no arbiter", b, s, ""},
		{"malformed key", b, s, "not-a-key"},
	} {
		if _, err := newEscrow(tc.b, tc.s, tc.a, ""); err == nil {
			t.Errorf("%s was accepted", tc.name)
		}
	}
}

// A payout is a normal multisig spend, which is the point: the signatures are
// collected by the same tool, and consensus has one multisig path, not two.
func TestEscrowPayoutIsAnOrdinaryMultisigSpend(t *testing.T) {
	buyer, seller, arbiter := roleKeys(t)
	e, err := newEscrow(buyer.PublicKeyHex(), seller.PublicKeyHex(), arbiter.PublicKeyHex(), "one bicycle")
	if err != nil {
		t.Fatal(err)
	}
	payee, _ := e.payee(roleSeller)
	tx, err := buildMultisigSpend(2, e.escrowMembers(), payee, 3*core.Coin, 1000, 0, "", e.Terms)
	if err != nil {
		t.Fatal(err)
	}
	if tx.From != e.Address {
		t.Fatalf("the spend leaves %s, not the escrow %s", tx.From, e.Address)
	}

	// One party alone cannot pay themselves, whichever party it is — including the
	// arbiter, whose signature is worth no more than anyone else's.
	for name, w := range map[string]*wallet.Wallet{"buyer": buyer, "seller": seller, "arbiter": arbiter} {
		alone := tx
		alone.Signatures = nil
		if err := addMemberSignature(&alone, w); err != nil {
			t.Fatal(err)
		}
		if err := alone.VerifySignature(); err == nil {
			t.Fatalf("the %s alone could move the escrowed coin", name)
		}
	}

	// Each of the three pairings works, which is what makes the arbiter useful:
	// buyer+seller settles it, and either party plus the arbiter breaks a deadlock.
	for _, pair := range [][2]*wallet.Wallet{
		{buyer, seller}, {buyer, arbiter}, {seller, arbiter},
	} {
		signed := tx
		signed.Signatures = nil
		for _, w := range pair {
			if err := addMemberSignature(&signed, w); err != nil {
				t.Fatal(err)
			}
		}
		if err := signed.VerifySignature(); err != nil {
			t.Fatalf("a valid pairing did not verify: %v", err)
		}
	}

	// And a stranger's signature is not one of the three, however many they add.
	stranger, _ := wallet.New()
	if err := addMemberSignature(&tx, stranger); err == nil {
		t.Fatal("a stranger signed an escrow payout")
	}
}

// The file is edited by hand in practice (it is JSON with three keys in it), so
// reading it re-derives the address rather than trusting the stored one: a file
// whose address does not follow from its roles would collect signatures against
// an account the roles do not control.
func TestReadEscrowRederivesTheAddress(t *testing.T) {
	restore := core.NetworkName()
	defer core.SetNetwork(restore)
	if err := core.SetNetwork("regtest"); err != nil {
		t.Fatal(err)
	}

	buyer, seller, arbiter := roleKeys(t)
	e, err := newEscrow(buyer.PublicKeyHex(), seller.PublicKeyHex(), arbiter.PublicKeyHex(), "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "escrow.json")
	if err := writeEscrow(path, e); err != nil {
		t.Fatal(err)
	}

	// Reading it on a machine that defaults elsewhere must move to the recorded
	// network, exactly as a multisig spend file does.
	if err := core.SetNetwork("mainnet"); err != nil {
		t.Fatal(err)
	}
	back, err := readEscrow(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if core.NetworkName() != "regtest" {
		t.Fatalf("reading the escrow left this process on %s", core.NetworkName())
	}
	if back.Address != e.Address {
		t.Fatalf("address changed on the round trip: %s vs %s", back.Address, e.Address)
	}

	// A hand-edited address is caught.
	tampered := e
	tampered.Address = "dnasdeadbeef"
	writeJSON(t, path, tampered)
	if _, err := readEscrow(path); err == nil {
		t.Fatal("an address that does not follow from the roles was accepted")
	}
	// So is a role swapped for someone else's key while the address stays put:
	// the re-derivation no longer produces the stored address.
	stranger, _ := wallet.New()
	swapped := e
	swapped.Seller = stranger.PublicKeyHex()
	writeJSON(t, path, swapped)
	if _, err := readEscrow(path); err == nil {
		t.Fatal("a swapped role key was accepted against the stored address")
	}
	// And the usual file-level refusals.
	future := e
	future.Version = escrowFileVersion + 1
	writeJSON(t, path, future)
	if _, err := readEscrow(path); err == nil {
		t.Fatal("an escrow in an unknown format version was accepted")
	}
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readEscrow(path); err == nil {
		t.Fatal("an empty escrow file was accepted")
	}
}
