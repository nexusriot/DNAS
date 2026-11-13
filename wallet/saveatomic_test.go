package wallet

import (
	"os"
	"path/filepath"
	"testing"
)

// Saving over an existing key file must leave it owner-only even if it was
// looser before, and must not leave temporary files behind.
func TestSaveTightensPermissionsAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wallet.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for _, save := range []func() error{
		func() error { return w.Save(path) },
		func() error { return w.SaveEncrypted(path, "pass") },
	} {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := save(); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("key file mode %v after save, want 0600", st.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries after saving, want just the key file", len(entries))
	}
	got, err := LoadEncrypted(path, "pass")
	if err != nil || got.Address() != w.Address() {
		t.Fatalf("reload: %v", err)
	}
}
