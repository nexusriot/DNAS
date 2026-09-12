# Changelog

Notable changes to DNAS. The current release number is in
[VERSION](VERSION); `scripts/version.sh` turns it into what a build stamps.

This file starts at 0.3.0. Everything before it is in the git history — the
entries below for earlier commits in the 0.3.0 cycle are summarized from it
rather than written at the time.

It is still a learning project, not money. Do not point it at the internet.

## 0.3.0 — unreleased

### Fixed

- **A failed reorg can no longer corrupt the block store silently.**
  `reorgLocked` truncates the store to the fork point *before* appending the
  winning suffix, so once the truncate lands there is no old chain left on disk
  to roll back to. A write that failed partway used to leave disk holding a
  prefix of a chain the node was not running, and the next `AddBlock` would
  stack the old chain's continuation on top of it — producing a log that would
  fail to replay on some later restart, long after the disk error that caused
  it. The store is now **poisoned** on any such failure
  ([core/store.go](core/store.go)): every subsequent write is refused with an
  error naming the original cause and `dnas db verify`. This bounds the damage
  rather than preventing it; making the sequence atomic is still open work
  ([ROADMAP.md](ROADMAP.md) §1).

- **A refused reorg is no longer silent.** When the finality guards decline a
  reorg that fork choice preferred, the node may have stopped following the
  network's chain — permanently, since the same guard refuses the same switch
  every time. That was returned as a bare error, discarded at the call site and
  counted nowhere, so a diverged node looked perfectly healthy: peers connected,
  fresh tip, not behind by its own reckoning. It is now a typed
  `core.ReorgRefusedError`, counted with a high-water depth and reason, logged at
  WARN with an explicit recovery action, reported by `/reorgs`, exported as
  `dnas_reorgs_refused_total`, and surfaced as a `/health` failure reason.

- **The state root is a trie root, so absence is provable.** The header used to
  commit a Merkle fold over the sorted account set, which proves membership and
  nothing else: a prover who omitted a leaf produced a tree a client could not
  distinguish from the truth, so "holds nothing" and "I am not showing you this"
  looked identical. A light client had no way to reject a forged "you were never
  paid". The header now commits a trie root ([core/state.go](core/state.go)) —
  a key's position is fixed by the key, so landing on an empty slot IS the proof
  — and `/stateproof` answers for an address it has never seen instead of
  returning 404. **Consensus change**: genesis and every block hash after it
  differ.
- **Block time 5s → 60s, and the finality window is now named and guarded.**
  `MaxReorgDepth` (100 blocks) at a 5-second block time tolerated about eight
  minutes of divergence, so any partition longer than a coffee break left both
  halves needing a rollback consensus refuses — the deep-reorg guard turned an
  attack into a permanent split. `FinalityWindow` makes the trade-off explicit
  and a test fails if it drops below 30 minutes. At 60s the window is **1h40m**;
  raising the block time rather than the depth avoids dragging `MinPruneKeep`
  with it, so pruning is unaffected. Fast local chains come from `-regtest` and
  `POST /generate`. **Consensus change.**

### Performance

- **Store compaction no longer freezes the node.** It is O(whole store) and ran
  inside the chain's write lock on the block-application path, so on a pruning
  node every 128th block stalled the miner, every API read and every peer handler
  for a full file rewrite (49ms on a 198 KB store, growing with the file). It is
  now snapshot-under-read-lock, stage-with-no-lock, swap-under-write-lock. Worst
  append after: 20.5ms against a 24.5ms no-pruning baseline — the spike is gone.
- **The block store is a compact binary format**, not JSON. Records stored every
  hash, key and signature as hex text; they now store raw bytes behind one
  encode/decode pair ([core/storecodec.go](core/storecodec.go)). **79% of the
  JSON size and 5.1x faster to decode** on a 200-transaction block, which is what
  startup pays. An older JSON store still loads — the leading byte distinguishes
  them.
- **Timestamp validation uses network-adjusted time.** It read the local clock,
  so one machine's wrong clock was that machine's consensus problem. Peers report
  their clock at handshake and the node applies the bounded median
  ([core/nettime.go](core/nettime.go)); the median resists a lying minority, the
  70-minute bound resists a lying majority, one peer is ignored, and the system
  clock is untouched.
- **Block assembly is ~188× faster on a full mempool.** `Mempool.Select` used to
  rescan the whole pool once per chosen transaction, re-deriving every
  candidate's hash and canonical size on each pass — O(pool × block) sha256
  work, which made building a block cost far more than mining one should. It now
  computes everything static once up front, and exploits the fact that only one
  transaction per sender can ever be ready (readiness needs an exact nonce match,
  and the pool holds at most one transaction per `(sender, nonce)`), so each
  round examines one head per *sender* rather than the whole pool. Measured on a
  2000-transaction pool building a full block: **5.7 s → 30 ms**.

### Consensus

These change what a node accepts. The two header changes below alter every block
hash, so a store written by an earlier build does not replay — which is what the
golden vectors in [core/testdata/consensus_vectors.json](core/testdata/consensus_vectors.json)
are there to make loud rather than silent.

- **Supply conservation is a RULE, not a report.** `minted − burned ==
  circulating` was tracked and printed: a node whose accounting had silently
  inflated the supply reported `consistent: false` and went right on building on
  the block that did it. Applying a block must now change the coin held by
  accounts by exactly its subsidy minus the base fee its transactions burned, and
  change each asset's total only by what that block issued, minted or burned
  ([core/conservation.go](core/conservation.go)).

  Stating it per block rather than chain-wide is what makes it affordable: the
  delta is read from the undo log, so it costs the block's own footprint rather
  than a walk of the ledger, and summing the per-block identity over a chain
  gives back the global one. It is deliberately NOT height-activated, unlike the
  rules in `upgrade.go`: those are tightenings that can refuse a transaction which
  was legal when it was mined, and this one cannot — a chain that fails it was
  already invalid under the rules that produced it.

- **Block headers carry a `Version`, and rule changes can be put to a miner
  vote.** Upgrades activated at a height every operator had to be told by hand,
  and nothing checked that the hashpower actually producing blocks understood the
  new rule. A BIP9 deployment claims one bit of the new field; when a window of
  blocks meets the threshold the rule LOCKS IN and activates one whole window
  later ([core/versionbits.go](core/versionbits.go)). A locked-in deployment has a
  known height, which is exactly what the existing upgrade table consumes, so
  every validation rule still just asks `IsUpgradeActive`. A reorg that unwinds a
  lock-in withdraws the height again, because a rule that stayed on by the
  strength of a discarded branch would have the node validating against a chain
  nobody else is on. Configure with
  `-deployments name:bit:start:timeout:window:threshold`, vote with
  `-signalbits`, watch with `/deployments`. **Consensus change.**

- **Unique coinbase transactions (BIP34)** — the `uniquecoinbase` upgrade. A
  coinbase committed only (recipient, amount), so two blocks paying the same
  miner the same subsidy had the same txid and an inclusion proof for one could
  only ever point at one of the blocks. From the activation height a coinbase
  carries the block's height in its `Nonce`, which no two blocks share. It is the
  first real user of the version-bits mechanism above.

- **Assets can be minted and burned by their issuer** — the `assetops` upgrade. A
  supply was fixed at issuance forever. It is height-activated because a holder of
  a fixed-supply asset knows the issuer cannot dilute them, so turning it on
  changes what they are trusting.

  Authority is checked without a lookup. "Only the issuer may mint" looks like it
  needs the asset registry, but that registry is DERIVED state no header commits,
  so a validation rule reading it would depend on nothing. An asset id is
  `sha256(issuer | ticker | nonce)`, so an operation naming its ticker and issuing
  nonce PROVES the sender is the issuer by reproducing the id
  ([core/asset.go](core/asset.go)). Freeze authority and fees paid in an asset are
  still open — both need state the root does not commit yet.

### Networking

- **Transactions are announced and pulled, not pushed to everyone.** Every
  transaction went to every peer in full, so a node with eight peers sent the same
  body eight times, seven of them to peers that already had it.
  `MsgTxInv`/`MsgGetTx`/`MsgTxs` ([node/txrelay.go](node/txrelay.go)) replace all
  but one of those copies with 64 bytes, gated on the `txinv` capability so an
  older peer loses nothing.

  The detail that makes it correct rather than merely smaller: an announcement is
  NOT recorded in the seen set. "Seen" means "we have it and processed it", and
  marking a transaction on its announcement would mean that a peer which announced
  and never delivered made it permanently unfetchable from anyone else. Requests
  are tracked separately — one in flight per id, with an expiry — so eight peers
  announcing the same id produce one download, and a peer that goes quiet does not
  block the transaction forever. Dandelion++ is untouched: its stem still pushes
  the body, because the bandwidth argument does not apply to one peer and the
  extra round trip would widen the timing signal the stem exists to hide.

- **Compact block relay.** A block was announced as a hash and pulled in full, so
  every peer downloaded every transaction a second time at exactly the moment
  latency costs the network the most wasted work. A compact block
  ([node/compactblock.go](node/compactblock.go)) sends the header, the coinbase and
  8-byte short ids; a peer rebuilds the block from its own mempool and asks only
  for what it lacks.

  Two things make it safe. The short ids are KEYED BY THE BLOCK HASH, so a
  colliding transaction cannot be prepared in advance — the hash does not exist
  until the proof of work is found. And a reconstruction is checked against the
  merkle root the header commits before it is believed, so an honest short-id
  collision costs one round trip rather than a wrong chain.

- **A Stratum-shaped mining pool.** Shares existed with no pool around them: one
  target for every miner however fast, no payout scheme, no persistence, no
  authentication. `dnas node -stratum :3333` ([node/stratum.go](node/stratum.go))
  gives each connection its own extranonce (written into the coinbase memo, so two
  miners on one tip search different spaces rather than racing over identical
  nonces), its own difficulty retuned toward one share every ten seconds, and
  PPLNS payouts weighted by each share's own difficulty
  ([node/pool.go](node/pool.go)) — so what a miner earns tracks the work it did
  and not the shares it happened to submit. `/pool` says what the pool owes on the
  next block it finds.

  It is deliberately NOT Bitcoin-compatible, and says so: Stratum V1's job
  encoding is built around Bitcoin's 80-byte header, so a cgminer pointed at this
  port would hash the wrong bytes. The framing, the method names and the session
  lifecycle are Stratum's; the job carries a DNAS block.

- **The reorg log, share ledger and payout window survive a restart.** All three
  were in-memory rings, so a restart lost exactly the record you want after an
  incident and cost a pool its accounting. The reorg ENTRIES are restored and the
  reorg COUNTERS deliberately are not: `refused: 3` means "since this node
  started", and an operator needs to know whether those happened in this run.

### Changed

- **Block templates are deterministic.** Fee-rate ties in `Mempool.Select` now
  break on the transaction hash instead of on Go's map iteration order, so two
  nodes holding the same mempool build the same template rather than merely
  equally good ones.
- **The version comes from the [VERSION](VERSION) file**, not from `git
  describe`. The old scheme could not read a version from the source alone: the
  e2e container has no `.git` (it is excluded by `.dockerignore`) and a source
  tarball has none either, so both fell back to a hard-coded `0.1.0` or reported
  `dev`. `scripts/version.sh` now reads the file and adds the git detail only
  when git is there to supply it (`0.3.0`, `0.3.0+g1a2b3c4`,
  `0.3.0+g1a2b3c4.dirty`). The e2e image stamps its binary too.

### Added

- **Unidirectional payment channels** — `dnas channel`
  ([cmd/dnas/channel.go](cmd/dnas/channel.go)). A stream of payments settles with
  ONE transaction on the chain: two parties lock funds in a 2-of-2, pass
  signed-but-unbroadcast settlements between themselves for as long as they like,
  and publish only the last. No consensus change: it is a protocol over a multisig
  address, `LockUntil`, multi-output transfers and the partial-signature envelope,
  all of which already existed.

  Two properties carry it, and both fall out of the account model. Every
  transaction that can spend the channel uses the SAME nonce, and an account's
  nonce advances exactly once, so of all the alternatives at most one can ever
  confirm — an old settlement is not a second payment, it is a dead one. And
  because the balance only moves one way, the newest settlement is also the one
  paying the receiver most, and only the receiver can complete it; no revocation
  or penalty machinery is needed, which is the reason this stops at
  unidirectional.

  The one rule that cannot be relaxed is an ordering one, so the tool enforces it:
  `channel fund` refuses to broadcast until the funder holds a refund the RECEIVER
  has countersigned. Without it, a receiver who vanishes keeps the capacity
  forever.

- **`dnas doctor`** — one command that runs the operational checks an operator
  would otherwise have to know to run by hand
  ([cmd/dnas/doctor.go](cmd/dnas/doctor.go)). Every number it reads was already
  exposed by `/info`, `/health`, `/metrics` and `/reorgs`; what those do not do is
  say which numbers MATTER or what a bad one means. That knowledge had been
  accumulating in this project's prose, and prose is not something you can run
  before going to bed. Each finding is a thing that goes wrong, how it is visible,
  and what to do about it — refused reorgs first, then peer count, outbound
  address-group concentration, clock skew against peers, orphan backlog, and an
  untokened write API on a non-loopback address. Exits non-zero when something is
  wrong, so it works from cron.

- **An OpenAPI 3.1 document at `/openapi.json`**, generated from the same route
  table that registers the handlers ([api/routes.go](api/routes.go),
  [api/openapi.go](api/openapi.go)). Registration used to be forty-odd
  `mux.HandleFunc` lines with a trailing comment each — readable, and not a
  description a machine can act on, so all four clients in this repo hand-rolled
  their own idea of what the endpoints return and nothing caught the day one of
  them drifted.

  Every response and request type is now named, and the schemas are derived by
  REFLECTION over the Go structs, so a renamed field renames itself in the spec.
  Tests walk the table and fail on an endpoint with no summary, a path parameter
  that is not declared, a `$ref` that does not resolve, a duplicate operationId —
  and, the one that matters most, on any route marked as requiring the token that
  does not actually refuse an unauthenticated request. Publishing a security
  property the server does not have would be worse than publishing none.

- **`dnas invoice serve`** — a watch-only daemon over a directory of invoices
  ([cmd/dnas/invoiceserve.go](cmd/dnas/invoiceserve.go)). `invoice watch` was a
  foreground poll over one file; when the terminal closed, the watching stopped.
  Its delivery guarantee is deliberately the opposite of the node's: the node's
  webhooks are at-most-once behind a bounded queue, which is the right trade for a
  node and the wrong one for money, so this keeps its state on disk and re-POSTs
  every pass until the receiver answers 2xx. A webhook endpoint that was down for
  an hour is told about the payment when it comes back.

- **bech32m addresses, as an interchange encoding**
  ([wallet/bech32.go](wallet/bech32.go)). The canonical spelling's checksum is
  enforced by consensus already, so what was missing was the QUALITY of the error
  detection: a truncated hex checksum says "these bytes are wrong" and nothing
  more. Bech32's is a BCH code over an alphabet that excludes the characters
  people confuse (1/l, 0/O, b/8), it catches up to four wrong characters, and it
  is case-insensitive so an address survives being read aloud. A test asserts that
  every single-character substitution and every transposition is rejected.

  It is NOT a second consensus format, deliberately. The same 20 bytes have two
  spellings and every user-facing entry point normalizes to the canonical one
  before signing; teaching consensus about both would give one owner two state
  keys, so coin sent to `dnas1…` would sit in a different account from coin sent
  to `dnas…`.

- **SLIP-0010 HD derivation** ([wallet/slip10.go](wallet/slip10.go)), checked
  against the standard's own Ed25519 test vectors, on
  `m/44'/9999'/account'/0'/index'`. The old scheme was invented here, so a
  mnemonic written down could only ever be restored here; it stays reachable as
  `-legacy` so existing mnemonics still reach their coin. What standard derivation
  does not buy is an extended public key, and it cannot: BIP32's public derivation
  works because a secp256k1 public key is a point you can add to, and an Ed25519
  public key is a hash of a scalar. Every level is hardened, and a watch-only
  export stays a list of addresses.

- **A monitoring bundle** — a 27-panel Grafana dashboard AND 15 Prometheus alert
  rules ([scripts/monitoring/](scripts/monitoring/)). A dashboard with fifty
  series and no alerting is a dashboard nobody opens. The rules are failure modes
  this project actually found: `DnasReorgRefused` first, because a diverged node
  has peers, a fresh tip and by its own reckoning is not behind, so nothing else
  would notice it. A test scrapes a live node and fails if either file names a
  metric that is not exported — a renamed series turns a panel into a flat line
  and an alert into one that can never fire, and both look exactly like nothing
  being wrong.

- **Charts and a rich list in the web explorer.** Five sparklines over time —
  difficulty, base fee, block interval, transactions per block, fees per block —
  drawn as inline SVG the page renders itself, because it is served by the node
  and must work with no internet. This needed a new endpoint after all:
  `/chainstats` summarizes a window into single numbers, which cannot show a
  quantity MOVING, so `/series` serves the per-height series. `/richlist` ranks
  holders through a bounded min-heap, so one request cannot sort a whole ledger.

- **`dnas tx export`** — an address's whole history as CSV or JSON
  ([cmd/dnas/export.go](cmd/dnas/export.go)), for accounting. It resolves each
  transaction into what it did TO THAT ADDRESS, because the same transaction is a
  debit for its sender and a credit for its recipient, and a fee is a cost rather
  than a payment — charged to the sponsor when there is one.

- **A hardened systemd unit and an RPM** ([scripts/packaging/](scripts/packaging/),
  `make rpm`). The unit sets `ProtectSystem=strict`, an empty capability bounding
  set, a syscall filter and one writable path, and keeps the API token in an
  `EnvironmentFile` rather than on a command line every local user can read out of
  `/proc`.

- **Differential fuzzing** ([core/difffuzz_test.go](core/difffuzz_test.go)). The
  existing targets checked that nothing panics, which finds crashes and is not
  what is most likely to be wrong: a consensus bug is usually a correct-looking
  function computing a subtly different answer, and it will not crash. Six new
  targets check the real implementation against a from-scratch oracle written to
  be obviously correct rather than fast — including a trie built in the REVERSE
  order, so an insertion-order dependence in the state root would show up, and a
  full state rebuild compared against what the undo log produced after a real
  reorg.


- **Consensus can check recipient addresses** — the `checkedaddresses` upgrade
  (`-upgrades checkedaddresses:HEIGHT`). Consensus validated only that an address
  was not absurdly long, so a buggy client could burn coin to a typo; every
  client checked before signing, but that is a convention, not a rule. Once
  scheduled, every address a transaction names — sender, each recipient, the fee
  payer — must be well-formed and checksummed. Height-activated, so an existing
  chain replays unchanged. It cannot recover coin already sent to a malformed
  address, and it does **not** change the address format: moving to bech32 is a
  separate change and is deliberately not bundled in.
- **Pruning now bounds disk, not just memory.** The append-only log is compacted
  to match the pruned chain, amortized and atomic. Because dropping bodies drops
  the transactions that produced the balances, a verified state snapshot is
  written beside the store and `Open` bootstraps from it the way a fast-synced
  node does — a snapshot that does not hash to the header's state root is
  rejected rather than becoming the node's ledger. `dnas db compact -keep N` runs
  it offline; `dnas db verify` reports which heights it could and could not
  re-check; `/info` and `dnas_store_bytes` expose the size. Pruned heights keep a
  header-only record (linkage, MTP and the retarget need them), so the saving is
  the transaction bodies — large on a busy chain, near zero on empty blocks.
- **Golden consensus vectors.**
  [core/testdata/consensus_vectors.json](core/testdata/consensus_vectors.json) is
  a language-neutral corpus of every consensus-visible value: 24 transactions
  across all three networks with their signing preimages, canonical encodings,
  txids, sizes and validity verdicts, plus address and script derivations, asset
  ids, the halving schedule, the compact-target encoding, merkle and state roots,
  header preimages and the fee split. Generated and verified from
  [core/vectors_test.go](core/vectors_test.go); documented for implementers in
  [core/testdata/README.md](core/testdata/README.md). It cannot prove the spec is
  right — a corpus agrees with whatever produced it — but it does mean a
  consensus-visible value can no longer move without a test failing that names
  the field.
- **An outbound address manager, closing the outbound eclipse vector.**
  Inbound connections have had eclipse caps for a while; outbound selection had
  none, so the first `maxpeers` addresses pulled out of a Go map got the slots
  and an attacker who gossiped nine addresses in one /16 stood a good chance of
  owning all of them. [node/addrman.go](node/addrman.go) adds tried/new tables —
  addresses that completed a handshake are preferred over ones merely gossiped —
  bounded and bucketed by network group, and **caps live outbound connections per
  group** (2 of 8 slots), so filling a node's outbound set now needs addresses in
  four distinct ranges. The tried table persists across restarts (`addrs.json`).
  Bucketing is by /16 rather than by ASN, which raises the cost of an eclipse
  rather than settling it; that limit is stated in the ROADMAP and the threat
  model rather than glossed.
- **DNS seed bootstrapping** (`-dnsseeds`, [node/dnsseed.go](node/dnsseed.go)).
  Joining a network previously meant being told somebody's address out of band.
  Seeds are consulted only when the node is short of addresses of its own, their
  results enter the `new` table with no special standing and under the same
  diversity cap, and one dead seed does not block the others.
- **An authenticated state trie** ([core/trie.go](core/trie.go)): a
  content-addressed, path-compressed sparse Merkle trie keyed by the hash of the
  address, with membership **and absence** proofs, a pluggable `NodeStore`, and
  O(depth) updates (~16µs at 10 000 accounts, flat as the ledger grows).
  **Not yet wired into consensus** — see *Known gaps* below.
- **A threat model** ([THREAT-MODEL.md](THREAT-MODEL.md)): assets, adversaries,
  what each defence assumes, what is explicitly out of scope (majority hashpower,
  side channels, supply chain), and where an outside reviewer should attack
  first. It is not an audit and says so.
- **Request bodies on the HTTP API are size-bounded.** Every write endpoint
  decodes through `decodeBody`, which wraps the body in an
  `http.MaxBytesReader` and answers **413** before parsing — 64 KiB for the
  small control payloads, 4× `MaxRelayTxBytes` for a transaction, 4×
  `MaxBlockBytes` for a block. Previously the read side was paged and the P2P
  side capped a frame, but a POST body had no bound at all: the only thing
  between a request and an arbitrarily large allocation was the size check that
  ran *after* the whole body had been decoded into memory. The cap is on the
  reader rather than on `Content-Length`, so a request that understates its
  length is still cut off mid-stream.
- **`/metrics` covers what the node actually knows** — 45 series, up from 12.
  Added reorg totals and depth, orphan count, ban scores and the threshold,
  hashrate and block intervals, supply (minted / burned / circulating), tip age,
  blocks-behind, mempool bytes, and webhook delivery counters. All of these
  existed already but only as JSON spread across `/reorgs`, `/chainstats`,
  `/bans`, `/supply` and `/health`, which is the wrong shape for the one
  consumer that wants them continuously. Five more cover the address manager,
  of which `dnas_outbound_groups` is the one to alert on: a node whose outbound
  peers all sit in one network group is cheap to eclipse however many peers it
  appears to have.
- **`dnas:` payment URIs are read, not just printed.** `dnas invoice new` has
  always emitted `dnas:ADDRESS?amount=…&memo=…&ref=…` and nothing ever parsed
  one back, so the payer still read the address off it and retyped it — which is
  precisely the step the format exists to remove, since consensus does not
  validate recipients and a typo that keeps the length burns the coin.
  `core.BuildPaymentURI` / `core.ParsePaymentURI` ([core/uri.go](core/uri.go))
  are now the canonical pair, and parsing **checksum-validates the address**.
  Every surface that would be handed one reads it: `dnas invoice pay -uri`,
  `dnas spv wallet send <uri>`, the TUI send prompt, the web explorer's send
  form and the PyQt client. Each fills in the amount and memo the URI carries;
  an amount typed alongside one that disagrees is **refused** rather than
  silently overridden, because the payee matches on (address, amount) and paying
  a different amount is the same as not paying.
- **This changelog**, and a regression test tying it to the VERSION file.

### Known gaps

- **The state trie is the state root, but the ledger is still resident.**
  `applyBlock` reads and writes a `map[string]Account`, so memory bounds the
  ledger, and the root is rebuilt from the whole account set per call rather than
  updated incrementally. Both need a state accessor threaded through the
  application path plus a disk-backed node store. ROADMAP §1 records the work.
- **Channels are unidirectional, and assets have no freeze or renounce.** Both
  stop where they do for the same kind of reason: a bidirectional channel needs
  revocation and penalty transactions, and freeze/renounce need per-asset or
  per-holder state the state root does not commit. Half-shipping either would be
  worse than not shipping it.
- **bech32 is interchange, not consensus.** Teaching consensus both spellings
  would give one owner two state keys, so coin sent to `dnas1…` would sit in a
  different account from coin sent to `dnas…`.
- **No independent audit.** The threat model was written by the same hands that
  wrote the code and inherits its blind spots. ROADMAP §7 still lists this open.

### Documentation

- `dnas help` listed 24 of the 31 flags `dnas node` accepts; the missing seven
  (`-prune`, `-webhook`, `-apirate`, `-apiburst`, `-console`, `-faucetamount`,
  `-faucetcooldown`) are now there, along with a mention of the interactive
  console, which the help text never acknowledged existed. A test now reads the
  flag names back out of `runNode`'s own source and fails if the help text and
  the binary disagree in either direction.
- DESIGN.md §17 was missing `DefaultMempoolSize`, `DefaultMempoolBytes`,
  `MinPruneKeep` and the API rate-limit defaults.
- DESIGN.md and QUICKSTART.md gained tables of contents (23 and 37 sections
  respectively, neither previously navigable).
- Two ROADMAP items contradicted each other about whether P2P frames were
  bounded; they now agree on what is actually true (a coarse 64 MiB cap, no
  per-message-type limits).
- Several claims had gone stale against the code and are now corrected: DESIGN.md
  §21 and this file's own "Known gaps" both still said the state trie was "not
  yet wired into consensus" *while the Fixed section above recorded shipping it*;
  ROADMAP §6 listed "Binary block bodies on disk" twice, once struck through as
  done and once still open below it; and the README's limitations still described
  client-side-only address checksums, membership-only state proofs, HMAC-SHA512
  HD derivation, a share ledger lost on restart, and pruning that did not touch
  the store — all of which had since changed.

### Earlier in this cycle

Summarized from git history between the `0.2` tag and this release:

- A large feature round ("Integrity"): fee sponsorship, 2-of-3 escrow, on-chain
  file anchoring, invoices with SPV-verified settlement, spending from a
  multisig account, time-delayed vaults, native-asset tooling, chain-store
  tooling (`dnas db`), the operator commands (`peers`, `stats`, `reorgs`,
  `health`), the interactive console, per-client API rate limiting, and a
  light-client header cache.
- Hash-time-locked contracts and coin-for-asset atomic swaps.
- Finality checkpoints, and consensus upgrades activated at a fixed height.
- A binary, length-framed block store replacing whole-file rewrites.
- A containerized end-to-end suite (`make e2e-docker`) that runs with no
  network, a read-only root and no privileges.
- A node-lifecycle race fix, and a review round that tightened transaction
  sanity rules and multisig verification limits.

## 0.2 and earlier

See the git history. This file did not exist yet.
