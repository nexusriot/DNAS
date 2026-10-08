# DNAS design

This document explains **how DNAS works and why it is built the way it is** —
the data model, consensus, networking, and the deliberate trade-offs behind each
choice. For usage see [README.md](README.md) and [QUICKSTART.md](QUICKSTART.md);
each module also has its own `README.md`. For the deliberate gaps and what would
close them, see [ROADMAP.md](ROADMAP.md).

DNAS ("Definitely Not A Scam") is a small but genuinely working proof-of-work
cryptocurrency. It is a learning project, not money.

---

## Contents

The section numbers are stable: they are referenced as `§n` from the other
documents and from comments in the code, so a new section is inserted with a
suffix (§19b) rather than by renumbering everything after it.

| | | | |
|---|---|---|---|
| [1. Goals and non-goals](#1-goals-and-non-goals) | [7. Proof of work and difficulty](#7-proof-of-work-and-difficulty) | [13. Wallet](#13-wallet) | [19. Testing](#19-testing) |
| [2. Design principles](#2-design-principles) | [8. Fork choice, timestamps, reorgs, maturity](#8-fork-choice-timestamps-reorgs-and-maturity) | [14. Light clients (SPV)](#14-light-clients-spv) | [19b. Logging and operability](#19b-logging-and-operability) |
| [3. Module layout](#3-module-layout) | [9. Issuance and monetary policy](#9-issuance-and-monetary-policy) | [15. HTTP API and explorer](#15-http-api-and-explorer) | [20. Build and release](#20-build-and-release) |
| [4. Ledger and state model](#4-ledger-and-state-model) | [10. Mempool](#10-mempool) | [16. Clients](#16-clients) | [21. Known limitations](#21-known-limitations) |
| [5. Transactions](#5-transactions) | [11. Networking](#11-networking) | [17. Consensus parameters](#17-consensus-parameters) | |
| [6. Blocks, headers, Merkle proofs](#6-blocks-headers-and-merkle-proofs) | [12. Persistence](#12-persistence) | [18. Key decisions and trade-offs](#18-key-decisions-and-trade-offs) | |

Section 5 has the transaction sub-forms:
[5.1 Networks](#51-networks-and-what-binds-a-chain-to-one) ·
[5.2 Time-delayed vaults](#52-time-delayed-vaults) ·
[5.3 Fee sponsorship](#53-fee-sponsorship) ·
[5.4 Spending from a multisig account](#54-spending-from-a-multisig-account-and-escrow-on-top-of-it) ·
[5.5 The memo](#55-the-memo-and-what-it-made-possible) ·
[5.6 Script-authorized spends](#56-script-authorized-spends) ·
[5.7 Names](#57-names)

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
| `wallet` | Ed25519 keys, checksummed addresses, at-rest encryption, BIP39/SLIP-0010 HD, and the address kinds derived from a spending condition (multisig, HTLC, vault, script) |
| `core`   | Networks, transactions, blocks, headers, Merkle, the state trie and its incremental commitment, work, reorgs, mempool and package relay, the authorization script VM, names, assets, persistence, params, share targets, store tooling |
| `node`   | Encrypted/authenticated P2P, peer identity, ban scoring, discovery, sync, mempool reconciliation, the miner, the share ledger, the faucet |
| `api`    | HTTP interface + embedded web explorer                                |
| `cmd`    | The `dnas` CLI/daemon (`cmd/dnas`), and `cmd/wasmverify` — `core`'s verification primitives compiled to WebAssembly for the browser |

Two clients live **outside** the Go workspace and talk only to the HTTP API:

- `tui/` — a bubbletea terminal client. It is its **own module** with a nested
  `tui/go.work`, deliberately excluded from the root workspace: it pulls in
  external deps (bubbletea/lipgloss) whose `go.sum` verification would otherwise
  be forced onto the internal `v0.0.0` modules. It imports no DNAS module and
  **cannot**: adding `use ../core` to its workspace still sends Go to the proxy
  for `wallet`, because the module path looks like a sub-module of a repo and
  resolution wants a `wallet/v0.0.0` tag that does not exist.
- `gui/` — a PyQt6 desktop client (Python standard library + PyQt6 only).

Neither re-implements a consensus rule. Both shell out to the `dnas` binary for
signing *and* for verification, and the explorer served by `api` calls into
`core` compiled to WebAssembly. They used to carry their own transcriptions of
the header preimage, and every one of those transcriptions had silently drifted
out of step with it (§21).

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
    Outputs          []Output        // OR pay many recipients at once (see below)
    AssetID          string          // move this native asset instead of coin (§4)
    Issue            *AssetIssue     // OR mint a new native asset to From (§4)
    PubKey, Signature string         // single-key authorization (both hex)
    Multisig         *MultisigScript // OR multisig authorization
    Signatures       []string        // hex
    HTLC             *HTLCScript     // OR hash-time-locked-contract authorization
    Preimage         string          // hex; revealed on the HTLC claim branch
    Vault            *VaultScript    // OR time-delayed vault authorization (§5.2)
    FeePayer         string          // a third party pays the fee (§5.3)
    FeePayerPubKey   string          // hex; must derive FeePayer
    FeePayerSig      string          // hex; the sponsor's signature over the same signing bytes
}
```

**Signing.** The signed message (`signingBytes`) covers every consensus-relevant
field (From/To/Amount/Fee/Nonce/Expiry/LockUntil/AssetID/Issue/Memo, plus Outputs
and FeePayer when present, and the NETWORK ID — see §5.1) but **not** the
signature fields. Crucially it is *identical* for single-key and multisig
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
- **Time-delayed vault:** `From` must equal `VaultAddress(Hot, Cold, Unlock)`, and
  either key may have signed — the height rule that separates them is applied at
  block application, not here (§5.2).
- **M-of-N multisig:** `From` must equal `MultisigAddress(Threshold, PubKeys)`
  (so the script is bound to the address it spends), and at least `Threshold`
  signatures from *distinct* listed members must verify. `verifyMultisig`
  enforces distinctness so one member can't satisfy a 2-of-N alone. Two further
  rules exist for reasons that are not cosmetic:
  - **`N ≤ MaxMultisigKeys` (16).** Matching signatures to members is inherently
    `O(signatures × keys)` Ed25519 verifications, and that work is done by every
    node that merely *relays* the transaction, before any fee is charged — an
    unbounded `N` is a free network-wide CPU exhaustion. The bound also makes the
    address unambiguous: the threshold is folded in as a single byte, so an `N`
    above 255 would let a 1-of-N script hash to the same address as an M-of-N one.
  - **Every supplied signature must match a member.** Padding the list with junk
    is rejected rather than ignored, which keeps a *failed* verification linear
    instead of quadratic and removes a malleability handle — `Signatures` is
    covered by the txid but by no signature, so a relay could otherwise change a
    transaction's id and inflate the byte size its base fee is charged on.
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

**Multi-recipient transfers.** `Outputs []Output{To, Amount}` pays several
addresses from one transaction: one fee, one nonce, one signature, and one copy of
the sender's address and public key in the block, instead of N of each. It is coin
only (an asset move or an issuance uses the single-recipient form), bounded by
`MaxTxOutputs`, and every rule that applies to a recipient applies to each output
individually — the dust limit included, so a batch cannot smuggle dust past it.
Credits are applied one at a time against live state, so a repeated recipient
accumulates and a sender paying itself nets correctly.

Two properties are worth spelling out, because they are what make the change safe
to add to a running chain:

- **Existing transactions are untouched.** The outputs are appended to the
  canonical encoding *only when present* ([core/codec.go](core/codec.go)), so a
  single-recipient transaction's signing bytes, txid and fee-bearing size are
  byte-for-byte what they always were, and a stored chain still replays.
- **The rule is height-activated.** `UpgradeMultiOutput` (§8) gates acceptance,
  so the whole network starts allowing the new form at one agreed height instead
  of some nodes treating a block as valid while others reject it. Operators
  schedule it with `-upgrades multioutput:HEIGHT`.

**Time windows.** `Expiry` (upper bound) and `LockUntil` (lower bound) constrain
the height range in which a transaction is valid; both are signed. Expired
transactions are pruned from the mempool and rejected at submission; locked ones
are skipped during selection.

**Replace-by-fee.** A stuck transaction is bumped by re-submitting it at the
*same* `(From, Nonce)` with a strictly higher fee; the mempool replaces the old
one (§10).

**Coinbase.** The block's first transaction mints `reward + tips` to the miner
and has no signature (`IsCoinbase`) — the base-fee portion of every fee is burned
rather than paid to the miner (§9). Its *shape* is pinned by
`validateCoinbaseShape`: a recipient and an amount, nothing else, and at most
`MaxCoinbaseBytes`. The coinbase pays no per-byte base fee and is excluded from
`MaxBlockBytes` (both meter the paid transactions), so without its own cap a miner
could commit an arbitrarily large block that every node must store and relay; and
fields that mean nothing in a coinbase are required to be empty rather than left
as somewhere for two implementations to disagree.

**Verification cost is metered and paid once.** Matching signatures is the most
expensive thing a node does per transaction, and bytes do not bound it: a 16-key
multisig spend costs up to 256 verifications in a couple of kilobytes, so a
byte-legal block could take longer for the network to check than for its miner to
produce. `VerifyOps` prices that worst case per transaction and
`MaxBlockVerifyOps` caps it per block (Bitcoin meters the same thing as sigops),
with `Mempool.Select` honouring the same budget so the miner never builds a block
over it.

The work is also paid only once. A `ValidationCache`
([core/valcache.go](core/valcache.go)) records which txids have had their
authorization verified; the node shares one between the mempool and the chain, so
a payment verified on admission costs a map lookup when the block carrying it
applies. Because the cache is keyed on the txid — which commits to the signatures
themselves — a mutated copy is a different transaction and is still checked in
full. Before the serial application pass, a block's signatures are verified across
all cores, so initial sync is not bound to one. Measured on a 400-signature block:
18.7 ms serial, 7.4 ms in parallel, 0.5 ms when already cached.

**Context-free validity.** Every rule about a transaction that depends on neither
chain state nor height lives in one exported function, `CheckTxSanity` — memo and
address lengths, `Amount + Fee` overflow, asset/issuance shape, and the rule that
a transaction carries exactly one kind of authorization. Both the mempool
(`Mempool.Add`) and block application (`applyTxsAndCoinbase`) call it, and the
height-dependent rules are shared the same way through `checkTxAtHeight`. That
sharing is a correctness requirement, not tidiness: a transaction the mempool
admits but a block cannot contain is selected into every candidate the miner
builds, so every candidate is invalid and **block production stops** until the
transaction is evicted. One function for both callers makes that divergence
impossible for this class of rule.

### 5.1 Networks, and what binds a chain to one

A DNAS process runs on exactly one **network** — `mainnet`, `testnet` or
`regtest` ([core/network.go](core/network.go)) — selected once at startup with
`-network` and identical on every node meant to converge. A network is not a
label: its ID is bound into three places, and each closes a hole that existed
while it was not.

| Bound into | Closes |
|------------|--------|
| the genesis block, via `PrevHash` | two networks cannot share a chain; every other parameter matching no longer makes them the same chain |
| the transaction **signing preimage** (`signedFields`) | a signature made on one network does not authorize the same transfer on another — cross-network replay |
| the peer **handshake** (`MsgVersion.Network`) | nodes on different networks disconnect immediately instead of failing to converge forever |

Mainnet's id is deliberately the **empty string**, and both the genesis
`PrevHash` and the signing preimage omit an empty id entirely. So every mainnet
encoding is byte-for-byte what it was before networks existed: no flag day, no
changed txids, and a stored chain still replays. The other networks get their own
genesis hash and their own signatures for free.

`regtest` additionally holds difficulty at the genesis target (`NoRetarget`) and
defaults to its own pre-shared network key, which is how it was isolated before —
now belt *and* braces.

### 5.2 Time-delayed vaults

A `VaultScript{Hot, Cold, Unlock}` is a third script-bound address kind alongside
multisig and HTLC. Coin at `VaultAddress(Hot, Cold, Unlock)` is spendable by the
**cold** key at any height, and by the **hot** key only from `Unlock` on. The hot
key lives on a warm machine and signs day to day; the cold key stays offline. If
the hot key is stolen, the thief must wait out the delay, and the cold key can
move the coin somewhere safe first.

Which branch a spend took is decided by *which key signed*, so the height rule
(`VaultHotNotReady`) has to re-derive that independently of the signature cache —
it verifies the cold key's signature, and a spend that is not the cold key's is
the hot key's. That costs one extra verification, which `VerifyOps` charges for.

Vault spends are gated by the `vault` height-activated upgrade, and — like
multi-output transfers — the script is encoded only when present, so scheduling
the upgrade changes no existing txid and no stored chain.

One interaction is worth knowing before trusting a vault with anything. A hot-key
spend below `Unlock` is *admitted to the mempool* and merely never selected (the
same treatment an HTLC refund before its timeout gets). A thief holding the hot
key can therefore park an unmineable spend at the vault's nonce, and the cold
key's rescue — which uses that same nonce — has to displace it by
**replace-by-fee**, i.e. by paying strictly more. The cold key can always do that,
since it is sweeping the whole balance, but it is a step the rescuer has to take
rather than a race they automatically win.

This is the cheap version of what a script VM would express generically (the
`[L]` item in the ROADMAP): it buys a real new spending condition today at the
cost of being one more hand-rolled special case in consensus.

A consequence of §5.1 that bites in practice: **a client must be on the node's
network**. A transaction's hash and its signing preimage both carry the network
id, so a client that thinks it is on mainnet while the node runs regtest produces
signatures the node rejects and merkle roots that do not match — failures that
surface as "invalid signature" or "merkle root mismatch" rather than as the
configuration error they are. Rather than a flag that has to match, the CLI
*asks*: `GET /info` reports the network and every client command that signs,
verifies a genesis, or recomputes a transaction hash adopts it before doing so
([cmd/dnas/network.go](cmd/dnas/network.go)). The external miner needs this as
much as a wallet does, because it recomputes the candidate block's merkle root.

### 5.3 Fee sponsorship

With `FeePayer` set, the **fee** is charged to that account instead of the
sender's. An address holding no coin at all can then transact, because someone
else pays for its block space — the onboarding problem every account-model chain
has, solved without a faucet or a special case in the fee rules. The fee itself is
unchanged: the base-fee portion is still burned and the tip still goes to the
miner; only who is debited moves.

Two signatures authorize it. The sender signs `FeePayer` along with everything
else (it is in the signing preimage), so the sponsor cannot be swapped in or out
without invalidating the transaction; the sponsor signs the *same bytes*, which
name the sender, the amount and the nonce — so a sponsorship is bound to exactly
one transfer and cannot be lifted onto another.

The sponsor has **no nonce of its own**. Replay is already impossible because the
sender's nonce is consumed exactly once, and leaving the payer's nonce alone means
sponsoring does not disturb transactions the payer has in flight. The payer's
own coinbase-maturity reserve still applies, so a miner cannot pay everyone's fees
out of a reward a reorg may take back.

The mempool tracks a running total per payer (`Mempool.sponsored`), so a sponsor
is held to everything it has promised across the pool rather than one fee at a
time — otherwise a payer could sponsor a hundred transactions it can afford one at
a time and only the first would be mineable, which is the same "queue full of
unminable work" problem the sender rules exist to prevent, moved one account over.

Gated by the `feesponsor` height-activated upgrade. Only a single-key account can
sponsor — the payer's key must derive the payer's address — so a multisig or
script account cannot currently pay someone else's fee.

Because it takes two parties, it takes two steps, and a transaction has to travel
between them half-authorized: `dnas sponsor request` builds and sender-signs it
(committing to who pays), `dnas sponsor pay` counter-signs and submits. The
half-signed file is not a bearer instrument — its sender, recipient, amount and
nonce are all covered by the sender's signature, so a payer can only agree to it
or not, never alter it. It is the shape a PSBT has, for the one case that needs
it today.

### 5.4 Spending from a multisig account (and escrow on top of it)

Consensus has verified M-of-N multisig from early on: `verifyMultisig` checks
that the script hashes to the sender address and that M *distinct* listed members
signed. Four separate surfaces would derive a multisig address (`dnas wallet
multisig`, `POST /multisig/address`, the TUI, the GUI) and nothing could spend
one — `Transaction.AddSignature` was called by tests and by nothing else. The
feature was complete except for the part where you get your coin back.

`dnas multisig` is that part, and it borrows the shape `dnas sponsor` already
had: the transaction travels between the members as a file, gaining one signature
per stop. Three properties make that safe:

- **The file is not a bearer instrument at any stage.** The recipient, the
  amount, the fee and the nonce are covered by every signature already on it, so
  a later signer can only agree to the same transfer or refuse.
- **The script is bound into the address.** A file naming a different member set
  simply does not hash to the account it is trying to drain.
- **It carries its network.** Signing is offline — the members that matter are
  the ones kept away from the machine running a node — and a signature commits to
  the network id, so a member whose CLI defaulted elsewhere would produce a
  worthless signature and nobody would find out until submission, with the coin
  still stuck. Reading the file selects the chain; there is deliberately no flag
  to get wrong.

The tool refuses the two mistakes that reach the node as the same message ("not
enough signatures"): a non-member signing, and one member signing twice. The
second matters because consensus counts M *distinct* members and rejects a
signature matching no unused one, so a doubly-signed file is not redundant, it is
unusable.

**Escrow** (`dnas escrow`) is a 2-of-3 whose three members have names: buyer,
seller, arbiter. Consensus needs none of this — it is a multisig account and
nothing more — and what the tool adds is the part that is easy to get wrong by
hand: remembering which public key is whose role, and building a spend that pays
the right one of them. It refuses two roles sharing a key, which would quietly
make a 2-of-3 into a 1-of-2 that one party can spend alone, and it re-derives the
address from the roles on every read rather than trusting the stored one. Its
payouts are ordinary multisig spend files, so the signatures are collected by the
same tool — there is no second signing protocol to keep in step with the first.

### 5.5 The memo, and what it made possible

`Memo` and the `Expiry`/`LockUntil` window are signed consensus fields that every
client left at zero for as long as they existed. Giving the light wallet flags for
them (§14) turned two other things from ideas into commands:

- **Anchoring** (`dnas anchor`) publishes `sha256(file)` in a zero-value
  self-payment and later proves it: the header chain's proof of work, a merkle
  path to a header, and — the step it would be easy to skip — reading the
  transaction to see what the proven txid actually commits to. The merkle proof
  binds a txid to a block; only the body says what that txid *means*.
- **Invoices** (`dnas invoice`) state what is wanted, hand the payer a `dnas:`
  URI, and verify settlement the way a merchant needs it verified: proof of work
  checked locally, compact filters to find the blocks touching the address, those
  bodies authenticated against their headers, and confirmations required before
  the answer is yes. A payment in the tip block alone can still be reorganized
  away, and a merchant shipping on one confirmation has been paid reversibly.
- **Payment URIs** (`core/uri.go`) are the format that invoice hands over:
  `dnas:ADDRESS?amount=DECIMAL&memo=TEXT&ref=TOKEN`. It was write-only for a long
  time — printed by `invoice new`, parsed by nothing — which meant the payer still
  read the address off it and retyped it, and retyping the address is precisely
  the step the URI exists to remove: consensus does not validate recipients (§21),
  so a typo that keeps the length is a burn. Parsing now checksum-validates the
  address, so a URI that survives cannot direct a payment at a mistyped one, and
  every surface that would be handed one reads it: `invoice pay -uri`, `spv wallet
  send`, the TUI prompt, the explorer form and the PyQt client.

  Two decisions are worth naming. The amount is **decimal DNAS, not base units** —
  a human reads this string, and a factor of 10⁸ is the most expensive mistake the
  format could invite — and building and parsing live in one file so a round trip
  is a test rather than a hope. And an amount given both in the URI and on the
  command line must **agree**: silently preferring either one means a payer who
  mistyped pays a different amount and believes they paid the right one, which for
  an invoice is the same as not paying, since (address, amount) is what the payee
  matches on. The clients that import no DNAS package (TUI, GUI, explorer) keep
  their own small parsers by necessity; a shared, checksum-guarded fixture keeps
  them agreeing with the canonical one.

Consensus gained one rule from this: an **inverted height window** is rejected
outright. Below `LockUntil` a transaction is not yet valid and above `Expiry` it
is too late, so if the two cross there is no height at which it could be mined —
the check changes the validity of no block (application already refuses it
everywhere) and keeps the mempool from holding something unminable while telling
whoever built it what is wrong.

---

### 5.6 Script-authorized spends

Multisig, HTLC and the time-delayed vault are each a hard-coded shape: a struct
on `Transaction`, a branch in `verifySender`, an address derivation, a codec
block, a CLI command and a consensus upgrade. The fifth spending condition
costs the same again, which in practice means the fifth never gets written and
somebody contorts one of the existing four instead.

`UpgradeScript` replaces the shapes with a language
([core/script.go](core/script.go)). The sender publishes a predicate, the
spender supplies a witness, consensus runs the predicate, and the spend is
allowed if it ends with exactly one truthy value on the stack. `From` is the
address derived from the script under its own domain tag, so publishing the
script is what creates the account and nobody can attach a different predicate
to an address afterwards.

**What it is not.** It has no loops, no backward jumps, no memory beyond a
bounded stack, and no access to chain state beyond the current height. It
decides one question — may this transaction spend from this account — and the
only thing it can return is yes or no. A language that could also say "and pay
Bob 5 coin" is a different project with a different safety argument.

Three decisions carry the safety, and each closes a hole a naive stack machine
leaves open:

1. **Execution is priced before it runs, not watched while it runs.** Script
   length, stack depth, operation count and signature checks all have hard
   caps, and `VerifyOps` charges a script its WORST CASE rather than what the
   taken path happened to cost — otherwise a cheap-looking script with an
   expensive other branch would slip past the budget it was admitted under.

2. **The height opcode cannot be branched on.** `OP_CHECKHEIGHTVERIFY` aborts
   rather than pushing a result, exactly as Bitcoin's CLTV does. If height
   could push a boolean into `OP_IF`, the branch taken would depend on when the
   script ran, so a transaction's authorization would mean one thing in the
   mempool and another in the block — it would verify in one place and fail in
   the other. Aborting makes the height a constraint on WHEN, never an input to
   WHAT. The requirement it records is enforced in `checkTxAtHeight`, the same
   split the HTLC and the vault already use.

3. **The witness is data and is never executed.** A witness that could carry
   operations would let the spender rewrite the predicate it is supposed to
   satisfy.

The signature opcodes verify over `canonicalSigningBytes` — the same preimage
every other authorization shape signs — so one wallet signs for all of them and
there is no second signing format to drift. The witness itself cannot be
covered by a signature inside it, so it is covered by the txid instead, which
the block's merkle root commits once mined.

[core/scriptbuild.go](core/scriptbuild.go) writes the four hard-coded shapes in
the language. That is the specification by example, and it is also where the
witness ORDER for each is documented: the stack is filled bottom-first, so an
HTLC claim puts the preimage above the signature because `OP_SHA256` takes the
top. Getting that backwards makes a perfectly good signature fail as a hash
mismatch, which is why `dnas script spend` runs the predicate locally and names
the failing check before it broadcasts anything.

The hard-coded shapes remain in consensus. A chain with the upgrade active has
two ways to express a vault; retiring the old ones would invalidate every
funded address using them, which is a migration nobody has needed.

---

### 5.7 Names

`UpgradeNameService` adds human-readable names that resolve to addresses
([core/name.go](core/name.go)). The reason it is in consensus rather than
beside it is the only reason that matters: a name service you have to trust a
server about is a phishing surface with extra steps. The records live in the
state trie, so a light client can verify both that a name belongs to an address
and that a name is **unclaimed**, against a header the proof of work covers.
`dnas name resolve` proves by default and `-trust` opts out, which is the right
way round.

**Where they live.** Names are entries in the same account map as balances,
under a key prefixed with a NUL byte. Two independent things keep the key
spaces apart, and both are needed:

- No address may contain the prefix — `CheckTxSanity` rejects one. This is a
  string comparison.
- The trie hashes a name key under a **different domain tag** than an address
  (`trieKeyFor`). This is the cryptographic one, and it is what actually holds:
  even a crafted address string cannot land on a name's slot.

Without the second, a transfer to a carefully chosen `To` would write a balance
into a name's trie position.

**A lease, not a freehold.** A registration expires and an expired name is
claimable by anyone. A namespace nobody can reclaim is squatted once and then
dead, and there is no authority here to appeal to. Renewal extends from the
current expiry rather than from now, so renewing early never costs time, and
`resolve` distinguishes three answers rather than two: UNREGISTERED, LAPSED
(with the address it used to point at, and a warning not to pay it), and held.

A released name is **deleted** from state rather than stored empty, so a chain
where a name was claimed and given up commits the same state root as one where
it never existed. An empty record left behind would be permanent state
committing to nothing.

Two registrations of one name both look valid against the chain and the second
fails when the block is applied, which would invalidate the whole candidate —
so `Mempool.Select` tracks the names it has already placed, the same way it
simulates balances and asset holdings.

---

## 6. Blocks, headers, and Merkle proofs

```go
type Block  struct { Version, Index, Timestamp, Nonce; PrevHash; MerkleRoot; StateRoot;
                     BaseFee; Bits; FilterRoot; HistoryRoot; Transactions; Hash }
type Header struct { Version, Index, Timestamp, Nonce; PrevHash; MerkleRoot; StateRoot;
                     BaseFee; Bits; FilterRoot; HistoryRoot; Hash }
```

A block hash commits to header fields **plus two merkle roots** — a `MerkleRoot`
of its transactions and a `StateRoot` of the account set *after* the block — and
the block's `BaseFee` (§9). A `Header` hashes *identically* to its block, so a
client holding only headers can verify proof-of-work, the hash chain, that a
transaction is included (via `MerkleRoot`), and that an account holds a given
balance (via `StateRoot`) — all without block bodies. This is what makes SPV
(§14) possible.

**Two of those fields are SCHEDULED, and that is how genesis survived them.**
`FilterRoot` and `HistoryRoot` enter the proof-of-work preimage only from the
activation height of `UpgradeFilterCommit` and `UpgradeHistoryRoot`
respectively — `appendPreimagePrefix` asks `IsUpgradeActive(name, h.Index)`,
and a header's own `Index` is the height the rule is asked about, which is what
lets the preimage be height-aware with no chain in hand. Below the activation
height the bytes hashed are exactly what they always were, so **genesis and
every block under it keep their hashes**. Every previous header change here
moved genesis; this one did not. A header carrying either field before its
upgrade activates is rejected, because a field nobody validates is a field
anyone may stuff, and it would change the hash the moment the rule turned on.

The two commitments are what make non-inclusion and ancestry trustless rather
than a matter of trusting whoever answered; see §14.

`core/merkle.go` builds the shared merkle fold (`MerkleProof`, `VerifyMerkleProof`);
an odd level duplicates its last node (Bitcoin's rule), and proof and root use the
same rule so they agree. `core/state.go` reuses that fold over the sorted account
set to produce the `StateRoot` and account-membership proofs.

**Transaction index.** Finding *which* block holds a txid used to mean scanning
every block body on every lookup, which put an O(chain × txs) walk behind
`/proof/{txhash}` and the explorer. [`core/txindex.go`](core/txindex.go) keeps a
`txid → (height, position)` map maintained as blocks connect, so a lookup is one
map hit. It is derived state — `buildTxIndex` rebuilds it from the chain, and
replaying a store on open rebuilds it for free — and it follows reorgs: blocks are
unindexed from the tip downwards before the winning suffix is indexed upwards.

One wrinkle it has to absorb: a coinbase commits only (recipient, amount), so two
blocks paying the same miner the same subsidy carry a byte-identical coinbase and
therefore **the same txid**. The index keeps the *first* (lowest) occurrence,
which is exactly what the linear scan it replaces returned, and only deletes an
entry when the block being disconnected is the one it points at. Since blocks are
only ever disconnected from the top, the surviving first occurrence is never
dropped by mistake. The underlying duplication is a real wart — Bitcoin removed it
by putting the height in the coinbase (BIP34) — and is listed in §21.

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
(like checkpoints) via `-upgrades name:height` (or the `upgrades` key in the JSON
config), which refuses an unknown name outright — a misspelled upgrade that
silently never activates means being forked off a network where the others did.
Two exist: `UpgradeMultiOutput` gates multi-recipient transfers (§5), and
`UpgradeDustLimit` is a worked example that rejects coin transfers below
`DustThreshold`. This is *height* activation; miner version-bit *signaling* (BIP9)
would additionally need a header version field and is left as future work.

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

**Transaction resurrection.** A reorg un-confirms every transaction in the branch
it discards, but those transactions are not *invalid* — they are validly signed
transfers that happened to be mined on the losing side. `ReorgFrom`/`ReplaceChain`
therefore return the discarded blocks, and the node feeds their non-coinbase
transactions back into its mempool (`Node.resurrectTxs`) so they can be mined
again. Without this, a payment confirmed only in the orphaned branch would drop
out of the network entirely until its sender noticed and re-broadcast it.

Two things are deliberately *not* resurrected: coinbases (minted by a block that
no longer exists, so they can never be re-mined) and anything the winning branch
already confirms, which the transaction index answers in O(1). Resurrection runs
*before* `reconcileMempool`, so a transfer whose nonce the winning branch has
since spent — a double-spend that lost — is re-queued and then immediately dropped
rather than sitting in the pool forever as unmineable.

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

**Supply accounting.** Because those are the only two ways coin moves in or out of
existence, the total is auditable, and `core/supply.go` tracks the two sides
*independently* so they can be checked against each other rather than derived from
one another:

- **minted** is a pure function of height — `CumulativeSubsidy(height)` sums the
  subsidy of every block, walking halving epochs rather than blocks, so it is
  O(halvings) at any chain length. Nothing else mints.
- **burned** is accumulated from block bodies as they connect (`blockBurned` =
  `BaseFee × size` summed over a block's transactions) and un-accumulated when a
  reorg disconnects them, so it tracks the canonical chain exactly.
- **circulating** is read straight out of the account state.

`Blockchain.Supply()` reports all three plus `Consistent`, the identity
`minted − burned == circulating`, which must hold for every valid chain — a false
there means coin was created or destroyed outside the subsidy and the burn, i.e. an
accounting bug. It is exposed as `GET /supply` and `dnas supply`, and asserted
across mining, reorgs, reopening a store, and snapshot bootstrap in the tests.
(This is the *observable* half of the supply-conservation item in
[ROADMAP.md](ROADMAP.md) §1; enforcing the identity as a consensus rule per block
is still open.)

A fast-synced node is the one case that cannot sum the burn directly, because the
bodies below its snapshot are pruned. It derives the seed instead: everything
minted through the snapshot height that accounts no longer hold must have been
burned. That comes from the snapshot's account set, whose state root the node has
already verified against a proof-of-work-checked header, so it is exactly as
trustworthy as the balances themselves.

---

## 10. Mempool

`core/mempool.go` is a bounded pool of validated, unmined transactions.

- **Bounded with lowest-*rate* eviction.** Capped at `DefaultMempoolSize` (5000);
  once full, a new transaction is admitted only by out-bidding the queued
  transaction paying the least per byte (fee *rate*), which it evicts — so scarce
  block space, a per-byte resource, goes to the highest-paying bytes.
- **Bounded in BYTES as well as in count** (`DefaultMempoolBytes`, 32 MiB).
  A count limit is not a memory limit: 5000 transactions at the relay size limit
  is roughly half a gigabyte of resident state, all of it valid, all of it paying
  the floor, and therefore unevictable — about one block reward's worth of fees
  to hold. Whichever budget binds first evicts, and because one large transaction
  may have to displace several small ones the check is a loop, not a single
  eviction. Occupancy for the dynamic relay floor is `max(count%, bytes%)`, so a
  pool full by bytes stops quoting the floor fee.
- **Replace-by-fee.** A conflicting `(From, Nonce)` may be replaced only by a
  strictly higher fee.
- **Package relay and child-pays-for-parent** ([core/package.go](core/package.go)).
  Admission used to measure a sender against its CONFIRMED balance, so a
  recipient could not queue a spend of coin a queued transaction was paying
  them — even though `Select` simulates credits as it goes and would have put
  both in one block. The pool was refusing transactions the miner two lines
  over was ready to mine. Admission now counts the confirmed balance plus what
  the pool is already paying the sender.

  That reintroduces exactly the risk the old rule existed to prevent — a queue
  entry that can never be mined, holding a nonce and never paying its fee —
  because the parent might be replaced or evicted and take the child's
  mineability with it. So the dependency is BOUNDED: `MaxPackageAncestors`,
  `MaxPackageDescendants` and `MaxPackageBytes`. The descendant limit is the
  anti-pinning one: without it an attacker attaches a large cheap tree to a
  transaction and makes replacing it mean out-bidding the whole tree.

  The ancestor and descendant COUNTS are over other senders only. A sender's
  own nonce run is a chain of ancestors too, but it is already bounded by
  `MaxPerSender` and the sender controls every entry, so counting it would
  silently lower that limit from 64 to 25.

  Once the pool knows what depends on what, a parent's worth to a miner is no
  longer its own rate but the best rate of any package it heads, so a child's
  fee can unstick a parent the sender cannot bump — and the receiver, who is
  the one actually waiting, usually cannot bump it either. Pricing costs a pass
  over the pool per block template (~28ms → ~35ms on 2,000 transactions, after
  caching sizes).

  What this is NOT is package SUBMISSION: a parent and child still arrive
  separately and the parent must arrive first. Accepting an orphan child and
  holding it for a parent that may never come is a different feature with its
  own denial-of-service surface.
- **An unmineable transaction cannot hold a nonce hostage.** Replace-by-fee
  normally requires a strictly higher fee, with one exception: a queued
  transaction that the height rules would reject for the NEXT block (expired, not
  yet time-locked, an HTLC refund before its timeout, a vault hot-key spend
  before its unlock) has no claim on the slot it occupies, so a transaction that
  *is* mineable displaces it regardless of fee. Without that rule a thief holding
  a vault's hot key could park a spend no block will accept and force the cold
  key's rescue — same account, same nonce — to out-bid them to recover their own
  coin (§5.2). The exception is narrow on purpose: between two mineable
  transactions, the higher fee still wins.
- **Sponsors are held to their whole queue.** A fee payer's outstanding
  sponsorships are totalled per payer and checked against its spendable balance on
  every admission, and re-checked on every new block (§5.3), so a sponsor cannot
  promise the same coin to a hundred transactions.
- **Reconciliation with peers.** A node asks each peer, once, for the
  transactions it has pending (`MsgGetMempool`) — see §11. Everything it is told
  goes through this same admission path; nothing is trusted for having arrived in
  bulk.
- **Fee-rate distribution.** `Mempool.Stats` buckets the queue by fee *rate* and
  reports the min/median/max, served at `GET /mempool/stats`. A queue depth alone
  cannot tell a sender whether their fee will be picked up next block or sit
  behind a wall of higher bidders; the distribution can, and computing it needs
  the canonical size only the node has.
- **Admission requires a transaction that could plausibly be mined.** The pool
  holds, per sender, a **contiguous run of nonces starting at that sender's
  confirmed nonce**, whose **total cost the sender can afford** from its spendable
  balance, up to `MaxPerSender` entries. Both halves are load-bearing rather than
  tidiness: without them an address holding *nothing* can sign transactions at
  nonces the chain will never reach, and they are admitted, never selected, never
  expire and never pay a fee — filling the pool for free and pricing out every
  real payment. The rules need chain state, so `Mempool` is bound to an
  `AccountSource` (`node.New` does it); an unbound pool is permissive, which only
  affects tests exercising it in isolation.
- **Eviction never breaks a run.** Only the *last* entry in a sender's nonce run
  is an eligible victim, since dropping from the middle would strand every higher
  nonce behind a gap it can never cross — the exact state admission exists to
  prevent. Among those, the lowest fee per byte goes first. A sender cannot make
  room for its own next nonce by dropping its predecessor, so in that case the
  newcomer loses however much it pays.
- **`Reconcile` is the counterpart to admission.** A new block moves nonces and
  balances, so entries that were admissible on arrival may not be mineable any
  more. After every tip change the node keeps each sender's affordable contiguous
  run and drops the rest, so the pool cannot silently accumulate dead weight.
- **Consensus sanity before anything else.** `Add` runs `CheckTxSanity` (§5) first,
  so a transaction no block can contain is never queued — otherwise the miner would
  select it into every candidate and stop producing blocks. Only then come the
  cheap policy checks (`MaxRelayTxBytes`, the fee floor), and the Ed25519
  verification comes **last**: it is by far the most expensive step, and an
  unauthenticated peer must not be able to make a node pay for it on a transaction
  it would refuse anyway.
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
  transaction is still valid. It is deliberately separate from the consensus base
  fee (§9): that one is committed in the header, burned, and decides block
  *validity*; this one only decides what a node is willing to hold and relay.
- **Nonce-aware, rate-ordered, byte-bounded selection.** `Select` greedily builds
  a valid sequence for the next block: each transaction must have its sender's next
  nonce, cover its per-byte base fee, and be affordable *from spendable balance*
  (so immature coinbase is never spent); recipients are credited within the
  simulation so chained spends can share a block; the highest fee *rate* wins among
  ready candidates, and selection stops at `MaxBlockBytes` of total size, the
  `MaxBlockVerifyOps` verification budget (§5), and the transaction-count cap. It also re-applies `CheckTxSanity` and
  `checkTxAtHeight` for the height being built, so a rule that activates *after*
  admission (the dust limit, §8) cannot make the miner select a transaction its
  own consensus rules would then reject.
- **Selection is indexed, and deterministic.** The obvious way to write the above
  is to rescan the pool once per chosen transaction, and that is what it used to
  do — re-deriving every candidate's hash and canonical size on every pass, which
  is O(pool × block) sha256 work and made *building* a block cost far more than
  mining one should. Two observations fix it. Everything static — hash, size,
  verification cost, the base-fee floor, standalone validity — is fixed for the
  duration of one `Select`, so it is computed once up front. And only ONE
  transaction per sender can ever be ready, because readiness requires an exact
  nonce match and the pool holds at most one transaction per `(sender, nonce)`
  (§the `bySender` index); so each round examines one head per *sender*, with a
  cursor that advances past anything already confirmed, rather than the whole
  pool. On a 2000-transaction pool building a full block that is 5.7 s → 30 ms.
  A side effect worth having: fee-rate ties now break on the transaction hash
  rather than on Go's map iteration order, so two nodes holding the same mempool
  build the *same* block template instead of merely equally good ones.
- **Expiry pruning** drops transactions that can no longer be mined.
- **A doomed candidate is never hashed.** `buildBlockFor` computes the candidate's
  state root before mining; if a selected transaction is refused, the
  `core.TxRejection` names it, so the node drops it from the mempool and
  reassembles rather than mining a block the chain will reject. If no valid block
  can be built at all, the miner backs off instead of spinning a core on
  candidates that cannot connect.

---

## 11. Networking

**Outbound peer selection (`node/addrman.go`).** Inbound connections have had
eclipse caps for a while — a total, plus a per-network-group limit — but outbound
selection had none, and outbound is the half that matters: those are the peers a
node *chose*, and therefore the ones it trusts to tell it the truth about the
chain. Every gossiped address went into one flat set and the first `maxpeers` of
them pulled out of a Go map got a dial loop, so nine addresses in one /16 stood a
good chance of owning every slot.

The address manager is the bitcoind shape, minus the parts that only pay at
internet scale. Two tables: `new` for addresses merely heard about, `tried` for
ones that completed a handshake, with selection biased toward `tried` because an
address that worked once is evidence and an address someone mentioned is not.
Both tables are bounded and bucketed by network group, and — the control that
actually does the work — **live outbound connections are capped per group**
(`maxOutboundPerGroup`, 2 of 8 slots), so an attacker needs addresses in four
distinct ranges rather than eight anywhere. A dial loop holds its reservation for
its whole life, so a peer that keeps reconnecting keeps its group's share rather
than freeing it between attempts. The `tried` table persists (`addrs.json`),
because it is the node's own hard-won evidence about who is real and starting
cold means trusting gossip again on every restart.

Two honest limits. The bucketing is a **/16 prefix, not an ASN** — two ranges can
share an operator, so this raises the cost of an eclipse rather than settling it;
real ASN diversity needs routing data this project has no business shipping. And
loopback is exempt from the cap, or the demos and the containerized e2e suite
(every node on 127.0.0.1) could hold two peers between them.

**Bootstrapping (`node/dnsseed.go`).** Joining a network meant knowing somebody's
address out of band, which is not a network anyone can join. `-dnsseeds` resolves
A/AAAA records into the `new` table. A seed is the one thing a bootstrapping node
believes before it can verify anything, so: seeds are consulted only when the node
is genuinely short of addresses, their results get no special standing and are
subject to the same diversity cap as gossip, one seed's contribution is bounded,
and a dead seed does not block the others. A seed cannot forge a chain — every
peer still handshakes and every block is still validated — but a *single* seed can
eclipse, so configure more than one.

`node/` implements an authenticated, encrypted, identified peer-to-peer network.

**Secure transport (`secureConn`, `secureHandshake`) — permissionless by
default.** Every connection begins with an X25519 ECDH key exchange; traffic is
then AES-256-GCM encrypted and length-framed, with the **peer wire format**
([node/wire.go](node/wire.go)) over it: a format byte, a length, and a payload
that is either JSON or the binary encoding. Frames are self-describing so a
reader accepts both, which is what let the wire change without a flag day — a
connection carries a JSON handshake and binary blocks afterwards, and the
writer switches as soon as the peer's version says it can read them. Block,
header and transaction bytes come from `core` ([core/wirecodec.go](core/wirecodec.go)),
which is the store's encoding; a second binary format for a block would be a
second thing to keep in step with the first. Every length a peer announces is
checked against the bytes remaining before it is used to allocate.
Whether it is *authenticated* depends on the network key: with **no `-netkey`
(the default) the network is open/permissionless** — anyone may connect (the
handshake is anonymous but encrypted), which is what an actual cryptocurrency
requires. Supplying a `-netkey` authenticates a **private** network: a peer that
doesn't know it fails an HMAC check and is dropped (used for a devnet or regtest).
Either way, peers are still cryptographically identified afterwards by their
Ed25519 node identity (below). The handshake sends and receives concurrently so it
also works over `net.Pipe` (used in tests).

**Peer identity — and why it is NOT the wallet key.** The handshake yields a
deterministic session id; each peer then signs it with its **Ed25519 node
identity** (`MsgIdentity`), so peers are cryptographically identified, not merely
"knows the key".

That message carries the identity's PUBLIC key, and a DNAS address is
`hash(pubkey)` — so a node whose identity is its wallet key hands every peer the
address holding its coin, and they can watch its balance, its mining income and
every payment it makes. It also substantially undoes the Dandelion++ origin
privacy below: hiding which peer first relayed a transaction matters much less
when peers know which address each peer owns. The identity is therefore its own
key file ([node/identity.go](node/identity.go), `-nodekey`, default
`nodekey.json` beside the chain), holding no coin and needing no backup — losing
it costs a node its accumulated peer reputation and nothing else. An in-process
node with no identity given still falls back to the wallet, and says so at WARN.

**Self-connections.** Two connections cannot be told apart by address: a node
advertising `:3000` is reached as `localhost:3000`, and a string comparison sees
two different hosts — so a node would dial itself whenever a peer gossiped its
address back in a different spelling, burning an outbound slot, an inbound slot
and a goroutine on a loop into the same process, then gossiping the alias onward
so others dialed it twice. Identity settles it: a handshake that returns our own
public key is closed, and the address is recorded as a self-alias
(`peerbook.noteSelf`) so it is never dialed or gossiped again.

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

**Protocol version, network, & capabilities.** Right after the identity exchange
each side sends a `MsgVersion` carrying its `ProtocolVersion`, its **network name**
and a list of capability strings. A peer below `MinProtocolVersion` is dropped, so
the wire format can evolve; a peer on a *different network* is dropped too (§5.1)
— its genesis and its signatures are not ours, so the connection could only ever
fail to converge. A peer predating network names sends none, which reads as
mainnet. Capabilities (`dand` for Dandelion++, `mpool` for mempool
reconciliation) let optional features be negotiated per-connection without a
version bump.

**Mempool reconciliation.** Transactions are otherwise only ever *pushed* as they
arrive, so a node that was down when a payment was broadcast never learns of it
until someone rebroadcasts or it is mined — and a miner that just joined builds
emptier blocks than it should. Each peer is therefore asked once, via
`MsgGetMempool`, for what it has pending; the answer is a bounded batch
(`maxMempoolBatch`) that goes through ordinary admission on arrival.

The request waits until we are **caught up**, which is not a nicety: admission is
checked against confirmed state, so a node still downloading the chain would
reject every transaction it was told about — the sender's coin does not exist yet
as far as it knows. The sync loop issues it once the tip reaches the best height a
peer has announced (`reconcileMempoolWithPeers`).

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
| Ranged body sync   | `MsgGetBlocks` → `MsgBlocks` (several ranges in flight, tracked and timed out) |
| Deep-fork/bootstrap fallback | `MsgGetChain` → `MsgChain`       |
| Discovery          | `MsgGetPeers` → `MsgPeers`                 |

Fork resolution uses a **block locator** (`Locator` / `LocatorFork`): the peer
finds the last common block and only the suffix is transferred and applied via
`ReorgFrom`. Whole-chain exchange (`getchain`) remains only for pathologically
deep forks and initial bootstrap.

**Sync liveness ([node/sync.go](node/sync.go)).** Catch-up used to be purely
reactive — an announcement triggered headers, headers triggered bodies, bodies
were applied — with *nothing watching whether the answers ever came*. A peer that
keeps replying to pings (so the idle timeout never fires) but silently stops
serving bodies would stall a node's sync indefinitely, and a mining node in that
state keeps building on its stale tip, forking itself off the network. So the node
now tracks what it asked for, from whom and when:

- every ranged block request is recorded against its peer, and a `syncLoop` tick
  disconnects (and ban-scores) a peer that has not answered within
  `blockRequestTimeout`, freeing the slot for someone else;
- `bestHeight` records the highest height any peer has announced, so the loop can
  tell it is behind and restart the headers-first pipeline when nothing is
  outstanding — announcements are only hints, and nothing is trusted until the
  blocks themselves validate;
- with a request already in flight and a long way still to go, up to
  `maxSyncPeers` *other* peers are asked for the windows beyond it, so catch-up is
  not limited to one peer's upload speed.

**Orphan blocks ([node/orphan.go](node/orphan.go)).** A block whose parent has not
arrived yet is buffered rather than discarded, and connected the moment the parent
lands. This happens constantly — a gossip race announces N+1 while N is in flight,
and parallel ranges arrive out of order — and without the buffer each occurrence
costs a full headers-then-bodies round trip to fetch again. The pool is bounded
and only accepts blocks that are valid on their own terms (`SelfValid`: proof of
work, merkle root, leading coinbase), so a peer cannot fill a node's memory with
cheap junk.

**Bounded memory.** The gossip de-duplication sets, the orphan pool and the
validation cache are all bounded FIFOs, and the mempool is capped, so a
long-running node's memory doesn't grow without limit.

**The miner (`mineLoop`).** The miner runs whenever the node has a wallet; an
atomic flag (`SetMining`, `POST /mine`) gates whether it actually produces
blocks, so it can be toggled at runtime. Each round it builds a candidate
(`buildBlock`), then searches for a nonce. Two counters make the search
interruptible: `tipGen` (bumped whenever the tip changes) aborts the hash loop so
work is rebuilt on the new tip instead of racing a block that already won, and
`txGen` (bumped when a transaction enters the mempool) wakes an idle miner so a
payment isn't left waiting for the next interval.

A block with **no transactions in it** is deliberately throttled: the miner waits
`Config.EmptyBlockInterval` (default one `TargetBlockTime`) before minting one, so
an idle network isn't flooded with empty blocks. This is *local mining policy*,
not consensus — peers needn't agree on it — which is why it is a knob: a devnet
or a test lowers it so proof of work is the only thing pacing block production.
(Regtest goes further and mines on demand via `Node.Generate`, bypassing both the
interval and the toggle.)

**Node lifecycle.** `Start` launches the accept loop, a dial loop per known peer,
the sync loop, and the miner. `Shutdown` closes a `quit` channel that every one of those loops
selects on — including the nonce search's abort check — and closes the listener to
unblock `Accept`, so a stopped node leaves nothing running behind it. It is
idempotent (`sync.Once`), which matters because the daemon shuts down explicitly
on a signal while embedders and tests also register it as cleanup. Without it a
"stopped" node went on redialing its peers every `dialRetryInterval` forever —
wasted work in production, and in tests a finished node that could wander into the
next one once the OS recycled its port.

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

**Compaction, and why it needs a snapshot.** `-prune` drops block bodies from
memory; for a long time the file kept every original record, so a pruning node
bounded its RAM and nothing else. Compaction rewrites the log to mirror the
pruned chain — amortized over `storeCompactInterval` blocks, because it is a
whole-file rewrite, and atomic via temp-file + rename.

The subtlety is what a pruned store can no longer do. Dropping a body drops the
transactions that produced the balances, so the header chain alone cannot rebuild
state: a store that shrank and then failed to load would be strictly worse than
one that never shrank. So compaction writes a **state snapshot** beside the log
(`chain.db.state`) *before* shrinking it — that order matters, because a crash
between the two then leaves a complete store and a harmless extra file, where the
reverse leaves a pruned store with nothing to replay from. `Open` bootstraps from
that snapshot exactly as a fast-synced node does, and verifies it the same way:
the accounts must hash to the state root committed in a proof-of-work-covered
header, so a stale or tampered snapshot is rejected rather than silently becoming
this node's idea of everyone's balances.

Pruned heights keep a header-only record rather than vanishing, because linkage,
median-time-past and the difficulty retarget all read those headers. The saving
is therefore the transaction bodies: large on a busy chain, near zero on a devnet
of empty blocks. And a pruned store is no longer fully re-verifiable — `dnas db
verify` replays what bodies remain and reports which heights it could not check
instead of claiming a clean bill.

**The poisoned store.** Those two writes are not equally recoverable. `AddBlock`
appends one record, and if it fails the block is simply undone in memory — disk
and RAM still agree. A reorg cannot do that: it truncates to the fork point
*before* appending the winning suffix, so the moment the truncate lands the log no
longer holds the losing branch, and a failure partway through the appends leaves
disk holding a prefix of a chain the node is not running. There is nothing left to
roll back to.

So instead of returning an error and carrying on — which would let the next
`AddBlock` stack the old chain's continuation on top of the new suffix, producing
a log that silently will not replay on the *next* restart, possibly days later —
the store is marked **poisoned** and refuses every subsequent write, naming the
original cause and `dnas db verify`. The node stays up and readable; it just stops
pretending its disk is authoritative. This bounds the damage rather than
preventing it: making the sequence genuinely atomic (a side region and a single
switch) is the open half, and is in the ROADMAP.

**Pruning.** A `Blockchain` keeps its blocks in a slice, so a long chain costs
its whole size in RAM. `-prune N` (`core/prune.go`) keeps the state and every
header for all time and drops the bodies deeper than N, replacing them with the
same header-only placeholders a fast-synced node already uses below its snapshot
(`blockFromHeader`) — which is why nothing in validation, supply accounting or
maturity had to change.

Three things make that safe rather than merely smaller:

- **The floor.** `MinPruneKeep = MaxReorgDepth + 32`. A reorg replays the bodies
  it disconnects, so pruning inside the range a reorg can reach would leave a
  node unable to follow the chain it must follow. A smaller `-prune` is raised to
  the floor and logged, rather than accepted and failing later.
- **No empty filters.** A compact filter built from a missing body is a valid
  EMPTY filter, and an empty filter is a proof of *absence*. Serving one would
  tell a light client its address is provably not in a block the node cannot even
  read, so `BlockFilterAt` reports a body-less height as having no filter and the
  API answers `410 Gone` — never `404`, which a client would read as "not in the
  chain".
- **A cached filter-header chain.** The chain is a running hash over bodies, so a
  pruning node that re-folded over what it still has would produce values that
  agree with nobody. It is therefore maintained incrementally as blocks connect
  and truncated on reorg (`extendFilterHeadersLocked`), at 32 bytes a block — the
  BIP157 bargain: keep the commitments, drop the data. A node that followed the
  chain from genesis keeps serving all of it after pruning; a fast-synced node
  never saw those bodies and publishes `filter_base` above them.

The transaction and address indexes drop their entries for a pruned body (a
location holding nothing is worse than a miss); the **asset registry** does not,
because an asset issued at height 5 still exists and is still held — what is lost
is the transaction that issued it, not the fact of it.

Pruning is a memory bound, not a disk one: the append-only store still holds
every block and a restart replays it (see [ROADMAP](ROADMAP.md) §1).

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
coins. (Checksums are enforced **client-side** — in `/send` and the console — not in
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

**Message signing.** `wallet/message.go` signs arbitrary data under a *domain
tag*: the preimage is `sha256("DNAS signed message v1\n" ‖ len(msg) ‖ msg)`. The
tag is not decoration. A wallet that signs whatever bytes it is handed can be
asked to "prove you own this address" with the serialization of a transaction,
and the answer is a valid transfer — so a message preimage is a tagged hash while
a transaction preimage is a codec-versioned encoding, and neither can ever be
presented as the other. The length is committed too, so two messages cannot share
a preimage by shifting bytes across a field boundary. `VerifyMessage` returns the
address a signature *proves*, which a caller must compare against the one it
expected: a verifier that skips the comparison accepts any valid signature from
anybody.

**Passphrase rotation.** Encryption at rest used to be write-once —
`DNAS_WALLET_PASSPHRASE` decided how a file was created and nothing could change
it. `dnas wallet passphrase` re-encrypts in place (and `-remove` decrypts), and
because it rewrites the only copy of a key it reopens the file and compares the
address before reporting success. An encrypted file opened without a passphrase
now says so instead of failing on a seed-length check.

**Encrypted blobs.** `wallet/blob.go` is the same KDF and cipher for something
that is not a single key — a backup bundle of key files, identities and watch
lists (`dnas backup`). It writes 0600 through a temp file and a rename, because an
interrupted write must not leave a truncated backup where the previous good one
was, and it refuses an empty passphrase rather than writing key material in the
clear. GCM's authentication is what makes a modified bundle unrestorable rather
than silently wrong; a nonce of the wrong length is rejected instead of panicking
(GCM panics rather than erroring, and these files are untrusted input).

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
addresses it touches. Items are placed with SipHash-2-4 and delta-encoded with
Golomb-Rice coding (P=19, M=784931). Testing the filter for an address gives no
false negatives: a non-match is a **proof of non-inclusion** for that block; a
match is "probably present, download to confirm" (≈1/M false positives). The API
serves `/cfilter/{index}`, `/cfilters`, and a BIP157-style filter-header chain at
`/cfheaders`; `dnas spv scan <addr>` PoW-verifies the headers, checks each filter
is bound to its header and consistent with the filter-header chain, then reports
matches and how many blocks the address is provably absent from.

**The filter key, and why it had to move.** Below `UpgradeFilterCommit` the
SipHash key is the block's own hash, as BIP158 does it, and the filter's
correctness rests on the honest-node / multi-peer assumption — a client
cross-checks peers or falls back to downloading the block.

From the activation height the header commits the block's filter header in
`FilterRoot`, so a forged filter does not fold to the committed value and
non-inclusion is backed by the chain's work instead of by the honesty of
whoever answered. The two halves of that change are inseparable: a filter keyed
by the hash of the block it describes cannot have its commitment placed inside
that same hash. So above the activation height the key is derived from
`PrevHash` and `MerkleRoot` instead — both fixed before the nonce search
begins, which is what makes the filter computable before the header exists.
Both inputs are needed: a merkle root alone repeats across two blocks with
identical transaction sets, and a prev-hash alone repeats across siblings at
one height.

The committed chain starts AT the activation height, because the parent of the
first committed block carries no root to extend. A client verifies filters
against the commitment from there forward and falls back to the uncommitted
filter-header chain below it. `core.VerifyCommittedFilter` is the check.

**Ancestry proofs.** A header chain proves ancestry only by holding all of it:
knowing block 10 is an ancestor of block 1,000,000 means having the 999,990
headers in between. From `UpgradeHistoryRoot` the header commits a **Merkle
Mountain Range** over every block hash below it ([core/mmr.go](core/mmr.go)),
so the same fact is a path through an accumulator — logarithmic in the chain's
length, independent of the distance. `GET /ancestry?height=N&as_of=M` serves
one and `core.VerifyAncestryProof` checks it against a root the client takes
from a header whose proof of work it verified. The verifier recomputes
everything the proof asserts and ignores the proof's own `Root` field, because
a prover that could choose it could prove anything.

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

**Memo and height window.** `send`/`sendmany` take `-memo`, `-expiry`/
`-expire-in` and `-lock-until`/`-lock-for`. All three are signed consensus fields
that had carried zero from every client since they existed. An expiry is how a
payment stops being an open-ended liability: without one a transaction signed
today can be mined next month at a nonce that has not moved, and the only way to
retract it is to spend that nonce on something else. The relative forms are what
a person means ("expire in twenty blocks") and are resolved against the tip
*before* signing, because a signature has to commit to a specific window. What
can be checked offline is checked before signing — an over-long memo, an inverted
window — because afterwards the wallet has already advanced its own next-nonce
and the node's answer is a bare rejection. Consensus also now rejects an inverted
window outright: below `LockUntil` a transaction is not yet valid and above
`Expiry` it is too late, so if the two cross there is no height in between, and
such a transaction could never have been mined at any height anyway.

**A node that cannot show you everything.** A pruning or fast-synced node serves
filters only for the bodies it holds. `verifiedFilters` therefore asks `/info`
where the node's data starts and scans from there, and the reports name the range
they covered ("provably absent from heights 8..140") plus what they could not
see. "Absent from every block I was given" is not "absent from the chain", and
saying the latter is the one way a non-inclusion claim could mislead.

**The verified-header cache.** A light client that re-downloads the header chain
on every command is not light, and that is exactly what `dnas spv` did: every
command PoW-verifies the chain before trusting anything, and every command
fetched all of it. Headers are self-authenticating — each commits to its
predecessor's hash and to its own proof of work — so the verified prefix can be
KEPT ([cmd/dnas/headercache.go](cmd/dnas/headercache.go)) and only the suffix
downloaded. Trust is unchanged: a fetched batch must link onto the cached tip and
satisfy `ValidateHeaderChain`, a cache from another network or format version is
discarded, and a node serving a different hash at a height we already hold is
treated as a reorg — the cache is dropped and rebuilt, because a client cannot
tell a reorg from a lie and the honest answer to both is to verify again. The
filter-header chain is cached alongside for a stronger reason: it is a running
hash, so a client with a verified prefix folds new filters onto it
(`core.FoldFilterHeaders`) instead of re-folding from genesis, and a wallet that
has scanned to height H fetches filters for H+1.. only.

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

- **Read:** `/info` (network, height, tip, work, mempool, `min_relay_fee`,
  `base_fee`, peers, mining, and whether the address index and faucet are
  available), `/chain`, `/balance/{addr}`, `/account/{addr}`, `/mempool`,
  `/mempool/stats` (the pending queue's fee-rate distribution, §10),
  `/tx/{txhash}` (one transaction by id, answering for both stages of its life —
  `confirmed` with block and confirmation count via the transaction index, or
  `pending` from the mempool — so a wallet polls one endpoint from submission to
  confirmation instead of guessing which to ask), `/supply` (minted, burned,
  circulating and the conservation check, §9), `/peers`, `/address`,
  `/estimatefee?blocks=N` (recommended fee = base fee + estimated tip),
  `/metrics` (Prometheus text — 55 series: chain, mempool count *and bytes*, peers
  and their ban scores, reorg totals and depth, orphan count, hashrate and block
  intervals, supply, tip age, blocks-behind, shares, and webhook delivery. Most of
  those numbers existed already but only as JSON on five different endpoints,
  which is the wrong shape for the one consumer that wants them continuously),
  and `/shares` (the share ledger, below).
- **Paged bulk reads.** `/chain`, `/headers`, `/cfilters` and `/cfheaders` take
  `?from=HEIGHT&limit=N` and answer at most `defaultPageLimit` (2000) entries.
  They used to serialize the WHOLE chain into one response, which is a
  memory-amplification attack anyone can run with curl, and which made the light
  client the heaviest participant on the network (`dnas spv` re-fetched every
  header on every command — tens of megabytes per invocation on a long chain).
  The response stays a plain JSON array, so a client pages with `from` and reads
  the total from `/info`. Since the chain is cached rather than re-folded (§12),
  serving filter headers is now O(range) rather than O(chain).

  `?last=N` was the missing half. Paging silently changed what an
  *unparameterized* request means: every client that draws a chain view wants the
  TAIL, and each of them was fetching the whole chain and slicing. Once `/chain`
  was paged, "no parameters" started meaning "the OLDEST page", so the explorer,
  the TUI and the GUI would all have sat frozen at genesis on a long chain.
  Asking for the newest N has to be one request that does not depend on knowing
  the height first; `from` and `last` together are refused rather than one being
  quietly ignored.
- **Peers, bans, and control:** `GET /peers` reports every live connection in
  full — advertised address, remote IP, authenticated identity, negotiated
  version, capabilities, direction, uptime, ban score, and whether a block
  request is outstanding to it. All of that was already known per connection and
  discarded in favour of a list of address strings, which cannot tell you which
  peer is misbehaving or stalling. `GET /bans` lists scored keys *including those
  below the threshold* (seeing a peer at 80 of 100 is most of the value of
  scoring), `POST /unban` clears one — previously impossible without stopping the
  node and editing `bans.json` — and `POST /addpeer` / `POST /droppeer` manage
  connections at runtime.
- **Chain analytics:** `GET /chainstats?window=N` ([core/chainstats.go](core/chainstats.go))
  turns the header numbers into what they imply: estimated network hashrate
  (window work ÷ elapsed time), the block-interval distribution against
  `TargetBlockTime`, the difficulty range, fee/burn/tip totals with the fullest
  block as a percentage of the limit, and the coinbase recipients of the window —
  the closest thing to a hashpower distribution a chain this size has. Reporting
  only; a node computing it differently is not on a different chain.
- **Reorg history:** `GET /reorgs` ([node/reorghist.go](node/reorghist.go)) is a
  bounded ring of the chain switches this node has lived through — depth, fork
  height, both tips, and how many of the orphaned branch's payments were
  re-queued — plus lifetime counters and the current orphan-pool depth. The SSE
  stream announces a reorg to whoever is listening at that instant and then
  forgets it; this is the record you want afterwards.
- **Readiness:** `GET /health` answers 200 only when the node is actually usable
  and **503 with the reasons** when it is not (no peers, behind the best known
  height, at genesis, or sitting on a stale tip). `/info` cannot serve this
  purpose: it answers 200 while syncing, un-peered, or on a tip that stopped
  moving hours ago. `dnas health` exits non-zero to match, so it works as a
  supervisor or CI check.
- **Address history:** `GET /address/{addr}/history?from=&limit=` lists the
  transactions that touched an address, oldest first, from the optional address
  index (`-addrindex`, [core/addrindex.go](core/addrindex.go)). Without the index
  the endpoint answers **503** rather than "no history", because a node that
  simply is not indexing must not be mistaken for an address that has done
  nothing. The index is opt-in because it is the one index whose size is not
  bounded by the chain — an address appearing in a million transactions has a
  million entries — and it is maintained across reorgs by the same
  unindex-from-the-tip-down discipline as the transaction index.
- **SPV / filters / state:** `/headers`, `/header/{index}`, `/block/{index}`,
  `/proof/{txhash}`, `/cfilters`, `/cfilter/{index}`, `/cfheaders`,
  `/stateproof/{addr}` (a balance proof against the header state root), and
  `/snapshot/{height}` (the full account state at a height, for fast-sync).
- **Assets:** `GET /assets` (with `?ticker=X`) and `GET /asset/{id}` describe
  what the chain has issued (`core/assetindex.go`). An asset id is
  `hash(issuer, ticker, nonce)` and cannot be unpacked, so an account holding one
  showed `tok3f2a…: 500` and nothing else — not the ticker, not the issuer, not
  whether 500 is most of the supply. The registry is built as blocks connect,
  rolled back with them, and always on: it is bounded by the number of issuances,
  not of transfers. `?ticker=` returns a LIST on purpose — anyone may issue
  "GOLD", so the id is the identifier and collapsing a ticker to one asset would
  be choosing an issuer for the caller. `/asset/{id}` serves the held total next
  to the issued supply, because an asset's total is conserved and a client can
  then check that rather than trust the figure.
- **Webhooks:** `-webhook URL` POSTs every event to a service that wants to be
  *called* rather than hold an SSE connection open — a shop backend, a bot, a
  cron job ([node/webhook.go](node/webhook.go)). Delivery runs off a bounded
  queue on its own goroutine and never blocks block processing: a receiver far
  enough behind to fill the queue loses events, which it had lost either way, and
  stalling a node to wait for somebody's web server would be the wrong trade in
  every direction. A 5xx or a transport error is retried a few times with a
  growing delay; a 4xx is not, because a retry cannot fix a request the receiver
  says is wrong. Every delivery carries the network and the node's height, so a
  receiver can tell a regtest event from a mainnet one and can spot a gap.
  `GET /webhooks` reports sent/failed/dropped/queued — a silently failing webhook
  is otherwise invisible from outside, since "nothing" is also what a quiet chain
  looks like.
- **Bounded request bodies.** Every write endpoint decodes through `decodeBody`,
  which wraps the body in an `http.MaxBytesReader` and answers **413** *before*
  parsing: 64 KiB for the small control payloads (an address, a peer, a bool),
  4× `MaxRelayTxBytes` for a transaction and 4× `MaxBlockBytes` for a block —
  the multiplier because JSON with hex-encoded signatures runs several times
  larger than the canonical encoding those constants bound. The read side was
  paged long before this and the P2P side caps a frame, but a POST body used to be
  read with no bound at all: the only thing between a request and an arbitrarily
  large allocation was the size check that ran *after* the whole body had been
  decoded into memory. The cap sits on the reader rather than on
  `Content-Length`, so a request that understates its length is still stopped
  mid-stream. Endpoints gated on node configuration (`/generate`, `/faucet`)
  refuse with **403** before reading anything at all.
- **Rate limiting:** a token bucket per client IP in front of everything
  ([api/ratelimit.go](api/ratelimit.go)), `-apirate`/`-apiburst`, answering
  **429** with `Retry-After`. The peer protocol has had a per-peer bucket from
  early on and the HTTP API had nothing, while being the cheaper target of the
  two: no handshake, no protocol, just a URL — and the expensive endpoints are
  plain GETs (`/chain?limit=2000` serializes two thousand blocks, `/snapshot`
  walks the whole account state, `/stateproof` builds a proof per call). It keys
  on the IP rather than host:port (a client uses a new source port per request)
  and deliberately ignores `X-Forwarded-For`: trusting a client-set header would
  let anyone claim a new identity per request, turning the limiter into a memory
  allocator for the attacker. `/events` is exempt — it is one long-lived request
  that then sends many messages, and dropping a live feed because of an unrelated
  burst of reads would be the wrong answer.
- **What this node can serve:** `/info` publishes `body_height`, `filter_base`
  and the pruning counters, and `/block/{i}`, `/cfilter/{i}` and `/cfheaders`
  answer **410 Gone** rather than 404 for data this node has pruned (§12). The
  distinction is the whole point: a client told "not found" concludes the chain is
  shorter than it is, when the right move is to ask another node.
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
  HTLC, a vault spend, a sponsored transfer, an asset transfer or an issuance),
  `POST /mine` (`{on}` toggles mining at runtime), `POST /generate` (`{n}`,
  regtest only: mine N blocks on demand), `POST /submitblock` (accept a block
  mined by an external miner), `POST /submitshare` (below), and `POST /faucet`
  (`{address}`, testnet/regtest only).
- **Mining:** `GET /blocktemplate?address=ADDR` returns a candidate block (every
  field filled but the winning nonce) so an external miner (`dnas miner`) can hash
  it off-node and submit the result to `/submitblock` — mining is fully decoupled
  from the node. Two things make that efficient rather than merely possible
  ([node/shares.go](node/shares.go)):
  - **Long poll.** `?longpoll=1&prev=HASH` holds the request until the tip moves
    off `HASH` (or `&timeout=SECONDS` elapses), so a miner starts on a fresh
    candidate the moment the old one dies instead of hashing a dead template until
    its next poll. A `prev` the node has already passed answers immediately, so a
    miner can never be parked waiting for a change it has missed.
  - **Shares.** The template also carries `share_bits`, a deliberately easier
    target. A hash that clears it proves work was done without being a block, which
    is what lets a pool pay for hashpower that has not found one; `POST /submitshare`
    accepts them and `GET /shares` reports the ledger. A share that also clears the
    real target is submitted as the block it is, so a miner never has to tell the
    two apart. None of this is consensus — no share is stored in the chain, and a
    node that ignores them is on the same network.
- **Faucet.** `POST /faucet {"address":…}` pays out of the node's own wallet on a
  network whose parameters permit it — never mainnet, by definition rather than by
  policy (`core.NetworkParams.Faucet`), so no flag can turn a real chain into a
  free one. It is off unless the operator passes `-faucet`, and rate-limited by
  recipient *and* by the client's connection IP (deliberately not a forwarded
  header, which a client can set freely). It exists because otherwise joining a
  throwaway network means asking a stranger for coin.

  When `DNAS_API_TOKEN`
  is set these require an `Authorization: Bearer <token>` header
  (constant-time compared); read endpoints stay open, and an unset token leaves
  the whole API open (the localhost/toy default). The token is read from the
  environment, not a flag, so it doesn't leak into `ps`.
- **Stateless wallet helpers:** `POST /multisig/address`, `POST /htlc/address`,
  `POST /vault/address`, and `POST /wallet/hd` compute an address or derive HD addresses without
  touching node state or holding a secret. They exist so the thin clients (§16)
  share one crypto implementation instead of re-deriving it.

The web explorer (`api/explorer.html`, embedded via `//go:embed`) is served at
`/`: live status, expandable blocks, mempool, the asset registry, a panel for
what the node can serve (readiness, hashrate, reorgs, pruning), a send form, and
an in-browser SPV verifier.

One **search box** takes whatever identifier a person has and works out which it
is: digits are a height, a `dnas` prefix is an address, and a 64-character hash
is resolved by asking the node (transactions first, since that is what somebody
pasting a hash almost always has, then the recent blocks — a node has no index
from block hash to height, and the page says so rather than implying the search
covered the whole chain).

Its transaction line renders every form the ledger allows — multi-output, asset
transfer, issuance, multisig, HTLC, vault, sponsored, memo, height window —
because a page that shows a multi-recipient payment as a transfer of zero to
nobody, or drops a memo and a deadline that are signed parts of the transaction,
is showing something that did not happen. A memo is arbitrary data chosen by a
stranger, so it is escaped like everything else on the page.

The page reads `/health` through a helper that decodes the body whatever the
status: readiness answers **503** with the reasons, and treating that as a
transport error would drop the readiness line on exactly the nodes that are not
ready.

**Verification runs in WebAssembly, and there is no fallback.** The page's SPV
check calls into `core` itself, compiled for the browser
([cmd/wasmverify](cmd/wasmverify)) and served from `/wasm/`. It used to be
JavaScript, and that JavaScript was wrong: it hashed a preimage that had never
gained the header's `Version` field, so every "verification FAILED" the page
produced was the page's own bug, for months, because nothing compiles a
template literal (§21). The bundle is a build artifact — `make wasm` — and a
binary built without it answers 404, at which point the page says its verifier
is unavailable. That is deliberate: a JavaScript fallback is the bug class
being removed, not a safety net.

Before verifying anything the page installs the node's scheduled upgrades from
`/info`, because the scheduled header commitments are part of the
proof-of-work preimage from their activation height and a verifier working
from the wrong schedule computes the wrong hash. That is configuration
arriving from the server, not a trust decision: a node that reports the wrong
heights only makes its own headers fail to verify.

---

## 16. Clients

Both clients drive the same HTTP API and can launch a local node so mining works
out of the box.

- **TUI** (`tui/`, Go/bubbletea): live dashboard, send, SPV verify, mining
  toggle, a **fee-rate histogram** of the pending queue (from `/mempool/stats`), a
  **transaction watcher** (`w`) that follows one payment from submission to
  confirmation on the same refresh signal as everything else, and — via the
  stateless helpers — multisig address derivation (`x`) and HD wallet
  generate/restore (`h`).
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

**Self-custodial mode.** Both clients paid through `POST /send`, which asks the
*node* to sign with the *node's* wallet: fine for a private node you own, and
against a shared one it spends somebody else's coin. Given `-key`/`--key` they
sign locally instead — by running `dnas spv wallet -key … send`, not by
implementing the transaction encoding a second and third time.

**Verification goes the same way, and did not used to.** `SPV verify` in both
clients now runs `dnas spv verify` rather than recomputing the header hash
locally. The local versions were wrong — both of them, along with the
explorer's and the demo script's, all four having missed the `Version` field
BIP9 signaling added — so each reported failure for valid proofs (§21). The
asymmetry is the argument: a signer that is wrong produces a transaction a
node rejects and you find out, while a verifier that is wrong tells you a
payment is proven and you do not. What remains local in each client is
cosmetic only, labelled as such: the TUI and the GUI still decode an nBits
target to render a difficulty column, where a drift misdraws a number instead
of asserting that money arrived.

That delegation is the deliberate part. The canonical encoding is what
signatures cover; a hand-written copy of it in a UI is how a client comes to
produce signatures a node rejects, or to verify one incorrectly. This project has
already had exactly that bug, in the GUI's SPV header format, which went unnoticed
because nothing exercised it. The cost is a process launch per payment and a
dependency on the binary; a wallet is not a hot path, and `-spawn` already assumes
the binary is there.

Two details that only show up in practice: the CLI writes its log lines to stderr
and its result to stdout, so the helpers keep the two streams apart (a merged
stream has no reliable last line); and a refusal the CLI handles itself — an
insufficient balance, a rejected transaction — is printed with a *zero* exit
status, so the outcome is read from the output rather than inferred from the exit
code.

---

## 17. Consensus parameters

Mostly in [`core/params.go`](core/params.go); the rest live next to the code they
govern — the proof-of-work targets and `lwmaWindow` in
[core/target.go](core/target.go), `MaxTickerLen`/`MaxAssetSupply` in
[core/asset.go](core/asset.go), `MaxPerSender` in [core/mempool.go](core/mempool.go),
`DefaultShareFactor` in [core/share.go](core/share.go), `MinPruneKeep` in
[core/prune.go](core/prune.go), the address-manager and DNS-seed bounds in
[node/addrman.go](node/addrman.go) and [node/dnsseed.go](node/dnsseed.go),
`MaxMultisigKeys` in
[wallet/wallet.go](wallet/wallet.go), the script bounds in
[core/script.go](core/script.go), the name bounds in
[core/name.go](core/name.go), the package bounds in
[core/package.go](core/package.go), `ProtocolVersion` in
[node/protocol.go](node/protocol.go), the rate-limit defaults in
[api/ratelimit.go](api/ratelimit.go), and the network ids in
[core/network.go](core/network.go). Every node must agree on the consensus ones.

| Parameter             | Value            | Meaning                                   |
|-----------------------|------------------|-------------------------------------------|
| `Coin`                | 100 000 000      | base units per DNAS                       |
| `InitialBlockReward`  | 50 · Coin        | first-epoch coinbase subsidy              |
| `HalvingInterval`     | 210 000          | blocks between reward halvings            |
| `GenesisBits`         | ~2^240 target    | compact PoW target at genesis (nBits)     |
| `PowLimit`            | ~2^244 target    | easiest target (difficulty floor); no hard ceiling — difficulty is unbounded |
| `TargetBlockTime`     | 60 s             | desired spacing (LWMA retarget target). Was 5 s; see `FinalityWindow` |
| `FinalityWindow`       | `MaxReorgDepth` × `TargetBlockTime` = 1 h 40 m | wall-clock divergence the chain can heal from. At the old 5 s it was ~8 min, so any longer partition split the network permanently |
| `MaxTimeOffset`        | 70 min           | bound on how far peers may move this node's clock for timestamp validation ([core/nettime.go](core/nettime.go)) |
| `lwmaWindow`          | 20               | blocks the LWMA retarget averages over    |
| `ProtocolVersion`     | 2                | P2P wire version (peers below `MinProtocolVersion` = 1 are dropped) |
| network id            | "" / `dnas-testnet` / `dnas-regtest` | bound into genesis, the signing preimage and the handshake (§5.1) |
| `CoinbaseMaturity`    | 3                | blocks before a reward is spendable       |
| `MaxReorgDepth`       | 100              | deepest reorg allowed (finality guard). Raising it drags `MinPruneKeep` with it, which is why the finality window was widened via the block time instead |
| `MaxBlockTxs`         | 1000             | non-coinbase txs per block                |
| `MaxBlockBytes`       | 1 000 000        | total non-coinbase tx bytes per block     |
| `MaxBlockVerifyOps`   | 8000             | worst-case signature verifications per block |
| `MaxTxOutputs`        | 64               | recipients in a multi-recipient transfer  |
| `MaxMemoBytes`        | 256              | per-tx memo cap                           |
| `MaxAddressBytes`     | 90               | per-tx From/To length cap (bounds state-key bloat) |
| `MaxCoinbaseBytes`    | 1024             | serialized coinbase cap (it pays no per-byte fee) |
| `MaxMultisigKeys`     | 16               | N in an M-of-N script (bounds verification cost) |
| `MaxTickerLen`        | 8                | native-asset ticker length cap            |
| `MaxAssetSupply`      | 2^62             | native-asset supply cap (overflow-safe)   |
| `DustThreshold`       | 1000             | min coin transfer once UpgradeDustLimit is active |
| `MaxFutureDrift`      | 120 s            | how far ahead a timestamp may be          |
| `DefaultMinRelayFee`  | 10 /byte         | base of the dynamic fee floor (policy, per byte) |
| `MaxRelayTxBytes`     | 100 000          | largest tx a node will queue/gossip (policy, not consensus) |
| `MaxPerSender`        | 64               | queued txs one address may hold (policy)  |
| `DefaultMempoolSize`  | 5000             | pending txs kept before eviction (policy, `-mempool`) |
| `DefaultMempoolBytes` | 32 MiB           | the pool's **byte** bound; the count alone does not bound memory (policy, §10) |
| `MinPruneKeep`        | `MaxReorgDepth` + 32 = 132 | floor on `-prune`: a node may not drop a body a legal reorg could still need |
| `InitialBaseFee`      | 10 /byte         | EIP-1559 base fee at genesis (consensus, per byte) |
| `MinBaseFee`          | 1 /byte          | base-fee floor (per byte)                 |
| `BaseFeeTargetTxs`    | MaxBlockTxs / 2  | per-block tx count the base fee targets    |
| `BaseFeeMaxChangeDenominator` | 8        | max base-fee change per block (1/8 = 12.5%) |
| `GenesisTimestamp`    | 1735689600       | fixed genesis time (2025-01-01Z)          |
| `DefaultShareFactor`  | 256              | how many times easier a mining share is than a block (pool accounting, not consensus) |
| `DefaultAddressHistoryLimit` / `MaxAddressHistoryLimit` | 100 / 1000 | paging bounds for the optional address index (policy) |
| `defaultPageLimit` / `maxPageLimit` | 2000 | entries one bulk read returns (policy, [api/api.go](api/api.go)) |
| `DefaultAPIRate` / `DefaultAPIBurst` | 20 /s / 60 | per-client HTTP token bucket (policy, `-apirate`/`-apiburst`, [api/ratelimit.go](api/ratelimit.go)) |
| `maxControlBody` / `maxTxBody` / `maxBlockBody` | 64 KiB / 4× `MaxRelayTxBytes` / 4× `MaxBlockBytes` | HTTP request-body ceilings by endpoint class (policy, 413, [api/api.go](api/api.go)) |
| `DefaultStatsWindow`  | 144              | blocks `/chainstats` covers by default (reporting) |
| `reorgHistoryCapacity` | 64              | reorgs kept in the in-memory ring ([node/reorghist.go](node/reorghist.go)) |
| `banThreshold`        | 100              | ban score at which a peer is cut off ([node/ban.go](node/ban.go)) |
| `maxOutboundPerGroup` | 2                | live outbound peers allowed per network group — the outbound eclipse control ([node/addrman.go](node/addrman.go)) |
| `maxNewEntries` / `maxTriedEntries` | 4096 / 1024 | address-table bounds; gossip is attacker-controlled, so both evict |
| `maxEntriesPerGroup`  | 64               | addresses one network group may occupy in the tables |
| `maxDialFailures` / `maxTriedDialFailures` | 5 / 12 | consecutive failures that retire an address (a `tried` one is kept longer — it worked once) |
| `peerRefillInterval`  | 20 s             | how often the outbound set is topped back up |
| `dnsSeedThreshold`    | 8                | known addresses below which DNS seeds are consulted ([node/dnsseed.go](node/dnsseed.go)) |
| `maxAddrsPerSeed`     | 32               | addresses one DNS seed may contribute per round |
| `dnsSeedTimeout`      | 10 s             | bound on one round of seed resolution |
| `MaxScriptBytes` / `MaxScriptOps` / `MaxScriptStack` | 1024 B / 256 / 64 | script VM bounds, checked before execution rather than during ([core/script.go](core/script.go)) |
| `MaxScriptItemBytes` / `MaxScriptWitness` | 520 B / 32 | one stack item, and how many witness items a spender may supply |
| `MaxScriptSigChecks`  | 20               | signature verifications one script may perform; `VerifyOps` charges a script this many twice, its worst case |
| `MinNameLen` / `MaxNameLen` | 3 / 32     | name length bounds ([core/name.go](core/name.go)) |
| `NameLeasePeriod`     | 100 000          | blocks one registration or renewal lasts |
| `MaxNameLeases`       | 10               | lease periods buyable in one operation |
| `MetadataHashLen`     | 64               | hex length of a non-fungible asset's sha256 commitment ([core/asset.go](core/asset.go)) |
| `MaxPackageAncestors` / `MaxPackageDescendants` | 25 / 25 | unconfirmed dependants, **over other senders only** (policy, [core/package.go](core/package.go)) |
| `MaxPackageBytes`     | 101 000          | total size of one dependency package (policy) |
| `DefaultTrieCacheNodes` | 250 000        | retained account-trie nodes; a bounded cache, so a miss costs a rebuild and never a wrong root (policy, [core/statetrie.go](core/statetrie.go)) |
| `wireMinBinaryVersion` | 2               | peer protocol version from which binary frames are sent ([node/wire.go](node/wire.go)) |

---

## 18. Key decisions and trade-offs

| Decision | Why | Trade-off |
|----------|-----|-----------|
| Unbounded PoW difficulty (LWMA, no hard cap) + `NoRetarget` for devnet | Real economic security — rewriting history costs ever-growing work; a devnet still gets instant blocks | Genesis starts easy; security only exists once real hashpower is present |
| Canonical binary consensus encoding (not JSON) | txid/size/signing are reproducible by any implementation, so a second client can't silently fork | The wire transport is still JSON (a separate efficiency concern) |
| Height-activated consensus upgrades | Rule changes roll out on a coordinated flag-day, not an uncoordinated fork | No miner version-bit signaling yet (needs a header version field) |
| Mempool admission checks the sender's confirmed state | Occupying the pool costs real balance, so it cannot be filled for free by an account holding nothing | A recipient cannot spend funds that are still unconfirmed (§21) |
| Multi-recipient outputs encoded only when present | The new form costs nothing to add: every existing txid, signature and stored chain stays valid | One transaction shape has two encodings to reason about |
| Verification cost metered per block, and cached across mempool and chain | A block cannot cost more to check than to produce, and a signature is verified once rather than twice | Another consensus limit to agree on; the cache is memory |
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
| State root in the header, over a TRIE | Light clients prove balances AND that an address holds nothing | Another header field, and the trie root is a consensus change from the earlier Merkle fold |
| Regtest = on-demand `/generate`, not fast continuous mining | Deterministic, controlled block production; no runaway chain | A separate mode; isolated by netkey rather than a distinct genesis |
| Miner throttles empty blocks by one `TargetBlockTime`, overridable per node | An idle network doesn't fill with coinbase-only blocks | It caps how fast an idle chain advances regardless of hashpower, so devnets and tests must lower `EmptyBlockInterval` rather than wait it out |
| Network id bound into genesis, the signing preimage and the handshake | A chain and its signatures belong to exactly one network; cross-network replay and accidental peering become impossible | A third thing every node must be configured with identically; mainnet keeps the empty id so nothing already stored changes |
| Fee sponsorship with no sponsor nonce | An address holding nothing can transact; replay is already prevented by the sender's nonce, and the payer's own in-flight transactions are undisturbed | The pool must track a per-payer total, and a sponsor's affordability is state the sender cannot see |
| Vault as a third hand-rolled script kind | A real new spending condition (delayed hot key, instant cold recovery) today, reusing the multisig/HTLC plumbing | One more special case consensus must carry until a script VM subsumes it; the height rule costs a second verification |
| Address index opt-in, in memory | An explorer or wallet on a full node stops re-deriving history from filters | Its size is not bounded by the chain, and it is rebuilt at every startup |
| Mempool reconciliation once per peer, after catch-up | A node that was offline learns pending payments instead of waiting for a rebroadcast | Asking before catch-up would reject everything (admission needs confirmed state), so it is one request, not a continuous protocol |
| Shares as pool accounting outside consensus | A pool can pay for hashpower that has not found a block, and the mining path gets exercised far harder | The ledger is unauthenticated and node-local: it is lost on restart and a node operator could fake it |
| Faucet allowed by the network, not by config | No flag, config key or API call can make a real chain give coin away | Testnet coin is free for anyone who can reach the node; the cooldown is a speed bump, not a defence |
| Node identity in its own key file, never the wallet | The identity public key goes to every peer, and an address is a hash of a public key — sharing them publishes the node's wallet address | One more file to keep; an in-process node with no identity still falls back to the wallet (with a warning) |
| Self-connections detected by identity, not address | The same host is reachable under several spellings, so a string comparison cannot see it; identity always can | The alias is only learned by completing a handshake with ourselves once |
| Bulk reads paged, response shape unchanged | A node can no longer be asked to serialize its whole chain into one response, and clients page instead | Callers must page; and paging bounds the response, not the cumulative fold behind filter headers |
| Light client keeps its verified headers | A command downloads only what is new instead of the whole chain, which is what "light" was supposed to mean | A cache file per wallet, and a reorg below its tip forces a full rebuild |
| An unmineable transaction is displaced for free | A vault's cold-key rescue cannot be held hostage by a parked hot-key spend at the same nonce | One narrow exception to replace-by-fee that both the pool and its readers must know about |
| Reorg history in a bounded in-memory ring | The one question worth asking after a surprise becomes answerable, without unbounded memory | Lost on restart; it is telemetry, not chain state |
| /health separate from /info, exiting non-zero in the CLI | A supervisor can tell "running" from "usable"; /info answers 200 while syncing, un-peered or stale | Another endpoint, and a readiness policy (stale-tip window) that is a judgement call |
| Log levels + optional JSON, wrapping the stdlib logger | A node's output can be quietened, turned up, or counted | Two output formats to keep readable; call sites carry key/value fields |
| Checksums client-side only | Avoids a consensus validation cascade | A malicious client can still burn its own coins |
| SLIP-0010 HD derivation | A mnemonic written down here restores in a standard wallet, which is what made a fresh address per invoice worth exporting | The pre-SLIP-0010 scheme still has to be derivable (`DeriveLegacy`) for keys made before the change |
| TUI as its own module | Keeps external deps' `go.sum` off the internal v0.0.0 modules | It cannot import `core` or `wallet` at all — a workspace `use ../core` still resolves `wallet` through the proxy, because the module path looks like a sub-module and Go wants a `wallet/v0.0.0` tag. Multisig/HD go through API helpers, and verification and signing shell out to the binary |
| Static (CGO-free) binaries | One binary runs on any matching kernel | lintian flags "statically-linked" (acknowledged via an overrides file) |
| HTLC as a script-bound address (not a new UTXO type) | Reuses the multisig pattern; account model unchanged | The claim/refund timeout is enforced at apply-time, not in signature checks |
| Compact filters committed in the header, behind an upgrade | Non-inclusion backed by proof of work instead of by the honesty of whoever answered | It forced the filter to be re-keyed off `PrevHash`+`MerkleRoot`: a filter keyed by its own block hash cannot be committed inside that hash. Below the activation height the old keying and the old trust assumption stand |
| Both new header commitments gated inside the preimage, not bolted on | `filtercommit` and `historyroot` add a field to the proof-of-work preimage only from their activation height, so **genesis and every block below it hash exactly as before** — the first header change here that did not move genesis | Two nodes that disagree about the activation height disagree about block hashes, which is a louder failure than a soft-forked rule |
| A script VM alongside the hand-rolled shapes, not replacing them | Spending conditions become a language without invalidating any funded multisig, HTLC or vault | A chain with `script` active has two ways to express a vault; retiring the old ones is a migration nobody has needed |
| Names in the account map under a NUL-prefixed key | They are committed by the state root for free, so a resolution is provable and an unclaimed name is provably absent | The key space has to be kept apart from addresses in two independent ways (a reserved prefix AND a separate trie domain tag), and `CheckTxSanity` must reject addresses that look like one |
| A non-fungible asset id derived under its own domain tag | Supply-one immutability is proven by construction: a mint cannot reproduce the id, so no registry lookup and no committed per-asset state is needed | An NFT and a fungible asset with the same issuer, ticker and nonce are different ids, which is correct and still surprising |
| Package limits count foreign ancestors only | A sender's own nonce run is already bounded by `MaxPerSender`; counting it would silently lower that from 64 to 25 | Two different notions of "ancestor" in one file — the pricing set includes a sender's own run, the limit set does not |
| SSE for the event stream, not WebSockets | One-directional, plain HTTP, zero deps, native `EventSource` | No client→server messaging over it (not needed) |
| API auth as relay-style policy (token on writes only) | Locks spending/mining without breaking public reads or the explorer | Not per-user auth; a shared bearer token, reads unauthenticated |
| Soft state persisted, chain authoritative | Warm restart (bans/peers/mempool survive) without trusting them | A hard kill can lose the latest soft state (re-synced from peers) |
| Mempool and orphan pool bounded in BYTES as well as in count | A count limit is not a memory limit: 5000 relay-size transactions is ~500 MB of valid, unevictable state for about one block reward | Two budgets to reason about, and a big arrival may displace several small ones |
| Pruning drops bodies but keeps the filter-header chain | A node's resident size stops tracking the chain, and it can still serve light clients the part it holds | It cannot serve old bodies, proofs or filters, and says so with 410 rather than 404 |
| A body-less block serves NO filter | An empty filter is a proof of absence; serving one would tell a light client its address is provably not in a block the node cannot read | Filter lists are sparse on a pruning node, and clients must read the range they were actually given |
| `MinPruneKeep` above `MaxReorgDepth` | A reorg replays the bodies it disconnects, so pruning into that range would leave a node unable to follow the chain | An operator's smaller `-prune` is silently raised (and logged) rather than honoured |
| Message signing domain-separated from transaction signing | "Sign this to prove it's you" cannot be answered with a valid transfer | One more preimage format to keep straight, and the two must never converge |
| Multisig/escrow spends travel as a file carrying their network | Members sign offline, which is the point of multisig, and a signature commits to one chain | A new file format per flow, versioned, that has to be refused rather than guessed at when unknown |
| Webhooks never block the node | A shop's web server being down cannot slow block processing | Delivery is at-most-once: a receiver far enough behind loses events |
| API rate limit keyed on IP, ignoring `X-Forwarded-For` | A client-set header would hand out a fresh bucket per request, making the limiter an allocator for the attacker | A node behind a real proxy must limit at the proxy |
| The TUI and GUI delegate signing to the `dnas` binary | One copy of the consensus-critical encoding; this project has already shipped a client whose hand-written copy had silently rotted | A process launch per payment, and the binary must be present |
| The console reads the node in process, not over HTTP | It works on a node whose API is unreachable, which is when it is most wanted | It duplicates the API's shape, and `-console` exists only so a script can drive it |

---

## 19. Testing

- **Unit tests** in every module, run under `-race`; the standing rule is that
  each feature lands with a focused test.
- **Native Go fuzzers** (`core/fuzz_test.go`, `wallet/fuzz_test.go`) over
  transaction/header/Merkle/amount decoding and address/mnemonic validation.
- **In-process integration tests** (`node/integration_test.go`) spin up multiple
  nodes on ephemeral ports and assert sync/discovery/convergence. They run the real
  miner, so they shorten `EmptyBlockInterval`: at the production default, three
  blocks cost 15 s of waiting before any hashing, which under `-race` on a loaded CI
  runner ate the entire deadline. `node/lifecycle_test.go` covers the other half of
  that story — that `Shutdown` really stops the node's loops, so nodes left behind
  by a finished test can't slow down or interfere with the next one.
- **Adversarial network simulation** (`node/simnet_test.go`) wires nodes through a
  switchboard (injected via `Node.dialFn`/`listenFn`) that adds latency and can
  partition links on demand, then asserts that a network which forks under a
  partition re-converges on the most-work chain after healing — stressing reorg,
  fork choice and sync under conditions the plain integration tests don't reach.
- **Delegation tests for the clients.** The TUI and GUI sign by running the
  `dnas` binary (§16), so their tests stand a shell script in for it and check the
  two things that can actually be wrong: the arguments passed, and that the
  outcome is read from stdout rather than assumed from the exit code. Each script
  writes its log line AFTER the result, which is precisely what a merged
  stdout+stderr stream cannot survive.
- **GUI tests** (`gui/test_dnas_gui.py`) run headless (`QT_QPA_PLATFORM=offscreen`)
  against a stub HTTP server. They now also cover the client's *own* crypto:
  `header_string`, `compact_to_big` and `meets_target` must match
  `core.Header.headerString`, `core.CompactToBig` and `meetsTarget` exactly, or
  the client's proof-of-work check is meaningless — and it HAD fallen out of step,
  silently, when the chain moved to an nBits target and a state root. Nothing
  exercised the SPV path, so nothing noticed.
- **Black-box end-to-end suite** (`e2e/`, behind the `e2e` build tag) starts the
  shipped `dnas` binary and drives it over HTTP and the CLI, importing no DNAS
  package. Everything above tests the code; this tests the *product* — wire
  formats, CLI output, startup flags, persistence across a restart — so it fails
  on breaks the unit tests cannot see. Each node gets a kernel-assigned port and
  its own temp directory, and runs in regtest so blocks are mined on demand
  (deterministic heights, no race with a miner). `make e2e-docker` runs the same
  suite hermetically — both binaries compiled at image build time from pinned,
  digest-locked base images with `GOPROXY=off`, then run with `--network none`
  on a read-only root as a non-root user — which is how CI runs it. The host
  supplies Docker and nothing else, and a red run can only be the source.
- **End-to-end demo** (`scripts/demo.sh`) runs a three-node network exercising
  auth, discovery, a converging transfer, expiry, the fee floor, multisig, HD,
  and SPV. `scripts/htlc-demo.sh` settles both HTLC branches, and
  `scripts/swap-demo.sh` settles a whole asset-for-coin atomic swap (both legs
  funded, the preimage published by one claim and used by the other).
- **What unit tests structurally cannot catch.** Everything above runs in one
  process on one network, so the class of bug where a *client* and a *node*
  disagree is invisible to it: the network id is part of a transaction's hash and
  its signing preimage (§5.1), so a client on the wrong network produces
  signatures and merkle roots the node rejects, and every in-process test agrees
  with itself. Every bug of that shape found so far — an external miner
  recomputing merkle roots on the wrong network, the faucet pricing its fee below
  the node's own relay floor, and a multisig member signing offline on whatever
  network their CLI defaulted to — was caught by running a real node and the real
  CLI against it. That is what `e2e/` is for, and why it is worth its runtime.
  The same round found two more that only a live run shows: `/account` served no
  formatted balance, so the explorer's address search rendered "balance
  undefined"; and a CLI flag written *after* a positional argument
  (`dnas assets show ID -api URL`) silently queried the default node, because
  Go's flag parser stops at the first positional.
- **What regtest cannot deliver.** Block timestamps advance a second per block
  and `MaxFutureDrift` is 120 seconds, so a single `/generate` call stops around
  120 blocks in — a node refuses its own block as too far in the future. Any test
  needing a deeper chain (pruning's 132-body floor, for instance) belongs in the
  unit suites, where the chain is built directly, rather than in `e2e/`.

- **Performance benchmarks with a recorded baseline**
  ([core/bench_test.go](core/bench_test.go), [scripts/bench.sh](scripts/bench.sh)).
  There were already a few benchmarks here, but nothing ever compared two runs,
  so a regression could only be caught by someone who happened to remember the
  old number — which is how a state root that cost a full fold over the whole
  ledger survived for months. Correctness tests use tiny chains, so a term that
  grows with the LEDGER never shows up in one; the benchmarks are therefore
  parameterised by ledger size, so a cost scaling with the wrong thing appears
  as a shape rather than a number. `make bench-check` fails past a loose
  threshold — loose because this runs on developer machines, where the signal
  worth catching is "several times slower", not "ten percent slower".
- **A guard against the client verifiers growing back.** `gui/test_dnas_gui.py`
  asserts that `header_string`, `meets_target` and `sha` are *absent* from the
  module. A comment saying "do not re-implement this" did not work — the file
  carried one, describing the first time it had drifted, while it was drifting
  again.

`make test` runs the Go suites plus the GUI tests (skipped if PyQt6 is absent);
`make test-race` runs the Go suites under the race detector; `make e2e` /
`make e2e-docker` run the end-to-end suite, which the tagged build keeps out of
the default runs; `make bench` / `make bench-check` measure and compare.

---

## 19b. Logging and operability

Everything a node had to say went through `log.Printf` at one volume: every
accepted block, every peer connect and disconnect, with no way to quieten a busy
node or turn up detail on one problem. On a chain producing a block every five
seconds that is a log nobody reads, which is the same as no log at all.

`node/logging.go` adds levels (`error`, `warn`, `info`, `debug`; `-loglevel`) and
an optional machine-readable form (`-logjson`), one JSON object per line with the
fields already separated — `accepted block 41 0000abc…` is fine for a human and
useless to anything that wants to count blocks per hour. It wraps the standard
logger rather than replacing it, so any remaining `log.Printf` still lands at
info. Misbehaviour is WARN (a rejected peer, a dropped transaction, a discarded
block), an unbuildable candidate is ERROR, and the rest is INFO.

`-printconfig` answers the other operability question: a node's settings come
from a JSON file and the command line, and several are then adjusted by the code
(`-regtest` rewrites the network, a network supplies a default netkey, an unset
`-nodekey` resolves to a path beside the chain, a `-prune` below the floor is
raised, zero means "default"). The effective configuration is therefore not
readable off either input, so the node can print the merged, resolved result and
exit without touching the chain.

**The console** (`cmd/dnas/repl.go`) is the third. A node started in a terminal
drops into a prompt, and it is the only way to look at a node whose HTTP API is
unreachable — which is exactly when somebody most wants to look. It had six
commands (send, balance, address, info, peers, mempool) while the node grew a
couple of dozen surfaces around it, so it now covers the same ground the API does,
read directly out of the node in process: no HTTP, no token, no listener. Its
commands are a table rather than a switch, so `help` cannot drift from what
exists; `-console` forces the prompt on when stdin is a pipe, which is what makes
it drivable by a script and testable at all.

---

## 20. Build and release

- **[Makefile](Makefile)** is the entry point: `build`, `wasm`, `test`,
  `test-race`, `bench`, `vet`, `fmt`, `dist`, `deb`, `install`, `demo`.
- **`make wasm`** compiles [cmd/wasmverify](cmd/wasmverify) for `js/wasm` into
  `api/wasmdist/`, alongside the `wasm_exec.js` shim copied from the toolchain
  that built it (the two must match). Both are gitignored build artifacts; the
  directory is embedded with `//go:embed all:wasmdist` so a plain `go build`
  still works without them, and `build` depends on `wasm` so a locally built
  binary serves a working explorer.
- **`scripts/build.sh`** cross-compiles static binaries (`CGO_ENABLED=0`,
  `-trimpath`) for `linux/amd64` + `linux/arm64` and tarballs them.
- **Releases are reproducible**, and that is checked rather than asserted:
  `make verify-reproducible` builds the whole release twice, into two
  *different* directories, and compares every artifact. Two directories is the
  point — the build path is the usual thing that leaks into a binary, and a
  check that built twice in one place would not catch it. Getting there needed
  `-buildid=` cleared (Go derives it from inputs that include toolchain paths),
  every file in the tarball given the same mtime from `SOURCE_DATE_EPOCH`
  (defaulting to the commit date, which is a property of the source and
  therefore the same for everyone), sorted tar entries with numeric owners, and
  `gzip -n`. Without reproducibility a published checksum attests only to
  whatever the release machine happened to produce, which is exactly the thing
  a reader cannot check.
- **`SHA256SUMS`** covers every artifact, so one signature covers the release.
  It is signed when `DNAS_SIGN_KEY` names a GPG key and the build says
  **UNSIGNED** out loud when it does not — a checksum file anyone can rewrite
  proves nothing about who built it.
- **`scripts/build-deb.sh`** builds lintian-clean `.deb` packages (amd64/arm64)
  with `dpkg-deb --root-owner-group`.
- The build **version is stamped** via `-ldflags "-X main.version=…"` and
  reported by `dnas version`. The release number lives in the
  [VERSION](VERSION) file and `scripts/version.sh` turns it into what a build
  stamps. It used to come from `git describe`, which cannot answer in the two
  places it most needs to: the e2e container has no `.git` (`.dockerignore`
  drops it) and neither does a source tarball, so both silently fell back to a
  hard-coded `0.1.0` or reported `dev`. Now the file is the source of truth and
  git only adds the detail it alone knows — which commit, and whether the tree
  is dirty: `0.4.0`, `0.4.0+g1a2b3c4`, `0.4.0+g1a2b3c4.dirty`. `VERSION=1.2.3`
  in the environment overrides the lot, and [CHANGELOG.md](CHANGELOG.md) records
  what each release contains (a test fails if the two disagree).
- **CI** ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) runs two jobs on
  every push and pull request: a gofmt check plus `make vet`, `make build` and
  `make test-race`, and — in parallel — the containerized black-box suite via
  `make e2e-docker` (§19). On a version tag (`v*`), and only if both pass, a
  release job runs `make dist` + `make deb` and uploads the tarballs and `.deb`s
  as build artifacts. Note the trigger is `v*`: the existing `0.2` tag has no
  `v` prefix and therefore never produced one.

See [scripts/README.md](scripts/README.md) for the script details.

---

## 21. Known limitations

- The network is open/permissionless with eclipse caps on both directions now —
  inbound (total + per-IP-group) and outbound (a tried/new address manager with a
  per-group cap on live connections, §11) — plus per-peer rate limiting. But
  identities and IPs are still cheap and the diversity signal is a **/16 prefix
  rather than an ASN**, so an attacker with addresses across enough distinct
  ranges still wins: this raises the cost of an eclipse, it does not settle it.
  There is no proof-of-work/stake peer gating. The open handshake is anonymous —
  it has no MITM authentication; safety rests on connecting to many peers. Bans
  persist across a graceful restart but a hard kill can lose them (re-learned
  from peers).
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
- Mempool admission counts a sender's **confirmed** balance plus what the pool
  is already paying it, so a recipient can spend coin that is still unconfirmed
  (§10, [core/package.go](core/package.go)). The dependency is bounded by
  ancestor, descendant and package-byte limits, which is what keeps it from
  being a way to fill the pool with work that can never be mined. A parent is
  priced by the best package it heads, so a child's fee can unstick it.

  It is not free. Pricing every queued transaction by the best package it heads
  costs a pass over the pool per block template: `BenchmarkSelect` on a
  2,000-transaction pool went from ~28ms to ~35ms, after caching transaction
  sizes (each `Size()` is a full canonical re-encode, and the naive version
  read one per package member, which cost nearly 2x rather than 1.25x). At a
  60-second block time that is not a problem; on a much larger pool it would
  want per-sender suffix sums instead of a set per transaction.

  Two honest limits. The ancestor and descendant counts apply to OTHER senders
  only — a sender's own nonce run is already bounded by `MaxPerSender`, and
  counting it would silently lower that from 64 to 25. And there is no package
  SUBMISSION: a parent and child arrive separately and the parent must arrive
  first, because accepting an orphan child and holding it for a parent that may
  never come is a different feature with its own denial-of-service surface. Chained spends still work within a *single
  sender's* nonce run, and for a recipient whose funds have confirmed.
- Transactions are announced by hash and pulled (`MsgTxInv`/`MsgGetTx`/`MsgTxs`,
  gated on the `txinv` capability), so a body crosses each link once rather than
  once per peer; a peer that does not advertise the capability still receives the
  full push. Blocks are relayed compactly (`MsgCmpctBlock`, §11): a header plus
  8-byte short ids that a peer resolves against its own mempool, asking only for
  what it lacks. The short ids are keyed by the block hash, and a reconstruction
  that does not reproduce the committed merkle root falls back to a full fetch —
  so a collision costs a round trip, never a wrong chain.
- Merkle SPV proves inclusion trustlessly. Compact filters add non-inclusion,
  and from `UpgradeFilterCommit` that is backed by the chain's work rather than
  by the honest-node/multi-peer assumption: the filter's header is a field in
  the proof-of-work preimage. The two halves of that change are inseparable — a
  BIP158 filter is keyed by the hash of the block it describes, so the filter's
  commitment cannot also live inside that hash. Above the activation height the
  key is derived from `PrevHash` and `MerkleRoot` instead, which are fixed
  before the nonce search. Below it, filters are keyed and trusted as before.
  State proofs prove account membership AND absence against the header's trie
  state root ([core/trie.go](core/trie.go), [core/state.go](core/state.go)), so
  "this address holds nothing" is verifiable rather than taken on a node's word:
  a key's position in the trie is fixed by the key, so arriving at an empty slot
  is itself the proof. The root is now computed INCREMENTALLY against a
  retained node store ([core/statetrie.go](core/statetrie.go)): an update
  rewrites only the path from the accounts a block touched, so committing a
  block and serving a state proof both cost the block rather than the ledger
  (at 10,000 accounts, 157ms to 19µs and 157ms to 5µs respectively). The store
  is a bounded cache and a miss falls back to the full rebuild, so correctness
  never depends on it being warm. What the trie still does NOT buy is the last
  thing it could: the state is a resident map, so memory bounds the ledger
  (ROADMAP §1).
- A coinbase transaction commits only its recipient and amount **until
  `UpgradeUniqueCoinbase` activates**, so before that height two blocks paying
  the same miner the same subsidy share a **txid** (Bitcoin's pre-BIP34 problem)
  and lookups resolve a duplicated coinbase to its first occurrence (§6). From
  the activation height the coinbase carries the block's height in its `Nonce`,
  which makes every coinbase distinct. The upgrade may be scheduled by hand or
  put to a BIP9 miner vote (`-deployments`, [core/versionbits.go](core/versionbits.go)).
- Supply conservation is a consensus RULE, not only a report: applying a block
  must change the coin held by accounts by exactly its subsidy minus the base fee
  its transactions burned, and must change each asset's total only by what that
  block issued, minted or burned ([core/conservation.go](core/conservation.go)).
  The per-block identity sums to the chain-wide `minted − burned == circulating`
  that §9 reports, so a block that would inflate or destroy coin is rejected
  rather than noticed afterwards.
- HTLC refund timing and coinbase maturity are enforced at block application, not
  in signature verification. HD is not SLIP-0010. Proof of work is a continuous
  256-bit target (nBits) retargeted by an LWMA with **no hard difficulty cap**, so
  on a real network difficulty tracks hashpower without bound; only a devnet /
  regtest (`NoRetarget`) holds it at the easy genesis floor for instant blocks.
- Snapshot fast-sync bootstraps trustlessly (state verified against the header
  state root, anchored by a checkpoint) but the resulting pruned chain runs in
  memory — persisting it through the index-based block store is future work. A
  fast-synced node also cannot fold the filter commitments of bodies it never
  saw, so it reports `filter_base` above its snapshot and answers 410 below it:
  it can validate the chain it has, and it cannot serve a light client the old
  part of it.
- `-prune` now bounds a node's disk as well as its memory: the log is compacted
  to match the pruned chain and a verified state snapshot is written beside it to
  restart from (§12). Two honest limits remain. Pruned heights keep a header-only
  record — the headers are load-bearing for linkage, MTP and the retarget — so
  the saving is the transaction bodies, which on a chain of empty blocks is close
  to nothing. And a pruned store is no longer fully re-verifiable: `dnas db
  verify` replays the bodies it still has and takes everything below the cutoff
  on the snapshot's authority, which it says rather than glosses. The bodies it
  drops still take their inclusion proofs and compact filters with them (410,
  `body_height`).
- Webhook delivery is at-most-once behind a bounded queue: a receiver far enough
  behind loses events rather than the node growing a backlog for it. A service
  that must not miss a payment should reconcile against `/chain` or
  `/address/{addr}/history`.
- The API rate limit keys on the client IP and ignores `X-Forwarded-For` on
  purpose (a client-set header would hand out a fresh bucket per request), so a
  node behind a real proxy needs the limit at the proxy. It is also per-process:
  nothing is shared between two nodes behind one address. Request *bodies* are
  bounded (413 past 64 KiB / 400 KB / 4 MB by endpoint class, §15), but the P2P
  side still has only the coarse 64 MiB `maxFrame` cap with no per-message-type
  limits.
- An invoice is matched by (address, amount, height ≥ its own), so two invoices
  for the same amount at the same address cannot be told apart. The file says so;
  a fresh address per invoice is the fix, and needs standard HD derivation to be
  worth exporting.
- A message signature is domain-separated from a transaction signature, so
  neither can be replayed as the other — but it proves only control of a key at
  the moment it was made. There is no revocation and no expiry on one.
- No client re-implements a consensus rule, and this is the strongest statement
  in this document because it was bought with a real defect. There were four
  hand-written SPV verifiers outside `core` — the explorer's JavaScript, the
  TUI's Go, the PyQt GUI's Python and `scripts/demo.sh`'s Python — and every
  one of them computed a header preimage starting at `index`, having never been
  given the `Version` field that BIP9 signaling added. All four were hashing
  something that was never a header, and all four reported failure for valid
  proofs. The GUI's test built its fixture from the GUI's own hashing, so the
  two agreed with each other and both disagreed with the chain.

  They are gone. The explorer loads `core` compiled to WebAssembly
  ([cmd/wasmverify](cmd/wasmverify), served at `/wasm/`) and there is
  deliberately NO JavaScript fallback — a page that says its verifier is
  unavailable is strictly better than one quietly running a second
  implementation. The TUI, the GUI and the demo script delegate to the `dnas`
  binary, which is what the TUI and GUI already did for signing. The cost is a
  process launch per operation, a dependency on the binary being present, and
  `make wasm` in the build. The alternative cost was a verifier that said
  "PROVEN" about arithmetic nobody had checked.
- The asset registry describes what the chain issued, but a fast-synced node
  cannot recover the issuances below its snapshot: the balances are in the
  snapshot's state and the descriptions are not, and they only appear if the
  chain is walked from a full peer.
- Dandelion++ hides a transaction's origin along the stem, but on a tiny devnet
  with few peers the anonymity set is small; it is a demonstration of the scheme.
- Native assets are balances committed in the state root and fees are always
  paid in coin (no per-asset fee market). Asset logic is issue, transfer, mint,
  burn, and — under `nonfungible` — a supply-one token committing a digest.
  There is a script VM for AUTHORIZATION (§5.6), but no contract layer: a
  script decides whether a spend is allowed and cannot cause an effect of its
  own, so "transfer this asset when X" is not expressible.
- Regtest and testnet now have their **own genesis** and their own signing
  preimage (§5.1), so they are separate chains rather than the same chain behind a
  different pre-shared key. Still point each at its own data directory: a store
  from one network is refused by a node on another, but the error is easier to
  read than to prevent.
- The address index (`-addrindex`) is in memory and rebuilt at every startup, and
  its size is bounded by *usage*, not by the chain: an address appearing in a
  million transactions has a million entries. It is off by default for that
  reason.
- Mempool reconciliation is one request per peer, sent once we are caught up — not
  a continuous set-reconciliation protocol. A transaction broadcast during the
  window between that request and the peer's next push is still missed until
  someone rebroadcasts it.
- Mining shares are node-local, unauthenticated accounting: they are not stored in
  the chain, they are lost on restart, and nothing stops a node operator from
  reporting whatever ledger they like. That is enough to run a toy pool between
  machines you control and nothing more.
- Fee sponsorship makes a transaction's affordability depend on an account the
  sender does not control. The mempool holds a sponsor to its whole queued total,
  but a sponsor that spends its balance elsewhere still invalidates the
  sponsorships it has outstanding, which are then dropped at the next block.
- The faucet spends the node's own wallet with a per-address and per-IP cooldown.
  On any network where the coin were worth something that would be far too weak —
  which is why the network parameters, not a flag, decide whether one may exist.
- Reorg history is a bounded (64-entry) in-memory ring, lost on restart: operator
  telemetry rather than chain state.
- A reorg whose persistence fails **poisons** the block store (§12): the node
  stops writing and says so, rather than continuing onto a log that has diverged
  from memory. That contains the damage; it does not undo it. Recovery is manual
  — `dnas db verify`, then re-sync — because making the truncate-and-append
  sequence atomic is still open work.
- `/chainstats` reports estimates. Hashrate is window work ÷ elapsed time, and
  proof-of-work variance means a short window describes luck as much as
  hashpower; a window whose blocks share a timestamp reports no rate at all
  rather than an infinite one.
- Paging bounds responses, not always work: a range of filter headers still
  requires folding the chain from genesis, since that chain is cumulative. A
  client holding its own verified prefix (the SPV header cache) pays neither.
- `/health`'s stale-tip window is a fixed multiple of `TargetBlockTime`, not
  something the operator can tune.

- The script VM ([core/script.go](core/script.go)) decides one question — may
  this transaction spend from this account — and can return only yes or no. It
  has no loops, no backward jumps, no state beyond a bounded stack, and no view
  of the chain beyond the height. It is not a contract system and cannot become
  one without a different safety argument. The four hard-coded authorization
  shapes also remain in consensus, so a chain with the upgrade active has two
  ways to express a vault; retiring the old ones is a migration nobody has
  needed yet. `OP_CHECKSENDERVERIFY` is reserved and rejected rather than
  ignored, so a script using it cannot be funded and then found unspendable.

- A name is a LEASE and an expired name is claimable by anyone. That is
  deliberate — a namespace nobody can reclaim is squatted once and then dead —
  but it means a payer who cached a resolution must re-check it, and
  `dnas name resolve` distinguishes UNREGISTERED from LAPSED for exactly that
  reason. There is no renewal reminder, no grace period, and no dispute
  process, because there is no authority here to appeal to.

- A non-fungible asset commits a sha256 of what it represents and nothing more.
  Where those bytes live, and whether they are still there, is not something
  consensus can speak to. The chain can say this token is unique, that its
  supply is permanently one, and that it commits to this digest; it cannot say
  the digest still resolves to anything.

- The binary wire format saves about 18% on a batch of transactions, which is
  less than the change sounds. The payload is still mostly hex strings, because
  `Transaction` stores hashes and signatures as hex; storing them as bytes is a
  change to core's types rather than to the wire, and is the next increment.

- The browser verifier is a 4 MB WebAssembly binary that must be built
  (`make wasm`) and is gitignored. A binary built without it serves no
  verifier, and the explorer says so rather than falling back to JavaScript.
  That is the intended behaviour: the fallback IS the bug class being removed.

Each limitation is a chosen stopping point. [ROADMAP.md](ROADMAP.md) turns this
list into a prioritized plan (on-disk state trie, second implementation, script
VM, addrman/BIP152, and the rest).

It is a toy. Do not point it at the internet.
