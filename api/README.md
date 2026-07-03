# api

Module `github.com/nexusriot/DNAS/api` — a small HTTP interface to a running
node.

Read endpoints: `/info` (includes `min_relay_fee`, the current dynamic fee
floor), `/chain`, `/balance/{addr}`, `/account/{addr}`, `/mempool`, `/peers`,
`/address`, `/metrics` (Prometheus format, incl. `dnas_min_relay_fee`).

SPV / light-client endpoints: `/headers`, `/header/{index}`, `/proof/{txhash}`
(a merkle inclusion proof, verifiable against a header's merkle root).

Write endpoints: `POST /send` (`{"to","amount","fee","expiry"?,"lock_until"?,"memo"?,"nonce"?}`,
signed by the node's wallet — set `nonce`+higher `fee` to fee-bump), `POST /tx`
(submit a fully-signed transaction, including multisig), and `POST /mine`
(`{"on":bool}`, toggle mining at runtime).

Stateless wallet helpers (compute-only; no node state or secrets touched):
`POST /multisig/address` (`{"threshold","pubkeys":[…]}` → M-of-N address) and
`POST /wallet/hd` (`{"mnemonic"?,"passphrase"?,"count"?}` → a BIP39 mnemonic,
generated when omitted, plus the first `count` derived HD addresses).

The root path `/` serves a self-contained web explorer (`explorer.html`, embedded
via `//go:embed`): live status, blocks, mempool, a send form, and in-browser SPV
verification. `Handler()` returns the mux so tests can drive it with `httptest`.

See the root README for the full table. Depends on `core` and `node`.
