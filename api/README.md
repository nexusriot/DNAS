# api

Module `github.com/nexusriot/DNAS/api` — a small HTTP interface to a running
node.

Read endpoints: `/info`, `/chain`, `/balance/{addr}`, `/account/{addr}`,
`/mempool`, `/peers`, `/address`.

SPV / light-client endpoints: `/headers`, `/header/{index}`, `/proof/{txhash}`
(a merkle inclusion proof, verifiable against a header's merkle root).

Write endpoints: `POST /send` (`{"to","amount","fee","expiry"?,"nonce"?}`, signed
by the node's wallet — set `nonce`+higher `fee` to fee-bump) and `POST /tx`
(submit a fully-signed transaction).

See the root README for the full table. Depends on `core` and `node`.
