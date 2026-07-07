# DNAS design

This document explains **how DNAS works and why it is built the way it is** —
the data model, consensus, networking, and the deliberate trade-offs behind each
choice. For usage see [README.md](README.md) and [QUICKSTART.md](QUICKSTART.md);
each module also has its own `README.md`.

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

- Not money, not sybil-resistant, not for the open internet. Difficulty bounds
  are tiny so a laptop mines instantly.
- No smart contracts, no privacy features, no economic security guarantees.

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
type Account struct { Balance uint64; Nonce uint64 }
```

- **Balances** are integer base units. `Coin = 100_000_000` (1 DNAS), so amounts
  on the wire and in state never touch floating point.
- **Nonces** are per-account and strictly sequential. A transaction must use the
  sender's next nonce, which prevents replay and orders a sender's transactions.
- Chain state is a `map[address]Account` derived by applying blocks in order. It
  is never trusted from the wire; it is recomputed from validated blocks.

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
    Signature        []byte          // single-key authorization
    Multisig         *MultisigScript // OR multisig authorization
    Signatures       [][]byte
}
```

**Signing.** The signed message (`signingBytes`) covers every consensus-relevant
field (From/To/Amount/Fee/Nonce/Expiry/LockUntil/Memo) but **not** the signature
fields. Crucially it is *identical* for single-key and multisig transactions, so
the two authorization paths sign the same bytes.

**Authorization** is resolved by `VerifySignature`:

- **Single key:** the signature must verify against the public key that hashes to
  `From`.
- **M-of-N multisig:** `From` must equal `MultisigAddress(Threshold, PubKeys)`
  (so the script is bound to the address it spends), and at least `Threshold`
  signatures from *distinct* listed members must verify. `verifyMultisig`
  enforces distinctness so one member can't satisfy a 2-of-N alone.

**Time windows.** `Expiry` (upper bound) and `LockUntil` (lower bound) constrain
the height range in which a transaction is valid; both are signed. Expired
transactions are pruned from the mempool and rejected at submission; locked ones
are skipped during selection.

**Replace-by-fee.** A stuck transaction is bumped by re-submitting it at the
*same* `(From, Nonce)` with a strictly higher fee; the mempool replaces the old
one (§10).

**Coinbase.** The block's first transaction mints `reward + fees` to the miner
and has no signature (`IsCoinbase`).

---

## 6. Blocks, headers, and Merkle proofs

```go
type Block  struct { Index, Timestamp, Nonce; PrevHash; Difficulty; Transactions; Hash }
type Header struct { Index, Timestamp, Nonce; PrevHash; MerkleRoot; Difficulty; Hash }
```

A block hash commits to header fields **plus a Merkle root** of its transactions.
A `Header` hashes *identically* to its block — so a client holding only headers
can verify proof-of-work and the hash chain without block bodies. This is what
makes SPV (§14) possible.

`core/merkle.go` builds `MerkleRoot`, `MerkleProof`, and `VerifyMerkleProof`. An
odd level duplicates its last node (Bitcoin's rule); the proof and root use the
same rule so they agree.

---

## 7. Proof of work and difficulty

Proof of work is a **leading-zero-hex** target: a block hash must begin with
`Difficulty` `0` nibbles. Cumulative work is defined so it can be compared across
forks:

```
BlockWork = 16 ^ difficulty        ChainWork = Σ BlockWork
```

Difficulty is **retargeted deterministically** every `RetargetInterval` blocks
toward `TargetBlockTime`, clamped to `[MinDifficulty, MaxDifficulty]`. Bounds are
tiny by design (3–5) so a devnet never stalls on an unreachable target or spins
uselessly on a trivial one, and every node derives the same next difficulty.

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
blocks until it reaches zero; miners also collect transaction fees. Genesis is
fixed (`GenesisTimestamp`, `GenesisPrevHash`) so every node computes an identical
genesis hash and can agree on the same chain.

---

## 10. Mempool

`core/mempool.go` is a bounded pool of validated, unmined transactions.

- **Bounded with lowest-fee eviction.** Capped at `DefaultMempoolSize` (5000);
  once full, a new transaction is admitted only by out-bidding the cheapest
  queued one, which it evicts.
- **Replace-by-fee.** A conflicting `(From, Nonce)` may be replaced only by a
  strictly higher fee.
- **Dynamic minimum relay fee — *policy, not consensus*.** `MinFee()` starts at a
  configurable base (`NewMempoolWithPolicy`, `-minrelayfee`, default
  `DefaultMinRelayFee = Coin/10000`) and rises quadratically with occupancy:

  ```
  floor = base · (1 + (feeFloorMaxMultiplier−1) · fill² / 100²)      fill = %full, 0..100
  ```

  so it stays near the base on an idle devnet and reaches `base·100` when full.
  A transaction below the current floor is refused entry, but this only governs
  what *this* node queues and gossips — a block that includes an under-floor
  transaction is still valid. This is intentionally **not** an EIP-1559 consensus
  base fee (which would need header/SPV/supply surgery).
- **Nonce-aware selection.** `Select` greedily builds a valid sequence for the
  next block: each transaction must have its sender's next nonce and be
  affordable *from spendable balance* (so immature coinbase is never spent);
  recipients are credited within the simulation so chained spends can share a
  block; higher fees win among ready candidates.
- **Expiry pruning** drops transactions that can no longer be mined.

---

## 11. Networking

`node/` implements an authenticated, encrypted, identified peer-to-peer network.

**Secure transport (`secureConn`, `secureHandshake`).** Every connection begins
with an X25519 ECDH key exchange **authenticated by a pre-shared network key**
(`-netkey`): a peer that doesn't know the key fails an HMAC check and is dropped.
Traffic is then AES-256-GCM encrypted, length-framed, with a JSON encoder/decoder
running over it. The handshake sends and receives concurrently so it also works
over `net.Pipe` (used in tests).

**Peer identity.** The handshake yields a deterministic session id; each peer
then signs it with its **Ed25519 node identity** (`MsgIdentity`), so peers are
cryptographically identified, not merely "knows the key".

**Ban scoring (`banbook`).** Peers that serve internally-invalid data (e.g. a bad
header chain) accrue ban points *by identity* and are cut off past a threshold. A
plain fork is not penalised. Keyed by identity, not IP, so localhost demo peers
aren't all banned together — but this means bans are evadable by key rotation
(accepted for a friendly devnet).

**Discovery (`peerbook`).** Nodes gossip known addresses (`MsgGetPeers` /
`MsgPeers`) and auto-dial discovered peers up to `-maxpeers`, excluding
self/dupes/already-connected.

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
and the web explorer does the same fold in-browser with the Web Crypto API. SPV
proves *inclusion*, not non-inclusion or full state.

---

## 15. HTTP API and explorer

`api/` exposes a small HTTP interface (`Handler()` is extracted so tests drive it
with `httptest`). Highlights:

- **Read:** `/info` (height, tip, work, mempool, `min_relay_fee`, peers, mining),
  `/chain`, `/balance/{addr}`, `/account/{addr}`, `/mempool`, `/peers`,
  `/address`, `/metrics` (Prometheus text).
- **SPV:** `/headers`, `/header/{index}`, `/proof/{txhash}`.
- **Write:** `POST /send` (built + signed by the node wallet; optional
  nonce/expiry/lock_until/memo), `POST /tx` (a fully-signed tx incl. multisig),
  `POST /mine` (`{on}` toggles mining at runtime).
- **Stateless wallet helpers:** `POST /multisig/address` and `POST /wallet/hd`
  compute an address or derive HD addresses without touching node state or
  holding a secret. They exist so the thin clients (§16) share one crypto
  implementation instead of re-deriving it.

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

Neither client can import the Go `wallet` package (the TUI is out of the
workspace; the GUI is Python), which is exactly why multisig/HD are exposed as
**stateless API endpoints** — one source of truth for the crypto.

---

## 17. Consensus parameters

All in [`core/params.go`](core/params.go). Every node must agree on these.

| Parameter             | Value            | Meaning                                   |
|-----------------------|------------------|-------------------------------------------|
| `Coin`                | 100 000 000      | base units per DNAS                       |
| `InitialBlockReward`  | 50 · Coin        | first-epoch coinbase subsidy              |
| `HalvingInterval`     | 210 000          | blocks between reward halvings            |
| `GenesisDifficulty`   | 4                | leading-zero nibbles at genesis           |
| `MinDifficulty`/`Max` | 3 / 5            | retarget clamp                            |
| `TargetBlockTime`     | 5 s              | desired spacing                           |
| `RetargetInterval`    | 10               | blocks between retargets                  |
| `CoinbaseMaturity`    | 3                | blocks before a reward is spendable       |
| `MaxBlockTxs`         | 1000             | non-coinbase txs per block                |
| `MaxMemoBytes`        | 256              | per-tx memo cap                           |
| `MaxFutureDrift`      | 120 s            | how far ahead a timestamp may be          |
| `DefaultMinRelayFee`  | Coin / 10 000    | base of the dynamic fee floor (policy)    |
| `GenesisTimestamp`    | 1735689600       | fixed genesis time (2025-01-01Z)          |

---

## 18. Key decisions and trade-offs

| Decision | Why | Trade-off |
|----------|-----|-----------|
| Account+nonce, not UTXO | Simpler state & replay logic to read | Coinbase maturity needs a history scan instead of per-coin locks |
| Smaller-tip-hash tie-break | Deterministic → all nodes converge; monotonic → no flapping | Not first-seen; a heavier/smaller-hash block always wins |
| Coinbase maturity by history scan | No new state, reverses on reorg for free | O(maturity) scan per spend check (tiny here) |
| Fee floor is relay policy | Avoids header/SPV/supply changes an EIP-1559 base fee needs | Doesn't affect block validity; a low-floor node still accepts the tx in a block |
| Checksums client-side only | Avoids a consensus validation cascade | A malicious client can still burn its own coins |
| HMAC-SHA512 HD, not SLIP-0010 | Small and self-contained | Not interoperable with standard wallets |
| TUI as its own module | Keeps external deps' `go.sum` off the internal v0.0.0 modules | It can't import `wallet`; multisig/HD go through API helpers |
| Static (CGO-free) binaries | One binary runs on any matching kernel | lintian flags "statically-linked" (acknowledged via an overrides file) |

---

## 19. Testing

- **Unit tests** in every module, run under `-race`; the standing rule is that
  each feature lands with a focused test.
- **Native Go fuzzers** (`core/fuzz_test.go`, `wallet/fuzz_test.go`) over
  transaction/header/Merkle/amount decoding and address/mnemonic validation.
- **In-process integration tests** (`node/integration_test.go`) spin up multiple
  nodes on ephemeral ports and assert sync/discovery/convergence.
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

See [scripts/README.md](scripts/README.md) for the script details.

---

## 21. Known limitations

- Not sybil-resistant; identities aren't cost-bound and bans reset on restart.
- Recipient checksums are client-side, not consensus.
- Locator sync transfers only the divergent suffix for normal forks; deep/losing
  forks fall back to whole-chain exchange.
- The fee market is eviction + replace-by-fee + a relay-policy floor — no
  consensus base fee.
- SPV proves inclusion only; HD is not SLIP-0010; difficulty bounds are tiny.

It is a toy. Do not point it at the internet.
