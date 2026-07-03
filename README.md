# DNAS

**DNAS** (Definitely Not A Scam) — a small but *working* proof-of-work
cryptocurrency in Go. It has real ownership (Ed25519 signatures, checksummed
addresses, passphrase-encrypted keys), real issuance (mining rewards), an
account+nonce ledger, most-work consensus with reorgs, a bounded mempool, an
authenticated + encrypted peer-to-peer network with peer discovery and
headers-first sync, append-only persistence, an HTTP API, and a built-in web
explorer with in-browser SPV verification.

It is a learning project, not money. Do not point it at the internet.

New here? See [QUICKSTART.md](QUICKSTART.md) to build, mine, run a wallet, send a
payment, form a network, and verify transactions.

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
- **Issuance.** New coins are created only by the coinbase transaction in each
  block, paying the miner `reward + fees`. The reward starts at 50 DNAS and
  halves every 210 000 blocks.
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
- **Light-client proofs (SPV).** Block hashes commit only to header fields plus
  a merkle root, so a client with headers alone can verify proof-of-work and the
  hash chain, then confirm a transaction is included via a compact merkle proof
  — without downloading block bodies. The bundled `dnas spv` command is exactly
  such a light client.
- **Deterministic genesis.** Every node computes the same genesis block, so
  independent nodes can actually agree on one chain.
- **Most-work consensus with a deterministic tie-break, MTP timestamps, and
  reorgs.** Proof of work (leading-zero hash) with a deterministic difficulty
  retarget. Peers adopt the chain with the greatest cumulative work
  (Σ 16^difficulty); equal-work forks are broken by preferring the smaller tip
  hash, so every node converges on the same canonical chain. A block's timestamp
  must exceed the median of the last 11 (median-time-past), bounding timestamp
  manipulation while tolerating small out-of-order stamps. Switching chains is a
  true **reorg**: state is rolled back to the common ancestor via per-block undo
  logs and only the new suffix is applied — no replay from genesis.

## Networking & security

- **Authenticated, encrypted, identified links.** Every connection begins with a
  symmetric X25519 key exchange authenticated by a pre-shared network key
  (`-netkey`) — a peer that doesn't know the key fails an HMAC check and is
  dropped, and traffic is AES-256-GCM encrypted. Each peer then proves its
  **Ed25519 node identity** by signing the session id, so peers are
  cryptographically identified (not just "knows the key").
- **Ban scoring.** Peers that misbehave (e.g. serve an internally-invalid header
  chain) accrue ban points by identity and are cut off past a threshold; a
  simple fork is *not* penalised.
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
- **Bounded memory & dynamic relay fee.** The mempool is capped and evicts the
  lowest-fee transaction under pressure; the gossip de-duplication sets are
  bounded FIFOs, so a long-running node's memory does not grow without limit. It
  also enforces a **dynamic minimum relay fee** (`-minrelayfee`) that starts at a
  configured base and rises quadratically as the pool fills — cheap to relay on
  an idle devnet, priced up under load. This is *local relay policy*, not a
  consensus base fee: a block that includes an under-floor transaction is still
  valid.

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
```

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
| `-minrelayfee` | `10000`      | base min relay fee (base units); rises with mempool load; 0 disables |
| `-mine`      | off            | enable mining                                  |
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
| GET    | `/info`           | height, tip, next difficulty, cumulative work, mempool, min relay fee, peers |
| GET    | `/chain`          | the full chain                                                |
| GET    | `/balance/{addr}` | balance (raw + formatted)                                     |
| GET    | `/account/{addr}` | balance and nonce                                             |
| GET    | `/mempool`        | pending transactions                                          |
| GET    | `/peers`          | connected peers                                               |
| GET    | `/address`        | this node's wallet address                                    |
| GET    | `/headers`        | all block headers (SPV)                                       |
| GET    | `/header/{index}` | one block header (SPV)                                        |
| GET    | `/proof/{txhash}` | transaction-inclusion merkle proof (SPV)                      |
| GET    | `/metrics`        | Prometheus-format node metrics                                |
| POST   | `/send`           | `{"to","amount","fee","expiry"?,"lock_until"?,"memo"?,"nonce"?}` — signed by the node wallet |
| POST   | `/tx`             | submit a fully-signed transaction (incl. multisig)            |
| POST   | `/mine`           | `{"on":bool}` — toggle mining at runtime                      |
| POST   | `/multisig/address` | `{"threshold","pubkeys":[…]}` → M-of-N multisig address (stateless helper) |
| POST   | `/wallet/hd`      | `{"mnemonic"?,"count"?}` → BIP39 mnemonic + derived HD addresses (stateless helper) |

`/send` validates the recipient's checksum, auto-selects the next nonce (pass an
explicit `nonce` with a higher `fee` to fee-bump a stuck transaction), and
accepts optional `expiry`/`lock_until` heights and a `memo`.

```sh
curl -s localhost:8080/info
curl -s -X POST localhost:8080/send -d '{"to":"dnas...","amount":300000000,"fee":10000000}'

# SPV: fetch a proof and the header it commits to
curl -s localhost:8080/proof/<txhash>
curl -s localhost:8080/header/<index>
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

(Edit the `190xx` ports at the top of the script if they are taken.)

## Known limitations (it's a toy)

- Node identities aren't cost-bound, so ban scoring (keyed by identity) can be
  evaded by rotating keys — fine for a friendly devnet, not sybil-resistant.
  There's no allow-list. Ban scores are in-memory and reset on restart.
- Locator sync transfers only the divergent suffix for normal forks; genuinely
  deep or losing forks still fall back to a whole-chain exchange.
- Recipient checksums are enforced client-side (in `/send` and the REPL), not in
  consensus — a malicious client can still burn its own coins.
- The fee market is eviction + replace-by-fee + a dynamic *relay-policy* floor;
  there is no consensus base fee (à la EIP-1559), so the floor governs only what a
  node will queue, not block validity. SPV proves *inclusion*, not non-inclusion
  or full state. HD derivation is a simple HMAC-SHA512 scheme, not SLIP-0010.
- Difficulty bounds are tiny so a laptop can mine instantly.
