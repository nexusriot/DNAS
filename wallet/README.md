# wallet

Module `github.com/nexusriot/DNAS/wallet` — key management for DNAS.

- Ed25519 keypairs (`New`, `Load`, `Save`, `LoadOrCreate`).
- Address derivation: `dnas` + first 20 bytes of `sha256(pubkey)`, hex-encoded
  (`Address`, `AddressFromPubKey`, `AddressFromPubKeyHex`).
- Signing and verification (`Sign`, `Verify`).

Key files store only the 32-byte Ed25519 seed, written with `0600` permissions.
No dependencies on other DNAS modules.
