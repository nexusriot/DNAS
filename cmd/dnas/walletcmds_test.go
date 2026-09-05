package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

func TestVerifySignedMessageChecksTheClaimedAddress(t *testing.T) {
	w, _ := wallet.New()
	body := []byte("I control this address")
	sm := signedMessage{
		Version: signedMessageVersion, Address: w.Address(),
		PubKey: w.PublicKeyHex(), Signature: w.SignMessage(body), Message: string(body),
	}
	addr, err := verifySignedMessage(sm, body)
	if err != nil || addr != w.Address() {
		t.Fatalf("verify gave %q, %v", addr, err)
	}

	// The address in the file is a CLAIM, and a forgery is exactly a file whose
	// claimed address is not the one that signed it. Reporting the claim rather
	// than the derived address would endorse the forgery.
	forged := sm
	forged.Address = "dnasdeadbeef"
	if _, err := verifySignedMessage(forged, body); err == nil {
		t.Fatal("a signature claiming another address verified")
	}
	// A file with no claim at all still verifies, and reports whose it is.
	unclaimed := sm
	unclaimed.Address = ""
	if got, err := verifySignedMessage(unclaimed, body); err != nil || got != w.Address() {
		t.Fatalf("an unclaimed signature gave %q, %v", got, err)
	}
	// The signature must cover the message it is presented with.
	if _, err := verifySignedMessage(sm, []byte("I control everything")); err == nil {
		t.Fatal("the signature verified over a different message")
	}
	// And a format this build does not understand is refused rather than
	// interpreted optimistically.
	future := sm
	future.Version = signedMessageVersion + 1
	if _, err := verifySignedMessage(future, body); err == nil {
		t.Fatal("a signature in an unknown format version verified")
	}
}

// The property that makes `dnas wallet sign` safe to use on an untrusted prompt:
// a message signature is not a transaction signature.
func TestAMessageSignatureIsNotATransactionSignature(t *testing.T) {
	w, _ := wallet.New()
	victim, _ := wallet.New()
	tx := core.Transaction{From: w.Address(), To: victim.Address(), Amount: 5 * core.Coin,
		Fee: 1000, Nonce: 0, PubKey: w.PublicKeyHex()}

	// Someone asks for "a signature proving you own your address" and hands over
	// the transaction's own signing bytes as the message. The signature they get
	// back must not authorize the transfer.
	tx.Signature = w.SignMessage(tx.SigningMessage())
	if err := tx.VerifySignature(); err == nil {
		t.Fatal("a message signature authorized a transaction")
	}
	// And the reverse: a transaction signature must not pass as a message
	// signature over the same bytes.
	if err := tx.Sign(w); err != nil {
		t.Fatal(err)
	}
	if _, err := wallet.VerifyMessage(w.PublicKeyHex(), tx.Signature, tx.SigningMessage()); err == nil {
		t.Fatal("a transaction signature verified as a message signature")
	}
}

func TestMessageBodyResolvesItsSources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "msg.txt")
	if err := os.WriteFile(path, []byte("from a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if body, _, err := messageBody("inline", ""); err != nil || string(body) != "inline" {
		t.Fatalf("-m gave %q, %v", body, err)
	}
	if body, _, err := messageBody("", path); err != nil || string(body) != "from a file" {
		t.Fatalf("-file gave %q, %v", body, err)
	}
	// Both at once is a contradiction: one of them would be silently ignored, and
	// the signature would cover something other than what was asked for.
	if _, _, err := messageBody("inline", path); err == nil {
		t.Fatal("-m and -file were both accepted")
	}
	if _, _, err := messageBody("", filepath.Join(dir, "absent")); err == nil {
		t.Fatal("a missing file was accepted")
	}
}

// Rotation rewrites the ONLY copy of a key, so the round trip has to preserve
// the address exactly — an address change here means the coin is gone.
func TestPassphraseRotationPreservesTheKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.json")
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Save(path); err != nil {
		t.Fatal(err)
	}
	want := w.Address()

	// Plaintext → encrypted → re-encrypted → plaintext again, which is the whole
	// cycle the command supports. Each step opens the file as the previous one
	// left it, so a rotation that only appeared to work would be caught here.
	current := ""
	for _, next := range []string{"first", "second", ""} {
		loaded, err := loadWalletWith(path, current)
		if err != nil {
			t.Fatalf("open with %q: %v", current, err)
		}
		if err := saveWalletWith(loaded, path, next); err != nil {
			t.Fatalf("save under %q: %v", next, err)
		}
		back, err := loadWalletWith(path, next)
		if err != nil {
			t.Fatalf("reopen under %q: %v", next, err)
		}
		if back.Address() != want {
			t.Fatalf("the key changed: %s, want %s", back.Address(), want)
		}
		current = next
	}
	// An encrypted file opened with no passphrase must fail rather than produce
	// some other key.
	if err := saveWalletWith(w, path, "final"); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWalletWith(path, ""); err == nil {
		t.Fatal("an encrypted key file opened without a passphrase")
	}
	if _, err := loadWalletWith(path, "wrong"); err == nil {
		t.Fatal("the wrong passphrase opened the key file")
	}
}

func TestCollectBackupTakesTheIrreplaceableFilesOnly(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("wallet.json", "the key")
	write("nodekey.json", "the identity")
	write("chain.db", strings.Repeat("blocks", 1000))
	write("notes.txt", "unrelated")

	bundle, err := collectBackup(dir, nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if _, ok := bundle.Files["chain.db"]; ok {
		t.Fatal("the chain was backed up; it is public and re-syncs")
	}
	if _, ok := bundle.Files["notes.txt"]; ok {
		t.Fatal("an unrelated file was backed up")
	}
	for _, want := range []string{"wallet.json", "nodekey.json"} {
		if _, ok := bundle.Files[want]; !ok {
			t.Fatalf("%s was not backed up", want)
		}
	}
	// A file that does not exist is skipped rather than failing the backup: a node
	// that never ran a light wallet has no spvwallet.json, and that is normal.
	if _, ok := bundle.Files["spvwallet.json"]; ok {
		t.Fatal("a nonexistent file appeared in the bundle")
	}
	// A key file under ANY name is found by content, because `-wallet mine.json`
	// is ordinary and a backup that silently omitted somebody's actual wallet
	// would be worse than no backup — it would be trusted.
	write("mine.json", `{"seed":"aabb","address":"dnasx"}`)
	write("cold.json", `{"version":1,"ciphertext":"ff","salt":"ee","address":"dnasy"}`)
	write("spvwallet-old.json", `{"addresses":["dnasz"]}`) // not a key file
	bundle, err = collectBackup(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mine.json", "cold.json"} {
		if _, ok := bundle.Files[want]; !ok {
			t.Errorf("%s was not recognized as a key file", want)
		}
	}
	if _, ok := bundle.Files["spvwallet-old.json"]; ok {
		t.Error("a file that is not a key file was swept in by name")
	}
	// An earlier encrypted BUNDLE has the same cipher fields as an encrypted key
	// file. Backing up the backup is pointless and nests on every run, so it is
	// told apart by its `kind`.
	write("bundle.json", `{"version":1,"kind":"dnas-backup","ciphertext":"ff","salt":"ee"}`)
	bundle, err = collectBackup(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bundle.Files["bundle.json"]; ok {
		t.Error("a previous backup bundle was backed up again")
	}
	if _, ok := bundle.Files["chain.db"]; ok {
		t.Fatal("the chain was swept in")
	}

	// -add takes anything else the operator knows matters.
	bundle, err = collectBackup(dir, []string{"notes.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Files["notes.txt"] != "unrelated" {
		t.Fatal("-add did not include the named file")
	}
	// An empty directory is an error, not an empty backup that looks like success.
	if _, err := collectBackup(t.TempDir(), nil); err == nil {
		t.Fatal("a backup of nothing was reported as a backup")
	}
}

func TestBackupBundleRoundTripThroughEncryption(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wallet.json"), []byte("the key"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := collectBackup(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bundle.json")
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := wallet.SaveEncryptedBlob(path, "dnas-backup", "pass", data); err != nil {
		t.Fatal(err)
	}
	back, err := readBackup(path, "pass")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if back.Files["wallet.json"] != "the key" {
		t.Fatalf("round trip gave %+v", back.Files)
	}
	if _, err := readBackup(path, "wrong"); err == nil {
		t.Fatal("the wrong passphrase read the bundle")
	}
	// A file of the right shape but the wrong kind is not a backup.
	other := filepath.Join(dir, "other.json")
	if err := wallet.SaveEncryptedBlob(other, "something-else", "pass", data); err != nil {
		t.Fatal(err)
	}
	if _, err := readBackup(other, "pass"); err == nil {
		t.Fatal("a bundle of another kind was read as a backup")
	}
}
