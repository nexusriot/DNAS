# core

Module `github.com/nexusriot/DNAS/core` — the ledger and consensus rules.

- `Transaction` — signed transfer (integer base units, per-account nonce, and an
  optional `Expiry` height); coinbase transactions mint the block reward.
- `Block` / `MerkleRoot` / `Mine` — proof-of-work blocks; the hash commits to a
  merkle root of the transactions.
- `Header` — a block without its transactions; it hashes identically, so a light
  client can verify PoW and the hash chain from headers alone.
- `merkle.go` — `MerkleProof` / `VerifyMerkleProof`: compact transaction-
  inclusion proofs for SPV clients.
- `Blockchain` — thread-safe chain plus derived account state. Validates and
  applies blocks (`AddBlock`), adopts a heavier valid chain (`ReplaceChain`), and
  serves `Headers` / `HeaderAt` / `FindTxProof` for light clients.
- `work.go` — cumulative proof-of-work (`BlockWork`, `ChainWork`) used for the
  most-work fork-choice rule.
- `Mempool` — bounded pool of pending transactions with lowest-fee eviction,
  replace-by-fee (fee-bumping), expiry pruning, and nonce-aware block selection
  (`Select`).
- `params.go` — monetary and consensus constants (coin, reward/halving,
  difficulty bounds and retarget, genesis).

Depends on `wallet` for signature verification and address derivation.
