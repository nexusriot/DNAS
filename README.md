# DNAS

**DNAS** (Definitely Not A Scam) — a small but *working* proof-of-work
cryptocurrency in Go. It has real ownership (Ed25519 signatures, checksummed
addresses, passphrase-encrypted keys, multisig, time-delayed vaults, and
hash-time-locked contracts for atomic swaps), separate **networks** whose
signatures cannot be replayed across them, real issuance (mining rewards + a
burned EIP-1559 base fee),
an account+nonce ledger, most-work consensus with reorgs, coinbase maturity, a
bounded mempool with fee estimation, an authenticated + encrypted peer-to-peer
network with peer discovery and headers-first sync, append-only persistence
(chain + peers/bans/mempool), and light clients that verify proof-of-work and —
via merkle proofs, BIP158-style compact filters, and a header **state root** —
prove transaction inclusion, *non*-inclusion, and account balances. It exposes an
HTTP API with an optional bearer-token guard, a real-time event stream, an
optional address index, a long-polling mining protocol with pool-style shares, a
testnet faucet, and a regtest mode for on-demand mining, plus a built-in web
explorer with in-browser SPV verification.

It is a learning project, not money. Do not point it at the internet.

New here? See [QUICKSTART.md](QUICKSTART.md) to build, mine, run a wallet, send a
payment, form a network, and verify transactions. For *how and why* it works —
the ledger model, consensus, networking, and the trade-offs behind each choice —
see [DESIGN.md](DESIGN.md). For what it deliberately does *not* do yet, and where
it would go next, see [ROADMAP.md](ROADMAP.md).

## What makes it a cryptocurrency (not just a hash-chain)

- **Ownership by signature.** An address is `dnas` + `hex(sha256(pubkey)[:20] ‖
  checksum)`. Only the holder of the Ed25519 private key can sign a transaction
  that spends from it, and the checksum makes a mistyped recipient fail
  validation instead of burning coins. Key files can be passphrase-encrypted at
  rest (PBKDF2 + AES-256-GCM) via `DNAS_WALLET_PASSPHRASE`, and a wallet can be
  backed up as a **BIP39 mnemonic** that deterministically derives many **HD**
  addresses.
- **A node identity that is not your wallet.** A node proves itself to peers with
  an Ed25519 key and sends the *public* key in the handshake — and a DNAS address
  is a hash of a public key. Using the wallet key for both therefore hands every
  peer the address holding your coin, and undercuts the Dandelion++ origin privacy
  the node already has. The identity is its own file (`nodekey.json`, `-nodekey`),
  created on first run; it holds no coin and needs no backup.
- **Separate networks.** A node runs on `mainnet`, `testnet` or `regtest`
  (`-network`), and the network's id is bound into the **genesis block**, the
  **transaction signing preimage** and the **peer handshake**. So the three have
  different chains, a signature made on one does not authorize the same transfer
  on another, and nodes on different networks disconnect instead of failing to
  converge. Mainnet's id is empty, so every existing encoding is byte-for-byte
  unchanged.
- **M-of-N multisig, spendable.** An address can be the hash of a multisig script
  (threshold M + N public keys); spending it requires M valid signatures from
  distinct members, verified in consensus. `dnas multisig propose|sign|submit`
  passes the spend between the members as a file that gains one signature per
  stop — signing is entirely **offline**, and the file records which chain it is
  for, because a signature commits to the network id. `dnas escrow` is that with
  role names on the three members (buyer, seller, arbiter), so the happy path is
  buyer + seller and the arbiter's only power is to break a tie.
- **Time-delayed vaults.** An address can be the hash of a vault script (a *hot*
  key, a *cold* key, and an unlock height): the cold key spends at any time, the
  hot key only from the unlock height on. Steal the hot key and you must wait out
  the delay — long enough for the offline cold key to move the coin somewhere
  safe. `dnas vault address|spend` builds them; gated by the `vault` consensus
  upgrade.
- **Someone else can pay your fee.** A transaction may name a `FeePayer`: the
  sender signs who pays, that party counter-signs the same bytes, and the fee is
  charged to *them*. An address holding no coin at all can therefore transact.
  The sponsor spends no nonce, so a sponsorship is bound to exactly one transfer
  and cannot be replayed or lifted onto another. `dnas sponsor request` / `dnas
  sponsor pay` are the two halves of the flow. Gated by the `feesponsor`
  consensus upgrade.
- **Hash-time-locked contracts (HTLCs).** An address can also be the hash of an
  HTLC script: coins in it are spendable either by the recipient revealing a
  preimage of a committed hash (the *claim* branch, at any height) or by the
  sender after a timeout height (the *refund* branch). Revealing the preimage
  on-chain is what enables **cross-chain atomic swaps**; `dnas htlc` builds and
  spends them and `scripts/htlc-demo.sh` walks both branches end to end.
- **A payment can carry a memo and a deadline.** `Memo`, `Expiry` and
  `LockUntil` are signed consensus fields — an expiry is how a payment stops
  being an open-ended liability, since without one a signed transaction can be
  mined much later at a nonce that has not moved. `dnas spv wallet -memo
  -expire-in -lock-for` sets them, and an inverted window (a lock above the
  expiry) is refused by consensus as unminable at any height.
- **Proving things without spending.** `dnas wallet sign|verify` proves control
  of an address off-chain, domain-separated from transaction signing so such a
  signature can never be replayed as a transfer. `dnas anchor` publishes
  `sha256(file)` on the chain and later proves the file existed before a block.
  `dnas invoice` states what is wanted, prints a `dnas:` URI, and verifies
  settlement with confirmations against a chain it proof-of-work-checked itself.
- **Payment URIs that are actually read.** `dnas:ADDRESS?amount=2.5&memo=…` is
  what a payee hands over, and every surface that would be handed one parses it:
  `dnas invoice pay -uri`, `dnas spv wallet send <uri>`, the TUI send prompt, the
  web explorer and the PyQt client all fill in the amount and memo it carries. The
  address is checksum-validated on the way in, so pasting a URI is the version of
  paying that cannot mistype the recipient — which matters because consensus does
  not check recipient checksums. An amount typed alongside a URI that asks for a
  different one is refused rather than silently overridden.
- **Issuance & a per-byte EIP-1559 base fee.** New coins are created only by the
  coinbase transaction, paying the miner `reward + tips`. The reward starts at 50
  DNAS and halves every 210 000 blocks. Each block also has a **consensus base
  fee priced per byte**: every transaction must pay at least `base fee × its size`,
  and that portion is **burned** (removed from supply); the miner keeps only the
  tip (`fee − base fee × size`). The base fee adjusts each block toward a target
  fullness, so it rises under load and decays when idle, and a block is bounded by
  `MaxBlockBytes` of transaction data — block space is a metered, priced resource
  and the mempool ranks transactions by fee *rate* (fee per byte).
- **Native assets (tokens).** Beyond the base coin, any address can **issue** a
  named asset and transfer it; balances are tracked per account and **committed in
  the state root**, so a light client proves an asset balance just like a coin
  balance. Fees are always paid in coin. (`dnas spv wallet -key F issue <ticker>
  <supply>` / `... -asset <id> send <to> <amt>`.)
- **Coinbase maturity.** A freshly-mined coinbase reward cannot be spent until it
  is buried under `CoinbaseMaturity` further blocks (3 here; Bitcoin uses 100).
  Both consensus and the miner's own block builder enforce it, so a reorg that
  orphans a block can't let anyone spend a reward that has vanished.
- **No double-spends / no replay.** Amounts are integer base units
  (1 DNAS = 100 000 000 units). Each account has a running balance and a nonce;
  a transaction must use the sender's next nonce and cannot spend more than the
  balance.
- **Pay many people in one transaction.** `Outputs` carries up to
  `MaxTxOutputs` recipients, so a batch payment costs one fee, one nonce and one
  signature instead of N of each — `POST /send {"outputs":[…]}` or
  `dnas spv wallet -key F sendmany addr:amount …`. It is gated by a
  height-activated consensus upgrade (`-upgrades multioutput:HEIGHT`), and a
  single-recipient transaction encodes exactly as it always did, so no existing
  transaction id, signature or stored chain changes.
- **Time windows, memos & fee-bumping.** A transaction may carry a signed
  `Expiry` (highest valid height) and `LockUntil` (lowest valid height), so it is
  only mineable within a window and is dropped from mempools outside it, plus an
  optional bounded `Memo`. A stuck (low-fee) transaction can be replaced by
  re-sending it at the *same nonce* with a strictly higher fee (replace-by-fee).
- **Transactions survive a reorg.** Losing a race is not the same as being
  invalid. When a node reorgs, the payments confirmed only in the branch it
  abandoned are returned to its mempool to be mined again, instead of silently
  vanishing until their sender notices; anything the winning branch already
  confirms, or whose nonce it has since spent, is dropped instead. Confirmed
  transactions are located through an index (`GET /tx/{hash}` answers for both a
  mined and a still-pending transaction), so this costs one map lookup, not a scan.
- **Auditable supply.** Coin enters only through block subsidies and leaves only
  through the burned base fee, and both are tracked: `GET /supply` (or
  `dnas supply`) reports how much has ever been minted, how much has been burned,
  and how much accounts actually hold — plus the conservation check
  `minted − burned == circulating`, which must hold for every valid chain.
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
- **External mining, with long polling and shares.** Mining is decoupled from the
  node: `GET /blocktemplate` hands out a candidate block, an external miner
  (`dnas miner`) searches for the winning nonce off-node, and `POST /submitblock`
  accepts it — so hashpower can live on a separate machine. The template request
  can **long poll** (`?longpoll=1&prev=HASH`), so a miner starts on a fresh
  candidate the instant the old one dies rather than hashing a dead one until its
  next poll. It also carries a `share_bits` target several hundred times easier
  than the block's: `dnas miner -shares` submits every hash clearing it to
  `POST /submitshare`, which is how a pool can see (and pay for) hashpower that
  has not found a block. `GET /shares` reports the ledger.
- **Mempool reconciliation.** A node asks each peer, once it has caught up, for
  the transactions it has pending — so a node that was down when a payment was
  broadcast learns about it on connect instead of waiting for someone to
  rebroadcast. Everything it is told goes through ordinary admission.
- **Address history on the node.** With `-addrindex`, `GET /address/{addr}/history`
  lists every transaction that touched an address (sender, recipient or fee
  sponsor), oldest first, paged. It is opt-in because its size is bounded by usage
  rather than by the chain.
- **A faucet for throwaway networks.** `POST /faucet {"address":…}` (or
  `dnas faucet`) pays out of the node's wallet on testnet or regtest, rate-limited
  per address and per client. It is impossible on mainnet by definition rather
  than by policy — the network's parameters decide whether a faucet may exist at
  all, so no flag can turn a real chain into a free one.
- **Bounded reads, and a light client that stays light.** `/chain`, `/headers`,
  `/cfilters` and `/cfheaders` are paged (`?from=&limit=`) rather than serializing
  the whole chain into one response, and `dnas spv` keeps its **verified headers
  between runs** — so a command downloads only what is new instead of re-fetching
  every header (tens of megabytes per invocation on a long chain). The cache is
  trustless: a batch must link onto it and satisfy its own proof of work, and a
  reorg below the cached tip discards it and rebuilds.
- **Operator visibility.** `dnas peers` shows every connection in detail
  (identity, negotiated version, capabilities, direction, uptime, ban score, and
  whether it is currently serving blocks); `dnas peers bans` shows scored *and*
  banned keys with the threshold, `unban` clears one, and `add`/`drop` manage
  connections at runtime. `dnas stats` reports estimated network hashrate, the
  block-interval distribution against the target, fee/burn/tip flow and who has
  been mining. `dnas reorgs` lists the chain switches this node has lived through
  — the event stream announces one and then forgets it. `dnas health` answers
  "can I rely on this node?" and **exits non-zero** when the answer is no.
- **Fee-bumping and cancelling.** Replace-by-fee has always been in consensus;
  now a client can use it. `dnas spv wallet bump <txid>` re-sends the same
  payment at a higher fee under the same nonce, and `cancel <txid>` spends that
  nonce on a self-payment so the original can never be mined.
- **Assets you can read.** `GET /assets` / `dnas assets` describe what the chain
  has issued — ticker, issuer, fixed supply, and who holds it. An asset id is a
  hash of (issuer, ticker, nonce), so a balance of `tok3f2a…` said nothing on its
  own; a ticker resolves to a *list*, because anyone may issue "GOLD" and the id
  is the identifier.
- **A node that does not have to grow.** `-prune N` keeps the account state and
  every header for all time and drops the bodies deeper than N, so a node's
  resident size stops tracking the chain. What it can no longer serve it says
  plainly — `410 Gone`, plus `body_height` in `/info` — rather than answering
  "not found", and it never serves an *empty* compact filter for a body it lacks,
  because an empty filter is a proof of absence. The floor is above the reorg
  depth, since a reorg replays the bodies it disconnects.
- **Being told about a payment.** `-webhook URL` POSTs every block and
  transaction to a service that would rather be called than hold a connection
  open. Delivery never blocks the node, retries a 5xx with a growing delay,
  never retries a 4xx, and carries the network and height so a receiver can spot
  a gap; `GET /webhooks` reports sent/failed/dropped, because a silently failing
  webhook is otherwise invisible.
- **A bounded API.** Every HTTP request passes a per-client token bucket
  (`-apirate`/`-apiburst`, **429** with `Retry-After`). The peer protocol has had
  one per peer from early on while the API — the cheaper target, with no
  handshake and expensive plain GETs — had nothing.
- **An operator's console.** A node started in a terminal (or with `-console`)
  drops into a prompt covering the same ground the HTTP API does, read straight
  out of the running node: the only way to look at one whose API is unreachable,
  which is when it is most wanted.
- **Structured logging.** `-loglevel error|warn|info|debug` sets the volume and
  `-logjson` emits one JSON object per line, so a node's output can be counted
  rather than only read. `-printconfig` prints the effective configuration —
  flags merged over `-config`, with the defaults resolved — and exits.
- **Chain-store tooling.** `dnas db info|verify|export|import` summarizes a chain
  file, replays it through full validation and reports the first block that fails,
  and moves a chain in or out as a portable JSON file — all without a running
  node. `info` and `verify` are read-only, so they are safe against a live node;
  `export`/`import` write, so stop it first.
- **Reading a transaction before agreeing to it.** Half of this tooling passes
  transactions between parties as files. `dnas tx inspect` reports what the JSON
  does not: whether the signatures hold, the fee *rate* against the relay floor,
  which of the silent rejections applies (an expired window, a spent nonce, a
  fee below the floor), and which multisig members have signed. `dnas tx verify`
  exits non-zero when a transaction would be refused.
- **Keys you can look after.** `dnas wallet passphrase` re-encrypts a key file
  under a new passphrase (or removes it), reopening the file to compare the
  address before reporting success — it is rewriting the only copy of a key.
  `dnas backup` bundles what a re-sync cannot replace — keys, the node identity,
  watch lists, deliberately *not* the chain — into one encrypted file, finding
  key files by content so a wallet under another name is not silently omitted.
- **Deterministic genesis.** Every node computes the same genesis block, so
  independent nodes can actually agree on one chain.
- **Most-work consensus with a deterministic tie-break, MTP timestamps, and
  reorgs.** Proof of work is a **256-bit compact target** (Bitcoin-style nBits),
  retargeted every block by an **LWMA** toward the target block time with **no
  hard difficulty cap** — so difficulty rises without bound to match real
  hashpower (a devnet/regtest holds it fixed via `NoRetarget` for instant blocks).
  Peers adopt the chain with the greatest cumulative work
  (Σ 2^256/(target+1)); equal-work forks are broken by preferring the smaller tip
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

- **Permissionless, encrypted, identified links.** Every connection begins with a
  symmetric X25519 key exchange and AES-256-GCM encryption. By **default there is
  no shared key — the network is open**: anyone may connect (the defining property
  of a cryptocurrency). Supplying a `-netkey` instead authenticates a *private*
  network (a peer without the key fails an HMAC check). Either way each peer proves
  its **Ed25519 node identity** by signing the session id, so peers are
  cryptographically identified.
- **Eclipse & DoS resistance.** Inbound connections are capped in total and
  per-IP-group (/16), so an attacker can't monopolise a node's peer slots without
  many address ranges; each peer is rate-limited (token bucket) and the expensive
  whole-chain request is throttled per peer.
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
- **Sync that cannot silently stall.** Every ranged block request is tracked
  against the peer it went to: a peer that accepts one and never answers is timed
  out, ban-scored and dropped, and its slot goes to someone else — a peer that
  keeps answering pings while serving nothing can no longer wedge a node's
  catch-up (or leave a miner building on a stale tip). Several ranges are
  downloaded from different peers at once, and blocks that arrive before their
  parent are buffered and connected the moment it lands, instead of costing
  another round trip.
- **Peer discovery.** Nodes gossip the addresses they know (`getpeers`/`peers`),
  so a node seeded with a single peer discovers the rest and dials them, up to
  `-maxpeers`. Self-dials and duplicates are avoided.
- **A mempool slot costs real balance.** Admission requires that a transaction
  could plausibly be mined: per sender, the pool holds a contiguous run of nonces
  starting at that sender's confirmed nonce, whose total the sender can actually
  afford. Without that, an address holding *nothing* can sign transactions at
  nonces the chain will never reach — admitted, never mined, never expiring, never
  paying a fee — and fill the pool for free, pricing out every real payment.
  Eviction never breaks a run, and after each block the pool re-checks itself and
  drops whatever became unmineable.
- **Signatures verified once, and metered.** A transaction verified when it
  entered the mempool is not verified again when the block carrying it applies
  (they share a cache keyed on the transaction id), and a block's signatures are
  checked across all cores before it is applied. A block also has a
  *verification* budget, not just a byte budget, so it cannot cost the network
  more to check than it cost its miner to produce.
- **Bounded memory & dynamic relay fee.** The mempool is capped and, under
  pressure, evicts the transaction paying the least *per byte* (fee rate); the
  gossip de-duplication sets, the orphan-block pool and the validation cache are
  bounded FIFOs, so a long-running node's memory does not grow without limit. It also enforces a **dynamic minimum relay fee**
  (`-minrelayfee`, a per-byte rate) that starts at a configured base and rises
  quadratically as the pool fills — cheap to relay on an idle devnet, priced up
  under load. This is *local relay policy*, not the consensus base fee: a block
  that includes an under-floor transaction is still valid.
- **Persistent soft state.** Alongside the append-only chain store, a node
  persists its known peers, ban scores, and pending mempool beside the db file
  and restores them on the next start, so a graceful restart resumes warm (bans
  no longer reset). The chain itself remains authoritative and re-syncs from
  peers regardless.
- **Optional API auth.** Setting `DNAS_API_TOKEN` locks every mutating endpoint
  (`/send`, `/tx`, `/mine`, `/generate`, `/submitblock`) behind an
  `Authorization: Bearer <token>` header (constant-time compared); read endpoints
  stay open. Unset, the API is fully open — the localhost/toy default. All
  bundled clients send the token when the env var is set.

## Layout

DNAS is a **Go multi-module workspace**: each component is its own module
(`github.com/nexusriot/DNAS/<name>`), tied together by `go.work`. The repo root
is not itself a module.

```
VERSION             the release number (scripts/version.sh stamps builds from it)
CHANGELOG.md        what each release contains
go.work             workspace tying the modules together
wallet/             module .../wallet — Ed25519 keys, addresses, signing
core/               module .../core   — transactions, blocks, chain state, work, mempool   (→ wallet)
node/               module .../node   — encrypted P2P, discovery, gossip, sync, miner       (→ core, wallet)
api/                module .../api    — HTTP API                                            (→ core, node)
cmd/                module .../cmd    — CLI/daemon; main package in cmd/dnas                 (→ all)
e2e/                module .../e2e    — black-box suite driving the built binary (build tag `e2e`)
tui/                terminal client — its OWN module, outside go.work (external deps)
gui/                desktop client — Python / PyQt6
scripts/            demos: demo.sh (three-node network), htlc-demo.sh, swap-demo.sh
                    packaging: build.sh (cross-compiled tarballs), build-deb.sh
                    version.sh (the version a build stamps)
```

Dependency direction: `wallet → core → node → api → cmd` (no cycles). Each
directory above has its own `README.md`. The two clients (`tui/`, `gui/`) import
no DNAS package — they speak only the HTTP API, and when asked to sign locally
they run the `dnas` binary rather than re-implementing the transaction encoding.

## Build & test

The quickest path is the **Makefile**:

```sh
make build          # compile dnas + dnas-tui into bin/ (version stamped from git)
make test           # go tests (all modules + tui) + the GUI tests
make test-race      # the same under the race detector
make e2e            # black-box end-to-end suite: drives the real binary (see e2e/)
make e2e-docker     # the same suite, hermetically, in a container (needs only Docker)
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
PLATFORMS="linux/amd64 linux/arm64" ./scripts/build.sh
ARCHES="amd64 arm64"                ./scripts/build-deb.sh

# pin the stamped version instead of deriving it
VERSION=1.2.3 ./scripts/build.sh
```

**Versioning.** The release number is in the [VERSION](VERSION) file and
`scripts/version.sh` turns it into what a build stamps — the bare number on a
clean tree at the release commit, otherwise with the commit and dirty state
appended (`0.3.0+g1a2b3c4.dirty`). Keeping it in the tree rather than in a git
tag means the version survives where git does not: the e2e container excludes
`.git`, and so does a source tarball. `make version` prints it, `dnas version`
reports what a binary was stamped with, and [CHANGELOG.md](CHANGELOG.md) says
what each release contains.

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
graceful restart resumes warm. The node's **identity** key lands there too
(`nodekey.json`) — it holds no coin, but it is a private key, so like
`wallet.json` it is in `.gitignore`. [QUICKSTART §10](QUICKSTART.md) inventories
every file a node and wallet write and says which of them can be deleted for
free.

| Flag         | Default        | Meaning                                        |
|--------------|----------------|------------------------------------------------|
| `-listen`    | `:3000`        | p2p listen address                             |
| `-advertise` | = `-listen`    | address peers should dial us at                |
| `-api`       | `:8080`        | HTTP API address                               |
| `-peers`     | —              | comma-separated seed peer addresses            |
| `-wallet`    | `wallet.json`  | wallet key file (created if missing)           |
| `-db`        | `chain.db`     | append-only blockchain store file              |
| `-netkey`    | — (open)       | pre-shared key for a **private** net; empty = open/permissionless |
| `-maxpeers`  | `8`            | maximum outbound peer connections              |
| `-mempool`   | `5000`         | max pending transactions                       |
| `-prune`     | off            | keep only this many recent block bodies in memory (raised to a floor of 132, above the reorg depth) |
| `-webhook`   | —              | comma-separated URLs to POST every block/tx event to |
| `-apirate`   | `20`           | sustained HTTP API requests per second per client (0 disables) |
| `-apiburst`  | `60`           | API requests allowed back to back per client   |
| `-console`   | off            | run the interactive console even when stdin is not a terminal |
| `-minrelayfee` | `10`         | base min relay fee (base units **per byte**); rises with mempool load; 0 disables |
| `-mine`      | off            | enable mining                                  |
| `-regtest`   | off            | regtest mode: mine on demand via `POST /generate` (shorthand for `-network regtest`) |
| `-network`   | `mainnet`      | which network to run on: `mainnet`, `testnet` or `regtest` — separate genesis, signatures and peers |
| `-addrindex` | off            | index address → transactions, serving `/address/{addr}/history` |
| `-faucet`    | off            | hand out coin via `POST /faucet` (testnet/regtest only)        |
| `-faucetamount` | `10`        | faucet payout in DNAS                                          |
| `-faucetcooldown` | `60`      | seconds between faucet payouts to one address or requester     |
| `-sharefactor` | `256`        | how many times easier a mining share is than a block           |
| `-nodekey`   | `nodekey.json` beside `-db` | network identity key file — **not** the wallet |
| `-loglevel`  | `info`         | `error`, `warn`, `info` or `debug`                             |
| `-logjson`   | off            | one JSON object per log line                                   |
| `-printconfig` | —            | print the effective configuration (flags over `-config`) and exit |
| `-dandelion` | on             | relay new transactions via Dandelion++ stem/fluff (origin privacy) |
| `-checkpoints` | —            | finality checkpoints, comma-separated `height:hash` pairs |
| `-upgrades`  | —              | consensus upgrade activations, comma-separated `name:height` pairs (known: `dustlimit`, `multioutput`, `vault`, `feesponsor`) |
| `-config`    | —              | JSON config file (flags override its values)   |

A node shuts down cleanly on `SIGINT`/`SIGTERM` (stops mining, closes peers,
flushes the store). When stdin is a terminal — or with `-console`, which is what
makes it scriptable — you get a console covering the same ground the HTTP API
does, read straight out of the running node:

```
dnas> send <address> <amount> [fee=A] [expiry=N|+N] [lock=N|+N] [memo=TEXT]
dnas> balance [address]        coin, assets and nonce
dnas> address | info | peers   this node, its chain, its connections
dnas> mempool [list]           the pending queue, with contents
dnas> tx <hash>                one transaction, confirmed or pending
dnas> assets [id]              issued assets, and who holds one
dnas> supply | stats | health  minted/burned, hashrate, readiness
dnas> reorgs | prune | webhooks   what this node has lived through and can serve
dnas> mine [on|off] | generate [n]
dnas> help | quit
```

A second node on the same machine just needs distinct ports and a peer:

```sh
go run ./cmd/dnas node -listen :3001 -api :8081 -peers localhost:3000 -mine \
  -wallet w2.json -db chain2.json
```

## HTTP API

| Method | Path              | Purpose                                                       |
|--------|-------------------|---------------------------------------------------------------|
| GET    | `/info`           | network, height, tip, next difficulty, work, mempool, min relay fee, base fee, peers, mining, address-index/faucet/webhook availability, and what this node can serve (`body_height`, `filter_base`, `pruned`) |
| GET    | `/chain`          | block bodies, paged: `?from=HEIGHT&limit=N`, or `?last=N` for the newest N |
| GET    | `/balance/{addr}` | balance (raw + formatted)                                     |
| GET    | `/account/{addr}` | balance (raw + formatted), nonce, and any native-asset balances |
| GET    | `/assets`         | every asset the chain has issued (`?ticker=X` to filter)      |
| GET    | `/asset/{id}`     | one asset: ticker, issuer, supply, and who holds it            |
| GET    | `/mempool`        | pending transactions                                          |
| GET    | `/mempool/stats`  | the pending queue's fee-rate distribution (min / median / max + buckets) |
| GET    | `/tx/{txhash}`    | one transaction, confirmed (block + confirmations) or pending |
| GET    | `/supply`         | coin supply: minted, burned, circulating, conservation check  |
| GET    | `/peers`          | connected peers in detail (identity, version, caps, direction, uptime, ban score) |
| GET    | `/bans`           | scored and banned keys, with the threshold                    |
| POST   | `/unban` 🔒       | `{"key":"…"}` — clear a ban score                             |
| POST   | `/addpeer` 🔒     | `{"addr":"host:port"}` — dial a peer now                      |
| POST   | `/droppeer` 🔒    | `{"peer":"addr-or-identity"}` — close a connection            |
| GET    | `/chainstats`     | `?window=N` → hashrate, block intervals, fee flow, miners     |
| GET    | `/reorgs`         | chain switches this node has lived through                    |
| GET    | `/health`         | readiness: 200 when usable, 503 + reasons when not            |
| GET    | `/address`        | this node's wallet address                                    |
| GET    | `/address/{addr}/history` | transactions touching an address, oldest first (`?from=&limit=`; needs `-addrindex`) |
| GET    | `/estimatefee`    | `?blocks=N` → recommended fee **rate** per byte (base fee + estimated tip) |
| GET    | `/blocktemplate`  | `?address=ADDR` → candidate block + share target; `&longpoll=1&prev=HASH[&timeout=S]` waits for the tip to move |
| POST   | `/submitblock` 🔒 | submit an externally-mined block                          |
| POST   | `/submitshare` 🔒 | submit a hash meeting the easier **share** target (pool accounting) |
| GET    | `/shares`         | the share ledger: submitted / accepted / stale, and per-miner totals |
| GET    | `/headers`        | block headers, paged: `?from=HEIGHT&limit=N` or `?last=N` (SPV) |
| GET    | `/header/{index}` | one block header (SPV)                                        |
| GET    | `/block/{index}`  | one full block body (light clients fetch only flagged blocks); **410** when this node has pruned it |
| GET    | `/proof/{txhash}` | transaction-inclusion merkle proof (SPV)                      |
| GET    | `/stateproof/{addr}` | proof of an address's balance/nonce vs the header state root |
| GET    | `/snapshot/{height}` | full account state at a height (fast-sync; `/snapshot/latest`) |
| GET    | `/cfilters`       | compact block filters, paged (light-client scan)              |
| GET    | `/cfilter/{index}`| one block's compact filter; **410** when the body is pruned (an empty filter would falsely prove absence) |
| GET    | `/cfheaders`      | the filter-header chain, paged (BIP157-style); **410** below a fast-synced node's snapshot |
| GET    | `/events`         | Server-Sent Events stream of new blocks / reorgs / mempool txs |
| GET    | `/metrics`        | Prometheus-format node metrics: height, difficulty, mempool depth/bytes, peers, fees, plus reorgs, orphans, ban scores, hashrate, block intervals, supply, tip age and webhook delivery (36 series) |
| GET    | `/webhooks`       | webhook delivery counters (sent / failed / dropped / queued)  |
| POST   | `/send` 🔒        | `{"to","amount","fee","expiry"?,"lock_until"?,"memo"?,"nonce"?}` — signed by the node wallet |
| POST   | `/tx` 🔒          | submit a fully-signed transaction (plain / multisig / HTLC / vault / fee-sponsored) |
| POST   | `/mine` 🔒        | `{"on":bool}` — toggle mining at runtime                      |
| POST   | `/generate` 🔒    | `{"n":N}` — regtest only: mine N blocks on demand             |
| POST   | `/faucet` 🔒      | `{"address":"…"}` — testnet/regtest only: pay out of the node's wallet |
| POST   | `/multisig/address` | `{"threshold","pubkeys":[…]}` → M-of-N multisig address (stateless helper) |
| POST   | `/htlc/address`   | `{"hash","recipient","sender","timeout"}` → HTLC address (stateless helper) |
| POST   | `/vault/address`  | `{"hot","cold","unlock"}` → time-delayed vault address (stateless helper) |
| POST   | `/wallet/hd`      | `{"mnemonic"?,"passphrase"?,"count"?}` → BIP39 mnemonic + derived HD addresses (stateless helper) |

🔒 = requires `Authorization: Bearer $DNAS_API_TOKEN` when that env var is set (otherwise open).

Every request is rate-limited per client IP (a token bucket, `-apirate`/
`-apiburst`, generous enough that a person clicking around the explorer never
sees it). A refused request answers **429** with `Retry-After`. The limit covers
reads as well as writes, because the expensive requests here are reads — a paged
`/chain`, a `/snapshot`, a `/stateproof`.

Write bodies are size-bounded before they are parsed, so an enormous POST costs
the sender the upload rather than the node the memory: 64 KiB for the small
control payloads, 400 KB for a transaction and 4 MB for a block. Over the limit
answers **413**. The cap is on the reader rather than on `Content-Length`, so a
request that understates its length is still stopped mid-stream.

`/send` validates the recipient's checksum, auto-selects the next nonce (pass an
explicit `nonce` with a higher `fee` to fee-bump a stuck transaction), and
accepts optional `expiry`/`lock_until` heights and a `memo`.

```sh
curl -s localhost:8080/info
curl -s -X POST localhost:8080/send -d '{"to":"dnas...","amount":300000000,"fee":10000000}'

# follow one transaction from submission to confirmation (same endpoint for both)
curl -s localhost:8080/tx/<txhash>

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

dnas wallet pubkey -o alice.json               # print a wallet's public key (to build multisig/HTLC/vault)
dnas htlc new                                  # mint a preimage + its hash for a swap
dnas htlc address -hash H -recipient R -sender S -timeout T
dnas htlc claim  -wallet alice.json -hash H -sender S  -timeout T -preimage P -to <addr>
dnas htlc refund -wallet bob.json   -hash H -recipient R -timeout T           -to <addr>
dnas htlc claim  ... -asset <id>               # an HTLC can hold a native asset too
dnas htlc swap -hash H -asset-owner A -coin-owner B -asset <id> \
     -asset-amount 500 -coin-amount 10 -coin-timeout 100 -asset-timeout 200
                                               # derive BOTH legs of an asset-for-coin swap + print the steps

# fee sponsorship is a two-party flow: the sender signs, the payer counter-signs
dnas sponsor request -key sender.json -to <addr> -amount 1.5 -payer <payer-addr> -o tx.json
dnas sponsor pay -wallet payer.json -in tx.json -submit

dnas vault address -hot HOT_PUB -cold COLD_PUB -unlock H         # a time-delayed vault
dnas vault spend -wallet cold.json -hot HOT_PUB -unlock H -to <addr>   # cold key: any time
dnas vault spend -wallet hot.json -cold COLD_PUB -unlock H -to <addr>  # hot key: from H on

# spend FROM a multisig account: the spend travels as a file, gaining a signature
# per stop (the file records its network, so the members sign offline)
dnas multisig address -threshold 2 -pubkeys a,b,c
dnas multisig propose -threshold 2 -pubkeys a,b,c -to <addr> -amount all -o spend.json
dnas multisig sign -wallet mine.json -in spend.json     # each member, in turn
dnas multisig inspect -in spend.json                    # who has signed, and what it does
dnas multisig submit -in spend.json

# a 2-of-3 escrow is that with names on the members: buyer + seller normally,
# and the arbiter only to break a tie
dnas escrow new -buyer PK -seller PK -arbiter PK -terms "one bicycle"
dnas escrow release -in escrow.json     # a spend paying the SELLER
dnas escrow refund  -in escrow.json     # a spend paying the BUYER
                                        # then: dnas multisig sign / submit

# timestamp a file on the chain, and prove later that it existed
dnas anchor add    -file report.pdf -key wallet.json
dnas anchor verify -file report.pdf     # re-hashes, then verifies against a PoW chain

# ask to be paid, and verify that you were (SPV, with confirmations)
dnas invoice new   -amount 2.5 -memo "two coffees" -key shop.json
dnas invoice watch -in invoice.json -wait
dnas invoice pay   -in invoice.json -key mine.json
# or pay a pasted URI, with no invoice file in sight
dnas invoice pay   -uri 'dnas:dnas1abc…?amount=2.5&memo=two+coffees' -key mine.json
dnas spv -api localhost:8080 wallet -key mine.json send 'dnas:dnas1abc…?amount=2.5'

# prove you control an address without spending from it (domain-separated, so a
# message signature can never be replayed as a transaction)
dnas wallet sign   -o mine.json -m "I control this address" -out sig.json
dnas wallet verify -in sig.json -address <addr>

# change (or remove) a key file's passphrase; the address must survive, and is checked
DNAS_WALLET_PASSPHRASE=old dnas wallet passphrase -o wallet.json

# back up what a re-sync cannot replace (keys, identity, watch lists — not the chain)
dnas backup save -o bundle.json         # encrypted; DNAS_BACKUP_PASSPHRASE
dnas backup list    -in bundle.json
dnas backup restore -in bundle.json -d ./restored

# read a transaction before agreeing to it: fee rate, height window, who signed
dnas tx inspect -in spend.json          # or -hash <txhash>
dnas tx verify  -in spend.json          # exits non-zero if it would be refused

# what assets exist, and who holds them (an asset id is a hash: unreadable alone)
dnas assets
dnas assets show <asset-id>

# a payment with a memo and a height window (consensus has always allowed both)
dnas spv wallet -key F -memo "rent" -expire-in 20 -lock-for 1 send <addr> 1.5

# the light wallet keeps private labels and can export a key-free watch list
dnas spv wallet label <address> rent
dnas spv wallet note  <txhash> "invoice 41"
dnas spv wallet export watch.json              # addresses + labels, no keys
dnas spv wallet import watch.json              # on another machine: watch-only

# fee-bump or void a stuck payment (replace-by-fee, same nonce)
dnas spv wallet -key F bump   <txhash> [fee]
dnas spv wallet -key F cancel <txhash> [fee]

# look after a running node
dnas peers                              # connections, in detail
dnas peers bans                         # who is scored, and how close to the limit
dnas peers unban <identity-or-ip>
dnas peers add <host:port>              # dial now, no restart
dnas peers drop <addr-or-identity>
dnas stats [-window N]                  # hashrate, block timing, fee flow, miners
dnas reorgs                             # chain switches this node has lived through
dnas health                             # exits non-zero when the node is not ready

# inspect, check and move a chain store without a node
dnas db info   -db chain.db -network regtest
dnas db verify -db chain.db -network regtest
dnas db export -db chain.db -o chain.json
dnas db import -db restored.db -in chain.json

# on a testnet/regtest node started with -faucet
dnas faucet -api localhost:8080 -address <addr>
```

## Clients

Beyond the built-in console and web explorer, two standalone clients drive the
same HTTP API (live status, wallet balance, send, SPV verify, a mining toggle,
plus wallet tools to derive a **multisig** address and generate/restore an **HD**
wallet):

- **Terminal UI** (Go / bubbletea) — [`tui/`](tui/): `cd tui && go build -o dnas-tui . && ./dnas-tui -api localhost:8080` (keys: `[x]` multisig, `[h]` HD wallet, `[w]` watch a transaction to confirmation; it also draws a fee-rate histogram of the mempool)
- **Desktop GUI** (Python / PyQt6) — [`gui/`](gui/): `python3 gui/dnas_gui.py --api localhost:8080` (see the "Wallet tools" panel)

Both can also launch a local node so mining works out of the box.

Either can spend **your** key instead of the node's:

```sh
./dnas-tui -api localhost:8080 -key mine.json -dnas ./dnas
python3 gui/dnas_gui.py --api localhost:8080 --key mine.json --dnas ./dnas
```

Without `-key` a payment is `POST /send`, which asks the *node* to sign with the
*node's* wallet — fine for a private node you own, and against a shared one it
spends somebody else's coin. With `-key` the transaction is signed locally and
the node only ever receives a signed transaction.

Neither client implements the signing itself: it delegates to the `dnas` binary
(`dnas spv wallet -key … send`). The canonical transaction encoding is
consensus-critical, and a second hand-written copy of it in a UI is how a client
comes to produce signatures a node rejects — this project has had that bug once
already, in the GUI's SPV header format.

## Web explorer

Every node serves a self-contained web explorer at its API root — just open the
API address in a browser:

```sh
go run ./cmd/dnas node -api :8080 -mine   # then visit http://localhost:8080/
```

It shows live chain status, recent blocks (click to expand their transactions),
the mempool, the assets the chain has issued, what this node can and cannot
serve (readiness, hashrate, reorgs, whether it prunes), and the node wallet with
a send form. One search box takes whatever identifier you have — a height, a
block hash, a transaction hash or an address — and works out which it is.

It also acts as a **light client**: paste a transaction hash and it fetches the
proof + header and verifies inclusion *in your browser* — folding the merkle
proof and checking the header's proof-of-work with the Web Crypto API, exactly as
an SPV client would.

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

`./scripts/swap-demo.sh` goes one step further and settles a whole **asset-for-coin
atomic swap**: Alice issues a token and wants coin, Bob has coin and wants the
token, `dnas htlc swap` derives both legs, and the trade completes with no escrow
— Alice's claim publishes the preimage on-chain, which is what lets Bob take the
asset.

(Edit the `190xx`/`191xx` ports at the top of each script if they are taken.)

## Known limitations (it's a toy)

- The network is open/permissionless with inbound caps (total + per-IP-group) and
  per-peer rate limiting for eclipse/DoS resistance, but node identities and IPs
  aren't cost-bound, so it isn't fully sybil-resistant (no PoW/stake peer gating,
  no ASN-diversity addrman), and the open handshake is anonymous (no MITM auth).
  Ban scores persist across a *graceful* restart but a hard kill can lose the
  latest state (re-synced from peers).
- Consensus uses a canonical binary encoding (portable across implementations),
  but there is still only one implementation and no cross-client test vectors.
- Locator sync transfers only the divergent suffix for normal forks; genuinely
  deep or losing forks still fall back to a whole-chain exchange.
- Recipient checksums are enforced client-side (in `/send` and the console), not in
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
- Proof-of-work difficulty is unbounded (it tracks hashpower); a devnet/regtest
  pins it to the easy genesis floor (`NoRetarget`) so a laptop mines instantly.
- Consensus rules can change via height-activated upgrades (`core/upgrade.go`,
  `-upgrades name:height`), but there's no on-chain miner signaling (BIP9) —
  activation heights are set at startup, like checkpoints.
- Mempool admission measures a sender against its *confirmed* state, so a
  recipient cannot queue a spend of coin that is still unconfirmed. That is the
  account-model norm, and it is what makes a pool slot cost real balance; package
  relay would lift it.
- The address index (`-addrindex`) lives in memory and is rebuilt at every
  startup, and its size grows with an address's usage rather than with the chain.
- Mining shares are node-local, unauthenticated accounting: nothing is stored on
  chain, the ledger is lost on restart, and nothing stops an operator reporting
  whatever they like. Enough for a toy pool between machines you control.
- Mempool reconciliation is one request per peer once caught up, not a continuous
  set-reconciliation protocol; a transaction broadcast in the gap is still missed
  until someone rebroadcasts.
- Fee sponsorship makes a transaction depend on an account the sender does not
  control: a sponsor that spends its balance elsewhere invalidates the
  sponsorships it has outstanding, which are dropped at the next block.
- The faucet's per-address/per-IP cooldown is a speed bump, not a defence — which
  is why the *network parameters*, not a flag, decide whether one may exist.
- Pruning (`-prune`) drops block bodies from memory, not from the store on disk:
  the append-only file still holds every block, and a restart replays it. What it
  buys is a node whose *resident* size does not grow with the chain; what it
  costs is the ability to serve old bodies, their inclusion proofs and their
  compact filters, which the node reports (`410 Gone`, `body_height`) rather than
  answering "not found".
- Webhook delivery is at-most-once with a bounded queue: a receiver that is down
  long enough loses events rather than the node growing a backlog for it. A
  service that must not miss a payment should reconcile with `/chain` or
  `/address/{addr}/history` rather than trust the stream.
- The API rate limit keys on the client IP and deliberately ignores
  `X-Forwarded-For` (a client-set header would hand out a fresh bucket per
  request). A node behind a real proxy needs the limit at the proxy.
- An invoice is matched by (address, amount, height): an address can be paid more
  than once, so two invoices for the same amount at the same address cannot be
  told apart. Use a fresh key per invoice.
- Message signing is domain-separated from transaction signing, so neither can be
  replayed as the other — but a signature still only proves control of a key at
  the moment it was made, and this project has no revocation.
- The TUI and the GUI sign by shelling out to the `dnas` binary rather than
  implementing the transaction encoding themselves. That keeps one copy of the
  consensus-critical part, at the cost of a process launch per payment and a
  dependency on the binary being present.
- Reorg history is an in-memory ring (64 entries) and is lost on restart; it is
  operator telemetry, not chain state.
- `/chainstats` is a *reporting* view: the hashrate figure is an estimate over a
  window, and proof-of-work variance means a short window says more about luck
  than about hashpower.
- Paging bounds the RESPONSE, not always the work: serving a range of
  filter headers still folds the chain from genesis, because the fold is
  cumulative. A client that keeps its own verified prefix (which `dnas spv` now
  does) avoids both.

Each of these is a deliberate stopping point, not an oversight; the prioritized
plan for closing them is in [ROADMAP.md](ROADMAP.md).
