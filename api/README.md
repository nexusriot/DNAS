# api

Module `github.com/nexusriot/DNAS/api` — a small HTTP interface to a running
node.

The bulk reads — `/chain`, `/headers`, `/cfilters`, `/cfheaders` — are **paged**
(`?from=HEIGHT&limit=N`, capped at 2000 entries). They previously serialized the
whole chain into one response, which is a memory-amplification attack anyone can
run with curl and which made the light client re-download everything on every
command. The shape is still a plain JSON array; a client pages with `from` and
reads the total from `/info`, or asks for `?last=N` — the newest N entries in one
request. That last form is not a convenience: paging silently changed what an
*unparameterized* request meant, and every client that draws a chain view wants
the tail, so without it the explorer, the TUI and the GUI would all have sat at
genesis on a long chain.

Every request passes a **per-client token bucket** (`ratelimit.go`,
`-apirate`/`-apiburst`), answering **429** with `Retry-After` when it is
exceeded. It keys on the client IP and deliberately ignores `X-Forwarded-For`: a
client-set header would hand out a fresh bucket per request. An IPv6 client is
keyed by its **/64**, since one subscriber is routinely handed a whole /64 and
would otherwise have 2^64 buckets to rotate through. The bucket map itself is
capped (65536 clients, least-recently-seen evicted first, idle ones expired), so
the limiter's memory is bounded by a constant rather than by how many source
addresses an attacker has. `/events` is exempt from the budget, being one
long-lived request that then sends many messages — but it is capped instead: at
most 256 concurrent streams node-wide and 8 per client, **429** beyond that.

The bucket is **cost-weighted**: an endpoint debits tokens in proportion to
what it costs the node, rather than one each. It used to be flat, which priced
`/info` and `/stateproof` identically — and a state proof on a large ledger
cost several orders of magnitude more than an `/info`, so the nominal limit
and the actual CPU a client could demand were not the same number. The file's
own comment had named the expensive endpoints and then charged them like the
cheap ones. The weights (`routeCost`) are in units of one cheap read:

| Cost | Endpoints |
|------|-----------|
| 30 | whole-ledger work: `/snapshot/{h}`, `/audit` |
| 5 | a pass over every account or a window of blocks: `/richlist`, `/series`, `/chainstats`, `/supply` |
| 2 + 1 per 100 of `?limit=` (or `?max=`) | the paged reads: `/chain`, `/headers`, `/cfheaders`, `/cfilters` |
| 3 | a proof built per call: `/stateproof/`, `/nameproof/`, `/proof/`, `/ancestry`, `/cfilter/` |
| 3 | `/metrics`, `/address*` |
| 1 | everything else |

A cost above the burst is charged as the burst, so a client with a full bucket
can always make any single request.

The HTTP server sets `ReadHeaderTimeout` 10s, `ReadTimeout` 30s and
`IdleTimeout` 2m, so a slow-loris client cannot hold a connection open by
trickling a request. There is deliberately **no** `WriteTimeout`: `/events` and
the long-polled `/blocktemplate` are responses that are *meant* to stay open.

The browser verification bundle is served from `/wasm/` (`dnas.wasm`,
`wasm_exec.js`), embedded from `wasmdist/`. It is a build artifact: a binary
built without `make wasm` answers 404 and the explorer reports that its
verifier is unavailable. There is deliberately no JavaScript fallback — the
fallback is the bug class the bundle exists to remove.

Read endpoints: `/info` (includes the `network` this node runs on,
`next_bits`/`next_difficulty`, `min_relay_fee`, the current dynamic fee floor, the
consensus `base_fee` — both per byte — and whether the address index and faucet
are available), `/chain`, `/balance/{addr}`, `/account/{addr}` (balance, nonce,
and any native asset balances), `/mempool`, `/mempool/stats` (the pending queue's
fee-rate distribution), `/address/{addr}/history` (transactions touching an
address, oldest first, paged — 503 unless the node runs with `-addrindex`, since
"no index" must not be mistaken for "no history"), `/shares` (the mining share
ledger), `/tx/{txhash}` (one transaction by id — `confirmed`
with its block and confirmation count, resolved through the chain's transaction
index, or `pending` from this node's mempool, so a wallet polls one endpoint from
submission to confirmation), `/supply` (minted, burned, circulating, and the
`consistent` conservation check `minted − burned == circulating`),
`/assets` (every asset the chain issued, `?ticker=X` to filter — a LIST, since
anyone may issue "GOLD"), `/asset/{id}` (one asset plus its holders and the held
total, which must equal the issued supply), `/webhooks` (delivery counters),
`/peers`, `/address`, `/estimatefee?blocks=N`
(recommended per-byte fee = base fee + estimated tip, never below the relay
floor), `/metrics` (Prometheus format: 63 series covering the chain, the mempool
by count *and* bytes, peers and their ban scores, reorg totals and depth, orphan
count, hashrate and block intervals, supply, tip age, blocks-behind, the share
ledger and webhook delivery — most of these were previously reachable only as
JSON spread across `/reorgs`, `/chainstats`, `/bans`, `/supply` and `/health`).

**Write bodies are bounded.** Every write endpoint decodes through `decodeBody`,
which wraps the request in an `http.MaxBytesReader` and answers **413** before
parsing: 64 KiB for the small control payloads, 4× `core.MaxRelayTxBytes` for a
transaction, 4× `core.MaxBlockBytes` for a block (JSON with hex signatures runs
larger than the canonical encoding those bound). The cap is on the reader, not on
`Content-Length`, so a request that understates its length is still cut off
mid-stream. Endpoints gated on node configuration (`/generate`, `/faucet`) refuse
with **403** before reading the body at all.

A node that has **pruned** a block body, or fast-synced above it, answers
`/block/{i}`, `/cfilter/{i}` and `/cfheaders` with **410 Gone** rather than 404,
and publishes `body_height`/`filter_base`/`pruned` in `/info`. The distinction
matters: a client told "not found" concludes the chain is shorter than it is,
when the right move is to ask another node. A body-less block serves no filter at
all, because an empty filter is a proof of *absence*.

SPV / light-client endpoints: `/headers`, `/header/{index}`, `/block/{index}`
(one full block body, so a light client downloads only filter-flagged blocks),
`/proof/{txhash}` (a merkle inclusion proof, verifiable against a header's merkle
root), `/stateproof/{addr}` (an account balance/nonce proof against the header
state root — for an account that does not exist it answers a verifiable
*absence* proof with `found: false`, not 404, since arriving at an empty trie slot
is itself the proof that nothing is there), `/snapshot/{height}` (the full account
state at a height — or `/snapshot/latest` — verified against the header's state
root, for `dnas fastsync`), plus the compact block filters `/cfilters`,
`/cfilter/{index}`, and the BIP157-style filter-header chain `/cfheaders` for
non-inclusion scans. `/nameproof/{name}` is the state proof for a name (it takes
the name and derives the key server-side, because a name's key contains a NUL byte
and cannot travel in a path — the client must still check the proof is about the
key it derived itself), and `/ancestry?height=H[&as_of=T]` proves a block is an
ancestor of a later one through the header's Merkle Mountain Range, in
logarithmic size (needs the `historyroot` upgrade active at both heights).

Name service: `/names` (every name the chain holds) and `/name/{name}` (resolve a
name to its address, returning the state key so the answer can be checked with
`/stateproof/{key}` or `/nameproof/{name}`).

Charts and ranking: `/series?from=H&limit=N` (or `?last=N`) is per-height
difficulty, interval, base fee and fee flow — `/chainstats` summarizes a window
into single numbers, which cannot show a quantity *moving*. `/richlist?limit=N`
ranks the largest holders straight from resident account state, bounded to a
top-N so one request cannot sort a whole ledger.

Operator endpoints: `/peers` (every connection in detail — identity, negotiated
version, capabilities, direction, uptime, ban score, whether a block request is
outstanding), `/bans` (scored keys *including* those below the threshold, plus
the threshold itself), `POST /unban`, `POST /addpeer`, `POST /droppeer`,
`/chainstats?window=N` (estimated hashrate, block timing against the target, fee
flow, miners), `/reorgs` (the chain switches this node has lived through), and
`/health` — which answers **503 with reasons** when the node is not usable.
`/info` cannot serve that purpose: it answers 200 while syncing, un-peered, or on
a tip that stopped moving hours ago. `/deployments` reports each BIP9 versionbits
deployment — miner signal counts and the state the chain has taken it to — and
`/pool` the PPLNS payout accounting (who has a claim on the next block found).

`GET /audit` re-derives the ledger (rebuilds the state trie over every account
and re-checks the supply identity) independently of the incremental committer,
and answers **500** when it disagrees with what the chain commits. It is
**token-guarded despite being a read**: it is O(ledger), strictly more expensive
than `/snapshot`. Its *result* is public through `/health` and `/metrics`
(`dnas_audit_*`); running one on demand is the operator's call.

`GET /openapi.json` is the API's OpenAPI 3.1 document, generated from the same
route table (`routes.go`) that registers the handlers, so it cannot describe an
endpoint that is not served or miss one that is.

Event stream: `GET /events` is a Server-Sent Events stream that pushes a small
JSON envelope on every new block, reorg, and mempool transaction (fed by the
node's pub/sub bus).

Write endpoints: `POST /send` (`{"to","amount","fee","expiry"?,"lock_until"?,"memo"?,"nonce"?}`,
signed by the node's wallet — set `nonce`+higher `fee` to fee-bump; or
`{"outputs":[{"to","amount"},…]}` instead of `to`/`amount` to pay several
recipients in one transaction, every address checksum-validated before signing),
`POST /tx`
(submit a fully-signed transaction, including multisig, HTLC, a vault spend, a
fee-sponsored transfer, a native-asset transfer, or an issuance), `POST /mine`
(`{"on":bool}`, toggle mining at runtime), `POST /generate` (`{"n":N}`, **regtest
only**: mine N blocks on demand), and `POST /faucet` (`{"address":…}`, testnet or
regtest only, and only when the node was started with `-faucet` — it pays out of
the node's own wallet, rate-limited per recipient and per client IP). When
`DNAS_API_TOKEN` is set these require an `Authorization: Bearer <token>` header
(constant-time compared); reads stay open. `api.New(n)` reads the env var,
`api.NewWithToken(n, token)` sets it explicitly (used in tests), and
`Server.AuthEnabled()` reports whether it is on.

External mining: `GET /blocktemplate?address=ADDR` returns a candidate block
(every field but the winning nonce) and `POST /submitblock` accepts an
externally-mined block, so an off-node miner (`dnas miner`) can hash and submit
without the node mining itself. `/submitblock` is guarded like the other writes.

Two things make that efficient rather than merely possible. The template request
**long polls** with `&longpoll=1&prev=HASH[&timeout=S]`: it does not answer until
the tip moves off `HASH`, so a miner never burns time on a candidate that is
already dead (a `prev` the node has passed answers immediately, so a miner cannot
be parked). And the template carries `share_bits`, a target several hundred times
easier than the block's: `POST /submitshare` accepts hashes meeting it and
`GET /shares` reports who has been submitting work. A share that also clears the
real target is accepted as the block it is. Shares are pool accounting, never
consensus — nothing is written to the chain.

Stateless wallet helpers (compute-only; no node state or secrets touched):
`POST /multisig/address` (`{"threshold","pubkeys":[…]}` → M-of-N address),
`POST /htlc/address` (`{"hash","recipient","sender","timeout"}` → HTLC contract
address), `POST /vault/address` (`{"hot","cold","unlock"}` → time-delayed vault
address), and `POST /wallet/hd` (`{"mnemonic"?,"passphrase"?,"count"?}` → a BIP39 mnemonic,
generated when omitted, plus the first `count` derived HD addresses).

The root path `/` serves a self-contained web explorer (`explorer.html`, embedded
via `//go:embed`): live status, blocks, mempool, a send form, and in-browser SPV
verification. `Handler()` returns the mux so tests can drive it with `httptest`.

See the root README for the full table. Depends on `core` and `node`.
