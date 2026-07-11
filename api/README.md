# api

Module `github.com/nexusriot/DNAS/api` — a small HTTP interface to a running
node.

Read endpoints: `/info` (includes `min_relay_fee`, the current dynamic fee
floor, and the consensus `base_fee`), `/chain`, `/balance/{addr}`,
`/account/{addr}`, `/mempool`, `/peers`, `/address`, `/estimatefee?blocks=N`
(recommended fee = base fee + estimated tip, never below the relay floor),
`/metrics` (Prometheus format, incl. `dnas_min_relay_fee` and `dnas_base_fee`).

SPV / light-client endpoints: `/headers`, `/header/{index}`, `/block/{index}`
(one full block body, so a light client downloads only filter-flagged blocks),
`/proof/{txhash}` (a merkle inclusion proof, verifiable against a header's merkle
root), and `/stateproof/{addr}` (an account balance/nonce proof against the
header state root; 404 for an absent account), plus the compact block filters
`/cfilters`, `/cfilter/{index}`, and the BIP157-style filter-header chain
`/cfheaders` for non-inclusion scans.

Event stream: `GET /events` is a Server-Sent Events stream that pushes a small
JSON envelope on every new block, reorg, and mempool transaction (fed by the
node's pub/sub bus).

Write endpoints: `POST /send` (`{"to","amount","fee","expiry"?,"lock_until"?,"memo"?,"nonce"?}`,
signed by the node's wallet — set `nonce`+higher `fee` to fee-bump), `POST /tx`
(submit a fully-signed transaction, including multisig or HTLC), `POST /mine`
(`{"on":bool}`, toggle mining at runtime), and `POST /generate` (`{"n":N}`,
**regtest only**: mine N blocks on demand). When `DNAS_API_TOKEN` is set these
require an `Authorization: Bearer <token>` header (constant-time compared);
reads stay open. `api.New(n)` reads the env var, `api.NewWithToken(n, token)` sets
it explicitly (used in tests), and `Server.AuthEnabled()` reports whether it is on.

Stateless wallet helpers (compute-only; no node state or secrets touched):
`POST /multisig/address` (`{"threshold","pubkeys":[…]}` → M-of-N address),
`POST /htlc/address` (`{"hash","recipient","sender","timeout"}` → HTLC contract
address), and `POST /wallet/hd` (`{"mnemonic"?,"passphrase"?,"count"?}` → a BIP39 mnemonic,
generated when omitted, plus the first `count` derived HD addresses).

The root path `/` serves a self-contained web explorer (`explorer.html`, embedded
via `//go:embed`): live status, blocks, mempool, a send form, and in-browser SPV
verification. `Handler()` returns the mux so tests can drive it with `httptest`.

See the root README for the full table. Depends on `core` and `node`.
