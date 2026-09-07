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

### Performance

- **Block assembly is ~188× faster on a full mempool.** `Mempool.Select` used to
  rescan the whole pool once per chosen transaction, re-deriving every
  candidate's hash and canonical size on each pass — O(pool × block) sha256
  work, which made building a block cost far more than mining one should. It now
  computes everything static once up front, and exploits the fact that only one
  transaction per sender can ever be ready (readiness needs an exact nonce match,
  and the pool holds at most one transaction per `(sender, nonce)`), so each
  round examines one head per *sender* rather than the whole pool. Measured on a
  2000-transaction pool building a full block: **5.7 s → 30 ms**.

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
- **`/metrics` covers what the node actually knows** — 36 series, up from 12.
  Added reorg totals and depth, orphan count, ban scores and the threshold,
  hashrate and block intervals, supply (minted / burned / circulating), tip age,
  blocks-behind, mempool bytes, and webhook delivery counters. All of these
  existed already but only as JSON spread across `/reorgs`, `/chainstats`,
  `/bans`, `/supply` and `/health`, which is the wrong shape for the one
  consumer that wants them continuously.
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
