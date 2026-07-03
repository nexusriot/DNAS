# core

Module `github.com/nexusriot/DNAS/core` — the ledger and consensus rules.

- `Transaction` — signed transfer (integer base units, per-account nonce,
  optional `Expiry`/`LockUntil` height window and bounded `Memo`); coinbase
  transactions mint the block reward. Authorization is a single signature or an
  M-of-N `MultisigScript` (`VerifySignature` handles both).
- `Block` / `MerkleRoot` / `Mine` — proof-of-work blocks; the hash commits to a
  merkle root of the transactions. A block's timestamp must exceed the
  median-time-past of the last 11 blocks.
- `Header` — a block without its transactions; it hashes identically, so a light
  client can verify PoW and the hash chain from headers alone.
- `merkle.go` — `MerkleProof` / `VerifyMerkleProof`: compact transaction-
  inclusion proofs for SPV clients.
- `Blockchain` — thread-safe chain plus derived account state. Applies blocks
  incrementally with per-block undo logs (`AddBlock`); `ReplaceChain` and
  `ReorgFrom` perform a true reorg — roll back to the common ancestor, apply only
  the new suffix. `Locator`/`LocatorFork` support block-locator fork discovery;
  `Headers`/`HeaderAt`/`HeadersFrom`/`BlocksRange`/`BlockAt` + `ValidateHeaderChain`
  drive headers-first sync; `FindTxProof` serves light clients. `SpendableBalance`
  excludes **immature coinbase** — a reward is unspendable until buried under
  `CoinbaseMaturity` blocks, enforced both when applying a block and when the
  miner selects transactions, so a reorg can't let a vanished reward be spent.
- `store.go` — an append-only, length-framed block log. `Open` backs a chain with
  it so `AddBlock` persists in O(1) and reorgs truncate+append (no whole-file
  rewrite); `Save`/`Load` remain as a JSON import/export snapshot.
- `work.go` — cumulative proof-of-work (`BlockWork`, `ChainWork`). Fork choice is
  greatest cumulative work, with equal-work ties broken deterministically by the
  smaller tip hash so every node converges on the same chain.
- `Mempool` — bounded pool of pending transactions with lowest-fee eviction,
  replace-by-fee (fee-bumping), expiry pruning, and nonce-aware block selection
  (`Select`). Enforces a **dynamic minimum relay fee** (`MinFee`): a configurable
  base (`NewMempoolWithPolicy`) that rises quadratically with occupancy toward
  `feeFloorMaxMultiplier×base` when full. This is relay policy, not consensus —
  block validation ignores it.
- `params.go` — monetary and consensus constants (coin, reward/halving,
  difficulty bounds and retarget, `CoinbaseMaturity`, `DefaultMinRelayFee`,
  genesis).

Depends on `wallet` for signature verification and address derivation.
