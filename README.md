# DNAS

**DNAS** (Definitely Not A Scam) — a small but *working* proof-of-work
cryptocurrency in Go. It has real ownership (Ed25519 signatures, checksummed
addresses, passphrase-encrypted keys, multisig, and hash-time-locked contracts
for atomic swaps), real issuance (mining rewards + a burned EIP-1559 base fee),
an account+nonce ledger, most-work consensus with reorgs, coinbase maturity, a
bounded mempool with fee estimation, an authenticated + encrypted peer-to-peer
network with peer discovery and headers-first sync, append-only persistence
(chain + peers/bans/mempool), and light clients that verify proof-of-work and —
via merkle proofs, BIP158-style compact filters, and a header **state root** —
prove transaction inclusion, *non*-inclusion, and account balances. It exposes an
HTTP API with an optional bearer-token guard, a real-time event stream, and a
regtest mode for on-demand mining, plus a built-in web explorer with in-browser
SPV verification.

It is a learning project, not money. Do not point it at the internet.

New here? See [QUICKSTART.md](QUICKSTART.md) to build, mine, run a wallet, send a
payment, form a network, and verify transactions. For *how and why* it works —
the ledger model, consensus, networking, and the trade-offs behind each choice —
see [DESIGN.md](DESIGN.md).

## What makes it a cryptocurrency (not just a hash-chain)

- **Ownership by signature.** An address is `dnas` + `hex(sha256(pubkey)[:20] ‖
  checksum)`. Only the holder of the Ed25519 private key can sign a transaction
  that spends from it, and the checksum makes a mistyped recipient fail
  validation instead of burning coins. Key files can be passphrase-encrypted at
  rest (PBKDF2 + AES-256-GCM) via `DNAS_WALLET_PASSPHRASE`, and a wallet can be
  backed up as a **BIP39 mnemonic** that deterministically derives many **HD**
  addresses.
- **M-of-N multisig.** An address can be the hash of a multisig script (threshold
  M + N public keys); spending it requires M valid signatures from distinct
  members, verified in consensus.
- **Hash-time-locked contracts (HTLCs).** An address can also be the hash of an
  HTLC script: coins in it are spendable either by the recipient revealing a
  preimage of a committed hash (the *claim* branch, at any height) or by the
  sender after a timeout height (the *refund* branch). Revealing the preimage
  on-chain is what enables **cross-chain atomic swaps**; `dnas htlc` builds and
  spends them and `scripts/htlc-demo.sh` walks both branches end to end.
- **Issuance & a per-byte EIP-1559 base fee.** New coins are created only by the
  coinbase transaction, paying the miner `reward + tips`. The reward starts at 50
  DNAS and halves every 210 000 blocks. Each block also has a **consensus base
  fee priced per byte**: every transaction must pay at least `base fee × its size`,
  and that portion is **burned** (removed from supply); the miner keeps only the
  tip (`fee − base fee × size`). The base fee adjusts each block toward a target
  fullness, so it rises under load and decays when idle, and a block is bounded by
  `MaxBlockBytes` of transaction data — block space is a metered, priced resource
  and the mempool ranks transactions by fee *rate* (fee per byte).
- **Coinbase maturity.** A freshly-mined coinbase reward cannot be spent until it
  is buried under `CoinbaseMaturity` further blocks (3 here; Bitcoin uses 100).
  Both consensus and the miner's own block builder enforce it, so a reorg that
  orphans a block can't let anyone spend a reward that has vanished.
- **No double-spends / no replay.** Amounts are integer base units
  (1 DNAS = 100 000 000 units). Each account has a running balance and a nonce;
  a transaction must use the sender's next nonce and cannot spend more than the
  balance.
- **Time windows, memos & fee-bumping.** A transaction may carry a signed
  `Expiry` (highest valid height) and `LockUntil` (lowest valid height), so it is
  only mineable within a window and is dropped from mempools outside it, plus an
  optional bounded `Memo`. A stuck (low-fee) transaction can be replaced by
  re-sending it at the *same nonce* with a strictly higher fee (replace-by-fee).
- **Light-client proofs (SPV) + compact filters.** Block hashes commit only to
  header fields plus a merkle root, so a client with headers alone can verify
  proof-of-work and the hash chain, then confirm a transaction is included via a
  compact merkle proof — without downloading block bodies. Each block also has a
  **BIP158-style compact filter** (a Golomb-coded set of the addresses it
  touches): a wallet tests these to find blocks that may concern it and to prove
  the ones that *don't* (non-inclusion). Each header also commits a **state root**
  (a merkle root of all accounts), so a light client can *prove* an address's
  balance and nonce — not just transaction inclusion. The bundled `dnas spv`
  command is such a light client: `sync`, `verify <txhash>`, `scan <address>`,
  `balance <address>` (state proof), and `history <address>` (reconstructs a
  wallet's transfers from filters + authenticated blocks). `dnas spv wallet` is a
  **persistent** light wallet: it watches addresses across runs, stores its
  scanned height and reconstructed balances, syncs incrementally (downloading only
  new filter-flagged blocks), detects reorgs, and can `-watch` the live event
  stream. With a key file it is also **self-custodial**: `dnas spv wallet send`
  proves the balance/nonce trustlessly, signs locally (the key never leaves the
  client), and submits only the signed transaction.
- **Snapshot fast-sync.** Because each header commits a state root, a new node can
  bootstrap from a recent trusted point without replaying the whole chain:
  `dnas fastsync` fetches the account state at a (checkpoint) height, verifies it
  against the header's committed state root, then downloads and fully validates
  only the blocks above it — reaching the same cumulative work, balances proven
  rather than trusted.
- **Deterministic genesis.** Every node computes the same genesis block, so
  independent nodes can actually agree on one chain.
- **Most-work consensus with a deterministic tie-break, MTP timestamps, and
  reorgs.** Proof of work is a **256-bit compact target** (Bitcoin-style nBits),
  retargeted every block by an **LWMA** toward the target block time (continuous,
  not the old coarse 3–5 steps). Peers adopt the chain with the greatest
  cumulative work
  (Σ 16^difficulty); equal-work forks are broken by preferring the smaller tip
  hash, so every node converges on the same canonical chain. A block's timestamp
  must exceed the median of the last 11 (median-time-past), bounding timestamp
  manipulation while tolerating small out-of-order stamps. Switching chains is a
  true **reorg**: state is rolled back to the common ancestor via per-block undo
  logs and only the new suffix is applied — no replay from genesis.
- **Finality: bounded reorgs + checkpoints.** A reorg that would discard more than
  `MaxReorgDepth` (100) committed blocks, or fork below a pinned **checkpoint**, is
  refused — settled history is final, so a deep-reorg attack can't rewrite it.
  Genesis is an implicit checkpoint; operators pin more with
  `-checkpoints height:hash,…`. Initial sync and forward extension are unaffected.

## Networking & security

- **Authenticated, encrypted, identified links.** Every connection begins with a
  symmetric X25519 key exchange authenticated by a pre-shared network key
  (`-netkey`) — a peer that doesn't know the key fails an HMAC check and is
  dropped, and traffic is AES-256-GCM encrypted. Each peer then proves its
  **Ed25519 node identity** by signing the session id, so peers are
  cryptographically identified (not just "knows the key").
- **Ban scoring.** Peers that misbehave accrue ban points and are cut off past a
  threshold: failed handshakes are scored by IP (loopback exempt), and
  post-authentication fraud — a bad header chain, or a block that fails its own
  PoW/merkle root — by identity. A simple fork is *not* penalised.
- **Keepalive.** Idle connections are pinged and dropped if they fall silent past
  a timeout, so a half-open (dead) peer doesn't leak a goroutine and slot.
- **Protocol version + capabilities.** After the handshake, peers exchange a
  protocol version (incompatible peers are dropped) and capability flags, so the
  wire format can evolve and optional features are negotiated per connection.
- **Dandelion++ transaction privacy.** New transactions are relayed along a
  private *stem* (forwarded to one epoch-stable peer) before *fluffing* into a
  normal broadcast, so a network observer can't easily pinpoint the origin; an
  embargo timer guarantees delivery if a stem peer stalls. On by default
  (`-dandelion`), it degrades to plain broadcast when disabled.
- **Headers-first, ranged sync + inventory gossip.** New blocks are announced by
  hash (`inv`); peers pull only the bodies they lack (`getdata`). Catching up is
  headers-first: fetch and PoW-check headers (`getheaders`/`headers`), then
  download bodies by range (`getblocks`/`blocks`). Forks are resolved with a
  **block locator** — the peer finds the last common block, and only the
  divergent suffix is transferred and applied as a reorg. Whole-chain transfer
  remains only for deep/pathological forks and initial bootstrap.
- **Peer discovery.** Nodes gossip the addresses they know (`getpeers`/`peers`),
  so a node seeded with a single peer discovers the rest and dials them, up to
  `-maxpeers`. Self-dials and duplicates are avoided.
- **Bounded memory & dynamic relay fee.** The mempool is capped and, under
  pressure, evicts the transaction paying the least *per byte* (fee rate); the
  gossip de-duplication sets are bounded FIFOs, so a long-running node's memory
  does not grow without limit. It also enforces a **dynamic minimum relay fee**
  (`-minrelayfee`, a per-byte rate) that starts at a configured base and rises
  quadratically as the pool fills — cheap to relay on an idle devnet, priced up
  under load. This is *local relay policy*, not the consensus base fee: a block
  that includes an under-floor transaction is still valid.
- **Persistent soft state.** Alongside the append-only chain store, a node
  persists its known peers, ban scores, and pending mempool beside the db file
  and restores them on the next start, so a graceful restart resumes warm (bans
  no longer reset). The chain itself remains authoritative and re-syncs from
  peers regardless.
- **Optional API auth.** Setting `DNAS_API_TOKEN` locks the mutating endpoints
  (`/send`, `/tx`, `/mine`) behind an `Authorization: Bearer <token>` header
  (constant-time compared); read endpoints stay open. Unset, the API is fully
  open — the localhost/toy default. All bundled clients send the token when the
  env var is set.

## Layout

DNAS is a **Go multi-module workspace**: each component is its own module
(`github.com/nexusriot/DNAS/<name>`), tied together by `go.work`. The repo root
is not itself a module.

```
go.work             workspace tying the modules together
wallet/             module .../wallet — Ed25519 keys, addresses, signing
core/               module .../core   — transactions, blocks, chain state, work, mempool   (→ wallet)
node/               module .../node   — encrypted P2P, discovery, gossip, sync, miner       (→ core, wallet)
api/                module .../api    — HTTP API                                            (→ core, node)
cmd/                module .../cmd    — CLI/daemon; main package in cmd/dnas                 (→ all)
scripts/demo.sh     three-node end-to-end demo
```

Dependency direction: `wallet → core → node → api → cmd` (no cycles). Each
module has its own `README.md`.

## Build & test

The quickest path is the **Makefile**:

```sh
make build          # compile dnas + dnas-tui into bin/ (version stamped from git)
make test           # go tests (all modules + tui) + the GUI tests
make test-race      # the same under the race detector
make dist           # cross-compiled release tarballs (linux/amd64 + arm64) → dist/
make deb            # .deb packages (amd64 + arm64) → dist/
make install        # install dnas + dnas-tui to /usr/local/bin (PREFIX overridable)
make help           # list all targets
```

Override the stamped version or targets, e.g. `make deb VERSION=1.2.3`,
`make dist PLATFORMS="linux/amd64"`, `make deb ARCHES="arm64"`.

Under the hood (the root is not a module, so build/test through the workspace
with directory paths — a bare `./...` from the root won't match the sub-modules):

```sh
# everything
go build ./api/... ./cmd/... ./core/... ./node/... ./wallet/...
go test  -race ./api/... ./cmd/... ./core/... ./node/... ./wallet/...

# the TUI client is its own module (external deps); build/test from its dir
( cd tui && go test ./... )

# just the binary
go build -o dnas ./cmd/dnas
```

### Cross-compiling & Debian packages

`scripts/build.sh` cross-compiles static (CGO-free) binaries and bundles each
target as a tarball; `scripts/build-deb.sh` produces `.deb` packages. Both honor
`VERSION`, and select targets via `PLATFORMS` / `ARCHES`:

```sh
PLATFORMS="linux/amd64 linux/arm64" VERSION=0.1.0 ./scripts/build.sh
ARCHES="amd64 arm64"               VERSION=0.1.0 ./scripts/build-deb.sh
```

A package installs `dnas`, `dnas-tui`, and a `dnas-gui` launcher (plus the PyQt
script and docs); it `Recommends` `python3` + `python3-pyqt6` for the desktop
client. `dnas version` prints the stamped build version.

## Run a node

```sh
# creates wallet.json and chain.db in the current dir if missing
go run ./cmd/dnas node -listen :3000 -api :8080 -mine

# encrypt the wallet at rest (also honored by `dnas wallet new/address`)
export DNAS_WALLET_PASSPHRASE='correct horse battery staple'

# require a bearer token on write endpoints (/send, /tx, /mine); reads stay open
export DNAS_API_TOKEN='a-long-random-secret'
```

Peers, ban scores, and the pending mempool are persisted next to the `-db` file
(`peers.json`, `bans.json`, `mempool.json`) and restored on the next start, so a
graceful restart resumes warm.

| Flag         | Default        | Meaning                                        |
|--------------|----------------|------------------------------------------------|
| `-listen`    | `:3000`        | p2p listen address                             |
| `-advertise` | = `-listen`    | address peers should dial us at                |
| `-api`       | `:8080`        | HTTP API address                               |
| `-peers`     | —              | comma-separated seed peer addresses            |
| `-wallet`    | `wallet.json`  | wallet key file (created if missing)           |
| `-db`        | `chain.db`     | append-only blockchain store file              |
| `-netkey`    | `dnas-devnet`  | pre-shared network key; **peers must match**   |
| `-maxpeers`  | `8`            | maximum outbound peer connections              |
| `-mempool`   | `5000`         | max pending transactions                       |
| `-minrelayfee` | `10`         | base min relay fee (base units **per byte**); rises with mempool load; 0 disables |
| `-mine`      | off            | enable mining                                  |
| `-regtest`   | off            | regtest mode: mine on demand via `POST /generate` (isolated netkey) |
| `-dandelion` | on             | relay new transactions via Dandelion++ stem/fluff (origin privacy) |
| `-checkpoints` | —            | finality checkpoints, comma-separated `height:hash` pairs |
| `-config`    | —              | JSON config file (flags override its values)   |

A node shuts down cleanly on `SIGINT`/`SIGTERM` (stops mining, closes peers,
flushes the store). When stdin is a terminal you also get a REPL:

```
dnas> address
dnas> send <address> <amount> [fee]
dnas> balance [address]
dnas> info | peers | mempool
```

A second node on the same machine just needs distinct ports and a peer:

```sh
go run ./cmd/dnas node -listen :3001 -api :8081 -peers localhost:3000 -mine \
  -wallet w2.json -db chain2.json
```

## HTTP API

| Method | Path              | Purpose                                                       |
|--------|-------------------|---------------------------------------------------------------|
| GET    | `/info`           | height, tip, next difficulty, work, mempool, min relay fee, base fee, peers |
| GET    | `/chain`          | the full chain                                                |
| GET    | `/balance/{addr}` | balance (raw + formatted)                                     |
| GET    | `/account/{addr}` | balance and nonce                                             |
| GET    | `/mempool`        | pending transactions                                          |
| GET    | `/peers`          | connected peers                                               |
| GET    | `/address`        | this node's wallet address                                    |
| GET    | `/estimatefee`    | `?blocks=N` → recommended fee **rate** per byte (base fee + estimated tip) |
| GET    | `/headers`        | all block headers (SPV)                                       |
| GET    | `/header/{index}` | one block header (SPV)                                        |
| GET    | `/block/{index}`  | one full block body (light clients fetch only flagged blocks) |
| GET    | `/proof/{txhash}` | transaction-inclusion merkle proof (SPV)                      |
| GET    | `/stateproof/{addr}` | proof of an address's balance/nonce vs the header state root |
| GET    | `/snapshot/{height}` | full account state at a height (fast-sync; `/snapshot/latest`) |
| GET    | `/cfilters`       | compact block filters for every block (light-client scan)     |
| GET    | `/cfilter/{index}`| one block's compact filter                                    |
| GET    | `/cfheaders`      | the filter-header chain (BIP157-style)                        |
| GET    | `/events`         | Server-Sent Events stream of new blocks / reorgs / mempool txs |
| GET    | `/metrics`        | Prometheus-format node metrics                                |
| POST   | `/send` 🔒        | `{"to","amount","fee","expiry"?,"lock_until"?,"memo"?,"nonce"?}` — signed by the node wallet |
| POST   | `/tx` 🔒          | submit a fully-signed transaction (multisig / HTLC / plain)   |
| POST   | `/mine` 🔒        | `{"on":bool}` — toggle mining at runtime                      |
| POST   | `/generate` 🔒    | `{"n":N}` — regtest only: mine N blocks on demand             |
| POST   | `/multisig/address` | `{"threshold","pubkeys":[…]}` → M-of-N multisig address (stateless helper) |
| POST   | `/htlc/address`   | `{"hash","recipient","sender","timeout"}` → HTLC address (stateless helper) |
| POST   | `/wallet/hd`      | `{"mnemonic"?,"count"?}` → BIP39 mnemonic + derived HD addresses (stateless helper) |

🔒 = requires `Authorization: Bearer $DNAS_API_TOKEN` when that env var is set (otherwise open).

`/send` validates the recipient's checksum, auto-selects the next nonce (pass an
explicit `nonce` with a higher `fee` to fee-bump a stuck transaction), and
accepts optional `expiry`/`lock_until` heights and a `memo`.

```sh
curl -s localhost:8080/info
curl -s -X POST localhost:8080/send -d '{"to":"dnas...","amount":300000000,"fee":10000000}'

# SPV: fetch a proof and the header it commits to
curl -s localhost:8080/proof/<txhash>
curl -s localhost:8080/header/<index>

# live event stream (blocks / reorgs / mempool txs) as Server-Sent Events
curl -sN localhost:8080/events

# if the node is locked down with DNAS_API_TOKEN, writes need the token
curl -s -H "Authorization: Bearer $DNAS_API_TOKEN" -X POST localhost:8080/mine -d '{"on":true}'
```

### Command-line light client & contracts

```sh
dnas spv -api localhost:8080 sync              # verify the header chain (PoW only)
dnas spv -api localhost:8080 verify <txhash>   # prove a payment is included
dnas spv -api localhost:8080 scan    <address> # find/prove non-inclusion of an address (compact filters)
dnas spv -api localhost:8080 balance <address> # prove an address's balance against the state root
dnas spv -api localhost:8080 history <address> # reconstruct a wallet's history (light wallet)

# a regtest node mines on demand instead of waiting on the block interval
dnas node -regtest -api localhost:8080 &
curl -s -X POST localhost:8080/generate -d '{"n":10}'   # mine 10 blocks instantly

dnas wallet pubkey -o alice.json               # print a wallet's public key (to build multisig/HTLC)
dnas htlc new                                  # mint a preimage + its hash for a swap
dnas htlc address -hash H -recipient R -sender S -timeout T
dnas htlc claim  -wallet alice.json -hash H -sender S  -timeout T -preimage P -to <addr>
dnas htlc refund -wallet bob.json   -hash H -recipient R -timeout T           -to <addr>
```

## Clients

Beyond the built-in REPL and web explorer, two standalone clients drive the same
HTTP API (live status, wallet balance, send, SPV verify, a mining toggle, plus
wallet tools to derive a **multisig** address and generate/restore an **HD**
wallet):

- **Terminal UI** (Go / bubbletea) — [`tui/`](tui/): `cd tui && go build -o dnas-tui . && ./dnas-tui -api localhost:8080` (keys: `[x]` multisig, `[h]` HD wallet)
- **Desktop GUI** (Python / PyQt6) — [`gui/`](gui/): `python3 gui/dnas_gui.py --api localhost:8080` (see the "Wallet tools" panel)

Both can also launch a local node so mining works out of the box.

## Web explorer

Every node serves a self-contained web explorer at its API root — just open the
API address in a browser:

```sh
go run ./cmd/dnas node -api :8080 -mine   # then visit http://localhost:8080/
```

It shows live chain status, recent blocks (click to expand their transactions),
the mempool, and the node wallet with a send form. It also acts as a **light
client**: paste a transaction hash and it fetches the proof + header and
verifies inclusion *in your browser* — folding the merkle proof and checking the
header's proof-of-work with the Web Crypto API, exactly as an SPV client would.

## Demo

`./scripts/demo.sh` builds the binary and starts a three-node network to show,
end to end:

- **auth/encryption** — a node started with the wrong `-netkey` is rejected;
- **discovery** — node 3 is seeded with node 1 only, yet finds node 2 by gossip;
- **consensus** — three racing miners produce equal-work forks that the
  deterministic tie-break resolves; a signed transfer confirms on every node and
  heights/work converge;
- **expiry** — a transaction whose expiry is in the past is refused;
- **fee floor / wallet tools** — the current dynamic minimum relay fee, plus a
  derived 2-of-3 multisig address and a freshly generated BIP39 HD wallet;
- **SPV** — an independent Python light client fetches a header + merkle proof
  and verifies the transfer's inclusion (header PoW + proof fold).

`./scripts/htlc-demo.sh` is a focused companion demo: on a single mining node it
funds two hash-time-locked contracts and settles one via the **claim** (preimage)
branch and the other via the **refund** (timeout) branch.

(Edit the `190xx` ports at the top of either script if they are taken.)

## Known limitations (it's a toy)

- Node identities aren't cost-bound, so ban scoring (keyed by identity) can be
  evaded by rotating keys — fine for a friendly devnet, not sybil-resistant.
  There's no allow-list. Ban scores persist across a *graceful* restart but a
  hard kill can lose the latest state (it re-syncs from peers anyway).
- Locator sync transfers only the divergent suffix for normal forks; genuinely
  deep or losing forks still fall back to a whole-chain exchange.
- Recipient checksums are enforced client-side (in `/send` and the REPL), not in
  consensus — a malicious client can still burn its own coins.
- The fee market is a burned EIP-1559 base fee (consensus) plus eviction,
  replace-by-fee, and a dynamic relay-policy floor. HD derivation is a simple
  HMAC-SHA512 scheme, not SLIP-0010.
- State-root balance proofs prove *membership* (a present account's exact
  balance/nonce); they do not prove *absence* of an account (that would need a
  sorted-tree non-membership proof).
- Merkle SPV proves *inclusion*; compact filters add *non-inclusion*, but the
  filters aren't committed in the PoW header, so (like BIP157/158) their
  correctness rests on the honest-node / multi-peer assumption rather than being
  trustless. Committing a filter root in the header would be a consensus change.
- Difficulty bounds are tiny so a laptop can mine instantly.
