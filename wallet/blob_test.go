package wallet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncryptedBlobRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bundle.json")
	secret := []byte(`{"wallet.json":"seed"}`)

	if err := SaveEncryptedBlob(path, "dnas-backup", "hunter2", secret); err != nil {
		t.Fatalf("save: %v", err)
	}
	// The plaintext must not be anywhere in the file — the whole point is that the
	// bundle can be stored somewhere the machine is not.
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), "seed") {
		t.Fatalf("the plaintext is in the file:\n%s", onDisk)
	}
	if fi, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %v, want 0600 for key material", perm)
	}

	back, kind, err := LoadEncryptedBlob(path, "hunter2")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if string(back) != string(secret) || kind != "dnas-backup" {
		t.Fatalf("round trip gave %q / %q", back, kind)
	}

	// A wrong passphrase and a tampered file must both fail, and neither may
	// return partial plaintext: AES-GCM authenticates, so a modified backup is
	// never silently restored.
	if _, _, err := LoadEncryptedBlob(path, "wrong"); err == nil {
		t.Fatal("the wrong passphrase decrypted the bundle")
	}
	tampered := strings.Replace(string(onDisk), `"ciphertext": "`, `"ciphertext": "00`, 1)
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadEncryptedBlob(path, "hunter2"); err == nil {
		t.Fatal("a tampered bundle decrypted")
	}
}

func TestEncryptedBlobRefusesToWriteInTheClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bundle.json")
	if err := SaveEncryptedBlob(path, "dnas-backup", "", []byte("secret")); err == nil {
		t.Fatal("an empty passphrase produced a backup")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a file was written anyway")
	}
}

// A backup that is overwritten while being written must not destroy the previous
// one, so the write goes through a temporary file and a rename.
func TestEncryptedBlobWriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.json")
	if err := SaveEncryptedBlob(path, "dnas-backup", "p1", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := SaveEncryptedBlob(path, "dnas-backup", "p2", []byte("second")); err != nil {
		t.Fatal(err)
	}
	back, _, err := LoadEncryptedBlob(path, "p2")
	if err != nil || string(back) != "second" {
		t.Fatalf("second write gave %q, %v", back, err)
	}
	// No temporary files left behind: a directory littered with half-written key
	// material is its own problem.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".dnas-tmp-") {
			t.Fatalf("a temporary file was left behind: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries, want just the bundle", len(entries))
	}
}

// A file that is not one of ours must be reported as such, not as a decryption
// failure, so the reader looks at the path rather than at the passphrase.
func TestLoadEncryptedBlobRejectsForeignFiles(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"plain.txt":  "hello",
		"other.json": `{"version":1,"kdf":"pbkdf2-sha256"}`,
		"future":     `{"version":99,"ciphertext":"aa","salt":"bb"}`,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadEncryptedBlob(path, "p"); err == nil {
			t.Errorf("%s was accepted as an encrypted bundle", name)
		}
	}
}

// A wallet key file is untrusted input from disk too, and GCM panics on a nonce
// of the wrong length instead of returning an error.
func TestLoadEncryptedWalletRejectsAMalformedNonce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.json")
	if err := os.WriteFile(path, []byte(
		`{"version":1,"kdf":"pbkdf2-sha256","iterations":1,"salt":"aabb","nonce":"00","ciphertext":"aabb"}`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEncrypted(path, "p"); err == nil {
		t.Fatal("a malformed encrypted wallet was accepted")
	}
}
