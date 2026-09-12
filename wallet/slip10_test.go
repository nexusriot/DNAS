package wallet

import (
	"encoding/hex"
	"strings"
	"testing"
)

// SLIP-0010's own test vector 1 for Ed25519. The point of implementing a
// standard is that another implementation produces the same keys from the same
// seed; this is the only test that can actually establish that, because it
// compares against numbers this code did not produce.
//
// Seed: 000102030405060708090a0b0c0d0e0f
func TestSLIP10MatchesTheStandardVectors(t *testing.T) {
	seed, err := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path      string
		chainCode string
		key       string
	}{
		{"m",
			"90046a93de5380a72b5e45010748567d5ea02bbf6522f979e05c0d8d8ca9fffb",
			"2b4be7f19ee27bbf30c667b642d5f4aa69fd169872f8fc3059c08ebae2eb19e7"},
		{"m/0'",
			"8b59aa11380b624e81507a27fedda59fea6d0b779a778918a2fd3590e16e9c69",
			"68e0fe46dfb67e368c75379acec591dad19df3cde26e63b93a8e704f1dade7a3"},
		{"m/0'/1'",
			"a320425f77d1b5c2505a6b1b27382b37368ee640e3557c315416801243552f14",
			"b1d0bad404bf35da785a64ca1ac54b2617211d2777696fbffaf208f746ae84f2"},
		{"m/0'/1'/2'",
			"2e69929e00b5ab250f49c3fb1c12f252de4fed2c1db88387094a0f8c4c9ccd6c",
			"92a5b23c0b8a99e37d07df3fb9966917f5d06e02ddbd909c7e184371463e9fc9"},
		{"m/0'/1'/2'/2'",
			"8f6d87f93d750e0efccda017d662a1b31a266e4a6f5993b15f5c1f07f74dd5cc",
			"30d1dc7e5fc04c31219ab25a27ae00b50f6fd66622f6e9c913253d6511d1e662"},
		{"m/0'/1'/2'/2'/1000000000'",
			"68789923a0cac2cd5a29172a475fe9e0fb14cd6adb5ad98a3fa70333e7afa230",
			"8f94d394a8e8fd6b1bc2f3f49f5c47e385281d5c17e65324b0f62483e37e8793"},
	}

	for _, c := range cases {
		indexes, err := parseDerivationPath(c.path)
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		node := masterKey(seed)
		for _, idx := range indexes {
			node = node.deriveChild(idx)
		}
		if got := hex.EncodeToString(node.key); got != c.key {
			t.Errorf("%s: key = %s, want %s", c.path, got, c.key)
		}
		if got := hex.EncodeToString(node.chainCode); got != c.chainCode {
			t.Errorf("%s: chain code = %s, want %s", c.path, got, c.chainCode)
		}
	}
}

// Ed25519 has no public derivation, so a soft index is a path copied from a
// secp256k1 wallet. Refusing it is better than hardening it silently and handing
// back keys the user did not ask for.
func TestDerivationPathRequiresHardenedLevels(t *testing.T) {
	for _, bad := range []string{"m/44", "m/44'/9999'/0", "44'/0'", "m/44'//0'", "m/x'", "m/4294967296'"} {
		if _, err := parseDerivationPath(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	for _, good := range []string{"m", "m/44'", "m/44'/9999'/0'/0'/0'", "m/44h/9999H/0'"} {
		if _, err := parseDerivationPath(good); err != nil {
			t.Errorf("%q was refused: %v", good, err)
		}
	}
	// The three hardening markers must mean the same thing.
	a, _ := parseDerivationPath("m/44'/9999'")
	b, _ := parseDerivationPath("m/44h/9999H")
	if len(a) != len(b) || a[0] != b[0] || a[1] != b[1] {
		t.Errorf("' h and H disagree: %v vs %v", a, b)
	}
}

func TestDeriveAccountMatchesItsPath(t *testing.T) {
	hd, err := HDFromMnemonic(testMnemonic(t), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ account, index uint32 }{{0, 0}, {0, 7}, {3, 2}} {
		byPath, err := hd.DerivePath(AccountPath(c.account, c.index))
		if err != nil {
			t.Fatal(err)
		}
		byAccount := hd.DeriveAccount(c.account, c.index)
		if byPath.Address() != byAccount.Address() {
			t.Errorf("account %d index %d: path gives %s, DeriveAccount gives %s",
				c.account, c.index, byPath.Address(), byAccount.Address())
		}
	}
	if got := AccountPath(3, 2); got != "m/44'/9999'/3'/0'/2'" {
		t.Errorf("AccountPath(3,2) = %q", got)
	}
}

// Different indexes and different accounts must be independent keys — the whole
// point of a tree.
func TestDerivedKeysAreDistinctAndDeterministic(t *testing.T) {
	m := testMnemonic(t)
	hd, err := HDFromMnemonic(m, "")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, c := range []struct{ account, index uint32 }{{0, 0}, {0, 1}, {0, 2}, {1, 0}, {1, 1}, {2, 5}} {
		addr := hd.DeriveAccount(c.account, c.index).Address()
		if prev, dup := seen[addr]; dup {
			t.Fatalf("account %d index %d collides with %s", c.account, c.index, prev)
		}
		seen[addr] = AccountPath(c.account, c.index)
	}

	// The same mnemonic must rebuild the same tree.
	again, err := HDFromMnemonic(m, "")
	if err != nil {
		t.Fatal(err)
	}
	if again.DeriveAccount(1, 1).Address() != hd.DeriveAccount(1, 1).Address() {
		t.Error("the same mnemonic produced different addresses")
	}
	// ...and a different passphrase must not.
	other, err := HDFromMnemonic(m, "passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if other.DeriveAccount(0, 0).Address() == hd.DeriveAccount(0, 0).Address() {
		t.Error("the passphrase did not change the tree")
	}
}

// The legacy scheme still has to reach its coin: a mnemonic written down before
// the change must not become worthless because derivation moved.
func TestLegacyDerivationStillReachesOldAddresses(t *testing.T) {
	hd, err := HDFromMnemonic(testMnemonic(t), "")
	if err != nil {
		t.Fatal(err)
	}
	legacy := hd.DeriveLegacy(0).Address()
	modern := hd.Derive(0).Address()
	if legacy == modern {
		t.Fatal("the two schemes produce the same address, so one of them is not being used")
	}
	// It is deterministic, which is all it needs to be.
	if hd.DeriveLegacy(3).Address() != hd.DeriveLegacy(3).Address() {
		t.Error("legacy derivation is not deterministic")
	}
	if hd.DeriveLegacy(3).Address() == legacy {
		t.Error("legacy derivation ignores the index")
	}
}

// Derive is account 0, which is what every client here uses.
func TestDeriveIsAccountZero(t *testing.T) {
	hd, err := HDFromMnemonic(testMnemonic(t), "")
	if err != nil {
		t.Fatal(err)
	}
	for i := uint32(0); i < 4; i++ {
		if hd.Derive(i).Address() != hd.DeriveAccount(0, i).Address() {
			t.Fatalf("Derive(%d) is not account 0 index %d", i, i)
		}
	}
}

func TestDerivePathRejectsGarbage(t *testing.T) {
	hd, err := HDFromMnemonic(testMnemonic(t), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hd.DerivePath("not a path"); err == nil {
		t.Error("garbage was accepted as a derivation path")
	}
	if _, err := hd.DerivePath("m/0"); err == nil || !strings.Contains(err.Error(), "hardened") {
		t.Errorf("a soft level gave %v, want a message about hardening", err)
	}
}

// testMnemonic is a fixed phrase, so these tests describe one tree rather than a
// different random one each run.
func testMnemonic(t *testing.T) string {
	t.Helper()
	const m = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	if err := ValidateMnemonic(m); err != nil {
		t.Fatalf("the fixed test mnemonic is not valid: %v", err)
	}
	return m
}
