# wallet

Module `github.com/nexusriot/DNAS/wallet` — key management for DNAS.

- Ed25519 keypairs (`New`, `Load`, `Save`, `LoadOrCreate`).
- Address derivation: `dnas` + `hex(sha256(pubkey)[:20] ‖ checksum)`
  (`Address`, `AddressFromPubKey`, `AddressFromPubKeyHex`). `ValidateAddress`
  rejects mistyped addresses via the checksum.
- Signing and verification (`Sign`, `Verify`).
- Encryption at rest: `SaveEncrypted` / `LoadEncrypted` / `LoadOrCreateEncrypted`
  derive a key from a passphrase with PBKDF2-HMAC-SHA256 and seal the seed with
  AES-256-GCM.
- BIP39 mnemonics (`bip39.go`, canonical embedded English wordlist) +
  HD derivation (`hd.go`: `NewHD`, `HDFromMnemonic`, `Derive(index)`) so one
  seed backs up many addresses.
- Multisig (`MultisigAddress(threshold, pubkeys)`): the address of an M-of-N
  script, same format/checksum as a normal address.
- HTLC (`htlc.go`, `HTLCAddress(hashHex, recipientHex, senderHex, timeout)`): the
  address of a hash-time-locked contract — a deterministic hash of its script
  params, in the same format/checksum as normal and multisig addresses, so it is
  funded and validated like any other.

Plaintext key files store only the 32-byte Ed25519 seed, `0600`. No dependencies
on other DNAS modules.
