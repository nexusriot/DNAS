# node

Module `github.com/nexusriot/DNAS/node` — the peer-to-peer daemon.

- `Node` / `Config` — ties the chain, mempool and wallet to the network and the
  miner. Handles inventory-based block gossip, headers-first ranged sync,
  transaction gossip, discovery, and most-work consensus with reorgs.
- **Mining & regtest** — `buildBlock` assembles a candidate (selecting mempool
  txs, setting its `StateRoot` via `chain.NextStateRoot` and `BaseFee` via
  `chain.NextBaseFee`, and paying the coinbase `reward + tips` with the base fee
  burned) and `commitMined` applies and gossips it. A candidate whose state root
  cannot be computed is never hashed: the `core.TxRejection` names the offending
  transaction, which is dropped from the mempool before reassembling, so one
  unmineable transaction cannot stop block production; both are shared by the miner
  loop and `Node.Generate`. The nonce search aborts when the tip moves (rebuild on
  the winner) and an idle miner wakes on a new mempool transaction. A block with no
  transactions in it waits `Config.EmptyBlockInterval` first (default one
  `TargetBlockTime`) so an idle network isn't flooded with empty blocks — local
  mining policy, not consensus, which is why a devnet or test can lower it. In
  **regtest** mode (`Config.Regtest`, `Node.Regtest`) `Node.Generate(n)` mines N
  blocks immediately to the node wallet — bypassing the interval and the mining
  toggle — the primitive behind `POST /generate`.
- `sync.go` — sync liveness. Every ranged block request is recorded against the
  peer it went to; a `syncLoop` tick drops (and ban-scores) a peer that has not
  answered within `blockRequestTimeout` and frees its slot, tracks the highest
  height any peer has announced so it knows when it is behind, and keeps up to
  `maxSyncPeers` ranges in flight across different peers. Without this a peer that
  answers pings while serving no bodies stalls catch-up indefinitely.
- `identity.go` — the node's NETWORK identity key, deliberately separate from
  the wallet: `MsgIdentity` carries the identity's public key and a DNAS address
  is a hash of a public key, so sharing them publishes the address holding the
  node's coin (and undercuts the Dandelion++ origin privacy below). Defaults to
  `nodekey.json` beside the chain; a node given none falls back to the wallet and
  warns.
- `peerinfo.go` — `Peers()` reports every live connection in full (identity,
  version, capabilities, direction, uptime, ban score, whether a block request is
  outstanding); `Bans()`/`Unban()` expose and clear the scores that were
  previously kept, persisted and unreachable; `AddPeer()`/`DropPeer()` manage
  connections at runtime.
- `reorghist.go` — a bounded ring of the chain switches this node has lived
  through (depth, fork height, both tips, transactions re-queued) plus lifetime
  counters. The event stream announces a reorg once and forgets it.
- `logging.go` — log levels (`error`/`warn`/`info`/`debug`) and an optional
  one-JSON-object-per-line form, wrapping the standard logger so any remaining
  `log.Printf` still lands at info.
- **Self-connection detection** — a handshake that returns our own identity is
  closed and the address recorded as a self-alias (`peerbook.noteSelf`), so it is
  never dialed or gossiped again. Addresses cannot settle this: a node
  advertising `:3000` is reached as `localhost:3000`.
- `shares.go` — the pool-facing half of external mining. `WaitForTip` powers the
  template **long poll** (`/blocktemplate?longpoll=1&prev=HASH`), so a miner starts
  on a fresh candidate the moment the old one dies instead of hashing a dead one;
  `SubmitShare` accepts a candidate that met the easier **share** target, credits
  its coinbase address in a ledger (`Shares`), and submits it as a block if it also
  met the real target. None of it is consensus — the ledger is node-local and
  nothing is written to the chain.
- `faucet.go` — hands out coin from the node's own wallet on a network whose
  parameters permit one (never mainnet, by definition rather than by policy), and
  only when the operator sets `Config.Faucet`. Rate-limited per recipient and per
  requester, with a refused request stamping nobody's cooldown.
- **Mempool reconciliation** — a node asks each peer once, via `MsgGetMempool`, for
  the transactions it has pending, so one that was down when a payment was
  broadcast learns of it on connect. The request waits until catch-up completes:
  admission is checked against confirmed state, so a node still syncing would
  reject everything it was told.
- `orphan.go` — `orphanPool`: bounded, `SelfValid`-gated buffer of blocks whose
  parent has not arrived yet (gossip races, and out-of-order parallel ranges),
  connected as soon as the parent lands instead of being refetched.
- **Lifecycle** — `Start` launches the accept loop, a dial loop per known peer, the
  sync loop and the miner; `Shutdown` closes an internal quit channel they all select on (plus the
  listener, to unblock `Accept`), so a stopped node stops mining, accepting and
  redialing rather than leaving loops running. It is idempotent, so a daemon that
  shuts down on a signal and a test that registers it as cleanup can both call it.
- `secure.go` — `secureConn`: an X25519 handshake + AES-256-GCM encrypted,
  length-framed message stream. **Permissionless by default** — with no network key
  the handshake is anonymous (anyone may connect); a non-empty key authenticates a
  private net (HMAC-gated). Returns a session id used to bind node identities.
- Node identity: after the encrypted handshake, each side signs the session id
  with its Ed25519 key (`MsgIdentity`) to prove who it is (open net or not).
- Eclipse/DoS resistance: inbound connections are admitted (`admitInbound`) under
  a total and per-IP-group cap (loopback exempt); each peer read loop runs a
  token-bucket `rateLimiter` (`ratelimit.go`) and the whole-chain request is
  throttled per peer. The orphan pool is bounded in **bytes** as well as in
  count (`orphan.go`): 200 maximum-size blocks is half a gigabyte, and an
  orphan's proof of work is cheap on a low-difficulty fork.
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
- `webhook.go` — `-webhook URL` POSTs every event to a service that wants to be
  *called* rather than hold an SSE connection open. Delivery runs off a bounded
  queue on its own goroutine and never blocks block processing (a receiver far
  enough behind loses events); 5xx and transport errors are retried with a
  growing delay, a 4xx is not, and every delivery carries the network and the
  node's height so a receiver can spot a gap. `WebhookStats` feeds `/webhooks`,
  because a silently failing webhook is otherwise invisible from outside.
- **Dial backoff** — a failed dial doubles its retry interval up to
  `dialRetryMax` (5 minutes), and an established connection resets it. A dead
  seed address was previously retried every 3 seconds for the node's whole
  uptime: 1200 attempts an hour, per address, which at the far end looks exactly
  like being scanned.
- `persist.go` — when `Config.StateDir` is set, the node persists `peers.json`,
  `bans.json`, and `mempool.json` there, loading them on `Start` and rewriting
  them on graceful `Shutdown` (via temp-file+rename). The chain stays
  authoritative and re-syncs from peers regardless.

Depends on `core` and `wallet`.
