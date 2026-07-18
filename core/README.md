# core

Module `github.com/nexusriot/DNAS/core` — the ledger and consensus rules.

- `Transaction` — signed transfer (integer base units, per-account nonce,
  optional `Expiry`/`LockUntil` height window and bounded `Memo`); coinbase
  transactions mint the block reward. Authorization is a single signature, an
  M-of-N `MultisigScript`, or a hash-time-locked `HTLCScript` (all resolved by
  `VerifySignature`).
- `HTLCScript{Hash, Recipient, Sender, Timeout}` — a hash-time-locked contract.
  `From` is the hash of the script (like multisig); coins unlock either via the
  *claim* branch (a `Preimage` with `sha256(Preimage) == Hash` plus a `Recipient`
  signature, valid at any height) or the *refund* branch (a `Sender` signature,
  valid only once the chain reaches `Timeout`). `VerifySignature` routes to
  `verifyHTLC`; the timeout is a height rule enforced when a block is applied
  (`Transaction.HTLCRefundNotReady`), like `LockUntil`. Enables cross-chain
  atomic swaps.
- `Block` / `MerkleRoot` / `Mine` — proof-of-work blocks; the hash commits to a
  merkle root of the transactions plus the post-block `StateRoot` and the block's
  `BaseFee`. A block's timestamp must exceed the median-time-past of the last 11
  blocks.
- `Header` — a block without its transactions; it hashes identically, so a light
  client can verify PoW and the hash chain from headers alone.
- **EIP-1559 base fee, priced per byte** (consensus) — each block commits a
  `BaseFee` in its header, derived from the parent's fullness by `expectedBaseFee`
  (rising above `BaseFeeTargetTxs` txs, falling below, ±1/`BaseFeeMaxChangeDenominator`
  = 12.5%, clamped to `MinBaseFee`); `Blockchain.NextBaseFee` exposes the next
  block's value. Every non-coinbase tx must pay a fee ≥ `BaseFee × Size()`
  (`BaseFeeFor`), that portion is **burned** — the coinbase may pay only
  `reward + tips` — and a block is bounded by `MaxBlockBytes` of transaction data.
- **Finality** — `MaxReorgDepth` bounds how many committed blocks a reorg may
  discard, and `checkpoint.go` pins known-good hashes at heights (genesis is
  implicit; `AddCheckpoint`); a block at a checkpointed height must match, and no
  reorg may fork below one.
- `merkle.go` — a shared `merkleRootOf` / `merkleProofOf` fold used by both the
  transaction tree and the state tree; `MerkleProof` / `VerifyMerkleProof` give
  compact inclusion proofs (unchanged, and used for both).
- `state.go` — `stateRoot` folds the sorted account set into the header
  `StateRoot` that every block commits. `Blockchain.ProveAccount(addr)` serves an
  `AccountProof` (balance + nonce + merkle path) and `VerifyAccountProof(p, root)`
  folds it, so a light client can prove an address's balance/nonce against the
  header state root — membership only, not the absence of an account.
- `cfilter.go` — BIP158-style **compact block filters**: a Golomb-Coded Set per
  block over the addresses it touches (every recipient + non-coinbase sender).
  SipHash-2-4 keyed by the block hash places items, Golomb-Rice coded (P=19,
  M=784931). `BuildBlockFilter` builds one; `BlockFilter.Match`/`MatchAny` test it
  with no false negatives (a non-match proves non-inclusion) and ≈1/M false
  positives; `FilterHeaderChain` folds them into a BIP157-style filter-header
  chain. `Blockchain` exposes `BlockFilterAt`, `BlockFilters`, and `FilterHeaders`.
- `Blockchain` — thread-safe chain plus derived account state. Applies blocks
  incrementally with per-block undo logs (`AddBlock`); `ReplaceChain` and
  `ReorgFrom` perform a true reorg — roll back to the common ancestor, apply only
  the new suffix. `Locator`/`LocatorFork` support block-locator fork discovery;
  `Headers`/`HeaderAt`/`HeadersFrom`/`BlocksRange`/`BlockAt` + `ValidateHeaderChain`
  drive headers-first sync; `FindTxProof` serves light clients. `SpendableBalance`
  excludes **immature coinbase** — a reward is unspendable until buried under
  `CoinbaseMaturity` blocks, enforced both when applying a block and when the
  miner selects transactions, so a reorg can't let a vanished reward be spent.
  Block validation is split into `validateBlockStructure` (linkage/PoW/merkle/
  base-fee/coinbase shape) and `applyTxsAndCoinbase` (state changes), so
  `NextStateRoot(candidate)` can compute a candidate block's resulting state root
  before it is mined.
- `store.go` — an append-only, length-framed block log. `Open` backs a chain with
  it so `AddBlock` persists in O(1) and reorgs truncate+append (no whole-file
  rewrite); `Save`/`Load` remain as a JSON import/export snapshot.
- `target.go` — proof of work is a **256-bit compact target** (`Bits`, nBits-style
  `CompactToBig`/`BigToCompact`); a hash must be ≤ the target. `expectedBits`
  (in `blockchain.go`) retargets **every block** with an LWMA toward
  `TargetBlockTime`, clamped to `[MinTarget, PowLimit]`.
- `work.go` — cumulative proof-of-work (`BlockWork = 2^256/(target+1)`,
  `ChainWork`). Fork choice is greatest cumulative work, with equal-work ties
  broken deterministically by the smaller tip hash so every node converges.
- `snapshot.go` — `Snapshot` (account state + header at a height), `SnapshotAt`,
  `VerifySnapshot` (accounts hash to the header's state root), and
  `NewFromSnapshot` (seed a chain from a verified snapshot for fast-sync).
- `Mempool` — bounded pool of pending transactions with lowest-*rate* eviction
  (fee per byte), replace-by-fee (fee-bumping), expiry pruning, and nonce-aware,
  rate-ordered, byte-bounded block selection (`Select`, capped at `MaxBlockBytes`).
  Enforces a **dynamic minimum relay fee** (`MinFee`, a per-byte rate): a
  configurable base (`NewMempoolWithPolicy`) that rises quadratically with
  occupancy toward `feeFloorMaxMultiplier×base` when full; a tx is admitted when
  `fee ≥ MinFee() × size`. This is relay policy, not consensus — block validation
  ignores it. `Select` skips transactions below their per-byte base fee, and
  `EstimateTip(baseFee, capacityBytes)` estimates the tip *rate* to land within the
  next `capacityBytes` of block space (0 when uncongested).
- `params.go` — monetary and consensus constants (coin, reward/halving,
  difficulty bounds and retarget, `CoinbaseMaturity`, `MaxReorgDepth`,
  `MaxBlockBytes`, `DefaultMinRelayFee`, the per-byte base-fee params
  `InitialBaseFee`/`MinBaseFee`/`BaseFeeTargetTxs`/`BaseFeeMaxChangeDenominator`,
  genesis) plus the `BaseFeeFor`, `Tips`, and `CoinbaseAmount` fee helpers.

Depends on `wallet` for signature verification and address derivation.
