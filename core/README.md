# core

Module `github.com/nexusriot/DNAS/core` — the ledger and consensus rules.

- `Transaction` — signed transfer (integer base units, per-account nonce,
  optional `Expiry`/`LockUntil` height window and bounded `Memo`); coinbase
  transactions mint the block reward. Authorization is a single signature, an
  M-of-N `MultisigScript`, a hash-time-locked `HTLCScript`, or a time-delayed
  `VaultScript` (all resolved by `VerifySignature`). A transaction may also name a
  `FeePayer`, in which case a second signature from that account authorizes it to
  pay the fee.
- `Output` / `Transaction.Outputs` — a multi-recipient coin transfer: many
  recipients under one fee, one nonce and one signature, bounded by
  `MaxTxOutputs` and gated by the height-activated `UpgradeMultiOutput`. The
  outputs are appended to the canonical encoding *only when present*, so a
  single-recipient transaction's txid, signature and size are unchanged.
- An **inverted height window** (`LockUntil > Expiry`) is refused by
  `CheckTxSanity`: below the lock a transaction is not yet valid and above the
  expiry it is too late, so there is no height at which it could be mined. It
  changes the validity of no block — application already refuses it everywhere —
  and keeps the pool from holding something unminable.
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
- `network.go` — `mainnet` / `testnet` / `regtest`. A network's id is bound into
  the genesis block (via `PrevHash`), into the transaction signing preimage, and
  into the peer handshake, so the three are separate chains, a signature made on
  one does not authorize the same transfer on another, and nodes on different
  networks disconnect rather than failing to converge. Mainnet's id is empty and
  writes nothing, so every existing encoding is byte-for-byte unchanged.
- `VaultScript{Hot, Cold, Unlock}` — a time-delayed vault. `From` is the hash of
  the script; the **cold** key may spend at any height and the **hot** key only
  from `Unlock` on, so a stolen hot key has to wait out the delay while the
  offline cold key moves the coin to safety. Gated by `UpgradeVault`; the height
  rule (`VaultHotNotReady`) re-derives which key signed, which is why a vault
  spend costs two verifications in `VerifyOps`.
- **Fee sponsorship** (`FeePayer`, gated by `UpgradeFeeSponsor`) — the fee is
  charged to a third party instead of the sender, so an address holding no coin
  can transact. The sender signs who pays and the sponsor counter-signs the same
  bytes; the sponsor spends no nonce, so a sponsorship is bound to exactly one
  transfer. `chargeSponsor` applies the payer's own coinbase-maturity reserve.
- `assetindex.go` — a registry of what the chain has issued: `Asset(id)`,
  `Assets()`, `AssetsByTicker(t)` and `AssetHolders(id)`. An asset id is
  `hash(issuer, ticker, nonce)` and cannot be unpacked, so a balance of
  `tok3f2a…` said nothing on its own. Built as blocks connect and rolled back
  with them (like the transaction index), and always on — it is bounded by the
  number of issuances, not of transfers. A ticker resolves to a LIST, because
  anyone may issue "GOLD" and the id is the identifier.
- `prune.go` — `EnablePruning(keep)` drops the bodies of blocks deeper than
  `keep`, leaving the header-only placeholders a fast-synced chain already uses,
  so a node's resident size stops tracking the chain. `MinPruneKeep` is above
  `MaxReorgDepth` (a reorg replays the bodies it disconnects), a body-less height
  serves NO filter (an empty filter is a proof of *absence*), and
  `BlockBodyAt`/`ErrPrunedBody`/`BodyHeight`/`HasBody` let a caller tell "no such
  block" from "pruned". The asset registry survives pruning; the transaction and
  address indexes drop the entries that would point into nothing.
- `addrindex.go` — an optional address → transactions index (`EnableAddressIndex`,
  `AddressHistory`), maintained across reorgs like the transaction index. Off by
  default: its size is bounded by an address's usage, not by the chain.
- `share.go` — mining **share** targets (`ShareBits`, `MeetsShareTarget`): a
  deliberately easier target so a miner can prove work without finding a block.
  Pool accounting, never consensus — no share is stored in the chain.
- `chainstats.go` — `Blockchain.Stats(window)`: estimated network hashrate (work
  ÷ elapsed time), the block-interval distribution against `TargetBlockTime`, the
  difficulty range, fee/burn/tip totals, and the coinbase recipients of the
  window. Derived from headers and bodies already in memory, so it is reporting,
  never consensus. `FormatHashrate` renders it.
- `FoldFilterHeaders(prev, filters)` (cfilter.go) — continues the filter-header
  chain from a previous value, so a client holding a verified prefix extends it
  with only the new filters instead of re-folding from genesis;
  `FilterHeaderChain` is the special case that starts at genesis.
  `BlockFiltersFrom`/`FilterHeadersFrom` page the two accessors. The chain itself
  is **cached** as blocks connect and truncated on reorg
  (`extendFilterHeadersLocked`, `FilterHeaderBase`) — it is a running hash over
  bodies, so a node that pruned them and re-folded over what remained would
  produce values agreeing with nobody, and serving a range is now O(range)
  instead of O(chain).
- `dbtool.go` — operator tooling over a chain store without a running node:
  `StoreStat` (what is in the file), `VerifyStore` (replay it and name the first
  bad block), `ExportStore`/`ImportStore` (move a chain as a portable JSON file).
  The first two go through `readStore`, which opens the file read-only and never
  repairs a torn trailing record — `openStore` does repair one, which is right for
  a node adopting its own store and wrong for a tool inspecting a live one.
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
- `store.go` — an append-only, length-framed block log, with `compact` to rewrite
  it so it matches a pruned chain (atomic: temp file + rename). `Open` backs a chain with
  it so `AddBlock` persists in O(1) and reorgs truncate+append (no whole-file
  rewrite); `Save`/`Load` remain as a JSON import/export snapshot. A reorg whose
  writes fail partway **poisons** the store: the truncate has already discarded
  the losing branch, so there is nothing to roll back to, and every later write is
  refused rather than deepening the divergence between disk and memory.
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
  `IsUpgradeActive(name, height)` gate a rule change to a coordinated flag-day.
  Five are defined: `UpgradeMultiOutput` (multi-recipient transfers),
  `UpgradeVault` (time-delayed vault spends), `UpgradeFeeSponsor` (a third party
  paying the fee), `UpgradeCheckedAddresses` (every address a transaction names
  must be well-formed and checksummed — the only thing that stops a client bug
  burning coin to a typo) and `UpgradeDustLimit` (the worked example). All are
  guarded in `checkTxAtHeight`, so the mempool, `Select` and block application
  enforce them identically.
  `KnownUpgrade`/`Upgrades` let the CLI reject a misspelled name at startup.
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
- `Mempool` — pool of pending transactions bounded in **both** count and bytes
  (`DefaultMempoolBytes`, 32 MiB; `NewMempoolWithLimits`) with lowest-*rate*
  eviction (fee per byte), replace-by-fee (fee-bumping), expiry pruning, and
  nonce-aware,
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
  next `capacityBytes` of block space (0 when uncongested), and `Stats()` buckets
  the queue by fee rate (served at `/mempool/stats`) so a sender can see what the
  queue is actually paying rather than only how deep it is. A **fee sponsor** is
  held to every fee it has promised across the pool, not one at a time.
- `state.go` — the header's **state root**, now the root of the trie below rather
  than a Merkle fold over the sorted accounts. That is what makes **absence**
  provable: `ProveAccount` answers for an address with no account, and
  `VerifyAccountProof` returns `(valid, present)` so a light client can tell
  "holds nothing" from "I am not showing you this". Still a resident
  `map[string]Account`, so memory bounds the ledger and the root is rebuilt per
  call — see ROADMAP §1.
- `nettime.go` — network-adjusted time: the bounded median of peers' clock
  offsets, applied to timestamp validation only. Stops one machine's wrong clock
  isolating it from the chain.
- `storecodec.go` — the block store's compact binary record format (local, not
  consensus), with hex fields kept as raw bytes. Reads legacy JSON records too.
- `trie.go` — a content-addressed, path-compressed **sparse Merkle trie** keyed
  by the hash of an address: `Trie`, a pluggable `NodeStore`, and `Prove` /
  `VerifyTrieProof` giving both **membership and absence** proofs. Absence is the
  capability `state.go`'s sorted-leaf fold cannot offer — a prover who omits a
  leaf produces a tree a client cannot distinguish from the truth. Updates are
  O(depth) and every historical root stays readable. The header **does** commit
  this trie's root (see `state.go`), so its proofs are bound to proof of work.
  What is not yet done is reading state THROUGH it: `applyBlock` still works on a
  resident map, so the memory bound and incremental root updates remain open
  (ROADMAP §1).
- `prune.go` — `-prune` drops old bodies from memory AND, via compaction, from
  disk. Because a header-only store cannot rebuild balances, a verified state
  snapshot is written beside it (`chain.db.state`) and `Open` bootstraps from it
  like a fast-synced node; a snapshot that does not hash to the header's state
  root is refused.
- `uri.go` — `dnas:ADDRESS?amount=…&memo=…&ref=…` payment URIs:
  `BuildPaymentURI` / `ParsePaymentURI` / `IsPaymentURI`. Parsing
  checksum-validates the address, so a URI that survives cannot aim a payment at a
  typo; the amount is decimal DNAS, not base units. Building and parsing live
  together so a round trip is a test rather than a hope.
- `params.go` — monetary and consensus constants (coin, reward/halving,
  difficulty bounds and retarget, `CoinbaseMaturity`, `MaxReorgDepth`,
  `MaxBlockBytes`, `MaxBlockVerifyOps`, `MaxTxOutputs`, `DefaultMinRelayFee`,
  `DefaultMempoolBytes`, `MinPruneKeep`, the per-byte base-fee params
  `InitialBaseFee`/`MinBaseFee`/`BaseFeeTargetTxs`/`BaseFeeMaxChangeDenominator`,
  genesis) plus the `BaseFeeFor`, `Tips`, and `CoinbaseAmount` fee helpers.

Depends on `wallet` for signature verification and address derivation.

Golden consensus vectors live in `testdata/consensus_vectors.json`, generated and
verified by `vectors_test.go` (`go test ./core -run TestConsensusVectors -update`
to regenerate). They pin every consensus-visible value — txids, signing
preimages, block hashes, state roots, address derivations, the difficulty
encoding — in a language-neutral form, so a second implementation has something
to check itself against. See `testdata/README.md`.
