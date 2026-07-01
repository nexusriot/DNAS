# node

Module `github.com/nexusriot/DNAS/node` — the peer-to-peer daemon.

- `Node` / `Config` — ties the chain, mempool and wallet to the network and the
  miner. Handles block/transaction gossip, chain sync and most-work consensus.
- `secure.go` — `secureConn`: a PSK-authenticated X25519 handshake and
  AES-256-GCM encrypted, length-framed message stream. `json.Encoder`/`Decoder`
  run over it transparently.
- `peerbook.go` — known-peer bookkeeping for discovery: dedup, self-exclusion
  and an outbound-dial cap.
- `seen.go` — bounded FIFO `seenSet` for gossip de-duplication.
- `protocol.go` — the JSON message envelope and message types
  (`hello`, `getchain`/`chain`, `block`, `tx`, `getpeers`/`peers`).

Depends on `core` and `wallet`.
