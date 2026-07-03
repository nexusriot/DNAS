# node

Module `github.com/nexusriot/DNAS/node` — the peer-to-peer daemon.

- `Node` / `Config` — ties the chain, mempool and wallet to the network and the
  miner. Handles inventory-based block gossip, headers-first ranged sync,
  transaction gossip, discovery, and most-work consensus with reorgs.
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

Depends on `core` and `wallet`.
