# core

Module `github.com/nexusriot/DNAS/core` — the ledger and consensus rules.

- `Transaction` — signed transfer (integer base units, per-account nonce,
  optional `Expiry`/`LockUntil` height window and bounded `Memo`); coinbase
  transactions mint the block reward. Authorization is a single signature, an
  M-of-N `MultisigScript`, or a hash-time-locked `HTLCScript` (all resolved by
  `VerifySignature`).
- `Output` / `Transaction.Outputs` — a multi-recipient coin transfer: many
  recipients under one fee, one nonce and one signature, bounded by
  `MaxTxOutputs` and gated by the height-activated `UpgradeMultiOutput`. The
  outputs are appended to the canonical encoding *only when present*, so a
  single-recipient transaction's txid, signature and size are unchanged.
- `CheckTxSanity` / `checkTxAtHeight` — the context-free and height-dependent
  consensus rules, shared by `Mempool.Add`, `Mempool.Select` and block
  application, so the mempool cannot admit or the miner select a transaction the
  chain would then reject (which would stop block production, not merely waste a
  slot). `TxRejection` names which transaction in a block failed.
- `valcache.go` — `ValidationCache`: a bounded set of transaction ids whose
  authorization has been verified, shared between the mempool and the chain so a
  signature is checked once rather than twice, plus a parallel pre-warm of a whole
  block's signatures. `VerifyOps`/`BlockVerifyOps` price the worst-case
  verification cost, capped per block by `MaxBlockVerifyOps` — bytes alone do not
  bound it, since a 16-key multisig spend costs up to 256 verifications in a
  couple of kilobytes.
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
  `TargetBlockTime`, clamped only to `PowLimit` on the easy side — **no hard
  difficulty cap**, so difficulty rises without bound with hashpower. `NoRetarget`
  (regtest/tests) holds it at the genesis target for instant blocks.
- `codec.go` — the **canonical, length-prefixed binary encoding** used for a
  transaction's hash (txid), its fee-determining `Size()`, and its signing bytes,
  so those are reproducible by any implementation (not tied to `encoding/json`).
- `upgrade.go` — height-activated consensus upgrades: `SetUpgradeHeight` /
  `IsUpgradeActive(name, height)` gate a rule change to a coordinated flag-day
  (`UpgradeDustLimit` is the worked example, guarded in `applyTxsAndCoinbase`).
- `work.go` — cumulative proof-of-work (`BlockWork = 2^256/(target+1)`,
  `ChainWork`). Fork choice is greatest cumulative work, with equal-work ties
  broken deterministically by the smaller tip hash so every node converges.
- `snapshot.go` — `Snapshot` (account state + header at a height), `SnapshotAt`,
  `VerifySnapshot` (accounts hash to the header's state root), and
  `NewFromSnapshot` (seed a chain from a verified snapshot for fast-sync).
- `asset.go` — native tokens: `AssetIssue`, `AssetID` (bound to issuer+ticker+nonce),
  ticker validation, and copy-on-write asset-balance updates. Balances live in
  `Account.Assets` and are committed in the state root (`stateLeaf`), so a coin-only
  account is unchanged and asset balances are light-client-provable.
- `Mempool` — bounded pool of pending transactions with lowest-*rate* eviction
  (fee per byte), replace-by-fee (fee-bumping), expiry pruning, and nonce-aware,
  rate-ordered, byte-bounded block selection (`Select`, capped at `MaxBlockBytes`
  and `MaxBlockVerifyOps`). Admission requires a transaction that could plausibly
  be mined: per sender it keeps a contiguous run of nonces from that sender's
  confirmed nonce whose total the sender can afford (needs a bound
  `AccountSource`; `MaxPerSender` caps the count), eviction never breaks a run,
  and `Reconcile` re-checks the pool after each tip change. Without those rules an
  address holding nothing fills the pool for free with work that can never be
  mined.
  Enforces a **dynamic minimum relay fee** (`MinFee`, a per-byte rate): a
  configurable base (`NewMempoolWithPolicy`) that rises quadratically with
  occupancy toward `feeFloorMaxMultiplier×base` when full; a tx is admitted when
  `fee ≥ MinFee() × size`. This is relay policy, not consensus — block validation
  ignores it. `Select` skips transactions below their per-byte base fee, and
  `EstimateTip(baseFee, capacityBytes)` estimates the tip *rate* to land within the
  next `capacityBytes` of block space (0 when uncongested).
- `params.go` — monetary and consensus constants (coin, reward/halving,
  difficulty bounds and retarget, `CoinbaseMaturity`, `MaxReorgDepth`,
  `MaxBlockBytes`, `MaxBlockVerifyOps`, `MaxTxOutputs`, `DefaultMinRelayFee`, the per-byte base-fee params
  `InitialBaseFee`/`MinBaseFee`/`BaseFeeTargetTxs`/`BaseFeeMaxChangeDenominator`,
  genesis) plus the `BaseFeeFor`, `Tips`, and `CoinbaseAmount` fee helpers.

Depends on `wallet` for signature verification and address derivation.
