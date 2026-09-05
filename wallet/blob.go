package wallet

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Passphrase-encrypted storage for something that is not a single key.
//
// SaveEncrypted covers the common case (one wallet, one file). A backup is the
// other case: several key files, a mnemonic, a light wallet's watch list — all of
// it worth exactly one passphrase and none of it worth writing in the clear. The
// format and the KDF are the wallet file's, so there is one place where this
// project decides how it encrypts things at rest.
type encryptedBlobFile struct {
	Version    int    `json:"version"`
	Kind       string `json:"kind"` // what is inside, for a human reading the file
	KDF        string `json:"kdf"`
	Iterations int    `json:"iterations"`
	Salt       string `json:"salt"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// SaveEncryptedBlob writes plaintext to path, encrypted under passphrase. The
// file is written 0600 and through a temporary file, so an interrupted write
// cannot leave a truncated backup where the previous good one was.
func SaveEncryptedBlob(path, kind, passphrase string, plaintext []byte) error {
	if passphrase == "" {
		return errors.New("refusing to write an unencrypted backup of key material")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	gcm, err := walletCipher(passphrase, salt, walletKDFIterations)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	data, err := json.MarshalIndent(encryptedBlobFile{
		Version:    1,
		Kind:       kind,
		KDF:        "pbkdf2-sha256",
		Iterations: walletKDFIterations,
		Salt:       hex.EncodeToString(salt),
		Nonce:      hex.EncodeToString(nonce),
		Ciphertext: hex.EncodeToString(gcm.Seal(nil, nonce, plaintext, nil)),
	}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}

// LoadEncryptedBlob reads and decrypts a file written by SaveEncryptedBlob,
// returning its contents and the kind label it carries.
func LoadEncryptedBlob(path, passphrase string) (plaintext []byte, kind string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var f encryptedBlobFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, "", errors.New("not an encrypted DNAS file")
	}
	if f.Version != 1 {
		return nil, "", errors.New("unknown encrypted file version")
	}
	salt, err := hex.DecodeString(f.Salt)
	if err != nil {
		return nil, "", err
	}
	nonce, err := hex.DecodeString(f.Nonce)
	if err != nil {
		return nil, "", err
	}
	ct, err := hex.DecodeString(f.Ciphertext)
	if err != nil {
		return nil, "", err
	}
	gcm, err := walletCipher(passphrase, salt, f.Iterations)
	if err != nil {
		return nil, "", err
	}
	// GCM PANICS on a nonce of the wrong length rather than returning an error, so
	// a hand-edited or truncated file would crash the process instead of being
	// rejected. The file is untrusted input: check the shape before decrypting.
	if len(nonce) != gcm.NonceSize() {
		return nil, "", errors.New("not an encrypted DNAS file (bad nonce)")
	}
	// AES-GCM is authenticated, so this fails for a wrong passphrase and for a
	// tampered file alike, and there is no way to tell the two apart — which is
	// the property that matters: a modified backup is never silently restored.
	out, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, "", errors.New("cannot decrypt (wrong passphrase or corrupt file)")
	}
	return out, f.Kind, nil
}

// writeFileAtomic writes data to a temporary file in the same directory and
// renames it into place, so a reader never sees a half-written file and a crash
// never destroys the previous contents.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".dnas-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // a no-op once the rename below succeeds
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Flush to the device before the rename: on a crash, a rename can otherwise
	// land while the contents have not, leaving an empty file in place of the old
	// one — precisely the failure a backup must not have.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
