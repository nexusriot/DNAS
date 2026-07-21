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
- `secure.go` — `secureConn`: an X25519 handshake + AES-256-GCM encrypted,
  length-framed message stream. **Permissionless by default** — with no network key
  the handshake is anonymous (anyone may connect); a non-empty key authenticates a
  private net (HMAC-gated). Returns a session id used to bind node identities.
- Node identity: after the encrypted handshake, each side signs the session id
  with its Ed25519 key (`MsgIdentity`) to prove who it is (open net or not).
- Eclipse/DoS resistance: inbound connections are admitted (`admitInbound`) under
  a total and per-IP-group cap (loopback exempt); each peer read loop runs a
  token-bucket `rateLimiter` (`ratelimit.go`) and the whole-chain request is
  throttled per peer.
- `ban.go` — `banbook`: ban scoring. Failed handshakes are scored by IP (loopback
  exempt); post-authentication fraud (bad header chains, or blocks failing their own
  `SelfValid` PoW/merkle check) by identity. A plain fork is not penalised.
- Keepalive: an established connection is pinged (`ping`/`pong`) and dropped if it
  falls silent past an idle read deadline, so a dead peer doesn't leak a slot.
- `peerbook.go` — known-peer bookkeeping for discovery: dedup, self-exclusion
  and an outbound-dial cap.
- `seen.go` — bounded FIFO `seenSet` for gossip de-duplication.
- Fork sync uses a **block locator**: `getheaders` carries locator hashes, the
  peer replies from the last common block, and only the divergent suffix is
  pulled and applied via `ReorgFrom`. `getchain`/`chain` remains a deep-fork /
  bootstrap fallback.
- `protocol.go` — the JSON message envelope and message types: `identity`,
  `version` (protocol version + capability negotiation), `hello`,
  `getpeers`/`peers`, `tx` (with a Dandelion++ `stem` flag),
  `inv`/`getdata`/`block`, `getheaders`/`headers` (+locator),
  `getblocks`/`blocks`, `getchain`/`chain`, and `ping`/`pong` (keepalive).
- `dandelion.go` — Dandelion++ transaction relay: a new tx is forwarded down a
  private **stem** to one epoch-stable successor, then **fluffs** into a normal
  broadcast (by chance, on embargo timeout, or when no capable successor exists),
  hiding its origin. `relayTx`/`fluff`/`stemSuccessor` in `node.go` drive it.
- **External mining:** `BuildTemplate(addr)` returns a candidate block for an
  off-node miner and `SubmitMinedBlock` validates+appends a mined one (behind the
  `/blocktemplate` and `/submitblock` API endpoints; `dnas miner` is the client).
- **Injectable transport:** `Node.dialFn`/`listenFn` default to TCP but let tests
  drive nodes over an in-memory switchboard with latency and partitions
  (`simnet_test.go`), for adversarial reorg/finality/sync convergence testing.
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
