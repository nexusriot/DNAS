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
key rather than the operator's wallet). What remains is the long tail that turns
a correct toy into something you could defend on a live network.

It is still a learning project. Nothing below should be read as a promise to ship,
and none of it makes DNAS money. **Do not point it at the internet.**

**Effort legend:** `[S]` ≈ hours · `[M]` ≈ a day or two · `[L]` ≈ structural /
multi-day. Items are roughly priority-ordered within each section; the earlier
sections matter most for "toy → real".

---

## 1. Production-grade consensus & state

- **`[L]` On-disk authenticated state trie.** State is an in-RAM `map` hashed into
  the header state root, so memory bounds the ledger and proofs can only show
  account *membership*, never *absence* (§21). A Merkle-Patricia or Verkle trie on
  disk gives O(1)-memory state, incremental root updates, and sorted-tree
  *non-membership* proofs. This also unblocks the item below.
- **`[M]` Persist the fast-synced (pruned) chain, and prune the STORE.** A node
  can now drop old bodies from memory (`-prune`, `core/prune.go`) and reports
  honestly what it can no longer serve, but the append-only file on disk still
  holds every block and a restart replays all of it — so pruning bounds resident
  size, not disk. Wire `SnapshotAt`/`NewFromSnapshot` through the index-based
  block store with a base offset, so a fast-synced or pruned node restarts
  without re-downloading and without keeping what it discarded.
- **`[M]` Fetch the filter-header chain during fast sync.** A fast-synced node
  never saw the bodies below its snapshot, so it cannot fold their filter
  commitments and reports `filter_base` above them (410 for anything lower).
  Fetching the chain from a peer during fast-sync — and checking it against a
  checkpoint — would close the one gap where such a node cannot serve a light
  client at all.
- **`[L]` A second implementation + cross-client consensus vectors.** The canonical
  codec makes a spec *possible*; only a second client (or, cheaper first step, a
  golden-vector suite — serialized tx/block/state hex → expected hash/validity)
  actually *proves* it. One implementation is one implementation, however careful.
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
- **`[M]` Consensus-checked addresses.** Nothing in consensus validates that `To`
  is a well-formed, checksummed address, so a buggy client can still burn coins to
  a typo, and `MaxAddressBytes` is only a length bound on how much junk can become
  a permanent state key. See the bech32 item in §5.
- **`[M]` BIP9-style miner signaling for upgrades.** `core/upgrade.go` flips rules
  at fixed activation heights, like checkpoints. Version-bits in the block header
  would let hashpower signal readiness and activate on a threshold instead of a
  hard-coded flag day.
- **`[S]` Enforce supply conservation in consensus.** The accounting now exists:
  minted (a function of height) and burned (accumulated per block) are tracked
  independently, `Blockchain.Supply()` reports `minted − burned == circulating`,
  and the tests assert it across mining, reorgs, reopen and fast-sync
  ([core/supply.go](core/supply.go), §9). What remains is making it a *rule* —
  checked per block during application, ideally committed in the header — plus the
  same treatment for per-asset issue/transfer conservation, so an accounting bug
  is rejected rather than merely reported.
- **`[M]` Unique coinbase transactions (BIP34).** A coinbase commits only
  (recipient, amount), so two blocks paying the same miner the same subsidy have
  the same txid. Lookups work around it by resolving to the first occurrence
  ([core/txindex.go](core/txindex.go)), but the duplication is real: an inclusion
  proof for such a coinbase can only point at one of the blocks. Binding the block
  height into the coinbase fixes it at the cost of a consensus change (existing
  stores would not replay), so it wants an activation height via
  [core/upgrade.go](core/upgrade.go).

## 2. Networking hardening & reach

- **`[M]` Binary P2P wire format.** Consensus is binary, but the peer envelope
  ([node/protocol.go](node/protocol.go)) is still JSON. A binary framing on the hot
  path cuts bandwidth and parse CPU, and removes the last consensus-adjacent use of
  a text codec.
- **`[M]` Compact block relay (BIP152).** Relay short transaction ids plus the
  prefilled coinbase so a peer reconstructs a block from its own mempool. Large
  latency/bandwidth win over shipping full blocks, and it reduces orphan rates.
- **`[M/L]` Outbound address manager with ASN/group diversity + DNS seeds.**
  Inbound eclipse caps exist (total + per-/16 group), but outbound peer selection
  and bootstrap are still manual (`-peers`). A tried/new addrman that buckets by
  ASN/network group, plus DNS seeds, closes the outbound eclipse vector (§21) and
  removes hand-configured bootstrapping.
- **`[M]` Authenticated / Tor-friendly transport.** The open handshake is anonymous
  and has no MITM authentication (§21). Optional peer-key pinning, an onion
  transport, and NAT traversal would harden and widen reach without giving up the
  permissionless default. Peer-key pinning now has something to pin: a node's
  identity is a stable key of its own ([node/identity.go](node/identity.go))
  rather than its wallet key.
- **`[M]` Network-adjusted time.** Timestamp checks use the local clock; a
  median-of-peers offset (bounded, bitcoind-style) resists a node with a skewed
  clock being fooled on MTP/timestamp rules.
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
- **`[M]` Richer native-asset operations + optional per-asset fees.** Assets today
  support issue and transfer only, and fees are always paid in coin (§21). Add
  mint/burn/freeze authority ops and (optionally) allow fees to be paid in an
  asset.
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
- **`[M]` Announce transactions by hash (inv/getdata for `MsgTx`).** Blocks are
  announced and pulled; transactions are still pushed in full to every peer, so
  each one crosses each link once per peer whether or not the peer already has it.
  Announcing the txid and letting peers request what they lack is the biggest
  bandwidth win left after compact blocks. A node now *reconciles* its pool once
  per peer on catch-up (`MsgGetMempool`), which fixes the cold-start hole but is
  not continuous reconciliation: a transaction broadcast in the gap between that
  request and the next push is still missed until someone rebroadcasts. Announcing
  by hash subsumes both.
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

- **`[M]` SLIP-0010 / BIP32 HD + hardware wallets.** HD derivation is a simple
  HMAC-SHA512 scheme (§21), and it is *hardened*, so there is no extended public
  key to hand out — the light wallet's watch-only export is an address list
  instead ([cmd/dnas/spvlabels.go](cmd/dnas/spvlabels.go)). Standard derivation is
  the prerequisite for a real xpub, for Ledger / Trezor support, and for
  cross-wallet interop.
- **`[M]` bech32 addresses with consensus-checked checksums.** Recipient checksums
  are validated client-side only (§21), so a malicious client can still burn coins.
  A bech32 address format checked in consensus makes fat-finger and buggy-client
  burns impossible.
- **`[S]` Fresh addresses per invoice.** `dnas invoice` matches a payment by
  (address, amount, height), so two invoices for the same amount at the same
  address are indistinguishable — the file says so, which is not the same as
  fixing it. An HD-derived address per invoice would, and needs the standard
  derivation above to be worth exporting.
- **`[M]` A watch-only daemon around `invoice watch`.** Watching is a foreground
  poll (`-wait`, `-every`): a shop wants something that survives a restart,
  remembers which invoices are outstanding, and calls a webhook when one settles
  rather than holding a terminal open.
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

- **`[S]` A Grafana dashboard for the Prometheus metrics.** The metrics themselves
  are now complete: `GET /metrics` ([api/api.go](api/api.go)) exports 36 series —
  height, difficulty, mempool depth *and bytes*, peer count, relay floor, base
  fee, mining flag and the share ledger as before, plus everything that used to be
  JSON-only: reorg totals and depth ([node/reorghist.go](node/reorghist.go)),
  orphan count, ban scores and the threshold, hashrate and block intervals
  ([core/chainstats.go](core/chainstats.go)), supply (minted/burned/circulating),
  tip age, blocks-behind, and webhook delivery counters. What remains is a
  dashboard to ship alongside them.
- **`[M]` JSON-RPC 2.0 interface.** The HTTP API is REST; a bitcoind-style JSON-RPC
  surface eases integration with existing tooling and block explorers.
- **`[S]` systemd unit + RPM + wider release matrix.** A `.deb` and tagged CI
  releases exist; add a hardened systemd unit, an RPM, and darwin/windows to the
  default `dist` targets.
- **`[S/M]` Charts in the web explorer.** The page now has a universal search (a
  height, a block hash, a transaction hash or an address), the asset registry, and
  a panel for what the node can serve. What is still missing is anything over
  TIME: supply / difficulty / fee-rate charts and a rich list. Every input exists
  server-side — per-address history (`/address/{addr}/history` with `-addrindex`),
  the mempool's fee-rate distribution (`/mempool/stats`), and hashrate / block
  timing / fee flow (`/chainstats`) — so this is purely a rendering job. The TUI
  draws the histogram and the PyQt client shows the stats; the web explorer is the
  one client still showing only status, blocks and mempool.
- **`[M]` Persist the address index.** It is in memory and rebuilt at every
  startup ([core/addrindex.go](core/addrindex.go)), which is fine for a devnet and
  not for a chain of any length. It also grows with an address's usage rather than
  with the chain, so it wants an on-disk, paged representation rather than a map
  of slices.
- **`[M]` A real mining pool, not just shares.** Shares exist
  ([node/shares.go](node/shares.go)) but the pool side does not: no per-miner
  share difficulty (everyone gets the same target regardless of hashrate), no
  variance-smoothing payout scheme (PPLNS/PPS), no persistence — the ledger is
  lost on restart — and no authentication of who is submitting. Any of those makes
  the current implementation a demonstration rather than something to point real
  hashpower at.
- **`[S/M]` A public testnet with seeds and a hosted faucet.** The pieces exist —
  a distinct `testnet` network with its own genesis, and `-faucet` — but nothing is
  hosted, so joining still means knowing someone's address. It wants DNS seeds
  (the addrman item in §2), a public node, and a faucet whose abuse control is
  better than a per-IP cooldown.
- **`[M]` Persist the reorg history and the share ledger.** Both are in-memory
  rings today ([node/reorghist.go](node/reorghist.go), [node/shares.go](node/shares.go)),
  so a restart loses exactly the record you want after an incident, and a pool
  loses its accounting. They want the same treatment the peer/ban/mempool soft
  state already gets in [node/persist.go](node/persist.go).
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
- **`[S]` Binary block bodies on disk.** The block store's framing is binary but
  each record is still the block's JSON ([core/store.go](core/store.go)).
  Switching records to the canonical codec shrinks the file and speeds startup;
  `dnas db export`/`import` already provides the migration path.
- **`[S]` Expose the empty-block interval to operators.** The miner's idle throttle
  is a `node.Config` field (`EmptyBlockInterval`, default one `TargetBlockTime`),
  reachable only in Go — there is no `-emptyinterval` flag or `node.json` key, so a
  devnet that wants faster empty blocks has to use `-regtest`/`/generate` instead.

## 7. Assurance & testing

- **`[M]` Structural / differential fuzzing.** Fuzz the canonical codec (round-trip
  and hash-stability), transaction/block validation, and reorg+undo against a
  from-scratch oracle.
- **`[M]` Long-running adversarial testnet.** The in-process `simnet` harness proves
  convergence in seconds; a real multi-node testnet with churn, partitions, and a
  rogue miner running for days catches what a unit test cannot.
- **`[L]` Independent review / audit.** Before any word stronger than "toy" is used,
  an outside review of the consensus, cryptography, and P2P code.

---

*Have an idea that isn't here, or think a priority is wrong? This file is the place
to argue it.*
