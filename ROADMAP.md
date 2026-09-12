# Roadmap

This is the honest backlog of what DNAS does **not** do yet. For what it *does*
do, see [README.md](README.md) and [DESIGN.md](DESIGN.md); the trade-offs behind
each shortcut are catalogued in [DESIGN.md §21 "Known limitations"](DESIGN.md).

Where DNAS stands today: the properties that separate a real coin from a demo are
in place — **unbounded proof-of-work difficulty** (an LWMA-retargeted 256-bit
`nBits` target with no hard cap; `NoRetarget` only for regtest), a **canonical,
implementation-independent consensus encoding** ([core/codec.go](core/codec.go))
with a height-activated **upgrade path** ([core/upgrade.go](core/upgrade.go)),
**separate networks** whose id is bound into the genesis block, the signing
preimage and the peer handshake ([core/network.go](core/network.go)), so a
signature cannot be replayed across chains and nodes on different networks never
try to converge, and a **permissionless, eclipse/DoS-hardened network** (open
handshake, inbound caps, per-peer rate limiting, a node identity that is its own
key rather than the operator's wallet). Since then: **supply conservation is a
consensus rule** rather than a report, rule changes can be put to a **BIP9 miner
vote** instead of a hand-configured flag day, transactions are **announced and
pulled** rather than pushed to every peer, blocks relay **compactly**, and the
chain has a **Stratum-shaped pool** with per-miner difficulty and PPLNS payouts.
What remains is the long tail that turns a correct toy into something you could
defend on a live network.

It is still a learning project. Nothing below should be read as a promise to ship,
and none of it makes DNAS money. **Do not point it at the internet.**

**Effort legend:** `[S]` ≈ hours · `[M]` ≈ a day or two · `[L]` ≈ structural /
multi-day. Items are roughly priority-ordered within each section; the earlier
sections matter most for "toy → real".

---

## 1. Production-grade consensus & state

- **`[M]` Read state THROUGH the trie.** *The trie is now the state root; the
  memory bound is what remains.* The header commits a trie root
  ([core/trie.go](core/trie.go), [core/state.go](core/state.go)) instead of a
  Merkle fold over the sorted accounts, which is what makes **absence provable**:
  a key's position is fixed by the key, so arriving at an empty slot is itself
  the proof that nothing is there. `/stateproof` therefore answers for an
  address it has never seen — it used to 404 — and `dnas spv balance` prints
  "holds NOTHING, proven absent". That closes the light-client hole where a
  prover could omit a leaf and be indistinguishable from the truth.

  Two of the three original benefits are still open, and both need the same
  change: `applyBlock` reads and writes a resident `map[string]Account`, so
  **memory still bounds the ledger**, and the root is rebuilt from the whole
  account set per call rather than updated incrementally. Threading a state
  accessor through the application path (~10 functions, ~44 sites) plus a
  disk-backed `NodeStore` gets both. It is a big, careful refactor of the
  consensus core and is deliberately not bundled into the fork that landed the
  root change.
- ~~**`[M]` Prune the STORE, not just memory.**~~ **Done.** `-prune` bounded a
  node's resident size while the file kept every original record, so a pruning
  node still paid full disk for history it had discarded and still replayed all
  of it on restart. Compaction now rewrites the log to match the pruned chain
  ([core/store.go](core/store.go)), amortized over `storeCompactInterval` blocks
  and atomic (temp file + rename).

  The half that makes it correct rather than merely smaller: dropping bodies
  drops the transactions that produced the balances, so a header-only store
  cannot rebuild state. A verified **state snapshot** is written beside it
  (`chain.db.state`) and `Open` bootstraps from it exactly as a fast-synced node
  does — the snapshot's accounts must hash to the state root in a
  proof-of-work-covered header, so a corrupt or tampered one is rejected rather
  than becoming the node's ledger. `dnas db compact -keep N` does it offline,
  `dnas db verify` reports honestly which heights it could and could not
  re-check, and `/info`, `/metrics` (`dnas_store_bytes`) expose the size.

  Pruned heights keep a header-only record rather than disappearing: linkage,
  median-time-past and the retarget all read those headers. So the saving is the
  size of the transaction bodies — large on a busy chain, near zero on a devnet
  mining empty blocks.
- **`[M]` Fetch the filter-header chain during fast sync.** A fast-synced node
  never saw the bodies below its snapshot, so it cannot fold their filter
  commitments and reports `filter_base` above them (410 for anything lower).
  Fetching the chain from a peer during fast-sync — and checking it against a
  checkpoint — would close the one gap where such a node cannot serve a light
  client at all.
- **`[L]` A second implementation.** *The golden-vector half is done.*
  [core/testdata/consensus_vectors.json](core/testdata/consensus_vectors.json) is a
  language-neutral corpus — 24 transactions across all three networks with their
  signing preimages, canonical encodings, txids, sizes and validity verdicts, plus
  address and script derivations, the halving schedule, the compact-target
  encoding, merkle and state roots, header preimages and the fee split. It is
  generated and verified from [core/vectors_test.go](core/vectors_test.go)
  (`-update` to regenerate) and documented for implementers in
  [core/testdata/README.md](core/testdata/README.md). What it cannot do is prove
  the spec is *right*, only that it has not silently moved: a corpus agrees with
  whatever produced it. A second client remains the real item, and this is now
  the first thing it should be run against.
- **`[M]` Make a failed reorg persist atomic — *the divergence is now contained,
  not yet prevented*.** `reorgLocked` truncates the block store and appends the
  winning suffix *before* swapping the chain in memory
  ([core/blockchain.go](core/blockchain.go)). Once the truncate lands there is no
  old chain left on disk to roll back to, so a failure partway used to leave the
  store holding a prefix of a chain the node was not running — and the next
  `AddBlock` would stack the old chain's continuation on top of it, leaving a log
  that would not replay on restart. The store is now **poisoned** on any such
  failure ([core/store.go](core/store.go)): every later write is refused with an
  error naming `dnas db verify`, so the damage stops at one bad reorg instead of
  compounding silently. What remains is the *atomic* version — writing the suffix
  to a side region and switching in one step — so a disk error is recoverable
  rather than merely loud.
- ~~**`[M]` Consensus-checked addresses.**~~ **Done, as a scheduled upgrade.**
  Consensus validated only that `To` was not absurdly long, so a buggy client
  could burn coin to a typo and `MaxAddressBytes` merely bounded how much junk
  became a permanent state key. The `checkedaddresses` upgrade
  ([core/upgrade.go](core/upgrade.go)) requires every address a transaction names
  — sender, each recipient, the fee payer — to be well-formed and checksummed.
  Height-activated like the others, so an existing chain replays unchanged; it
  cannot recover coin already sent to a malformed address.

  Note what this is *not*: the address FORMAT is unchanged. The security hole was
  that the existing checksum was never enforced, and that is now closed. Moving
  to bech32 (§5) is a separate, cosmetic-plus-error-detection change that would
  invalidate every address string in the project, and is deliberately not bundled
  into this.
- ~~**`[M]` BIP9-style miner signaling for upgrades.**~~ **Done.** `core/upgrade.go`
  flipped rules at fixed activation heights, like checkpoints: every node had to
  be told the same number by hand, and nothing checked that the hashpower actually
  producing blocks was running code that understood the new rule. Activate too
  early and the miners fork; too late and the change waits on the slowest
  operator. The block header now carries a `Version`
  ([core/block.go](core/block.go)) and a deployment claims one of its bits
  ([core/versionbits.go](core/versionbits.go)): when a window of blocks meets the
  threshold the rule LOCKS IN, and activates one whole window later — so every
  node learns the activation height from the chain, with warning. A locked-in
  deployment has a known height, which is exactly what upgrade.go already
  consumes, so every validation rule still just asks `IsUpgradeActive` and needs
  no notion of signaling. A reorg that unwinds a lock-in withdraws the height
  again. Configured with `-deployments name:bit:start:timeout:window:threshold`
  and voted with `-signalbits`; `/deployments` reports where each stands.
  **Consensus change** (the header gained a field, so every block hash differs).
- ~~**`[S]` Enforce supply conservation in consensus.**~~ **Done.** The identity
  was tracked and *reported*: a node whose accounting had silently inflated the
  supply printed `consistent: false` and went on building on the block that did
  it. It is now a rule, checked per block during application
  ([core/conservation.go](core/conservation.go)): a block must change the coin
  held by accounts by exactly `subsidy − burned`, and change each asset's total
  only by what it issued, minted or burned. Stating it per block rather than
  chain-wide is what makes it affordable — the delta is read from the undo log,
  so it costs the block's own footprint rather than a walk of the ledger — and
  summing the per-block identity over a chain gives back the global one. It is
  deliberately NOT height-activated: a chain that fails it was already invalid
  under the rules that produced it, so there is no valid history to protect.
- ~~**`[M]` Unique coinbase transactions (BIP34).**~~ **Done, as a votable
  upgrade.** A coinbase committed only (recipient, amount), so two blocks paying
  the same miner the same subsidy had the same txid, and an inclusion proof for
  one could only point at one of the blocks. `UpgradeUniqueCoinbase` requires the
  coinbase's `Nonce` to equal the block height, which no two blocks in a chain
  share. It is the first real user of the version-bits mechanism above — the
  project had shipped three height-activated flag days already, and this would
  have been a fourth.
- ~~**`[?]` Pick a survivable finality window.**~~ **Done: block time 5s → 60s.**
  `MaxReorgDepth` was a bare 100 blocks and `TargetBlockTime` was 5 seconds, so
  the chain tolerated about **eight minutes** of divergence: any partition longer
  than a coffee break left both halves needing a rollback consensus refuses, and
  they never reconverged. The deep-reorg guard had turned an attack into a
  permanent split.
  
  The window is now named (`FinalityWindow`, [core/params.go](core/params.go)) and
  guarded by a test that fails if it drops below 30 minutes. Raising the block
  time was the cheap lever: unlike raising `MaxReorgDepth` it does not drag
  `MinPruneKeep` up with it, so pruning nodes are unaffected. At 60s the window
  is **1h40m** and coinbase maturity becomes 3 minutes. A fast local chain now
  comes from `-regtest` and `POST /generate`, which is what they are for.

  Still open, and deliberately separate: **coinbase maturity is 3 blocks.** At 60s
  that is 3 minutes, which passes the "not meaningless" bar but is far below
  Bitcoin's 100 blocks. Raising it slows every test and demo that mines then
  spends, so it wants its own decision rather than being folded in here.

## 2. Networking hardening & reach

- **`[M]` Binary P2P wire format.** Consensus is binary, but the peer envelope
  ([node/protocol.go](node/protocol.go)) is still JSON. A binary framing on the hot
  path cuts bandwidth and parse CPU, and removes the last consensus-adjacent use of
  a text codec.
- ~~**`[M]` Compact block relay (BIP152).**~~ **Done.** A block was announced as a
  hash and pulled in full, so every peer downloaded every transaction twice —
  once into the mempool, once inside the block — at exactly the moment latency
  costs the most work. A compact block ([node/compactblock.go](node/compactblock.go))
  sends the header, the coinbase and 8-byte short ids; a peer resolves them
  against its own pool and asks only for what it is missing. Two details make it
  safe rather than merely clever: the short ids are KEYED BY THE BLOCK HASH, so a
  colliding transaction cannot be prepared before the proof of work is found; and
  a reconstruction is verified against the committed merkle root before it is
  believed, so an honest collision costs a round trip rather than a wrong chain.
  Gated on the `cmpct` capability, so a peer that does not speak it still gets
  the hash it always got.
- ~~**`[M/L]` Outbound address manager + DNS seeds.**~~ **Done, with one caveat.**
  [node/addrman.go](node/addrman.go) is a tried/new address manager: addresses
  that completed a handshake are preferred over ones merely gossiped, tables are
  bucketed and bounded per network group, and — the part that actually matters —
  **live outbound connections are capped per group** (`maxOutboundPerGroup`, 2 of
  8 slots). Filling a node's outbound set now needs addresses in four distinct
  ranges rather than eight addresses anywhere. The tried table persists across
  restarts (`addrs.json`), so a node does not re-trust gossip on every start, and
  `/info` plus five `dnas_addr*`/`dnas_outbound*` metrics make the current
  diversity observable. `-dnsseeds` bootstraps from A/AAAA records when the node
  is short of peers ([node/dnsseed.go](node/dnsseed.go)); seed results get no
  special standing and are subject to the same cap.

  The caveat, restated because it is the honest limit: bucketing is by **/16, not
  by ASN**. Two ranges can share an operator, so this raises the cost of an
  eclipse rather than settling it. Real ASN diversity needs routing data this
  project has no business shipping.
- **`[M]` Authenticated / Tor-friendly transport.** The open handshake is anonymous
  and has no MITM authentication (§21). Optional peer-key pinning, an onion
  transport, and NAT traversal would harden and widen reach without giving up the
  permissionless default. Peer-key pinning now has something to pin: a node's
  identity is a stable key of its own ([node/identity.go](node/identity.go))
  rather than its wallet key.
- ~~**`[M]` Network-adjusted time.**~~ **Done.** Timestamp validation read the
  local clock, which made one machine's wrong clock that machine's consensus
  problem: an hour behind and it rejects every block the network produces; an
  hour ahead and it mines on a tip its peers refuse. Peers now report their clock
  at handshake, and the node applies the **median** of those offsets, **bounded**
  to `MaxTimeOffset` (70 min), for validation only ([core/nettime.go](core/nettime.go),
  [node/nettime.go](node/nettime.go)). The median resists a lying minority, the
  bound resists a lying majority, a single peer is ignored outright, and the
  system clock is never touched. A standing offset over a minute is logged at
  WARN with the fix, because the adjustment keeps the node on the chain while the
  machine still needs correcting.
- **`[M]` Windowed, scored block download.** Ranged requests are now tracked,
  timed out and spread across a few peers, with out-of-order arrivals buffered
  ([node/sync.go](node/sync.go)) — but the window is fixed, peers are not scored on
  throughput, and a range that times out is not re-requested from a *specific*
  better peer. A proper scheduler (per-peer speed, adaptive window, re-assignment)
  would make catch-up on a long chain predictable rather than merely unstuck.
- **`[S]` Bound P2P and HTTP request sizes.** A peer frame may be up to 64 MiB
  (`maxFrame`, [node/secure.go](node/secure.go)) and the API decodes request bodies
  with no cap, so one message can force a large allocation before anything
  validates it. Per-message-type limits plus an `http.MaxBytesReader` on the write
  endpoints would price that properly. The `MsgGetChain` fallback is the same shape
  of problem: it serializes the whole chain (throttled per peer, but unbounded in
  size) where a ranged request would do. The HTTP *read* side is now bounded —
  `/chain`, `/headers`, `/cfilters` and `/cfheaders` are paged — so what remains
  is inbound bodies and the P2P frames.

## 3. Programmability

- **`[L]` Authorization script VM.** Multisig, HTLC and now the time-delayed
  vault ([§5.2 in DESIGN.md](DESIGN.md)) are hand-rolled special cases in
  consensus — the vault is the cheap version of exactly what a VM would express
  generically, and it is the third of them, which is the argument for the VM
  rather than a fourth. A small, deterministic, gas/opcount-metered predicate
  language would unify them behind one verifier and unlock covenants, richer
  vault policies, and arbitrary spend conditions — the single highest-leverage
  expressiveness change.
- **`[M]` Asset freeze authority + optional per-asset fees.** *Mint and burn are
  done; these two are not.* An asset's supply was fixed at issuance forever. An
  issuer can now mint more of it or burn units it holds, under
  `UpgradeAssetOps` ([core/asset.go](core/asset.go)) — height-activated because a
  holder of a fixed-supply asset knows the issuer cannot dilute them, so turning
  it on changes what they are trusting.

  The part worth stealing is how AUTHORITY is checked. "Only the issuer may mint"
  seems to need a registry lookup, but the asset registry is DERIVED state that no
  header commits, so a validation rule reading it would be a consensus rule
  depending on nothing. An asset id is `sha256(issuer | ticker | nonce)`, so an
  operation that names its ticker and issuing nonce PROVES the sender is the
  issuer by reproducing the id — one hash, computed from data the transaction
  itself carries.

  What is still open: FREEZE, which needs per-holder state committed in the state
  root and is the least defensible of the three anyway, and paying fees in an
  asset, which is a much deeper change to fee accounting and conservation.
- **`[L]` Confidential amounts / stealth addresses.** Amounts and parties are fully
  public; Dandelion++ only hides a tx's *origin* at relay time. Pedersen-commitment
  confidential amounts plus one-time stealth addresses would add real on-chain
  privacy.

## 4. Mempool & fee policy

- **`[M]` Package relay + CPFP + ancestor/descendant limits.** RBF, a per-byte
  relay floor, rate-based eviction, and per-sender nonce-contiguity/affordability
  admission all exist. What is missing is *dependency* tracking: because admission
  measures a sender against its confirmed state, a recipient cannot queue a spend
  of coin that is still unconfirmed (DESIGN §21), and a stuck low-fee parent cannot
  be bumped by its child. Ancestor/descendant accounting would restore both and
  close pinning attacks.
- ~~**`[M]` Announce transactions by hash (inv/getdata for `MsgTx`).**~~ **Done.**
  Every transaction was pushed in full to every peer, so on a well-connected node
  the same body went out eight times, seven of them to peers that already had it.
  `MsgTxInv`/`MsgGetTx`/`MsgTxs` ([node/txrelay.go](node/txrelay.go)) replace all
  but one of those copies with 64 bytes. The detail that makes it correct rather
  than merely smaller: an announcement is NOT recorded in the seen set — marking
  it there would mean that a peer which announced and never delivered made the
  transaction permanently unfetchable from anyone else. Requests are tracked
  separately, one in flight per id with an expiry, so eight peers announcing the
  same id produce one download and a peer that goes quiet does not block it
  forever. Dandelion++ is untouched: its stem still pushes the body, because the
  bandwidth argument does not apply to one peer and the extra round trip would
  widen the timing signal the stem exists to hide. This also subsumes the
  cold-start hole `MsgGetMempool` covered.
- **`[S/M]` Weight-based congestion signal.** The EIP-1559 base fee currently
  responds to transaction *count* (§9); switching the signal to block weight/bytes
  makes it track real demand.
- ~~**`[S]` Index the mempool for selection.**~~ **Done.** `Mempool.Select` used to
  rescan the whole pool once per chosen transaction, recomputing each candidate's
  hash and canonical size every pass — O(txs × block txs) sha256 work, which on a
  full pool made building one block far more expensive than mining it should be.
  It now derives hash/size/ops/rate once up front, and exploits the fact that only
  ONE transaction per sender can be ready (readiness requires an exact nonce match
  and the pool holds at most one transaction per (sender, nonce)), so each round
  examines one head per *sender* rather than the whole pool. Measured on a
  2000-transaction pool building a full block: **5.7 s → 30 ms**. Selection is also
  deterministic now — fee-rate ties break on the transaction hash instead of on
  Go's map iteration order, so two nodes with the same mempool build the same
  template.

## 5. Wallet & UX

- **`[M]` Hardware wallets.** *SLIP-0010 is done; the devices are not.* Derivation
  was a one-level scheme of this project's own invention, so a mnemonic written
  down here could only ever be restored here. It is now SLIP-0010
  ([wallet/slip10.go](wallet/slip10.go)), checked against the standard's own
  Ed25519 test vectors, on the path `m/44'/9999'/account'/0'/index'`. The old
  scheme remains reachable as `DeriveLegacy` (`-legacy`) so a mnemonic from
  before the change still reaches its coin.

  What standard derivation did NOT buy is an extended public key, and it cannot:
  BIP32's public derivation works because a secp256k1 public key is a point you
  can add to, and an Ed25519 public key is a hash of a scalar. Every level is
  therefore hardened, there is no xpub, and a watch-only export stays a LIST of
  addresses. Ledger/Trezor support is now a matter of the device protocol rather
  than of the derivation.
- ~~**`[M]` bech32 addresses**~~ **Done — as an interchange encoding, deliberately
  not a second consensus format.** The consensus-checksum half of this item was
  already closed by `UpgradeCheckedAddresses`, so what remained was the *quality*
  of the error detection: a truncated hex checksum says "these bytes are wrong"
  and nothing more. Bech32m ([wallet/bech32.go](wallet/bech32.go)) is a BCH code
  over an alphabet that excludes the characters people confuse (1/l, 0/O, b/8):
  it catches up to four wrong characters, is case-insensitive so an address
  survives being read aloud, and a test asserts that EVERY single-character
  substitution and transposition is rejected.

  The honest limit, and the reason it stops there: the same 20 bytes have two
  spellings, and every user-facing entry point normalizes to the canonical one
  before signing. Teaching consensus about both would mean two state keys for one
  owner — coin sent to `dnas1…` would sit in a different account from coin sent
  to `dnas…` — which is the kind of split that strands money. The error detection
  is worth having where typos happen; that is not.
- **`[S]` Fresh addresses per invoice.** *Now unblocked: SLIP-0010 landed above.*
  `dnas invoice` matches a payment by
  (address, amount, height), so two invoices for the same amount at the same
  address are indistinguishable — the file says so, which is not the same as
  fixing it. An HD-derived address per invoice would, and needs the standard
  derivation above to be worth exporting.
- ~~**`[M]` A watch-only daemon around `invoice watch`.**~~ **Done.**
  `invoice watch` was a foreground poll over one file; when the terminal closed,
  the watching stopped. `dnas invoice serve`
  ([cmd/dnas/invoiceserve.go](cmd/dnas/invoiceserve.go)) watches a whole directory,
  remembers what it has reported across restarts, and calls a webhook when an
  invoice settles or expires.

  Its delivery guarantee is deliberately the opposite of the node's. A node's
  webhooks are AT-MOST-once behind a bounded queue — the right trade for a node
  and the wrong one for money. This keeps its state on disk and re-POSTs on every
  pass until the receiver answers 2xx, so a webhook endpoint that was down for an
  hour is told about the payment when it comes back. That makes delivery
  at-least-once, which is why every notification carries the invoice reference to
  deduplicate on.
- **`[M]` Bidirectional payment channels.** *Unidirectional ones are done.*
  `dnas channel` ([cmd/dnas/channel.go](cmd/dnas/channel.go)) settles a stream of
  payments with ONE transaction on the chain: two parties lock funds in a 2-of-2,
  pass signed-but-unbroadcast settlements between themselves for as long as they
  like, and publish only the last. It is a protocol over primitives that already
  existed — a multisig address, `LockUntil`, multi-output transfers, and the
  partial-signature envelope — and needs no consensus change at all.

  Two properties carry it, and both fall out of the account model. Every
  transaction that can spend the channel uses the SAME nonce, and an account's
  nonce advances once, so of all the alternatives that exist at most one can ever
  confirm — an old settlement is not a second payment, it is a dead one. And
  because the balance only moves one way, the newest settlement is also the one
  that pays the receiver most, and only the receiver can complete it; a
  bidirectional channel would need revocation and penalty transactions, which is
  most of Lightning's complexity and the reason this stops here.

  The one rule that cannot be relaxed is an ordering one: `channel fund` refuses
  to broadcast until the funder holds a refund the RECEIVER has countersigned,
  because without it a receiver who vanishes keeps the capacity forever.

- **`[M]` PSBT-style partial signatures as a FORMAT.** `dnas multisig` and
  `dnas sponsor` pass a transaction between signers as a JSON envelope carrying
  the network it is for, which works and is not interoperable with anything: a
  documented, versioned partial-signature encoding would be.
- ~~**`[S]` `dnas:` URIs in the clients.**~~ **Done.** The format now round-trips:
  `core.BuildPaymentURI`/`core.ParsePaymentURI` ([core/uri.go](core/uri.go)) are
  the canonical pair (the address is checksum-validated on parse, so a URI that
  survives cannot direct a payment at a typo), and every surface that would be
  handed one reads it — `dnas invoice pay -uri`, `dnas spv wallet send <uri>`, the
  TUI send prompt, the web explorer's send form, and the PyQt client. Each fills
  in the amount and memo the URI carries; an amount typed alongside one that
  disagrees is refused rather than silently overridden. The TUI, explorer and GUI
  keep their own small parsers because they import no DNAS package by design; a
  checksum-guarded shared fixture keeps them honest.

## 6. Ops, tooling & observability

- ~~**`[S]` Surface refused reorgs.**~~ **Done.** A reorg the finality guards
  declined was returned as a bare error, discarded at the call site and counted
  nowhere — so the single most consequential thing a node can do quietly (stop
  following what may be the network's chain, permanently, because the same guard
  refuses the same switch every time) was invisible. It is now a typed
  `core.ReorgRefusedError`, counted with a high-water depth and a reason, logged
  at WARN with an explicit `action`, reported by `/reorgs`, exported as
  `dnas_reorgs_refused_total`, and — the part that matters — made a `/health`
  failure reason. Nothing else in that check would have noticed: a diverged node
  has peers, a fresh tip, and by its own reckoning is not behind.
- ~~**`[M]` Move store compaction off the hot path.**~~ **Done.** Compaction is
  O(whole store), and the first version ran it inside the chain's write lock on
  the block-application path — so on a pruning node every 128th block froze the
  miner, every API read and every peer handler for the length of a full file
  rewrite (measured: 49ms on a 198 KB store, growing with the file). It is now
  three phases ([core/store.go](core/store.go), [core/prune.go](core/prune.go)):
  snapshot under a read lock, stage the rewrite with **no lock held**, then swap
  under the write lock after appending whatever blocks arrived meanwhile. A reorg
  mid-rewrite discards the staged file and leaves the request pending. Measured
  after: worst append 20.5ms against a 24.5ms no-pruning baseline — the spike is
  gone, and what remains is the disk's own fsync cost.
- ~~**`[S]` Binary block bodies on disk.**~~ **Done.** Records were the block's
  JSON, which stores every hash, key and signature as hex TEXT — a 32-byte key
  in 64 bytes, a 64-byte signature in 128. Records are now a compact binary
  encoding ([core/storecodec.go](core/storecodec.go)) behind one
  encode/decode pair, with hex fields kept as raw bytes. A JSON record begins
  with `{` and a binary one with its version tag, so **an older store still
  loads** without guessing. Measured on a 200-transaction block: **79% of the
  JSON size, and decode 5.1x faster** (104µs vs 530µs) — which is what startup
  actually pays, since every record is parsed on open. It is explicitly not the
  consensus codec: these records are local, never hashed and never sent to a
  peer.
- ~~**`[S]` A Grafana dashboard for the Prometheus metrics.**~~ **Done, with the
  half that was missing from the request.** `/metrics` now exports 53 series, and
  a dashboard with fifty series and no alerting is a dashboard nobody opens. So
  [scripts/monitoring/](scripts/monitoring/) ships both: a 27-panel dashboard and
  15 Prometheus alert rules, grouped by what has gone wrong. The rules are the
  failure modes this project actually found rather than imagined —
  `DnasReorgRefused` first, because a diverged node has peers, a fresh tip and by
  its own reckoning is not behind, so nothing else on the dashboard would notice
  it. A test scrapes a live node and fails if either file names a metric that is
  not exported, since a renamed series turns a panel into a flat line and an
  alert into one that can never fire — both of which look exactly like nothing
  being wrong.
- **`[M]` JSON-RPC 2.0 interface.** The HTTP API is REST; a bitcoind-style JSON-RPC
  surface eases integration with existing tooling and block explorers.
- **`[S]` Wider release matrix.** *The unit and the RPM are done; darwin/windows
  are not.* [scripts/packaging/dnas.service](scripts/packaging/dnas.service) is a
  sandboxed unit (`ProtectSystem=strict`, an empty capability bounding set, a
  syscall filter, one writable path) with the API token in an `EnvironmentFile`
  rather than on a command line every local user can read out of `/proc`.
  `make rpm` builds an RPM from [scripts/packaging/dnas.spec](scripts/packaging/dnas.spec).
  Unlike the `.deb` it does not cross-compile — rpmbuild runs the build itself —
  so an RPM for another architecture must be built on or in a container for it.
  What remains is darwin and windows in the default `dist` targets.
- ~~**`[S/M]` Charts in the web explorer.**~~ **Done, plus the rich list.** The
  page showed status, blocks and mempool and nothing over TIME. It now draws five
  sparklines — difficulty, base fee, block interval, transactions per block, fees
  per block — as inline SVG it renders itself, because the page is served by the
  node and must work on a machine with no internet.

  It turned out not to be purely a rendering job after all: `/chainstats`
  summarizes a window into single numbers, which cannot show a quantity MOVING (a
  median interval of 60s is the same number whether every block took 60s or half
  took 5s and half took 115s). `/series` ([core/chainstats.go](core/chainstats.go))
  is the per-height series the charts needed. `/richlist`
  ([core/richlist.go](core/richlist.go)) ranks holders through a bounded min-heap,
  so one request cannot sort a whole ledger.
- **`[M]` Persist the address index.** It is in memory and rebuilt at every
  startup ([core/addrindex.go](core/addrindex.go)), which is fine for a devnet and
  not for a chain of any length. It also grows with an address's usage rather than
  with the chain, so it wants an on-disk, paged representation rather than a map
  of slices.
- ~~**`[M]` A real mining pool, not just shares.**~~ **Done.** Shares existed with
  no pool around them: one target for everyone, no payout scheme, no persistence,
  no authentication. All four are now closed.

  `dnas node -stratum :3333` ([node/stratum.go](node/stratum.go)) serves a
  Stratum-shaped protocol — line-delimited JSON-RPC,
  subscribe/authorize/notify/submit, jobs PUSHED the moment the tip moves. It is
  explicitly NOT Bitcoin-compatible: Stratum V1's job encoding is built around
  Bitcoin's 80-byte header, so a cgminer pointed at this port would hash the
  wrong bytes, and pretending otherwise would be worse than saying so. Each
  connection gets its own EXTRANONCE, written into the coinbase memo so two
  miners on one tip search different spaces rather than racing over identical
  nonces; its own DIFFICULTY, retuned toward one share every ten seconds
  ([node/pool.go](node/pool.go)); and its shares are weighted by their own
  difficulty and paid PPLNS, so what a miner earns tracks the work it did rather
  than the shares it happened to submit. `/pool` reports what the pool owes on
  the next block it finds.
- **`[S/M]` A public testnet with seeds and a hosted faucet.** The pieces exist —
  a distinct `testnet` network with its own genesis, and `-faucet` — but nothing is
  hosted, so joining still means knowing someone's address. It wants DNS seeds
  (the addrman item in §2), a public node, and a faucet whose abuse control is
  better than a per-IP cooldown.
- ~~**`[M]` Persist the reorg history and the share ledger.**~~ **Done.** Both
  were in-memory rings, so a restart lost exactly the record you want after an
  incident and cost a pool its accounting. They now get the same treatment the
  peer/ban/mempool soft state already had ([node/persist.go](node/persist.go)),
  along with the PPLNS payout window.

  One deliberate asymmetry: the reorg ENTRIES are restored and the reorg COUNTERS
  are not. `total`, `deepest` and the refusal tallies mean "what this node has
  seen since it started", and an operator reading `refused: 3` needs to know
  whether those happened in this run — because a refused reorg means the node may
  have stopped following the network's chain right now.
- ~~**`[S]` Size-bound the HTTP write endpoints.**~~ **Done.** Every write endpoint
  now decodes through `decodeBody` ([api/api.go](api/api.go)), which wraps the body
  in an `http.MaxBytesReader` and answers **413** before parsing: 64 KiB for the
  small control payloads, 4× `MaxRelayTxBytes` for a transaction and 4×
  `MaxBlockBytes` for a block (JSON with hex signatures runs several times larger
  than the canonical encoding those constants bound). The cap is on the *reader*,
  not on `Content-Length`, so a request that understates its length is still
  stopped mid-stream. What remains on this axis is the P2P half: an inbound frame
  has only the coarse 64 MiB `maxFrame` cap, which is the per-message-type item
  in §2.
- **`[S]` Expose the empty-block interval to operators.** The miner's idle throttle
  is a `node.Config` field (`EmptyBlockInterval`, default one `TargetBlockTime`),
  reachable only in Go — there is no `-emptyinterval` flag or `node.json` key, so a
  devnet that wants faster empty blocks has to use `-regtest`/`/generate` instead.

## 7. Assurance & testing

- ~~**`[M]` Structural / differential fuzzing.**~~ **Done.** The existing fuzz
  targets checked that nothing panics, which finds crashes and is not what is most
  likely to be wrong: a consensus bug is usually a correct-looking function that
  computes a subtly different answer, and it will not crash.
  [core/difffuzz_test.go](core/difffuzz_test.go) adds six differential targets,
  each checked against a from-scratch oracle written to be obviously correct
  rather than fast — a literal merkle fold, a trie built in the REVERSE order (so
  an insertion-order dependence shows up), a block-by-block subsidy sum, and a
  full state rebuild compared against what the undo log produced after a real
  reorg.
- **`[M]` Long-running adversarial testnet.** The in-process `simnet` harness proves
  convergence in seconds; a real multi-node testnet with churn, partitions, and a
  rogue miner running for days catches what a unit test cannot.
- **`[L]` Independent review / audit.** Before any word stronger than "toy" is used,
  an outside review of the consensus, cryptography, and P2P code.

---

*Have an idea that isn't here, or think a priority is wrong? This file is the place
to argue it.*
