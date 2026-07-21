# DNAS design

This document explains **how DNAS works and why it is built the way it is** —
the data model, consensus, networking, and the deliberate trade-offs behind each
choice. For usage see [README.md](README.md) and [QUICKSTART.md](QUICKSTART.md);
each module also has its own `README.md`. For the deliberate gaps and what would
close them, see [ROADMAP.md](ROADMAP.md).

DNAS ("Definitely Not A Scam") is a small but genuinely working proof-of-work
cryptocurrency. It is a learning project, not money.

---

## 1. Goals and non-goals

**Goals**

- Be a *real* cryptocurrency, not just a hash-chain: cryptographic ownership,
  issuance by mining, double-spend/replay protection, most-work consensus with
  reorgs, and independent nodes that converge on one chain.
- Be readable. Each concept lives in one place with a clear name; consensus
  parameters are centralized; modules have a strict dependency order.
- Be honest about layers. Consensus rules, relay policy, and client-side
  validation are kept distinct and labelled as such.

**Non-goals**

- Not money. It now runs an *open, permissionless* network (no shared key needed)
  with *real, unbounded* proof-of-work difficulty and a canonical, cross-language
  consensus encoding — the three properties that separate a simulation from a
  cryptocurrency — but it still has a single implementation, transparent balances,
  no external audit, and no economic-security guarantees. Don't secure value with
  it. (Difficulty is only clamped down to a trivial floor on a devnet/regtest,
  where `NoRetarget` holds it fixed for instant blocks.)
- No smart contracts, no confidential amounts or recipient privacy (Dandelion++
  only obscures which peer *originated* a transaction).

---

## 2. Design principles

1. **One source of truth for consensus.** Every rule a node must agree on lives
   in `core` — most of it in [`core/params.go`](core/params.go) and
   [`core/blockchain.go`](core/blockchain.go). Networking and clients never
   re-implement validation.
2. **Integers only in consensus.** All amounts are integer base units; there is
   no floating point anywhere in validation (floats appear only in UI
   formatting).
3. **Deterministic everything.** Genesis, difficulty retarget, fork-choice
   tie-breaks, and multisig address derivation are all deterministic, so every
   node computes identical results and converges.
4. **Separate consensus from policy.** Rules that affect block *validity* are
   consensus; rules that only affect what a node *relays or queues* (the fee
   floor, recipient checksums) are policy and are explicitly documented as such.
5. **Fail closed on ownership.** A mistyped address fails a checksum; an
   under-signed multisig spend is rejected; an immature coinbase can't be spent.

---

## 3. Module layout

DNAS is a **Go multi-module workspace** (`go.work`). Each component is its own
module `github.com/nexusriot/DNAS/<name>`; the repo root is not a module.

```
wallet → core → node → api → cmd        (dependency direction, no cycles)
```

| Module   | Responsibility                                                        |
|----------|-----------------------------------------------------------------------|
| `wallet` | Ed25519 keys, checksummed addresses, at-rest encryption, BIP39/HD, multisig address derivation |
| `core`   | Transactions, blocks, headers, Merkle, chain state, work, reorgs, mempool, persistence, params |
| `node`   | Encrypted/authenticated P2P, peer identity, ban scoring, discovery, sync, the miner |
| `api`    | HTTP interface + embedded web explorer                                |
| `cmd`    | The `dnas` CLI/daemon (`cmd/dnas`)                                     |

Two clients live **outside** the Go workspace and talk only to the HTTP API:

- `tui/` — a bubbletea terminal client. It is its **own module** with a nested
  `tui/go.work`, deliberately excluded from the root workspace: it pulls in
  external deps (bubbletea/lipgloss) whose `go.sum` verification would otherwise
  be forced onto the internal `v0.0.0` modules. It imports no DNAS module and
  reimplements only the little it needs (SHA-256 for SPV folding).
- `gui/` — a PyQt6 desktop client (Python standard library + PyQt6 only).

Because the root isn't a module, build/test with directory paths
(`go build ./core/... ./node/...`), not a bare `./...`. The
[Makefile](Makefile) wraps this.

---

## 4. Ledger and state model

DNAS uses an **account + nonce** ledger (Ethereum-style), not UTXO — a
deliberate choice for readability: balances and replay protection are a small
map rather than a set of coins to track.

```go
type Account struct { Balance uint64; Nonce uint64; Assets map[string]uint64 }
```

- **Balances** are integer base units. `Coin = 100_000_000` (1 DNAS), so amounts
  on the wire and in state never touch floating point.
- **Nonces** are per-account and strictly sequential. A transaction must use the
  sender's next nonce, which prevents replay and orders a sender's transactions.
- Chain state is a `map[address]Account` derived by applying blocks in order. It
  is never trusted from the wire; it is recomputed from validated blocks.

**Native assets (tokens).** Besides the base coin, an account may hold balances of
any number of native assets (`Assets`, asset id → amount). A transaction with
`Issue` set mints a new asset to its sender — the id is `hash(issuer|ticker|nonce)`,
so issuances never collide — and a transaction with `AssetID` set moves that asset
instead of coin (the **fee is always paid in coin**). Asset balances are committed
in the state root alongside coin (`stateLeaf` appends the sorted asset balances),
so a light client proves an asset balance exactly as it proves a coin balance —
and `Assets` is `omitempty`, so a coin-only account hashes byte-for-byte as before
(no genesis change). Supplies are capped (`MaxAssetSupply`) so asset arithmetic
can't overflow, and the asset state is updated copy-on-write so reorg undo logs
stay correct.

---

## 5. Transactions

```go
type Transaction struct {
    From, To         string
    Amount, Fee      uint64
    Nonce            uint64
    Expiry           uint64          // highest valid height (0 = none)
    LockUntil        uint64          // lowest valid height (time-lock)
    Memo             string          // ≤ MaxMemoBytes
    AssetID          string          // move this native asset instead of coin (§4)
    Issue            *AssetIssue     // OR mint a new native asset to From (§4)
    Signature        []byte          // single-key authorization
    Multisig         *MultisigScript // OR multisig authorization
    Signatures       [][]byte
    HTLC             *HTLCScript     // OR hash-time-locked-contract authorization
    Preimage         []byte          // revealed on the HTLC claim branch
}
```

**Signing.** The signed message (`signingBytes`) covers every consensus-relevant
field (From/To/Amount/Fee/Nonce/Expiry/LockUntil/AssetID/Issue/Memo) but **not**
the signature fields. Crucially it is *identical* for single-key and multisig
transactions, so the two authorization paths sign the same bytes. (A native-asset
transfer or issuance is an ordinary single-key spend by `From`; the fee is always
paid in coin — see §4.)

**Canonical encoding.** Signing bytes, the transaction hash (its txid), and the
fee-determining `Size()` are all taken over a **canonical, length-prefixed binary
encoding** ([`core/codec.go`](core/codec.go)), *not* `encoding/json`. This matters
for being a real cryptocurrency: a JSON encoder's field ordering, escaping and
omitempty rules are library- and language-specific, so hashing over them would let
a second implementation compute different txids and fee floors and silently fork.
The binary layout — a version byte, big-endian integers, 4-byte-length-prefixed
strings, presence-flagged optional structs — is unambiguous and reproducible by
any implementation, in any language. (The block header hash and the state-root
leaf are simple `%d|%s` ASCII formats, already reproducible, so they were left as
is.) The wire transport may still be JSON; the hash *preimage* is always these
canonical bytes.

**Authorization** is resolved by `VerifySignature`:

- **Single key:** the signature must verify against the public key that hashes to
  `From`.
- **M-of-N multisig:** `From` must equal `MultisigAddress(Threshold, PubKeys)`
  (so the script is bound to the address it spends), and at least `Threshold`
  signatures from *distinct* listed members must verify. `verifyMultisig`
  enforces distinctness so one member can't satisfy a 2-of-N alone.
- **Hash-time-locked contract (HTLC):** `From` must equal the hash of the
  `HTLCScript{Hash, Recipient, Sender, Timeout}`. Two spend branches unlock it:
  the *claim* branch needs a `Preimage` where `sha256(Preimage) == Hash` plus a
  signature by `Recipient` (valid at any height); the *refund* branch needs a
  signature by `Sender` and is only valid once the chain reaches `Timeout`. The
  timeout is a height rule enforced at block application (like `LockUntil`), since
  signature verification has no height context. Because the claim publishes the
  preimage on-chain, HTLCs compose into **cross-chain atomic swaps**: revealing
  the secret to claim on one chain lets the counterparty claim the mirror on the
  other. Same address format as any account, so it is funded by an ordinary
  transfer.

**Time windows.** `Expiry` (upper bound) and `LockUntil` (lower bound) constrain
the height range in which a transaction is valid; both are signed. Expired
transactions are pruned from the mempool and rejected at submission; locked ones
are skipped during selection.

**Replace-by-fee.** A stuck transaction is bumped by re-submitting it at the
*same* `(From, Nonce)` with a strictly higher fee; the mempool replaces the old
one (§10).

**Coinbase.** The block's first transaction mints `reward + tips` to the miner
and has no signature (`IsCoinbase`) — the base-fee portion of every fee is burned
rather than paid to the miner (§9).

---

## 6. Blocks, headers, and Merkle proofs

```go
type Block  struct { Index, Timestamp, Nonce; PrevHash; MerkleRoot; StateRoot; BaseFee; Bits; Transactions; Hash }
type Header struct { Index, Timestamp, Nonce; PrevHash; MerkleRoot; StateRoot; BaseFee; Bits; Hash }
```

A block hash commits to header fields **plus two merkle roots** — a `MerkleRoot`
of its transactions and a `StateRoot` of the account set *after* the block — and
the block's `BaseFee` (§9). A `Header` hashes *identically* to its block, so a
client holding only headers can verify proof-of-work, the hash chain, that a
transaction is included (via `MerkleRoot`), and that an account holds a given
balance (via `StateRoot`) — all without block bodies. This is what makes SPV
(§14) possible.

`core/merkle.go` builds the shared merkle fold (`MerkleProof`, `VerifyMerkleProof`);
an odd level duplicates its last node (Bitcoin's rule), and proof and root use the
same rule so they agree. `core/state.go` reuses that fold over the sorted account
set to produce the `StateRoot` and account-membership proofs.

---

## 7. Proof of work and difficulty

Proof of work uses a **256-bit target in compact form** (`Block.Bits`, like
Bitcoin's nBits — see [`core/target.go`](core/target.go)): a block hash, read
big-endian as an integer, must be ≤ the target the block commits to. A smaller
target is exponentially harder. This replaces the older leading-zero-nibble
difficulty (a coarse integer 3–5) with a *continuous* target. Cumulative work,
comparable across forks, is:

```
BlockWork = 2^256 / (target + 1)        ChainWork = Σ BlockWork
```

Difficulty is retargeted **every block** by an **LWMA** (linearly-weighted moving
average, [`expectedBits`](core/blockchain.go)) over the last `lwmaWindow` blocks,
weighting recent blocks more so it tracks `TargetBlockTime` smoothly rather than
in ±1 steps. Per-block solve times are clamped to `[1, 6·TargetBlockTime]`, which
also neutralises the enormous first gap from the far-past genesis timestamp — the
old step retarget collapsed to the floor on the first window; the LWMA does not.

**Difficulty is unbounded on the hard side.** The target is clamped only to
`PowLimit` on the *easy* side (a floor, the value a fresh chain starts at); there
is deliberately **no ceiling on difficulty**, so it rises without limit to match
whatever hashpower shows up. This is what gives proof of work its economic
security — rewriting history means redoing that ever-growing work — and is the
property that separates a real chain from a toy where blocks are free. For a
devnet or the test suite, `NoRetarget` (mirroring Bitcoin Core's
`fPowNoRetargeting`, set by `-regtest`) instead holds the genesis target so blocks
stay instant; a real network leaves it off. `TargetDifficulty` renders a target
as a human ratio (`PowLimit ÷ target`) for display only.

---

## 8. Fork choice, timestamps, reorgs, and maturity

**Fork choice — most work, deterministic tie-break.** Nodes adopt the chain with
the greatest `ChainWork`. Because difficulty is nearly constant, equal-work forks
are common; they are broken by preferring the **lexicographically smaller tip
hash**. This is deterministic (every node picks the same winner → immediate
convergence) and monotonic (a node only switches toward a smaller tip → no
flapping). It is *not* Bitcoin first-seen, which is per-node and wouldn't
converge here.

**Median-time-past.** A block's timestamp must exceed the median of the last 11
timestamps, and may not be more than `MaxFutureDrift` seconds ahead of local
time. This bounds timestamp manipulation of the retarget while tolerating small
out-of-order stamps — stricter "strictly increasing" would reject honest jitter.

**Reorgs with undo logs.** The chain keeps a per-block **undo log**
(`[][]undoEntry`). `AddBlock` applies a block incrementally and records how to
reverse it — no full-state clone per block. `ReplaceChain` / `ReorgFrom` find the
common ancestor (`commonPrefix`), roll a *copy* of state back to that point via
the undo logs, then apply only the new suffix. Validation happens on the copy, so
a bad suffix leaves the live chain and state untouched (atomic switch).

**Finality guards.** Before fork choice even runs, a reorg is refused if it would
discard more than `MaxReorgDepth` already-committed blocks — settled history is
treated as final, so a deep-reorg attack can't rewrite it — or if it would fork
below a **checkpoint**. Checkpoints (`core/checkpoint.go`) pin known-good block
hashes at given heights: a block at a checkpointed height must carry the pinned
hash, and no reorg may cross one. Genesis is always an implicit checkpoint;
operators add more with `-checkpoints height:hash,…` (e.g. hashes of
deeply-buried blocks from a trusted release) so a fresh or lagging node can't be
fed a bogus deep history. Both guards leave initial sync and forward extension
untouched — they only bound rolling *back* committed blocks.

**Consensus upgrades (flag-day activation).** Rule changes activate at a
configured block height ([`core/upgrade.go`](core/upgrade.go)): a validation rule
guards itself with `IsUpgradeActive(name, blockHeight)`, so blocks below the
activation height keep the old rule and blocks at/after it enforce the new one —
the whole network switching together on a coordinated flag-day instead of forking
uncoordinated. Heights are configuration, set identically on every node at startup
(like checkpoints). `UpgradeDustLimit` is a worked example (once active, coin
transfers below `DustThreshold` are rejected). This is *height* activation; miner
version-bit *signaling* (BIP9) would additionally need a header version field and
is left as future work.

**Coinbase maturity.** A coinbase mined at height `C` is spendable only once the
chain reaches `C + CoinbaseMaturity`. In an account model there are no coins to
"lock", so this is computed by a history scan: `immatureCoinbase(blocks, height,
addr)` sums coinbase-to-`addr` outputs still inside the maturity window, and
`SpendableBalance = Balance − immature`. It is enforced in **both** places that
matter:

- consensus (`applyTxTo` reserves the immature amount when checking a spend), and
- the miner (`Mempool.Select` uses `SpendableBalance`, so a node never builds a
  block its own rules would reject).

Because it is derived from block history, it reverses naturally on a reorg — no
extra state to roll back. This is what stops a reorg from letting someone spend a
reward that has since vanished.

---

## 9. Issuance and monetary policy

New coins are created **only** by the coinbase. The subsidy starts at
`InitialBlockReward` (50 DNAS) and halves every `HalvingInterval` (210 000)
blocks until it reaches zero. Genesis is fixed (`GenesisTimestamp`,
`GenesisPrevHash`) so every node computes an identical genesis hash and can agree
on the same chain.

**EIP-1559 base fee (consensus, burned, priced per byte).** Each block commits a
`BaseFee` in its header, derived deterministically from the parent's fullness
(`expectedBaseFee`): it rises up to 1/`BaseFeeMaxChangeDenominator` (12.5%) when
the parent held more than `BaseFeeTargetTxs` transactions and falls when fewer,
clamped to `MinBaseFee`. The base fee is a **price per byte**: every non-coinbase
transaction must pay a fee ≥ `BaseFee × its serialized size` (`Transaction.Size`),
so a larger transaction owes proportionally more. That base-fee portion is
**burned** (never credited to anyone, so it leaves the money supply), and the
miner's coinbase may pay only `reward + tips`, where a tip is a transaction's fee
above `BaseFee × size`. A block is additionally bounded by `MaxBlockBytes` of
transaction data, making block space a metered, priced resource. This is a genuine
consensus fee market — a block whose transactions underpay per byte, whose
coinbase over-pays, or which exceeds the byte budget is invalid — distinct from
the mempool's *relay* floor (§10), which only governs what a node queues. Because
the base fee is in the header, light clients see it and it is covered by proof of
work. (The *congestion signal* that moves the base fee is transaction count, not
bytes — a deliberate simplification that keeps the retarget cheap to reason about
and test; the *payment* is per byte.)

---

## 10. Mempool

`core/mempool.go` is a bounded pool of validated, unmined transactions.

- **Bounded with lowest-*rate* eviction.** Capped at `DefaultMempoolSize` (5000);
  once full, a new transaction is admitted only by out-bidding the queued
  transaction paying the least per byte (fee *rate*), which it evicts — so scarce
  block space, a per-byte resource, goes to the highest-paying bytes.
- **Replace-by-fee.** A conflicting `(From, Nonce)` may be replaced only by a
  strictly higher fee.
- **Dynamic minimum relay fee — *policy, not consensus*, priced per byte.**
  `MinFee()` is a *rate* (base units per byte): it starts at a configurable base
  (`NewMempoolWithPolicy`, `-minrelayfee`, default `DefaultMinRelayFee = 10`/byte)
  and rises quadratically with occupancy, and a transaction is admitted only when
  its fee ≥ `MinFee() × its size`:

  ```
  floor = base · (1 + (feeFloorMaxMultiplier−1) · fill² / 100²)      fill = %full, 0..100
  ```

  so it stays near the base on an idle devnet and reaches `base·100` when full.
  A transaction below the current floor is refused entry, but this only governs
  what *this* node queues and gossips — a block that includes an under-floor
  transaction is still valid. This is intentionally **not** an EIP-1559 consensus
  base fee (which would need header/SPV/supply surgery).
- **Nonce-aware, rate-ordered, byte-bounded selection.** `Select` greedily builds
  a valid sequence for the next block: each transaction must have its sender's next
  nonce, cover its per-byte base fee, and be affordable *from spendable balance*
  (so immature coinbase is never spent); recipients are credited within the
  simulation so chained spends can share a block; the highest fee *rate* wins among
  ready candidates, and selection stops at `MaxBlockBytes` of total size as well as
  the transaction-count cap.
- **Expiry pruning** drops transactions that can no longer be mined.

---

## 11. Networking

`node/` implements an authenticated, encrypted, identified peer-to-peer network.

**Secure transport (`secureConn`, `secureHandshake`) — permissionless by
default.** Every connection begins with an X25519 ECDH key exchange; traffic is
then AES-256-GCM encrypted, length-framed, with a JSON encoder/decoder over it.
Whether it is *authenticated* depends on the network key: with **no `-netkey`
(the default) the network is open/permissionless** — anyone may connect (the
handshake is anonymous but encrypted), which is what an actual cryptocurrency
requires. Supplying a `-netkey` authenticates a **private** network: a peer that
doesn't know it fails an HMAC check and is dropped (used for a devnet or regtest).
Either way, peers are still cryptographically identified afterwards by their
Ed25519 node identity (below). The handshake sends and receives concurrently so it
also works over `net.Pipe` (used in tests).

**Peer identity.** The handshake yields a deterministic session id; each peer
then signs it with its **Ed25519 node identity** (`MsgIdentity`), so peers are
cryptographically identified, not merely "knows the key".

**Ban scoring (`banbook`).** Misbehaving peers accrue points and are cut off past
a threshold. The key depends on when the misbehaviour is detectable: a **failed
handshake** happens before a peer proves an identity, so it is scored *by IP*
(loopback exempt, so many local nodes on 127.0.0.1 don't ban one another);
**protocol-level fraud after authentication** — a bad header chain, or a block
that fails its own proof-of-work / merkle root (`Block.SelfValid`) — is scored *by
identity*. A plain fork (a well-formed block that just doesn't link to our tip) is
never penalised. Identity keying is evadable by key rotation and IP keying by
changing address; both are accepted for a friendly devnet.

**Keepalive.** An established connection is pinged every `pingInterval` and
dropped if it sends nothing for `peerIdleTimeout` (a read deadline is reset on
every message). This detects a half-open connection — a peer that died without
closing — instead of leaking a goroutine and peer slot forever.

**Protocol version & capabilities.** Right after the identity exchange each side
sends a `MsgVersion` carrying its `ProtocolVersion` and a list of capability
strings. A peer below `MinProtocolVersion` is dropped, so the wire format can
evolve; capabilities (e.g. `dand` for Dandelion++) let optional features be
negotiated per-connection without a version bump.

**Dandelion++ transaction relay (origin privacy).** A newly submitted transaction
is not broadcast to every peer immediately — that would let a network observer
pinpoint its source. Instead it travels along a **stem**: the node forwards it to
a single, *epoch-stable* successor peer (re-chosen every `dandEpochDuration`, and
only among peers advertising the `dand` capability). At each hop the transaction
either continues down the stem or, with probability `1/dandFluffDenom`,
transitions to the **fluff** phase — an ordinary broadcast to all peers. Because a
relaying node and the originator behave identically on the stem, an observer can't
distinguish them. Two safety nets prevent a transaction getting stuck: a node with
no stem successor fluffs immediately, and every stem-forward arms an **embargo**
timer (`dandEmbargo`) that fluffs the transaction itself if it isn't seen fluffing
on the network first (defeating a peer that black-holes stem transactions). It is
enabled by default (`-dandelion`) and degrades to plain broadcast when off or when
no peer supports it. (`node/dandelion.go`.)

**Discovery (`peerbook`).** Nodes gossip known addresses (`MsgGetPeers` /
`MsgPeers`) and auto-dial discovered peers up to `-maxpeers`, excluding
self/dupes/already-connected.

**Eclipse & DoS resistance.** Because the network is open, a node bounds who can
fill its slots: inbound connections are admitted (`admitInbound`, before the
handshake) only under a total cap (`maxInbound`) and a **per-IP-group cap**
(`maxInboundPerGroup`, grouping by /16), so an attacker must control many distinct
address ranges to monopolise a victim's inbound peers (an eclipse attack) rather
than spinning up cheap connections from one host. Loopback is exempt so local
demos aren't limited. Each peer's read loop runs a **token-bucket rate limiter**
(`msgRatePerSec`/`msgRateBurst`): a peer that floods messages is dropped. The
expensive whole-chain request (`MsgGetChain`) is additionally throttled to once
per `getChainCooldown` per peer. These bound the resource-exhaustion vectors that
a permissionless network exposes.

**Sync protocol.** New blocks are announced by hash and pulled on demand; catch-up
is headers-first; forks transfer only the divergent suffix:

| Purpose            | Messages                                   |
|--------------------|--------------------------------------------|
| Handshake/identity | `MsgHello`, `MsgIdentity`                  |
| Tx gossip          | `MsgTx`                                    |
| Block announce/pull| `MsgInv` → `MsgGetData` → `MsgBlock`       |
| Headers-first sync | `MsgGetHeaders` (+ block locator) → `MsgHeaders` |
| Ranged body sync   | `MsgGetBlocks` → `MsgBlocks`               |
| Deep-fork/bootstrap fallback | `MsgGetChain` → `MsgChain`       |
| Discovery          | `MsgGetPeers` → `MsgPeers`                 |

Fork resolution uses a **block locator** (`Locator` / `LocatorFork`): the peer
finds the last common block and only the suffix is transferred and applied via
`ReorgFrom`. Whole-chain exchange (`getchain`) remains only for pathologically
deep forks and initial bootstrap.

**Bounded memory.** The gossip de-duplication sets are bounded FIFOs and the
mempool is capped, so a long-running node's memory doesn't grow without limit.

---

## 12. Persistence

`core/store.go` is an **append-only, length-framed block log** (`blockStore`:
4-byte length + JSON frame per block, with offsets, corrupt-tail truncation, and
a foreign-file guard). `Blockchain.Open(path)` backs a chain with it so:

- `AddBlock` persists in O(1) (one append), and
- a reorg **truncates** back to the fork point and appends the new suffix — no
  whole-file rewrite.

`Save`/`Load` remain as a JSON import/export snapshot. On restart a node loads its
store and re-syncs anything missing from peers.

**Soft state.** Beside the authoritative chain, a node also persists three
*conveniences* to the same directory (`peers.json`, `bans.json`, `mempool.json`),
loading them on start and rewriting them on graceful shutdown. This lets a
restart resume warm — known peers, accrued ban scores, and pending transactions
survive instead of resetting — while the chain stays the single source of truth
that re-syncs from peers regardless. A hard kill may lose the latest soft state
(re-learned from the network), so it is written via temp-file+rename to avoid
torn files.

---

## 13. Wallet

`wallet/` owns keys and identity.

**Addresses.** `dnas` + `hex( sha256(pubkey)[:20] ‖ checksum[4] )`, where the
checksum is `sha256("dnas" ‖ body)[:4]`. `ValidateAddress` verifies prefix,
length, and checksum so a mistyped recipient fails validation instead of burning
coins. (Checksums are enforced **client-side** — in `/send` and the REPL — not in
consensus, to avoid a validation cascade; a malicious client can still burn its
own coins.)

**At-rest encryption.** Key files can be encrypted with PBKDF2 + AES-256-GCM,
opt-in via the `DNAS_WALLET_PASSPHRASE` env var (kept out of flags so it doesn't
leak into `ps`).

**BIP39 + HD.** A wallet can be backed up as a BIP39 mnemonic (vendored canonical
English wordlist, PBKDF2 seed) that deterministically derives many addresses.
Derivation is `HMAC-SHA512(seed, "dnas/ed25519" ‖ index)` → an Ed25519 seed — a
simple, deterministic HD scheme, explicitly **not** SLIP-0010.

**Multisig.** `MultisigAddress(threshold, pubKeysHex)` sorts the keys (so the
address is order-independent) and hashes them into the *same* address format and
checksum as a normal address — so a multisig account is funded and spent like any
other address.

---

## 14. Light clients (SPV)

Because a `Header` hashes identically to its block and commits to a Merkle root,
a client with headers alone can:

1. verify each header's proof-of-work and that it links to its parent, then
2. confirm a transaction's inclusion by folding a compact `MerkleProof` to the
   header's Merkle root.

`Blockchain.FindTxProof` serves a `TxProof` (block index, confirmations, Merkle
root, proof steps). The API exposes `/headers`, `/header/{index}`,
`/proof/{txhash}`; `dnas spv` is a real light client that trusts only headers;
and the web explorer does the same fold in-browser with the Web Crypto API.

**Compact block filters (BIP158-style) — non-inclusion.** A merkle proof shows a
transaction *is* in a block; it says nothing about a block a client didn't ask
about. To let a wallet learn which blocks concern it — and prove which do *not* —
each block also has a **Golomb-Coded Set** filter (`core/cfilter.go`) over the
addresses it touches. Items are placed with SipHash-2-4 (keyed by the block hash,
so the filter is bound to a PoW-verifiable block) and delta-encoded with
Golomb-Rice coding (P=19, M=784931). Testing the filter for an address gives no
false negatives: a non-match is a **proof of non-inclusion** for that block; a
match is "probably present, download to confirm" (≈1/M false positives). The API
serves `/cfilter/{index}`, `/cfilters`, and a BIP157-style filter-header chain at
`/cfheaders`; `dnas spv scan <addr>` PoW-verifies the headers, checks each filter
is bound to its header and consistent with the filter-header chain, then reports
matches and how many blocks the address is provably absent from.

Because the filters are not committed in the PoW header (that would be a
consensus change), their correctness rests on the honest-node / multi-peer
assumption, exactly as in BIP157/158 — a client cross-checks peers or falls back
to downloading the block.

**State proofs (balances).** Inclusion and non-inclusion are about
*transactions*; the header's `StateRoot` (§6, §9) lets a client prove *state*.
`Blockchain.ProveAccount` serves an `AccountProof` — the account's balance and
nonce plus a merkle path to the tip's `StateRoot` — and `VerifyAccountProof`
folds it. `dnas spv balance <addr>` PoW-verifies the headers, then checks the
proof folds to the verified header's state root, proving the balance
trustlessly (`GET /stateproof/{addr}`). `dnas spv history <addr>` combines all
three: it uses the filters to find the (few) blocks touching an address,
downloads *only* those bodies (`GET /block/{index}`), authenticates each against
its PoW-verified header, reconstructs the transfers, and cross-checks the
resulting net against a state proof. This is a real light wallet — it never
trusts a served balance or downloads the whole chain.

**Persistent SPV wallet.** `dnas spv wallet` turns those one-shot commands into a
stateful light wallet (`cmd/dnas/spvwallet.go`). It watches a set of addresses and
stores, in a small JSON file, the height it has scanned to and each address's
reconstructed balance and history. Each `update` fetches and PoW-verifies the
header chain and filters, then folds in *only* the new blocks a filter flags for a
watched address (authenticating each body against its header) — so a resumed
wallet downloads just the handful of blocks since last time, not the chain. If the
block at the last-scanned height no longer carries the hash it recorded, a reorg
happened below it and the wallet rescans from scratch. With `-watch` it follows
the `/events` SSE stream and re-syncs the instant a block or reorg arrives. The
sync core is a pure function over (headers, filters, fetch), so it is unit-tested
without a node.

**Self-custodial sending.** Given a key file (`-key`, encrypted via
`DNAS_WALLET_PASSPHRASE`), `dnas spv wallet send` becomes a real wallet, not just
a viewer: it proves the sender's balance and nonce trustlessly (a state proof
against a PoW-verified header, `provenAccount`), **signs the transaction locally**
— the private key never leaves the client — and submits only the signed
transaction (`POST /tx`). A local next-nonce counter lets several sends queue
before a confirming block without colliding, catching up to the proven nonce as
they confirm.

**Snapshot fast-sync (`core/snapshot.go`).** Because a header commits a
`StateRoot`, the entire account set at a height can be verified in one shot:
recompute `stateRoot(accounts)` and check it equals the (PoW-verified, ideally
checkpointed) header's committed root. `SnapshotAt` serves such a `Snapshot`
(rolling state back through the undo logs), the API exposes it at
`/snapshot/{height}`, and `dnas fastsync` bootstraps from it — PoW-verifying the
header chain, verifying the snapshot against the header's state root and an
optional pinned `-checkpoint`, seeding a chain with `NewFromSnapshot` (header-only
placeholders below the snapshot, real state at it), then downloading and *fully*
validating only the block bodies above it. It reaches the full chain's cumulative
work without replaying settled history — the balances proven, never trusted.
Reorgs below the snapshot are impossible by the finality guards (§8), so the
pruned bodies are never needed. (The fast-synced chain currently runs in memory;
persisting a pruned chain through the append-only, index-based block store would
need a base-offset store format and is left as future work.)

So this SPV layer proves transaction *inclusion* trustlessly, *non-inclusion*
under the filter honest-node assumption, and account *balances* (membership)
against the PoW-committed state root.

---

## 15. HTTP API and explorer

`api/` exposes a small HTTP interface (`Handler()` is extracted so tests drive it
with `httptest`). Highlights:

- **Read:** `/info` (height, tip, work, mempool, `min_relay_fee`, `base_fee`,
  peers, mining), `/chain`, `/balance/{addr}`, `/account/{addr}`, `/mempool`,
  `/peers`, `/address`, `/estimatefee?blocks=N` (recommended fee = base fee +
  estimated tip), `/metrics` (Prometheus text).
- **SPV / filters / state:** `/headers`, `/header/{index}`, `/block/{index}`,
  `/proof/{txhash}`, `/cfilters`, `/cfilter/{index}`, `/cfheaders`,
  `/stateproof/{addr}` (a balance proof against the header state root), and
  `/snapshot/{height}` (the full account state at a height, for fast-sync).
- **Events:** `GET /events` is a **Server-Sent Events** stream that pushes a
  small JSON envelope on every new block, reorg, and mempool transaction, so a
  browser (`EventSource`) or any HTTP client refreshes the instant something
  changes instead of polling. SSE was chosen over WebSockets deliberately: it is
  one-directional server→client (exactly the need), plain HTTP with no framing or
  extra dependency (keeping the `api` module self-contained), and drops straight
  into the explorer. Internally a node has a tiny publish/subscribe bus
  (`node/events.go`) whose subscribers get a buffered channel; a slow consumer
  drops events rather than stalling the node's hot paths.
- **Write (guarded):** `POST /send` (built + signed by the node wallet; optional
  nonce/expiry/lock_until/memo), `POST /tx` (a fully-signed tx incl. multisig,
  HTLC, an asset transfer or an issuance), `POST /mine` (`{on}` toggles mining at
  runtime), `POST /generate` (`{n}`, regtest only: mine N blocks on demand), and
  `POST /submitblock` (accept a block mined by an external miner).
- **Mining:** `GET /blocktemplate?address=ADDR` returns a candidate block (every
  field filled but the winning nonce) so an external miner (`dnas miner`) can hash
  it off-node and submit the result to `/submitblock` — mining is fully decoupled
  from the node. When `DNAS_API_TOKEN`
  is set these require an `Authorization: Bearer <token>` header
  (constant-time compared); read endpoints stay open, and an unset token leaves
  the whole API open (the localhost/toy default). The token is read from the
  environment, not a flag, so it doesn't leak into `ps`.
- **Stateless wallet helpers:** `POST /multisig/address`, `POST /htlc/address`,
  and `POST /wallet/hd` compute an address or derive HD addresses without
  touching node state or holding a secret. They exist so the thin clients (§16)
  share one crypto implementation instead of re-deriving it.

The web explorer (`api/explorer.html`, embedded via `//go:embed`) is served at
`/`: live status, expandable blocks, mempool, a send form, and an in-browser SPV
verifier.

---

## 16. Clients

Both clients drive the same HTTP API and can launch a local node so mining works
out of the box.

- **TUI** (`tui/`, Go/bubbletea): live dashboard, send, SPV verify, mining
  toggle, and — via the stateless helpers — multisig address derivation (`x`) and
  HD wallet generate/restore (`h`).
- **GUI** (`gui/`, PyQt6): the same plus a "Wallet tools" panel. Polling runs on
  a background thread and updates the UI via a Qt signal so a slow node never
  freezes the window.

The web explorer and the TUI subscribe to the `/events` SSE stream and refresh
the instant a block or transaction arrives, keeping only a slow poll as a
fallback (for a dropped stream or a node without the endpoint); the GUI polls.

Neither standalone client can import the Go `wallet` package (the TUI is out of
the workspace; the GUI is Python), which is exactly why multisig/HD/HTLC address
derivation is exposed as **stateless API endpoints** — one source of truth for
the crypto. All of them attach `DNAS_API_TOKEN` to write requests when it is set,
so a locked-down node still works from the same host.

---

## 17. Consensus parameters

All in [`core/params.go`](core/params.go). Every node must agree on these.

| Parameter             | Value            | Meaning                                   |
|-----------------------|------------------|-------------------------------------------|
| `Coin`                | 100 000 000      | base units per DNAS                       |
| `InitialBlockReward`  | 50 · Coin        | first-epoch coinbase subsidy              |
| `HalvingInterval`     | 210 000          | blocks between reward halvings            |
| `GenesisBits`         | ~2^240 target    | compact PoW target at genesis (nBits)     |
| `PowLimit`            | ~2^244 target    | easiest target (difficulty floor); no hard ceiling — difficulty is unbounded |
| `TargetBlockTime`     | 5 s              | desired spacing (LWMA retarget target)    |
| `lwmaWindow`          | 20               | blocks the LWMA retarget averages over    |
| `ProtocolVersion`     | 1                | P2P wire version (peers below are dropped) |
| `CoinbaseMaturity`    | 3                | blocks before a reward is spendable       |
| `MaxReorgDepth`       | 100              | deepest reorg allowed (finality guard)    |
| `MaxBlockTxs`         | 1000             | non-coinbase txs per block                |
| `MaxBlockBytes`       | 1 000 000        | total non-coinbase tx bytes per block     |
| `MaxMemoBytes`        | 256              | per-tx memo cap                           |
| `MaxTickerLen`        | 8                | native-asset ticker length cap            |
| `MaxAssetSupply`      | 2^62             | native-asset supply cap (overflow-safe)   |
| `DustThreshold`       | 1000             | min coin transfer once UpgradeDustLimit is active |
| `MaxFutureDrift`      | 120 s            | how far ahead a timestamp may be          |
| `DefaultMinRelayFee`  | 10 /byte         | base of the dynamic fee floor (policy, per byte) |
| `InitialBaseFee`      | 10 /byte         | EIP-1559 base fee at genesis (consensus, per byte) |
| `MinBaseFee`          | 1 /byte          | base-fee floor (per byte)                 |
| `BaseFeeTargetTxs`    | MaxBlockTxs / 2  | per-block tx count the base fee targets    |
| `BaseFeeMaxChangeDenominator` | 8        | max base-fee change per block (1/8 = 12.5%) |
| `GenesisTimestamp`    | 1735689600       | fixed genesis time (2025-01-01Z)          |

---

## 18. Key decisions and trade-offs

| Decision | Why | Trade-off |
|----------|-----|-----------|
| Unbounded PoW difficulty (LWMA, no hard cap) + `NoRetarget` for devnet | Real economic security — rewriting history costs ever-growing work; a devnet still gets instant blocks | Genesis starts easy; security only exists once real hashpower is present |
| Canonical binary consensus encoding (not JSON) | txid/size/signing are reproducible by any implementation, so a second client can't silently fork | The wire transport is still JSON (a separate efficiency concern) |
| Height-activated consensus upgrades | Rule changes roll out on a coordinated flag-day, not an uncoordinated fork | No miner version-bit signaling yet (needs a header version field) |
| Permissionless by default; `-netkey` opt-in for a private net | Anyone can join — the defining property of a cryptocurrency | The open handshake is anonymous (no MITM authentication); safety rests on many peers + identity + eclipse caps |
| Inbound caps (total + per-IP-group) + per-peer rate limiting | Eclipse/DoS resistance for an open network | Heuristic caps, not a full addrman/ASN-diversity scheme |
| Account+nonce, not UTXO | Simpler state & replay logic to read | Coinbase maturity needs a history scan instead of per-coin locks |
| Smaller-tip-hash tie-break | Deterministic → all nodes converge; monotonic → no flapping | Not first-seen; a heavier/smaller-hash block always wins |
| Coinbase maturity by history scan | No new state, reverses on reorg for free | O(maturity) scan per spend check (tiny here) |
| Two-layer fees: consensus base fee + relay-policy floor | Base fee (burned, in-header) is a real fee market; the relay floor tunes what a node queues | Two knobs to understand; the relay floor doesn't affect block validity |
| Base fee committed in the header | Light clients see it; it's covered by PoW; supply drop is verifiable | Changing the header format invalidates old chains (fine for a dev chain) |
| Fees priced per byte; blocks bounded by bytes | Block space is a metered, priced resource; a big tx pays its share; the mempool ranks by fee rate | A cheap `Size()` (JSON length) approximates real serialized weight |
| Base fee *congestion signal* stays tx-count, not weight | Keeps the retarget cheap to reason about and test | Slightly inconsistent with per-byte pricing; documented in §9 |
| Finality: max-reorg-depth + checkpoints | Settled history can't be rewritten; a fresh/lagging node can't be fed a bogus deep chain | A genuinely longer fork past the depth is also refused (a node stuck offline too long must resync from a trusted store) |
| 256-bit compact target (nBits) + LWMA retarget | Continuous difficulty, smooth per-block retargeting, and fixes the genesis-timestamp collapse | Header change (genesis hash changed); compact encoding is lossy in the low bits |
| Snapshot fast-sync, verified against the state root | Trustless bootstrap without replaying settled history — composes checkpoints + state roots | The fast-synced (pruned) chain runs in memory; persisting it needs a base-offset store format (future work) |
| Dandelion++ on by default | Transaction-origin privacy | Adds relay latency, bounded by the embargo; a small devnet fluffs within a few hops |
| Light wallet signs locally | A real self-custodial wallet — the key never leaves the client | The next nonce is tracked locally between confirmations |
| Native assets in the account, committed in the state root | Tokens with light-client-provable balances; `omitempty` keeps coin-only state (and genesis) unchanged | Fees are always coin (no per-asset fee market); it's balances, not a scripting/contract system |
| External miner protocol (`getblocktemplate`/`submitblock`) | Mining decoupled from the node — hashpower can live elsewhere | A stale template is rejected; the miner refetches |
| Adversarial sim via an injected transport | Stress reorg/finality/sync/partitions in-process, deterministically | Test-only; a reliable stream transport models latency/partitions, not packet loss |
| State root in the header (balance proofs) | Light clients prove balances, not just inclusion | Another header field; proves membership only, not account absence |
| Regtest = on-demand `/generate`, not fast continuous mining | Deterministic, controlled block production; no runaway chain | A separate mode; isolated by netkey rather than a distinct genesis |
| Checksums client-side only | Avoids a consensus validation cascade | A malicious client can still burn its own coins |
| HMAC-SHA512 HD, not SLIP-0010 | Small and self-contained | Not interoperable with standard wallets |
| TUI as its own module | Keeps external deps' `go.sum` off the internal v0.0.0 modules | It can't import `wallet`; multisig/HD go through API helpers |
| Static (CGO-free) binaries | One binary runs on any matching kernel | lintian flags "statically-linked" (acknowledged via an overrides file) |
| HTLC as a script-bound address (not a new UTXO type) | Reuses the multisig pattern; account model unchanged | The claim/refund timeout is enforced at apply-time, not in signature checks |
| Compact filters *not* committed in the header | Non-inclusion without a consensus change | Trust rests on honest-node / multi-peer, not proof-of-work (BIP157/158 model) |
| SSE for the event stream, not WebSockets | One-directional, plain HTTP, zero deps, native `EventSource` | No client→server messaging over it (not needed) |
| API auth as relay-style policy (token on writes only) | Locks spending/mining without breaking public reads or the explorer | Not per-user auth; a shared bearer token, reads unauthenticated |
| Soft state persisted, chain authoritative | Warm restart (bans/peers/mempool survive) without trusting them | A hard kill can lose the latest soft state (re-synced from peers) |

---

## 19. Testing

- **Unit tests** in every module, run under `-race`; the standing rule is that
  each feature lands with a focused test.
- **Native Go fuzzers** (`core/fuzz_test.go`, `wallet/fuzz_test.go`) over
  transaction/header/Merkle/amount decoding and address/mnemonic validation.
- **In-process integration tests** (`node/integration_test.go`) spin up multiple
  nodes on ephemeral ports and assert sync/discovery/convergence.
- **Adversarial network simulation** (`node/simnet_test.go`) wires nodes through a
  switchboard (injected via `Node.dialFn`/`listenFn`) that adds latency and can
  partition links on demand, then asserts that a network which forks under a
  partition re-converges on the most-work chain after healing — stressing reorg,
  fork choice and sync under conditions the plain integration tests don't reach.
- **GUI tests** (`gui/test_dnas_gui.py`) run headless (`QT_QPA_PLATFORM=offscreen`)
  against a stub HTTP server.
- **End-to-end demo** (`scripts/demo.sh`) runs a three-node network exercising
  auth, discovery, a converging transfer, expiry, the fee floor, multisig, HD,
  and SPV.

`make test` runs the Go suites plus the GUI tests (skipped if PyQt6 is absent);
`make test-race` runs the Go suites under the race detector.

---

## 20. Build and release

- **[Makefile](Makefile)** is the entry point: `build`, `test`, `test-race`,
  `vet`, `fmt`, `dist`, `deb`, `install`, `demo`.
- **`scripts/build.sh`** cross-compiles static binaries (`CGO_ENABLED=0`,
  `-trimpath`) for `linux/amd64` + `linux/arm64` and tarballs them.
- **`scripts/build-deb.sh`** builds lintian-clean `.deb` packages (amd64/arm64)
  with `dpkg-deb --root-owner-group`.
- The build **version is stamped** via `-ldflags "-X main.version=…"` (default
  from `git describe`) and reported by `dnas version`.
- **CI** ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) runs a gofmt
  check, `make vet`, `make build`, and `make test-race` on every push and pull
  request, and on a version tag (`v*`) also runs `make dist` + `make deb` and
  uploads the tarballs and `.deb`s as build artifacts.

See [scripts/README.md](scripts/README.md) for the script details.

---

## 21. Known limitations

- The network is now open/permissionless with inbound caps (total + per-IP-group)
  and per-peer rate limiting for eclipse/DoS resistance, but identities and IPs are
  still cheap, so it is not fully sybil-resistant (no proof-of-work/stake peer
  gating, no ASN-diversity addrman). The open handshake is anonymous — it has no
  MITM authentication; safety rests on connecting to many peers. Bans persist
  across a graceful restart but a hard kill can lose them (re-learned from peers).
- Consensus is defined by a canonical binary encoding (portable across
  implementations), but there is still only ONE implementation — no second client
  has verified the spec, and there are no cross-client consensus test vectors.
- Recipient checksums are client-side, not consensus.
- API auth is a single shared bearer token on write endpoints, not per-user
  authentication; reads are unauthenticated.
- Locator sync transfers only the divergent suffix for normal forks; deep/losing
  forks fall back to whole-chain exchange. Reorgs deeper than `MaxReorgDepth`, or
  crossing a checkpoint, are refused (finality) — a node offline past that depth
  must resync from a trusted store rather than over the wire.
- The fee market is a burned, **per-byte** EIP-1559 base fee (consensus) plus
  rate-based eviction, replace-by-fee, and a per-byte relay-policy floor; its
  congestion signal is transaction count, not weight (§9).
- Merkle SPV proves inclusion trustlessly; compact filters add non-inclusion but
  under the honest-node/multi-peer assumption (they aren't header-committed).
  State proofs prove account *membership* (a present balance/nonce) against the
  header state root, not account *absence*.
- HTLC refund timing and coinbase maturity are enforced at block application, not
  in signature verification. HD is not SLIP-0010. Proof of work is a continuous
  256-bit target (nBits) retargeted by an LWMA with **no hard difficulty cap**, so
  on a real network difficulty tracks hashpower without bound; only a devnet /
  regtest (`NoRetarget`) holds it at the easy genesis floor for instant blocks.
- Snapshot fast-sync bootstraps trustlessly (state verified against the header
  state root, anchored by a checkpoint) but the resulting pruned chain runs in
  memory — persisting it through the index-based block store is future work.
- Dandelion++ hides a transaction's origin along the stem, but on a tiny devnet
  with few peers the anonymity set is small; it is a demonstration of the scheme.
- Native assets are balances committed in the state root; fees are always paid in
  coin (no per-asset fee market), and there is no scripting/contract layer — asset
  logic is limited to issue and transfer.
- Regtest is isolated from a devnet by its network key, not a distinct genesis;
  point it at a separate data directory.

Each limitation is a chosen stopping point. [ROADMAP.md](ROADMAP.md) turns this
list into a prioritized plan (on-disk state trie, second implementation, script
VM, addrman/BIP152, and the rest).

It is a toy. Do not point it at the internet.
