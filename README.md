# DNAS

**DNAS** (Definitely Not A Scam) — a small but *working* proof-of-work
cryptocurrency in Go. It has real ownership (Ed25519 signatures), real issuance
(mining rewards), an account+nonce ledger, most-work consensus, a bounded
mempool, an authenticated + encrypted peer-to-peer network with peer discovery,
JSON persistence and an HTTP API.

It is a learning project, not money. Do not point it at the internet.

## What makes it a cryptocurrency (not just a hash-chain)

- **Ownership by signature.** An address is `dnas` + the first 20 bytes of
  `sha256(pubkey)`. Only the holder of the Ed25519 private key can sign a
  transaction that spends from that address. Forged senders are rejected.
- **Issuance.** New coins are created only by the coinbase transaction in each
  block, paying the miner `reward + fees`. The reward starts at 50 DNAS and
  halves every 210 000 blocks.
- **No double-spends / no replay.** Amounts are integer base units
  (1 DNAS = 100 000 000 units). Each account has a running balance and a nonce;
  a transaction must use the sender's next nonce and cannot spend more than the
  balance.
- **Transaction expiry & fee-bumping.** A transaction may carry a signed
  `Expiry` — the highest block height at which it may be mined. Past that height
  it is invalid and is dropped from mempools. A stuck (low-fee) transaction can
  be replaced by re-sending it at the *same nonce* with a strictly higher fee
  (replace-by-fee).
- **Light-client proofs (SPV).** Block hashes commit only to header fields plus
  a merkle root, so a client with headers alone can verify proof-of-work and the
  hash chain, then confirm a transaction is included via a compact merkle proof
  — without downloading block bodies.
- **Deterministic genesis.** Every node computes the same genesis block, so
  independent nodes can actually agree on one chain.
- **Most-work consensus.** Proof of work (leading-zero hash) with a
  deterministic difficulty retarget. Peers adopt the chain with the greatest
  cumulative work (Σ 16^difficulty), which — unlike a longest-chain rule —
  correctly prefers a shorter chain built on harder blocks.

## Networking & security

- **Authenticated, encrypted links.** Every connection begins with a symmetric
  X25519 key exchange authenticated by a pre-shared network key (`-netkey`): a
  peer that doesn't know the key fails an HMAC check and is dropped, and traffic
  is encrypted with AES-256-GCM. Peers on different keys can't talk.
- **Peer discovery.** Nodes gossip the addresses they know (`getpeers`/`peers`),
  so a node seeded with a single peer discovers the rest of the network and
  dials them, up to `-maxpeers`. Self-dials and duplicates are avoided.
- **Bounded memory.** The mempool is capped and evicts the lowest-fee
  transaction under pressure; the gossip de-duplication sets are bounded FIFOs,
  so a long-running node's memory does not grow without limit.

## Layout

DNAS is a **Go multi-module workspace**: each component is its own module
(`github.com/nexusriot/DNAS/<name>`), tied together by `go.work`. The repo root
is not itself a module.

```
go.work             workspace tying the modules together
wallet/             module .../wallet — Ed25519 keys, addresses, signing
core/               module .../core   — transactions, blocks, chain state, work, mempool   (→ wallet)
node/               module .../node   — encrypted P2P, discovery, gossip, sync, miner       (→ core, wallet)
api/                module .../api    — HTTP API                                            (→ core, node)
cmd/                module .../cmd    — CLI/daemon; main package in cmd/dnas                 (→ all)
scripts/demo.sh     three-node end-to-end demo
```

Dependency direction: `wallet → core → node → api → cmd` (no cycles). Each
module has its own `README.md`.

## Build & test

Because the root is not a module, build/test through the workspace with
directory paths (a bare `./...` from the root won't match the sub-modules):

```sh
# everything
go build ./api/... ./cmd/... ./core/... ./node/... ./wallet/...
go test  -race ./core/... ./node/... ./wallet/...

# just the binary
go build -o dnas ./cmd/dnas
```

## Run a node

```sh
# creates wallet.json and chain.json in the current dir if missing
go run ./cmd/dnas node -listen :3000 -api :8080 -mine
```

| Flag         | Default        | Meaning                                        |
|--------------|----------------|------------------------------------------------|
| `-listen`    | `:3000`        | p2p listen address                             |
| `-advertise` | = `-listen`    | address peers should dial us at                |
| `-api`       | `:8080`        | HTTP API address                               |
| `-peers`     | —              | comma-separated seed peer addresses            |
| `-wallet`    | `wallet.json`  | wallet key file (created if missing)           |
| `-db`        | `chain.json`   | blockchain storage file                        |
| `-netkey`    | `dnas-devnet`  | pre-shared network key; **peers must match**   |
| `-maxpeers`  | `8`            | maximum outbound peer connections              |
| `-mempool`   | `5000`         | max pending transactions                       |
| `-mine`      | off            | enable mining                                  |

When stdin is a terminal you also get a REPL:

```
dnas> address
dnas> send <address> <amount> [fee]
dnas> balance [address]
dnas> info | peers | mempool
```

A second node on the same machine just needs distinct ports and a peer:

```sh
go run ./cmd/dnas node -listen :3001 -api :8081 -peers localhost:3000 -mine \
  -wallet w2.json -db chain2.json
```

## HTTP API

| Method | Path              | Purpose                                                       |
|--------|-------------------|---------------------------------------------------------------|
| GET    | `/info`           | height, tip, next difficulty, cumulative work, mempool, peers |
| GET    | `/chain`          | the full chain                                                |
| GET    | `/balance/{addr}` | balance (raw + formatted)                                     |
| GET    | `/account/{addr}` | balance and nonce                                             |
| GET    | `/mempool`        | pending transactions                                          |
| GET    | `/peers`          | connected peers                                               |
| GET    | `/address`        | this node's wallet address                                    |
| GET    | `/headers`        | all block headers (SPV)                                       |
| GET    | `/header/{index}` | one block header (SPV)                                        |
| GET    | `/proof/{txhash}` | transaction-inclusion merkle proof (SPV)                      |
| POST   | `/send`           | `{"to","amount","fee","expiry"?,"nonce"?}` — signed by the node wallet |
| POST   | `/tx`             | submit a fully-signed transaction                             |

`/send` auto-selects the next nonce; pass an explicit `nonce` with a higher
`fee` to fee-bump a stuck transaction, and an `expiry` height to time-limit it.

```sh
curl -s localhost:8080/info
curl -s -X POST localhost:8080/send -d '{"to":"dnas...","amount":300000000,"fee":10000000}'

# SPV: fetch a proof and the header it commits to
curl -s localhost:8080/proof/<txhash>
curl -s localhost:8080/header/<index>
```

## Demo

`./scripts/demo.sh` builds the binary and starts a three-node network to show,
end to end:

- **auth/encryption** — a node started with the wrong `-netkey` is rejected;
- **discovery** — node 3 is seeded with node 1 only, yet finds node 2 by gossip;
- **consensus** — a signed transfer confirms on every node and heights/work
  converge;
- **expiry** — a transaction whose expiry is in the past is refused;
- **SPV** — an independent Python light client fetches a header + merkle proof
  and verifies the transfer's inclusion (header PoW + proof fold).

(Edit the `190xx` ports at the top of the script if they are taken.)

## Known limitations (it's a toy)

- Most-work fork choice, but with constant difficulty ties are common; nodes
  converge only once one chain becomes strictly heavier (no "first-seen"
  tie-break yet). Timestamps are only loosely validated.
- The pre-shared network key gives authentication and confidentiality but not
  per-node identity or an allow-list; there's no peer scoring or ban list.
- The fee market is just mempool eviction / replace-by-fee — no dynamic base
  fee. SPV proofs cover transaction *inclusion*, not non-inclusion or full
  state; there's no compact header-sync protocol (a client fetches all headers).
- Difficulty bounds are tiny so a laptop can mine instantly.
