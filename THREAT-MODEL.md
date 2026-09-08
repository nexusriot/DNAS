# DNAS threat model

**This is not a security audit, and it is not a substitute for one.** An audit's
value is that it is performed by people who did not write the code and who are
paid to disagree with it. This document was written alongside the
implementation, so it inherits exactly the blind spots the implementation has:
it can tell you what the code *intends* to defend against, and it cannot tell
you what its author failed to imagine. Treat it as a map of the assumptions, and
as the thing an outside reviewer should start by trying to break.

[ROADMAP.md](ROADMAP.md) §7 still lists an independent review as an open item.
Nothing here closes it.

DNAS is a learning project. Do not point it at the internet.

---

## 1. What is being protected

| Asset | Why it matters |
|---|---|
| Coin balances and asset holdings | The ledger is the product. Loss or forgery is total failure. |
| Private keys (`wallet.json`, HD seeds) | Compromise means unrecoverable theft; there is no chargeback. |
| The node's view of the canonical chain | A node fed a false chain accepts payments that do not exist. |
| Node identity keys (`nodekey.json`) | Impersonation, and correlation of a node with its operator. |
| Availability of a node's API and P2P | An unreachable node stops accepting payments. |
| Transaction origin privacy | Linking a transaction to an IP deanonymises a payer. |

## 2. Who the adversaries are

- **A remote peer.** Speaks the P2P protocol, sends anything it likes, may run
  many identities in a few network ranges. The default adversary — the network
  is permissionless.
- **A malicious miner.** Some hashpower; wants to double-spend, censor, or claim
  more subsidy than the schedule allows.
- **A majority miner.** More hashpower than everyone else combined. Explicitly
  **out of scope for defence** — see §5.
- **A DNS seed operator.** Chooses which addresses a bootstrapping node learns.
- **An API client.** Can reach the HTTP API; may or may not hold the bearer token.
- **A local attacker.** Reads the node's disk or its process list.
- **The counterparty in a multi-party flow** (multisig, escrow, HTLC, sponsorship).

## 3. Defences that exist, and what each assumes

### Consensus and ledger integrity

| Threat | Defence | Assumption it rests on |
|---|---|---|
| Forged spend | Ed25519 signature over a canonical, length-prefixed preimage ([core/codec.go](core/codec.go)) | Ed25519 is sound; the signing key is secret |
| Replaying a transaction | Per-account nonce, consumed exactly once | — |
| Replaying across networks | Network id bound into the signing preimage (§5.1) | Operators run the right `-network` |
| Malleability (same transfer, different txid) | Every field, including the fee payer, is inside the txid preimage | — |
| Inflation | Subsidy is a function of height; the base-fee portion of every fee is burned | Supply conservation is *reported*, not yet a consensus rule (ROADMAP §1) |
| Spending an orphaned reward | `CoinbaseMaturity` (3 blocks) | Reorgs stay shallower than 3 blocks — thin at 5s blocks |
| Deep history rewrite | `MaxReorgDepth` (100) + operator checkpoints | See §5: this is also a liveness hazard. A refusal is now counted, logged and surfaced by `/health` rather than silent |
| Oversized/expensive blocks | `MaxBlockBytes`, `MaxBlockTxs`, `MaxBlockVerifyOps`, `MaxCoinbaseBytes` | The verification-cost model matches real cost |
| Timestamp manipulation | Median-time-past, `MaxFutureDrift` (120s), plus a bounded median-of-peers clock offset ([core/nettime.go](core/nettime.go)) | Peers are majority-honest. The offset is capped at `MaxTimeOffset` (70 min), so a hostile peer set moves the clock at most that far |
| A corrupt store after a failed reorg | The block store poisons itself and refuses further writes ([core/store.go](core/store.go)) | Recovery is manual |

### Networking

| Threat | Defence | Assumption |
|---|---|---|
| Inbound eclipse | Total + per-network-group inbound caps | A /16 approximates an operator — weaker than an ASN |
| **Outbound eclipse** | Address manager: tried/new tables, per-group cap on live outbound peers ([node/addrman.go](node/addrman.go)) | Same /16 approximation; an attacker with addresses in many ranges still wins |
| Gossip flooding memory | Bounded tables with eviction, per-group entry caps | — |
| Peer resource abuse | Per-peer token bucket, ban scoring, frame cap | 64 MiB frame cap is coarse; no per-message-type limits (ROADMAP §2) |
| Passive eavesdropping | Encrypted, authenticated transport ([node/secure.go](node/secure.go)) | — |
| **Active MITM on the handshake** | **None.** The open handshake is anonymous | Safety rests on connecting to many peers. No key pinning yet |
| Transaction origin linkage | Dandelion++ stem/fluff | Anonymity set is tiny on a small network |
| Hostile bootstrap | DNS seeds enter the `new` table with no special standing, subject to the same diversity cap; consulted only when short of peers | Configure more than one seed. A single seed *is* an eclipse vector |

### API and keys

| Threat | Defence | Assumption |
|---|---|---|
| Unauthorised writes | Bearer token on mutating endpoints, constant-time compare | One shared token; no per-user auth |
| Request flooding | Per-IP token bucket, 429 + `Retry-After` | Ignores `X-Forwarded-For` on purpose; needs a proxy-side limit behind a proxy |
| Memory amplification | Paged reads; `http.MaxBytesReader` on every write endpoint (413) | — |
| Key theft at rest | PBKDF2 + AES-256-GCM when `DNAS_WALLET_PASSPHRASE` is set | **Unencrypted by default** |
| Secrets in `ps` | Passphrase and API token come from the environment, never flags | Environment is readable by the same user |
| Node identity leaking the operator's address | Identity is a separate key from the wallet | Only if `-nodekey` is actually used |

### Light clients

| Threat | Defence | Assumption |
|---|---|---|
| Fake payment | Merkle inclusion proof against a PoW-verified header | Client verifies the header chain itself |
| Fake balance | State proof against the header's state root | — |
| **"You were never paid"** | An **absence proof** against the header's trie state root ([core/state.go](core/state.go)); compact filters additionally give probabilistic block-level non-inclusion | The state answer is now trustless. The FILTER answer still is not: filters are not header-committed, so block-level non-inclusion still rests on honest-node/multi-peer |
| Being served a minority chain | Most-work rule + checkpoints | Client talks to more than one node |

## 4. Trust boundaries

Everything crossing these is hostile input until validated:

1. **P2P frame → node.** Attacker-controlled. Validated by consensus rules.
2. **HTTP request → node.** Possibly unauthenticated. Size-bounded, rate-limited.
3. **DNS seed response → addrman.** Third-party controlled. Never dialed
   preferentially.
4. **Chain store file → node.** Trusted-ish; a foreign or corrupt file is
   refused rather than clobbered.
5. **Signed file between parties** (multisig, sponsorship, escrow). The
   counterparty is an adversary; the file records its network so a signature
   cannot be lifted onto another chain.
6. **Wallet file → process.** Trusted, but may be encrypted.

## 5. Explicitly out of scope

Being clear about this matters more than the list of defences, because an
unstated exclusion reads as a claim.

- **A majority of hashpower.** 51% can reorg, double-spend and censor.
  `MaxReorgDepth` bounds how far *this node* will follow, which converts a
  deep-reorg attack into a **network split** rather than preventing loss. The
  window that split opens in is now 1h40m rather than the ~8 minutes it was at a
  5-second block time (see `FinalityWindow`), and a refused reorg is counted,
  logged and surfaced by `/health` instead of being silent — so the failure is
  survivable and visible rather than routine and invisible. It is not *fixed*: a
  partition longer than the window still needs manual intervention, and no
  parameter choice makes a low-hashrate chain expensive to attack.
- **A new chain's hashrate economics.** A chain with little hashpower is cheap to
  attack outright. No amount of code quality changes that; the answers are merged
  mining, a different PoW, or leaning on checkpoints — none of them chosen yet.
- **Physical and host security.** A machine with an attacker on it is lost.
- **Side channels.** No constant-time review beyond the token compare; no
  attempt at timing or memory-access resistance.
- **Supply-chain integrity.** No reproducible builds, no signed releases.
- **Legal and regulatory exposure** of running or distributing a token.

## 6. Known weaknesses, ranked by what an attacker would actually do

1. **Eclipse via /16 approximation.** The outbound diversity cap buckets by /16,
   not by ASN. An attacker with addresses across enough distinct /16s — cheap at
   cloud providers — still fills a node's outbound set. Raises cost; does not
   settle it.
2. **No MITM authentication on the handshake.** An attacker on the path can be
   any peer. Peer-key pinning is possible now that node identity is a stable key,
   but is not implemented.
3. **Local clock trusted for timestamp rules.** A node with a skewed clock can be
   fooled on MTP and future-drift checks.
4. **Block-level non-inclusion is still not trustless.** *Account* absence now
   is: the header commits a trie root, so a client can be shown that an address
   holds nothing and verify it against proof of work. What remains is the
   compact-filter answer to "was this address in THIS block" — filters are not
   header-committed, so that still rests on the honest-node/multi-peer
   assumption.
5. **Recipient addresses are consensus-checked only once the upgrade is
   scheduled.** The `checkedaddresses` upgrade closes the burn-to-a-typo hole,
   but like every upgrade here it is inert until an operator sets an activation
   height — and every node on the network must set the same one. Until then the
   old behaviour stands.
6. **Supply conservation is reported, not enforced.** An accounting bug would be
   visible rather than rejected.
7. **One shared API token.** No per-user auth, no scopes, no revocation.
8. **Wallets are unencrypted unless the operator opts in.**
9. **Mining shares are unauthenticated node-local accounting.** Fine between
   machines you control, and nothing more.
10. **Fee sponsorship depends on an account the sender does not control.** A
    sponsor that spends elsewhere invalidates outstanding sponsorships.

## 7. What an outside reviewer should attack first

Ordered by where the author's confidence most exceeds the evidence:

1. **The canonical codec.** Every optional block is appended only when present
   and distinguished by a leading tag byte. Find two distinct transactions with
   the same canonical bytes, or a field value that can be mistaken for a tag.
   [core/testdata/consensus_vectors.json](core/testdata/consensus_vectors.json)
   pins the current behaviour; it does not prove no collision exists.
2. **Reorg and undo.** `applyUndo` reverses an undo log rather than recomputing
   state. Find a transaction shape whose undo is not exact — asset transfers,
   issuance and sponsorship all touch several accounts.
3. **The script special cases.** Multisig, HTLC and vault are three hand-rolled
   verifiers. Find a spend that satisfies one verifier's check while the address
   hash committed to something else.
4. **Mempool admission vs block validity.** Anything the pool admits that a block
   must reject is a way to make a miner build invalid candidates.
5. **The trie's proof verifier.** Both directions: forge a membership proof, and
   forge an absence proof for a key that exists.
6. **Sync and the finality guards.** Make two honest nodes permanently disagree
   using only legal messages — §5 says this is possible via partition, so find
   the cheaper versions.

---

*Written 2026-09-08 against the working tree at the time. If the code has moved,
this has not.*
