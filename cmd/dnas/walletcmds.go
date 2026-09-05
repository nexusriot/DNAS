package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// Three things a wallet has to be able to do that had no command.

// readLine reads one line from stdin, so a secret can be piped or typed rather
// than passed as an argument (where it would land in shell history and in `ps`).
func readLine() (string, error) {
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return "", err
		}
		return "", errors.New("nothing read from stdin")
	}
	fmt.Println()
	return sc.Text(), nil
}

// --- proving control of an address without spending from it ---

// signedMessage is the portable form of a message signature: everything a
// verifier needs except the message, which they already have (or which is
// carried inline for short ones).
type signedMessage struct {
	Version   int    `json:"version"`
	Address   string `json:"address"`
	PubKey    string `json:"pubkey"`
	Signature string `json:"signature"`
	Message   string `json:"message,omitempty"` // inline for short text; omitted for a file
	File      string `json:"file,omitempty"`
	SHA256    string `json:"sha256,omitempty"` // of the file, so the right one is checked
}

const signedMessageVersion = 1

// walletSign implements `dnas wallet sign`, and walletVerify its counterpart.
//
// Proving control of an address is a routine request — an exchange asking you to
// confirm a withdrawal address, a counterparty asking whether an address is
// really yours — and doing it by sending a transaction costs a fee and leaks a
// payment onto the chain for no reason.
//
// The one rule that makes this safe is in wallet.MessagePreimage: a message
// signature is domain-separated from a transaction signature, so a request to
// "just sign this to prove it's you" cannot smuggle in a transfer.
func walletSign(w *wallet.Wallet, args []string) {
	fs := flag.NewFlagSet("wallet sign", flag.ExitOnError)
	msg := fs.String("m", "", "message to sign")
	file := fs.String("file", "", "sign the contents of this file instead of -m")
	out := fs.String("out", "", "write the signature to this file (default: stdout)")
	_ = fs.Parse(args)

	body, source, err := messageBody(*msg, *file)
	if err != nil {
		log.Fatal(err)
	}
	sm := signedMessage{
		Version:   signedMessageVersion,
		Address:   w.Address(),
		PubKey:    w.PublicKeyHex(),
		Signature: w.SignMessage(body),
	}
	if *file != "" {
		sm.File = *file
		if sm.SHA256, err = hashFile(*file); err != nil {
			log.Fatal(err)
		}
	} else {
		sm.Message = string(body)
	}
	data, err := json.MarshalIndent(sm, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	data = append(data, '\n')
	if *out == "" {
		fmt.Print(string(data))
		fmt.Fprintf(os.Stderr, "signed %s as %s\n", source, w.Address())
		return
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}
	fmt.Printf("signed %s as %s -> %s\n", source, w.Address(), *out)
}

// messageBody resolves the message to sign or verify, and a phrase describing it.
func messageBody(msg, file string) ([]byte, string, error) {
	switch {
	case msg != "" && file != "":
		return nil, "", errors.New("give -m or -file, not both")
	case file != "":
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, "", err
		}
		return data, fmt.Sprintf("%s (%d bytes)", file, len(data)), nil
	case msg != "":
		return []byte(msg), fmt.Sprintf("%q", msg), nil
	default:
		// Reading stdin makes this composable, and keeps a long message out of the
		// shell history and out of `ps`.
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, "", err
		}
		if len(data) == 0 {
			return nil, "", errors.New("nothing to sign: pass -m TEXT, -file F, or pipe it in")
		}
		return data, fmt.Sprintf("%d bytes from stdin", len(data)), nil
	}
}

// verifySignedMessage is the check itself: the signature must verify, and the
// address it proves must be the one the file claims. Separated from the command
// so it can be unit-tested.
func verifySignedMessage(sm signedMessage, body []byte) (string, error) {
	if sm.Version != signedMessageVersion {
		return "", fmt.Errorf("this signature is format version %d, and this build understands %d",
			sm.Version, signedMessageVersion)
	}
	addr, err := wallet.VerifyMessage(sm.PubKey, sm.Signature, body)
	if err != nil {
		return "", err
	}
	// The stored address is informational, so it has to be CHECKED rather than
	// reported: a file claiming an address whose key did not sign it is exactly
	// what a forgery looks like, and printing the claim would endorse it.
	if sm.Address != "" && sm.Address != addr {
		return "", fmt.Errorf("this signature is from %s, but the file claims %s", short(addr), short(sm.Address))
	}
	return addr, nil
}

func walletVerify(args []string) {
	fs := flag.NewFlagSet("wallet verify", flag.ExitOnError)
	in := fs.String("in", "", "signature file from `dnas wallet sign` (required)")
	msg := fs.String("m", "", "the message it should cover (default: the one in the file)")
	file := fs.String("file", "", "the file it should cover")
	expect := fs.String("address", "", "the address you expect it to prove")
	_ = fs.Parse(args)
	if *in == "" {
		log.Fatal("wallet verify: -in is required")
	}
	data, err := os.ReadFile(*in)
	if err != nil {
		log.Fatalf("read %s: %v", *in, err)
	}
	var sm signedMessage
	if err := json.Unmarshal(data, &sm); err != nil {
		log.Fatalf("%s is not a signature file: %v", *in, err)
	}

	// What was signed: the caller's message if they gave one, the file the
	// signature names, or the message carried inline.
	var body []byte
	switch {
	case *msg != "" || *file != "":
		if body, _, err = messageBody(*msg, *file); err != nil {
			log.Fatal(err)
		}
	case sm.File != "":
		if body, err = os.ReadFile(sm.File); err != nil {
			log.Fatalf("this signature covers %s, which cannot be read: %v", sm.File, err)
		}
	case sm.Message != "":
		body = []byte(sm.Message)
	default:
		log.Fatal("this signature carries no message; say what it should cover with -m or -file")
	}
	// A file signature records the digest, so presenting a DIFFERENT file with the
	// same name fails on the digest rather than on a confusing signature error.
	if sm.SHA256 != "" && sm.File != "" && (*msg == "" && *file == "") {
		digest, err := hashFile(sm.File)
		if err != nil {
			log.Fatal(err)
		}
		if digest != sm.SHA256 {
			fmt.Printf("BAD: %s has changed since it was signed\n  now    %s\n  signed %s\n",
				sm.File, digest, sm.SHA256)
			os.Exit(1)
		}
	}

	addr, err := verifySignedMessage(sm, body)
	if err != nil {
		fmt.Println("BAD:", err)
		os.Exit(1)
	}
	if *expect != "" && *expect != addr {
		fmt.Printf("BAD: valid signature, but from %s and not the expected %s\n", addr, *expect)
		os.Exit(1)
	}
	fmt.Printf("✓ valid signature by %s\n", addr)
	if sm.File != "" {
		fmt.Printf("  over %s (sha256 %s)\n", sm.File, short(sm.SHA256))
	}
}

// --- changing (or removing, or adding) the passphrase on a key file ---

// walletPassphraseCmd implements `dnas wallet passphrase`.
//
// Encryption at rest existed and was one-way: DNAS_WALLET_PASSPHRASE decided how
// a file was written when it was created, and there was no way to change it
// afterwards. So a passphrase typed into a shared terminal, or one that has been
// somewhere it should not, could not be replaced — and a plaintext key file
// could never be encrypted without recreating the wallet, which changes the
// address.
//
// The old passphrase comes from the environment (as everywhere else) and the new
// one from stdin, so it does not land in shell history or in `ps`.
func walletPassphraseCmd(path string, args []string) {
	fs := flag.NewFlagSet("wallet passphrase", flag.ExitOnError)
	remove := fs.Bool("remove", false, "decrypt the file instead: store the key in the clear")
	_ = fs.Parse(args)

	old := walletPassphrase()
	w, err := loadWalletWith(path, old)
	if err != nil {
		log.Fatalf("open %s: %v", path, err)
	}
	before := w.Address()

	var next string
	if !*remove {
		if next, err = readNewPassphrase(); err != nil {
			log.Fatal(err)
		}
	}
	if next == old && !*remove {
		log.Fatal("that is the passphrase it already has")
	}

	// Write, then read back and compare the address BEFORE reporting success. A
	// re-encryption that silently produced an unopenable file would destroy the
	// key: there is no other copy, and the old ciphertext is already gone.
	if err := saveWalletWith(w, path, next); err != nil {
		log.Fatalf("write %s: %v", path, err)
	}
	back, err := loadWalletWith(path, next)
	if err != nil {
		log.Fatalf("the rewritten file cannot be opened (%v) — restore your backup", err)
	}
	if back.Address() != before {
		log.Fatalf("the rewritten file holds a different key (%s, was %s) — restore your backup",
			back.Address(), before)
	}
	if *remove {
		fmt.Printf("%s is now stored in the clear (address %s)\n", path, before)
		fmt.Println("unset DNAS_WALLET_PASSPHRASE for this file from now on")
		return
	}
	fmt.Printf("%s re-encrypted under the new passphrase (address %s)\n", path, before)
	fmt.Println("set DNAS_WALLET_PASSPHRASE to the new value from now on")
}

// loadWalletWith opens a key file, encrypted or not according to passphrase.
func loadWalletWith(path, passphrase string) (*wallet.Wallet, error) {
	if passphrase == "" {
		return wallet.Load(path)
	}
	return wallet.LoadEncrypted(path, passphrase)
}

// saveWalletWith is its counterpart.
func saveWalletWith(w *wallet.Wallet, path, passphrase string) error {
	if passphrase == "" {
		return w.Save(path)
	}
	return w.SaveEncrypted(path, passphrase)
}

// readNewPassphrase reads a new passphrase twice from stdin and requires the two
// to match — a typo here is unrecoverable, because it encrypts the only copy of
// the key under a value nobody knows.
func readNewPassphrase() (string, error) {
	if env := os.Getenv("DNAS_NEW_WALLET_PASSPHRASE"); env != "" {
		return env, nil
	}
	fmt.Print("new passphrase: ")
	first, err := readLine()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(first) == "" {
		return "", errors.New("an empty passphrase is not encryption; use -remove if that is what you want")
	}
	fmt.Print("again: ")
	second, err := readLine()
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errors.New("the two passphrases do not match")
	}
	return first, nil
}

// --- backing up the part of a node's directory that cannot be re-derived ---

// backupBundle is what `dnas backup` writes, inside one encrypted file.
type backupBundle struct {
	Version int               `json:"version"`
	Network string            `json:"network"`
	Files   map[string]string `json:"files"` // name → contents
}

const backupBundleVersion = 1

// backupNames are the files worth backing up, in the order they are reported.
//
// The chain is deliberately NOT among them: it is public, every peer has it, and
// a node re-downloads it. What cannot be re-derived is key material and the
// records that are only local — the signing key, the node's identity, a light
// wallet's watch list, an escrow's role assignments. That is a few kilobytes,
// which is why this can be one encrypted file rather than an archive format.
var backupNames = []string{
	"wallet.json",
	"nodekey.json",
	"spvwallet.json",
	"escrow.json",
}

func runBackup(args []string) {
	if len(args) == 0 {
		fmt.Println(`usage: dnas backup <save | list | restore> [flags]
  save    [-d DIR] [-o FILE] [-add F]   encrypt the irreplaceable files into one bundle
  list    -in FILE                      what a bundle holds (decrypts it; writes nothing)
  restore -in FILE [-d DIR]             write them back, refusing to overwrite
the passphrase comes from DNAS_BACKUP_PASSPHRASE, or is asked for on stdin.
the chain is not included: it is public and re-syncs. Keys do not.`)
		return
	}
	switch args[0] {
	case "save":
		backupSave(args[1:])
	case "list":
		backupList(args[1:])
	case "restore":
		backupRestore(args[1:])
	default:
		fmt.Println("unknown backup command:", args[0], "(save | list | restore)")
	}
}

// backupPassphrase is the passphrase for the bundle, which is deliberately NOT
// the wallet's: a backup usually leaves the machine, and the key file's
// passphrase should not have to.
func backupPassphrase(confirm bool) (string, error) {
	if env := os.Getenv("DNAS_BACKUP_PASSPHRASE"); env != "" {
		return env, nil
	}
	if !confirm {
		fmt.Print("backup passphrase: ")
		return readLine()
	}
	return readNewPassphraseFor("backup")
}

// readNewPassphraseFor asks twice for a passphrase that will encrypt something
// new, naming what it is for.
func readNewPassphraseFor(what string) (string, error) {
	fmt.Printf("new %s passphrase: ", what)
	first, err := readLine()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(first) == "" {
		return "", errors.New("an empty passphrase would leave the keys in the clear")
	}
	fmt.Print("again: ")
	second, err := readLine()
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errors.New("the two passphrases do not match")
	}
	return first, nil
}

// collectBackup reads the files that exist into a bundle. Pure enough to test:
// it takes a directory and returns what it found, and complains about nothing
// except finding nothing at all.
//
// Besides the known names it SNIFFS the directory for anything that is a key
// file under another name. The default names are only defaults — `-wallet
// mine.json` is ordinary — and a backup that silently omitted somebody's actual
// wallet because it was not called `wallet.json` would be worse than no backup,
// since it would be trusted.
func collectBackup(dir string, extra []string) (backupBundle, error) {
	bundle := backupBundle{Version: backupBundleVersion, Network: core.NetworkName(), Files: map[string]string{}}
	names := append(append([]string(nil), backupNames...), extra...)
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			if os.IsNotExist(err) {
				continue // a node that never ran an SPV wallet has no spvwallet.json
			}
			return backupBundle{}, err
		}
		bundle.Files[name] = string(data)
	}
	for name, content := range keyFilesIn(dir) {
		if _, already := bundle.Files[name]; !already {
			bundle.Files[name] = content
		}
	}
	if len(bundle.Files) == 0 {
		return backupBundle{}, fmt.Errorf("found nothing to back up in %s", dir)
	}
	return bundle, nil
}

// keyFilesIn finds the key files in a directory whatever they are called, by
// content rather than by name: a plaintext key file carries a hex seed, an
// encrypted one carries a ciphertext and a salt. Nothing else in a node's
// directory looks like either.
//
// It reads only small files, so it cannot be turned into "back up the chain" by
// a file that happens to be named plausibly.
func keyFilesIn(dir string) map[string]string {
	found := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return found
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if info, err := e.Info(); err != nil || info.Size() > maxKeyFileBytes {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var probe struct {
			Seed       string `json:"seed"`
			Ciphertext string `json:"ciphertext"`
			Salt       string `json:"salt"`
			Kind       string `json:"kind"` // present only on an encrypted BLOB
		}
		if json.Unmarshal(data, &probe) != nil {
			continue
		}
		// An encrypted backup bundle has the same cipher fields as an encrypted
		// key file and is told apart by its `kind`. Sweeping one in would back up
		// the backup — pointless, and it nests on every run.
		if probe.Kind != "" {
			continue
		}
		if probe.Seed != "" || (probe.Ciphertext != "" && probe.Salt != "") {
			found[e.Name()] = string(data)
		}
	}
	return found
}

// maxKeyFileBytes bounds what the sniff above will read. A key file is a few
// hundred bytes; this is generous and still nowhere near a chain.
const maxKeyFileBytes = 64 << 10

// sortedNames lists a bundle's files in a stable order, so two runs of `list`
// print the same thing.
func sortedNames(b backupBundle) []string {
	names := make([]string, 0, len(b.Files))
	for name := range b.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func backupSave(args []string) {
	fs := flag.NewFlagSet("backup save", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address, asked which network this is")
	dir := fs.String("d", ".", "directory holding the files")
	out := fs.String("o", "dnas-backup.json", "bundle to write")
	add := fs.String("add", "", "also include these files, comma-separated")
	_ = fs.Parse(args)

	// The keys themselves are network-independent, but a light wallet's state is
	// not, so the bundle records which chain it was taken on — asked of the node
	// rather than guessed, so a restore onto the wrong network says so.
	adoptNetwork(ensureHTTP(*apiAddr))
	bundle, err := collectBackup(*dir, parseMembers(*add))
	if err != nil {
		log.Fatal(err)
	}
	pass, err := backupPassphrase(true)
	if err != nil {
		log.Fatal(err)
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		log.Fatal(err)
	}
	if err := wallet.SaveEncryptedBlob(*out, "dnas-backup", pass, data); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}
	// Read it back before saying it worked: an unopenable backup discovered at
	// restore time is worse than no backup, because it was trusted in between.
	if _, err := readBackup(*out, pass); err != nil {
		log.Fatalf("the bundle just written cannot be read back: %v", err)
	}
	fmt.Printf("wrote %s (encrypted, %d file(s)):\n", *out, len(bundle.Files))
	for _, name := range sortedNames(bundle) {
		fmt.Printf("  %s (%d bytes)\n", name, len(bundle.Files[name]))
	}
	fmt.Println("the chain is not in here — it re-syncs. Keep this somewhere the machine is not.")
}

func readBackup(path, pass string) (backupBundle, error) {
	plain, kind, err := wallet.LoadEncryptedBlob(path, pass)
	if err != nil {
		return backupBundle{}, err
	}
	if kind != "dnas-backup" {
		return backupBundle{}, fmt.Errorf("this file holds %q, not a backup", kind)
	}
	var bundle backupBundle
	if err := json.Unmarshal(plain, &bundle); err != nil {
		return backupBundle{}, fmt.Errorf("the decrypted bundle is not readable: %w", err)
	}
	if bundle.Version != backupBundleVersion {
		return backupBundle{}, fmt.Errorf("this bundle is format version %d, and this build understands %d",
			bundle.Version, backupBundleVersion)
	}
	return bundle, nil
}

func backupList(args []string) {
	fs := flag.NewFlagSet("backup list", flag.ExitOnError)
	in := fs.String("in", "dnas-backup.json", "bundle to read")
	_ = fs.Parse(args)
	pass, err := backupPassphrase(false)
	if err != nil {
		log.Fatal(err)
	}
	bundle, err := readBackup(*in, pass)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s: %d file(s), taken on %s\n", *in, len(bundle.Files), bundle.Network)
	for _, name := range sortedNames(bundle) {
		fmt.Printf("  %s (%d bytes)\n", name, len(bundle.Files[name]))
	}
}

func backupRestore(args []string) {
	fs := flag.NewFlagSet("backup restore", flag.ExitOnError)
	in := fs.String("in", "dnas-backup.json", "bundle to read")
	dir := fs.String("d", ".", "directory to restore into")
	force := fs.Bool("force", false, "overwrite files that already exist")
	_ = fs.Parse(args)

	pass, err := backupPassphrase(false)
	if err != nil {
		log.Fatal(err)
	}
	bundle, err := readBackup(*in, pass)
	if err != nil {
		log.Fatal(err)
	}
	if bundle.Network != "" && bundle.Network != core.NetworkName() {
		fmt.Printf("note: this backup was taken on %s\n", bundle.Network)
	}
	// Refuse the whole restore if anything is in the way, rather than restoring
	// half of it: overwriting a live key file with an older one loses whatever the
	// live one held, and a partial restore leaves a directory nobody can reason
	// about.
	if !*force {
		var clash []string
		for _, name := range sortedNames(bundle) {
			if _, err := os.Stat(filepath.Join(*dir, name)); err == nil {
				clash = append(clash, name)
			}
		}
		if len(clash) > 0 {
			log.Fatalf("these already exist in %s: %s\nmove them aside, or pass -force to overwrite",
				*dir, strings.Join(clash, ", "))
		}
	}
	for _, name := range sortedNames(bundle) {
		// Key material comes back 0600 whatever it was; the mode is not in the
		// bundle, and the safe assumption for a restored key file is the strict one.
		if err := os.WriteFile(filepath.Join(*dir, name), []byte(bundle.Files[name]), 0o600); err != nil {
			log.Fatalf("write %s: %v", name, err)
		}
		fmt.Printf("restored %s\n", name)
	}
	fmt.Printf("%d file(s) restored into %s. The chain re-syncs on its own.\n", len(bundle.Files), *dir)
}
