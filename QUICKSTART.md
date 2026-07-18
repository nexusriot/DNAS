# DNAS quickstart

Everything you need to build DNAS, mine coins, run a wallet, send a payment,
form a network, and verify transactions. It's a toy PoW cryptocurrency — a
friendly devnet, not money.

## 0. Build

The quickest way is `make` (stamps a version from git, writes to `bin/`):

```sh
cd ~/workspace/my/DNAS
make build            # -> bin/dnas and bin/dnas-tui
./bin/dnas version
make help             # list all targets: test, dist, deb, install, …
```

The repo is a Go multi-module workspace, so you can also build the binary by
path (a bare `go build ./...` from the root won't match the sub-modules):

```sh
go build -o dnas ./cmd/dnas
./dnas help
```

Amounts: **1 DNAS = 100 000 000 base units**. The interactive REPL takes decimal
DNAS (`1.5`); the HTTP API takes integer base units (`150000000`).

## 1. Run a mining node (get coins)

```sh
mkdir -p ~/dnas-a && cd ~/dnas-a
dnas node -listen :3000 -api :8080 -mine
```

On first run it creates `wallet.json` (your keys) and `chain.db` (the append-only
chain) in the current directory. `-mine` starts mining; the block reward
(50 DNAS, halving every 210 000 blocks) is paid to this node's wallet. A block
is produced roughly every few seconds, so your balance climbs by 50 DNAS/block.

Each reward is **immature** until 3 more blocks are mined on top of it (coinbase
maturity), so it can't be spent immediately — `/balance` shows the full balance
while a brand-new reward is not yet spendable. This protects against spending a
reward that a reorg later removes.

Because you launched it in a terminal, you also get a REPL:

```
dnas> address
dnas> info                 # height, difficulty, cumulative work, mempool, peers
dnas> balance              # your node wallet's balance (grows as you mine)
```

Leave it running (mining continues) and use another terminal, or drive it over
the HTTP API below.

## 2. Wallet management

```sh
dnas wallet new -o alice.json          # create a key file, prints the address
dnas wallet address -o alice.json      # print an existing wallet's address
dnas wallet pubkey -o alice.json       # print its public key (to build multisig/HTLC scripts)
```

Encrypt the key file at rest (PBKDF2 + AES-256-GCM). Set the passphrase in the
environment (kept out of `ps`); it's honored by `wallet new/address` and by
`node`:

```sh
export DNAS_WALLET_PASSPHRASE='correct horse battery staple'
dnas wallet new -o alice.json          # now written encrypted
```

Addresses carry a checksum, so a mistyped recipient is rejected instead of
burning coins.

Back up a wallet as a BIP39 mnemonic (it derives many HD addresses), or build a
multisig address:

```sh
dnas wallet mnemonic -o alice.json     # create wallet + print a 12-word backup
echo "<phrase>" | dnas wallet addresses -n 5    # list the first 5 HD addresses
echo "<phrase>" | dnas wallet restore -o alice.json -index 0   # rebuild from backup

# an M-of-N multisig address (fund it like any address; spend via POST /tx)
dnas wallet multisig -threshold 2 -pubkeys <pk1>,<pk2>,<pk3>
```

A running node also exposes these as stateless HTTP helpers (handy for the GUI/TUI
and scripts — the node stores nothing and holds no secret):

```sh
# derive a multisig address
curl -s -X POST localhost:8080/multisig/address \
  -d '{"threshold":2,"pubkeys":["<pk1>","<pk2>","<pk3>"]}'

# generate a new HD wallet (omit "mnemonic") or restore one, listing addresses
curl -s -X POST localhost:8080/wallet/hd -d '{"count":5}'
curl -s -X POST localhost:8080/wallet/hd -d '{"mnemonic":"<phrase>","count":5}'
```

## 3. Send a payment

**From the REPL** (amounts in decimal DNAS):

```
dnas> send <recipient-address> 3 0.1          # send 3 DNAS with a 0.1 fee
dnas> send <recipient-address> 3 0.1 500      # ...that expires at height 500
dnas> mempool                                  # pending transactions
```

**Over the HTTP API** (amounts in base units), signed by the node's wallet:

```sh
curl -s -X POST localhost:8080/send \
  -d '{"to":"dnas...","amount":300000000,"fee":10000000}'
```

Optional fields on `/send`:

- `"expiry": <height>` — drop the tx if not mined by this height.
- `"lock_until": <height>` — not valid before this height (time-lock).
- `"memo": "..."` — attach a short note (≤256 bytes).
- `"nonce": <n>` — override the auto nonce; **fee-bump** a stuck tx by resending
  at its nonce with a higher fee (replace-by-fee).

Fees are priced **per byte** and have two layers. Every block carries a
**consensus base fee** (EIP-1559 style): each transaction must pay at least
`base fee × its size`, and that part is **burned** (removed from supply); the miner
keeps only the tip (`fee − base fee × size`). The base fee rises when blocks fill
and decays when idle, and a block is bounded by a byte budget. Separately, a node
won't *relay* a transaction paying below its **dynamic minimum relay fee**
(`-minrelayfee`, a per-byte rate, default 10, which also rises with mempool load).
See both in `/info` (`base_fee`, `min_relay_fee`, both per byte), or ask for a
recommended per-byte rate (multiply by your tx size):

```sh
curl -s 'localhost:8080/estimatefee?blocks=3'   # -> {base_fee, tip, fee} (per byte)
```

The transfer is signed, gossiped to peers, and confirmed when a miner includes
it in a block.

## 4. Inspect the chain (HTTP API)

```sh
curl -s localhost:8080/info                    # chain status
curl -s localhost:8080/balance/dnas...          # balance (raw + formatted)
curl -s localhost:8080/account/dnas...          # balance + nonce
curl -s localhost:8080/chain                    # full chain
curl -s localhost:8080/mempool                  # pending txs
curl -s localhost:8080/peers                    # connected peers
curl -s localhost:8080/estimatefee              # recommended fee (base fee + tip)
curl -s localhost:8080/stateproof/dnas...       # proof of an address's balance vs the state root
curl -sN localhost:8080/events                  # live stream (SSE): new blocks / reorgs / txs
```

## 5. Form a network

Start a second node that connects to the first. Peers must share the same
`-netkey` (default `dnas-devnet`); each needs its own ports and data dir:

```sh
mkdir -p ~/dnas-b && cd ~/dnas-b
dnas node -listen :3001 -api :8081 -peers localhost:3000 -mine
```

The two nodes authenticate (encrypted, identity-signed handshake), sync
headers-first, and gossip blocks and transactions. Peer **discovery** means a
node seeded with just one peer learns the rest of the network automatically.
Coins mined or received on one node appear on all of them once they sync.

Useful node flags: `-advertise` (address peers should dial you at, for multi-host),
`-maxpeers`, `-mempool`, `-minrelayfee` (base relay fee in base units **per byte**;
0 disables the floor), `-checkpoints height:hash,…` (pin finality checkpoints),
`-wallet FILE`, `-db FILE`, `-netkey KEY`.

## 6. Web explorer

Every node serves a self-contained explorer at its API root — open it in a
browser:

```
http://localhost:8080/
```

Live status, blocks (click to expand transactions), the mempool, a send form for
the node wallet, and an in-browser **SPV verifier**: paste a transaction hash and
it fetches the proof, folds the merkle path, and checks the block header's PoW —
proving inclusion without downloading block bodies.

## 7. Verify a payment like a light client (SPV)

Given a transaction hash (returned by `/send`):

```sh
curl -s localhost:8080/proof/<txhash>     # merkle inclusion proof + block info
curl -s localhost:8080/header/<index>     # that block's header (trusted root)
curl -s localhost:8080/headers            # all headers (verify the PoW chain)
```

A light client verifies the header chain's proof-of-work, then folds the merkle
proof to the header's merkle root — no full node required. `scripts/demo.sh`
shows this end to end with a small Python verifier.

## 8. See it all at once

```sh
./scripts/demo.sh
```

Spins up a three-node network and demonstrates: rejecting a wrong-`netkey` node,
peer discovery, a signed transfer converging on every node, transaction expiry,
the dynamic fee floor, deriving a multisig address, generating an HD wallet, and
light-client SPV verification. `./scripts/htlc-demo.sh` is a focused companion
that settles one hash-time-locked contract via the claim (preimage) path and one
via the refund (timeout) path.

## 8b. Verify a payment from the command line (light client)

The bundled light client trusts only headers (which it PoW-verifies) — no full
node. Beyond proving a payment is *included*, it can prove *non*-inclusion (via
BIP158 compact filters) and prove an address's *balance* (via the header state
root):

```sh
dnas spv -api localhost:8080 sync             # verify the header chain, print tip + work
dnas spv -api localhost:8080 verify <txhash>  # prove a payment IS in the chain
dnas spv -api localhost:8080 scan <address>   # which blocks touch it (+ prove the rest don't)
dnas spv -api localhost:8080 balance <address># PROVE its balance against the state root
dnas spv -api localhost:8080 history <address># reconstruct its transactions (a real light wallet)
```

For a **persistent** light wallet that remembers what it watches and stays in
sync, use `dnas spv wallet` (state lives in `spvwallet.json`, or pass `-f FILE`):

```sh
dnas spv -api localhost:8080 wallet add <address>    # watch an address; scans + prints its history
dnas spv -api localhost:8080 wallet update           # incremental sync (only new flagged blocks)
dnas spv -api localhost:8080 wallet update -watch    # follow /events and re-sync on each block
dnas spv -api localhost:8080 wallet status           # print watched balances without syncing
dnas spv -api localhost:8080 wallet list | forget <address>
```

It downloads only the blocks a compact filter flags for a watched address,
authenticates each against its PoW-verified header, and detects reorgs — never
trusting a served balance or downloading the whole chain.

With a **key file** it becomes self-custodial — it signs locally (the key never
leaves the client) and submits only the signed transaction:

```sh
dnas spv -api localhost:8080 wallet -key lw.json new              # create + watch own address
dnas spv -api localhost:8080 wallet -key lw.json send <addr> 5    # prove balance/nonce, sign, submit
```

## 8b2. Fast-sync from a snapshot (skip replaying history)

A new node can bootstrap from a recent trusted point instead of replaying the
whole chain. It fetches the account state at a (checkpoint) height, verifies it
against that header's committed **state root** (and an optional pinned checkpoint),
then downloads and fully validates only the blocks above it — reaching the same
cumulative work, balances proven rather than trusted:

```sh
dnas fastsync -api localhost:8080 -checkpoint 20:<hash> <addr>   # trustless bootstrap + report balances
dnas fastsync -api localhost:8080                               # from the server's latest safe height
```

## 8c. Ops: config file, metrics, clean shutdown

```sh
dnas node -config node.json            # JSON config seeds flags; flags still override
curl -s localhost:8080/metrics         # Prometheus-format node metrics
# Ctrl-C / SIGTERM stops mining, closes peers, and flushes the store cleanly.
```

`node.json` keys mirror the flags, e.g. `{"listen":":3000","api":":8080","mine":true,"maxpeers":8}`.

## 8d. Regtest: mine blocks on demand

Waiting ~5 s per block is tedious for testing. `-regtest` mines only when asked
(and defaults to an isolated network key so it won't peer with a devnet):

```sh
dnas node -regtest -api :8080 &
curl -s -X POST localhost:8080/generate -d '{"n":10}'   # mine 10 blocks instantly
```

## 8e. Hash-time-locked contracts (atomic swaps)

An HTLC address is spendable two ways: by the recipient revealing a preimage
(claim, any time) or by the sender after a timeout height (refund) — the building
block of cross-chain atomic swaps. Revealing the preimage on-chain is what lets a
counterparty claim the mirror on another chain.

```sh
dnas htlc new                                                 # mint a preimage + its hash
dnas htlc address -hash H -recipient R -sender S -timeout T   # derive the contract address
# fund the address with a normal /send, then spend one branch:
dnas htlc claim  -wallet alice.json -hash H -sender S    -timeout T -preimage P -to <addr>
dnas htlc refund -wallet bob.json   -hash H -recipient R -timeout T            -to <addr>
```

Run `./scripts/htlc-demo.sh` to watch both branches settle.

## 8f. Lock down the API

By default the API is fully open (a localhost toy). Set a token to require it on
the **write** endpoints (`/send`, `/tx`, `/mine`, `/generate`); reads stay open,
and all bundled clients send it automatically from the same env var:

```sh
export DNAS_API_TOKEN='a-long-random-secret'
dnas node -api :8080 -mine
curl -s -H "Authorization: Bearer $DNAS_API_TOKEN" \
  -X POST localhost:8080/mine -d '{"on":false}'
```

## 9. Desktop / terminal clients

Prefer a UI over curl? Two clients wrap the same API (status, balance, send, SPV
verify, mining toggle), and each can launch a local node so mining is turnkey:

```sh
# Terminal UI (Go / bubbletea)
cd tui && go build -o dnas-tui . && ./dnas-tui -api localhost:8080
#   keys: s=send  v=verify  m=toggle mining  x=multisig  h=HD wallet  r=refresh  q=quit
#   or: ./dnas-tui -spawn      # launches a local node for you

# Desktop GUI (Python / PyQt6)
python3 gui/dnas_gui.py --api localhost:8080
#   includes a "Wallet tools" panel for multisig addresses and HD/BIP39 wallets
```

Both surface the multisig and HD/BIP39 wallet helpers (previously CLI/API-only).

## 10. Files, persistence, reset

- `wallet.json` — your Ed25519 keys (encrypt with `DNAS_WALLET_PASSPHRASE`).
- `chain.db` — the append-only chain; it survives restarts, and a node re-syncs
  anything it's missing from peers.
- `peers.json`, `bans.json`, `mempool.json` — soft state written beside `chain.db`
  on graceful shutdown and reloaded on start, so a restart resumes warm. The chain
  stays authoritative; these are conveniences (re-learned from peers if lost).

To start clean, stop the node and delete `chain.db` (keep `wallet.json` to keep
your address). Different networks are separated by `-netkey`. Note: the block
header format evolves as the project gains features, so a `chain.db` from an older
build may be rejected as incompatible — delete it to start a fresh chain.
