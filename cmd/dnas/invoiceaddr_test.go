package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// The whole point: every invoice gets a different address.
func TestInvoiceSeedDerivesAFreshAddressEachTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed.json")
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		addr, index, created, err := nextInvoiceAddress(path, "regtest")
		if err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		if created != (i == 0) {
			t.Fatalf("%d: created=%v", i, created)
		}
		if index != uint32(i) {
			t.Fatalf("%d: index=%d", i, index)
		}
		if seen[addr] {
			t.Fatalf("address %s handed out twice", addr)
		}
		if err := wallet.ValidateAddress(addr); err != nil {
			t.Fatalf("derived an invalid address: %v", err)
		}
		seen[addr] = true
	}
}

// Every address handed out must be spendable from the one phrase, or the
// merchant has been collecting payments into keys nobody holds.
func TestInvoiceAddressesAreRecoverableFromTheSeed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed.json")
	var addrs []string
	for i := 0; i < 4; i++ {
		addr, _, _, err := nextInvoiceAddress(path, "regtest")
		if err != nil {
			t.Fatal(err)
		}
		addrs = append(addrs, addr)
	}
	for i, want := range addrs {
		w, err := invoiceKeyAt(path, "regtest", 0, uint32(i))
		if err != nil {
			t.Fatalf("index %d: %v", i, err)
		}
		if w.Address() != want {
			t.Fatalf("index %d derives %s, invoice said %s", i, w.Address(), want)
		}
	}
}

// The counter must survive a restart, or a merchant who restarts starts
// re-using addresses and is back to the bug this fixes.
func TestInvoiceIndexPersistsAcrossRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed.json")
	first, _, _, err := nextInvoiceAddress(path, "regtest")
	if err != nil {
		t.Fatal(err)
	}
	// A second "process" reads the same file.
	second, index, created, err := nextInvoiceAddress(path, "regtest")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("an existing seed file was overwritten with a new mnemonic")
	}
	if index != 1 || second == first {
		t.Fatalf("index=%d second=%s first=%s", index, second, first)
	}
}

// The seed is a wallet. It must not be world-readable.
func TestInvoiceSeedFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed.json")
	if _, _, _, err := nextInvoiceAddress(path, "regtest"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("seed file mode is %o, want 600", perm)
	}
}

// Handing a mainnet payer a regtest address is a payment nobody can find, so a
// seed bound to one network must refuse to serve another.
func TestInvoiceSeedRefusesAnotherNetwork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed.json")
	if _, _, _, err := nextInvoiceAddress(path, "regtest"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := nextInvoiceAddress(path, "mainnet"); err == nil {
		t.Fatal("a regtest seed served a mainnet invoice")
	}
}

// A corrupt file must be reported, never silently replaced: minting a new
// mnemonic would orphan every address already handed out.
func TestInvoiceSeedRefusesACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := nextInvoiceAddress(path, "regtest"); err == nil {
		t.Fatal("a corrupt seed file was silently replaced")
	}
}
