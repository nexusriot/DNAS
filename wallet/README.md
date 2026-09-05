# wallet

Module `github.com/nexusriot/DNAS/wallet` — key management for DNAS.

- Ed25519 keypairs (`New`, `Load`, `Save`, `LoadOrCreate`).
- Address derivation: `dnas` + `hex(sha256(pubkey)[:20] ‖ checksum)`
  (`Address`, `AddressFromPubKey`, `AddressFromPubKeyHex`). `ValidateAddress`
  rejects mistyped addresses via the checksum.
- Signing and verification (`Sign`, `Verify`).
- **Message** signing (`message.go`: `SignMessage`, `VerifyMessage`,
  `MessagePreimage`) — proving control of an address without spending from it.
  The preimage is a *domain-tagged*, length-committed hash, so a message
  signature can never be replayed as a transaction: a wallet that signed
  whatever bytes it was handed could be asked to "prove you own this address"
  with the serialization of a transfer. `VerifyMessage` returns the address a
  signature proves, which the caller must compare against the one it expected.
- Encryption at rest: `SaveEncrypted` / `LoadEncrypted` / `LoadOrCreateEncrypted`
  derive a key from a passphrase with PBKDF2-HMAC-SHA256 and seal the seed with
  AES-256-GCM. `Load` recognizes an encrypted file opened without a passphrase
  and says so, rather than failing on a seed-length check.
- **Encrypted blobs** (`blob.go`: `SaveEncryptedBlob`, `LoadEncryptedBlob`) — the
  same KDF and cipher for something that is not one key: a backup bundle of key
  files, identities and watch lists (`dnas backup`). Written `0600` through a
  temp file and a rename, since an interrupted write must not leave a truncated
  backup where the previous good one was; it refuses an empty passphrase, and a
  malformed nonce is rejected rather than panicking inside GCM.
- BIP39 mnemonics (`bip39.go`, canonical embedded English wordlist) +
  HD derivation (`hd.go`: `NewHD`, `HDFromMnemonic`, `Derive(index)`) so one
  seed backs up many addresses.
- Multisig (`MultisigAddress(threshold, pubkeys)`): the address of an M-of-N
  script, same format/checksum as a normal address.
- HTLC (`htlc.go`, `HTLCAddress(hashHex, recipientHex, senderHex, timeout)`): the
  address of a hash-time-locked contract — a deterministic hash of its script
  params, in the same format/checksum as normal and multisig addresses, so it is
  funded and validated like any other.
- Vault (`vault.go`, `VaultAddress(hotHex, coldHex, unlock)`): the address of a
  time-delayed vault, derived the same way. Its cold key spends at any height and
  its hot key only from `unlock` on, so a stolen hot key has to wait out the delay
  while the offline cold key rescues the coin. The two keys must differ — one key
  in both roles is just a slower single-key account.

Plaintext key files store only the 32-byte Ed25519 seed, `0600`. No dependencies
on other DNAS modules.
