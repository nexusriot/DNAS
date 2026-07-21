# Roadmap

This is the honest backlog of what DNAS does **not** do yet. For what it *does*
do, see [README.md](README.md) and [DESIGN.md](DESIGN.md); the trade-offs behind
each shortcut are catalogued in [DESIGN.md §21 "Known limitations"](DESIGN.md).

Where DNAS stands today: the three properties that separate a real coin from a
demo are in place — **unbounded proof-of-work difficulty** (an LWMA-retargeted
256-bit `nBits` target with no hard cap; `NoRetarget` only for regtest), a
**canonical, implementation-independent consensus encoding** ([core/codec.go](core/codec.go))
with a height-activated **upgrade path** ([core/upgrade.go](core/upgrade.go)), and
a **permissionless, eclipse/DoS-hardened network** (open handshake, inbound caps,
per-peer rate limiting). What remains is the long tail that turns a correct toy
into something you could defend on a live network.

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
- **`[M]` Persist the fast-synced (pruned) chain.** Snapshot fast-sync verifies
  state against the header state root and an anchoring checkpoint, but the pruned
  chain then lives in memory (§21). Wire `SnapshotAt`/`NewFromSnapshot` through the
  index-based block store with a base-offset so a fast-synced node restarts
  without re-downloading.
- **`[L]` A second implementation + cross-client consensus vectors.** The canonical
  codec makes a spec *possible*; only a second client (or, cheaper first step, a
  golden-vector suite — serialized tx/block/state hex → expected hash/validity)
  actually *proves* it. One implementation is one implementation, however careful.
- **`[M]` BIP9-style miner signaling for upgrades.** `core/upgrade.go` flips rules
  at fixed activation heights, like checkpoints. Version-bits in the block header
  would let hashpower signal readiness and activate on a threshold instead of a
  hard-coded flag day.
- **`[S/M]` Explicit supply-conservation invariant.** Assert per block that
  `Σ outputs + fees == Σ inputs + subsidy` (and per-asset issue/transfer
  conservation), ideally committed, so an accounting bug can't silently inflate
  supply.

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
  and bootstrap are still manual (`-peer`). A tried/new addrman that buckets by
  ASN/network group, plus DNS seeds, closes the outbound eclipse vector (§21) and
  removes hand-configured bootstrapping.
- **`[M]` Authenticated / Tor-friendly transport.** The open handshake is anonymous
  and has no MITM authentication (§21). Optional peer-key pinning, an onion
  transport, and NAT traversal would harden and widen reach without giving up the
  permissionless default.
- **`[M]` Network-adjusted time.** Timestamp checks use the local clock; a
  median-of-peers offset (bounded, bitcoind-style) resists a node with a skewed
  clock being fooled on MTP/timestamp rules.

## 3. Programmability

- **`[L]` Authorization script VM.** Multisig and HTLC are hand-rolled special
  cases in consensus. A small, deterministic, gas/opcount-metered predicate
  language would unify them behind one verifier and unlock covenants, vaults, and
  richer spend conditions — the single highest-leverage expressiveness change.
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
  relay floor, and rate-based eviction exist; child-pays-for-parent and package
  acceptance let a stuck low-fee parent be bumped and prevent pinning attacks.
- **`[S/M]` Weight-based congestion signal.** The EIP-1559 base fee currently
  responds to transaction *count* (§9); switching the signal to block weight/bytes
  makes it track real demand.

## 5. Wallet & UX

- **`[M]` SLIP-0010 / BIP32 HD + hardware wallets.** HD derivation is a simple
  HMAC-SHA512 scheme (§21). Standard derivation is the prerequisite for Ledger /
  Trezor support and cross-wallet interop.
- **`[M]` bech32 addresses with consensus-checked checksums.** Recipient checksums
  are validated client-side only (§21), so a malicious client can still burn coins.
  A bech32 address format checked in consensus makes fat-finger and buggy-client
  burns impossible.
- **`[M]` PSBT-style partially-signed transactions.** A portable partial-signature
  format so multisig co-signers can pass a transaction around and sign offline.
- **`[S]` BIP21 payment URIs + message sign/verify.** `dnas:` payment URIs for the
  wallet/explorer, and detached "prove I own this address" signatures.

## 6. Ops, tooling & observability

- **`[S/M]` Prometheus metrics + Grafana dashboard.** Export height, mempool depth,
  peer count, reorg/ban counters, and block interval, and ship a dashboard JSON.
- **`[M]` JSON-RPC 2.0 interface.** The HTTP API is REST; a bitcoind-style JSON-RPC
  surface eases integration with existing tooling and block explorers.
- **`[S]` systemd unit + RPM + wider release matrix.** A `.deb` and tagged CI
  releases exist; add a hardened systemd unit, an RPM, and darwin/windows to the
  default `dist` targets.
- **`[S/M]` Richer web explorer.** Supply / difficulty / fee-rate charts, a rich
  list, and per-address history on top of the existing in-browser SPV explorer.

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
