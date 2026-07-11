# node

Module `github.com/nexusriot/DNAS/node` — the peer-to-peer daemon.

- `Node` / `Config` — ties the chain, mempool and wallet to the network and the
  miner. Handles inventory-based block gossip, headers-first ranged sync,
  transaction gossip, discovery, and most-work consensus with reorgs.
- **Mining & regtest** — `buildBlock` assembles a candidate (selecting mempool
  txs, setting its `StateRoot` via `chain.NextStateRoot` and `BaseFee` via
  `chain.NextBaseFee`, and paying the coinbase `reward + tips` with the base fee
  burned) and `commitMined` applies and gossips it; both are shared by the miner
  loop and `Node.Generate`. In **regtest** mode (`Config.Regtest`, `Node.Regtest`)
  `Node.Generate(n)` mines N blocks immediately to the node wallet — bypassing the
  idle interval and the mining toggle — the primitive behind `POST /generate`.
- `secure.go` — `secureConn`: a PSK-authenticated X25519 handshake and
  AES-256-GCM encrypted, length-framed message stream (`json.Encoder`/`Decoder`
  run over it transparently). Returns a session id used to bind node identities.
- Node identity: after the encrypted handshake, each side signs the session id
  with its Ed25519 key (`MsgIdentity`) to prove who it is.
- `ban.go` — `banbook`: ban scoring keyed by peer identity for misbehaviour.
- `peerbook.go` — known-peer bookkeeping for discovery: dedup, self-exclusion
  and an outbound-dial cap.
- `seen.go` — bounded FIFO `seenSet` for gossip de-duplication.
- Fork sync uses a **block locator**: `getheaders` carries locator hashes, the
  peer replies from the last common block, and only the divergent suffix is
  pulled and applied via `ReorgFrom`. `getchain`/`chain` remains a deep-fork /
  bootstrap fallback.
- `protocol.go` — the JSON message envelope and message types: `identity`,
  `hello`, `getpeers`/`peers`, `tx`, `inv`/`getdata`/`block`,
  `getheaders`/`headers` (+locator), `getblocks`/`blocks`, and `getchain`/`chain`.
- `events.go` — a tiny in-process publish/subscribe bus. `Node.Subscribe()`
  hands out a buffered `<-chan Event` plus an unsubscribe func; an
  `Event{Type: "block"|"reorg"|"tx", …}` is emitted on mined/accepted/synced
  blocks, reorgs, and mempool txs. A slow subscriber drops events rather than
  blocking the node. The API's `/events` SSE endpoint consumes it.
- `persist.go` — when `Config.StateDir` is set, the node persists `peers.json`,
  `bans.json`, and `mempool.json` there, loading them on `Start` and rewriting
  them on graceful `Shutdown` (via temp-file+rename). The chain stays
  authoritative and re-syncs from peers regardless.

Depends on `core` and `wallet`.
