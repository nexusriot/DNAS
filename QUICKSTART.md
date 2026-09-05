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

Amounts: **1 DNAS = 100 000 000 base units**. The interactive console takes decimal
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

Because you launched it in a terminal, you also get a console:

```
dnas> address
dnas> info                 # height, difficulty, base fee, mempool, peers, identity
dnas> balance              # your node wallet's coin, assets and nonce
dnas> help                 # everything it can answer (see §8d9)
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

**From the console** (amounts in decimal DNAS):

```
dnas> send <recipient-address> 3 0.1               # send 3 DNAS with a 0.1 fee
dnas> send <recipient-address> 3 fee=0.1 expiry=+20   # ...expiring 20 blocks from now
dnas> send <recipient-address> 3 memo="rent"       # a quoted memo keeps its spaces
dnas> mempool list                                 # pending transactions, with contents
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
curl -s localhost:8080/tx/<txhash>              # one tx: "pending" or "confirmed" + confirmations
curl -s localhost:8080/supply                   # minted / burned / circulating coin
curl -s localhost:8080/peers                    # connected peers
curl -s localhost:8080/estimatefee              # recommended fee (base fee + tip)
curl -s localhost:8080/stateproof/dnas...       # proof of an address's balance vs the state root
curl -sN localhost:8080/events                  # live stream (SSE): new blocks / reorgs / txs
```

## 5. Form a network

Start a second node that connects to the first. By default the network is
**open/permissionless** (no `-netkey` needed); to run a *private* network instead,
give every node the same `-netkey`. Each node needs its own ports and data dir:

```sh
mkdir -p ~/dnas-b && cd ~/dnas-b
dnas node -listen :3001 -api :8081 -peers localhost:3000 -mine
```

The two nodes authenticate (encrypted, identity-signed handshake), sync
headers-first, and gossip blocks and transactions. Peer **discovery** means a
node seeded with just one peer learns the rest of the network automatically.
Coins mined or received on one node appear on all of them once they sync.

Useful node flags: `-network` (`mainnet`, `testnet` or `regtest` — separate
chains, see §8c2), `-advertise` (address peers should dial you at, for
multi-host), `-maxpeers`, `-mempool`, `-minrelayfee` (base relay fee in base units
**per byte**; 0 disables the floor), `-checkpoints height:hash,…` (pin finality
checkpoints), `-upgrades name:height,…` (schedule consensus upgrades — set the
same values on every node), `-addrindex` (serve address history),
`-faucet` (hand out coin on a throwaway network), `-wallet FILE`, `-db FILE`,
`-netkey KEY`.

Peers must be on the **same network**: the network's id is part of the handshake,
so nodes on different ones disconnect immediately instead of trying (and failing)
to converge. Client commands (`dnas spv`, `htlc`, `vault`, `sponsor`, `miner`,
`fastsync`) read the network from the node they are pointed at, so they need no
flag — but they *do* need to reach the node, since a signature made for the wrong
network is invalid.

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

# The verified header chain is kept between runs, so a command downloads only
# what is new instead of re-fetching every header (tens of MB on a long chain).
dnas spv -api localhost:8080 -cache myheaders.json sync
dnas spv -api localhost:8080 -cache "" sync   # disable it and refetch everything
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

A payment can carry a memo and a height window — signed fields consensus has
always had and no client could set. An expiry is how a payment stops being an
open-ended liability: without one, a transaction signed today can be mined next
month at a nonce that has not moved, and the only way to take it back is to spend
that nonce on something else.

```sh
dnas spv -api localhost:8080 wallet -key lw.json \
    -memo "rent" -expire-in 20 -lock-for 1 send <addr> 1.5
#   -expire-in / -lock-for count blocks from the current tip; -expiry / -lock-until
#   take absolute heights. Both are resolved BEFORE signing, because a signature
#   has to commit to a specific window.
```

An expiry already in the past, an inverted window, or a memo over the consensus
limit are all refused before anything is signed — after signing, the wallet has
already advanced its own nonce and the node's answer is a bare rejection.

### Paying several people at once

One transaction can carry many recipients — one fee, one nonce, one signature
instead of N of each. Every address is checksum-validated before anything is
signed, so a typo in a batch of fifty fails loudly rather than sending coin
nowhere:

```sh
dnas spv -api localhost:8080 wallet -key lw.json sendmany \
    dnas<addr1>:1.5 dnas<addr2>:0.25 dnas<addr3>:2 [-fee 0.001]
```

Or over the API, with the node's own wallet signing:

```sh
curl -sX POST localhost:8080/send -d '{"outputs":[
  {"to":"dnas<addr1>","amount":150000000},
  {"to":"dnas<addr2>","amount":25000000}],"fee":1000000}'
```

Multi-recipient transfers are a **height-activated consensus rule**, so start
every node on the network with the same activation height — otherwise the ones
that have it enabled will mine blocks the others reject:

```sh
dnas node -upgrades multioutput:1000     # accepted from block 1000 onwards
```

The same applies to every upgrade this build knows: `dustlimit`, `multioutput`,
`vault` (§8d6) and `feesponsor` (§8d5). Pass them together, and identically, on
every node:

```sh
dnas node -upgrades multioutput:1000,vault:1000,feesponsor:1000
```

A misspelled name is refused at startup rather than silently never activating —
which, on a network where the others did activate, would mean being forked off
it.

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
dnas supply -api localhost:8080        # minted / burned / circulating + conservation check
# Ctrl-C / SIGTERM stops mining, closes peers, and flushes the store cleanly.
```

`dnas supply` is the audit view of issuance: coin is created only by block
subsidies and destroyed only by the burned base fee, so `minted − burned` must
equal what accounts actually hold. A `conservation BROKEN` line would mean coin
appeared or vanished some other way.

`node.json` keys mirror the flags, e.g. `{"listen":":3000","api":":8080","mine":true,"maxpeers":8}`.

## 8c2. Networks: mainnet, testnet, regtest

A node runs on exactly one network, and they are genuinely separate chains: the
network's id is baked into the genesis block, into every signature, and into the
peer handshake. A testnet signature is not valid on mainnet, and two nodes on
different networks hang up on each other instead of failing to converge forever.

```sh
dnas node -network testnet -api :8080 -mine    # a throwaway public-style chain
dnas node -network regtest -api :8081          # local, blocks on demand
dnas node -api :8082                           # mainnet (the default)
curl -s localhost:8080/info | grep network
```

Give each network its own data directory (`-db`, `-wallet`): a store from one is
refused by a node on another.

## 8d. Regtest: mine blocks on demand

Waiting ~5 s per block is tedious for testing. `-regtest` (shorthand for
`-network regtest`) mines only when asked, holds difficulty at the easy floor, and
defaults to an isolated network key:

```sh
dnas node -regtest -api :8080 &
curl -s -X POST localhost:8080/generate -d '{"n":10}'   # mine 10 blocks instantly
```

## 8d2. External mining (with long polling and shares)

Mining is decoupled from the node — point a separate miner at its API. The miner
fetches a block template, finds the winning nonce locally, and submits the block:

```sh
dnas node -api :8080 &                       # a node that doesn't mine itself
dnas miner -api localhost:8080 -address <your-address>          # mine continuously
dnas miner -api localhost:8080 -address <your-address> -once    # mine one block
```

By default the miner **long polls**: its next template request hangs until the tip
actually moves, so it never burns time hashing a candidate that is already dead.
Add `-shares` and it also reports the hashes that clear the node's easier *share*
target — proof it is working even when it has not found a block, which is what a
pool pays for:

```sh
dnas miner -api localhost:8080 -address <addr> -shares
curl -s localhost:8080/shares            # who has been submitting work, and how much
curl -s 'localhost:8080/blocktemplate?address=<addr>' | grep share_bits
```

Shares are pool accounting, not consensus: nothing is stored on chain, and a
share that happens to clear the real target is submitted as the block it is.

## 8d3. Native assets (tokens)

Issue and move a token; fees are always paid in coin, and balances are provable
against the state root like any coin balance:

```sh
# from a self-custodial light wallet with a funded key (see 8b):
dnas spv -api localhost:8080 wallet -key lw.json issue GOLD 1000      # mint 1000 GOLD
dnas spv -api localhost:8080 wallet -key lw.json -asset <id> send <addr> 250
dnas spv -api localhost:8080 balance <addr>                          # shows proven asset balances
curl -s localhost:8080/account/<addr>                                # includes an "assets" map
```

An asset id is a hash of (issuer, ticker, nonce), so a balance of `tok3f2a…`
tells you nothing on its own. The chain keeps a registry of what was issued:

```sh
dnas assets                        # every asset, with ticker, supply and issuer
dnas assets show <asset-id>        # one asset and who holds it, with percentages
dnas assets -ticker GOLD           # a LIST: anyone may issue "GOLD", so the id is
                                   # the identifier and the ticker is not
```

## 8d4. Free coins on a throwaway network (faucet)

A testnet or regtest node started with `-faucet` gives coin away from its own
wallet — so joining does not mean asking someone for a transfer. It is impossible
on mainnet by definition: the network's parameters, not a flag, decide whether a
faucet may exist.

```sh
dnas node -network testnet -api :8080 -mine -faucet &
dnas faucet -api localhost:8080 -address <your-address>
# or: curl -s -X POST localhost:8080/faucet -d '{"address":"dnas..."}'
```

One payout per address and per requesting client per cooldown (`-faucetcooldown`,
default 60 s); `-faucetamount` sets the size.

## 8d5. Someone else pays your fee

A transaction can name a **fee payer**: the sender signs who pays, that party
counter-signs the same bytes, and the fee comes out of *their* balance. An address
holding no coin at all can therefore make its first payment. It is a consensus
change, so it activates at a height:

```sh
dnas node -api :8080 -mine -upgrades feesponsor:0 &

# the sender builds and signs it, naming who will pay the fee
dnas sponsor request -api localhost:8080 -key broke.json \
  -to <recipient> -amount 0.5 -payer <payer-address> -o tx.json

# the payer reviews what they are agreeing to, counter-signs, and sends it
dnas sponsor pay -api localhost:8080 -wallet payer.json -in tx.json -submit
```

`tx.json` is not a bearer instrument: its sender, recipient, amount and nonce are
covered by the sender's signature, so the payer can only agree to it or not. The
sponsor spends no nonce of its own, so a sponsorship is bound to exactly one
transfer — it cannot be replayed or moved onto another payment.

## 8d6. Time-delayed vaults

A vault address has two keys: a **cold** key that can spend at any time, and a
**hot** key that can only spend from an unlock height on. Steal the hot key and
you must wait out the delay — long enough for the offline cold key to move the
coin somewhere safe.

```sh
dnas node -api :8080 -mine -upgrades vault:0 &
dnas wallet new -o hot.json  && dnas wallet pubkey -o hot.json    # HOT_PUB
dnas wallet new -o cold.json && dnas wallet pubkey -o cold.json   # COLD_PUB

dnas vault address -hot HOT_PUB -cold COLD_PUB -unlock 5000       # fund this address
dnas vault spend -wallet cold.json -hot HOT_PUB  -unlock 5000 -to <addr>   # works now
dnas vault spend -wallet hot.json  -cold COLD_PUB -unlock 5000 -to <addr>  # only from 5000
```

One wrinkle worth knowing: an early hot-key spend is *accepted into the mempool*
and simply never mined, so a thief can park one at the vault's nonce. The cold
key's rescue uses that same nonce, so it has to out-bid it (replace-by-fee) —
pass a higher `-fee`. It always can, since it is sweeping the whole balance.

## 8d7. Address history from the node

With `-addrindex` a node keeps an index from address to the transactions that
touched it (as sender, recipient, or fee payer), so a wallet or explorer does not
have to reconstruct history from compact filters:

```sh
dnas node -api :8080 -mine -addrindex &
curl -s 'localhost:8080/address/<addr>/history?limit=20'
curl -s localhost:8080/mempool/stats          # what the pending queue is paying, by fee rate
```

It is opt-in: its size grows with an address's usage rather than with the chain.

## 8d8. Chain-store tooling

Inspect, check and move a chain file without a running node:

```sh
dnas db info   -db chain.db -network regtest   # size, height, tip, genesis check
dnas db verify -db chain.db -network regtest   # full replay; names the first bad block
dnas db export -db chain.db -network regtest -o chain.json
dnas db import -db restored.db -network regtest -in chain.json
```

`info` and `verify` open the file **read-only**, so they are safe to run against a
node that is still going: they *report* a torn trailing record rather than
repairing it, because repairing is the owning node's job and doing it from a tool
would destroy a block the node had just appended. `export` and `import` do open
the store for writing, so **stop the node first** — there is no lock file to stop
you.

`-network` matters here: a store's validity depends on it, and reading a regtest
chain as mainnet correctly reports a genesis mismatch.

## 8d9. Looking after a running node

A node knows a great deal about itself that used to be unreachable. These read
its own API, so they work against a local or a remote node:

```sh
dnas peers                       # every connection: identity, version, caps, direction, uptime, ban score
dnas peers bans                  # who is scored and how close to the cut-off
dnas peers unban <identity|ip>   # clear a score (previously impossible without a restart)
dnas peers add <host:port>       # dial now, no restart
dnas peers drop <addr|identity>  # close a wedged connection (an outbound peer is redialed)

dnas stats [-window N]           # estimated hashrate, block timing vs the target, fee flow, miners
dnas reorgs                      # the chain switches this node has lived through
dnas health                      # "can I rely on this node?" — exits non-zero if not
```

`dnas health` is the one to script: `/info` answers 200 while a node is syncing,
un-peered, or sitting on a tip that stopped moving hours ago, so it cannot be a
readiness check. `health` reports 503 with the reasons and the CLI exits
non-zero, which is what a supervisor or a CI step needs.

Logging has a volume control and a machine-readable form:

```sh
dnas node -loglevel warn                 # quieter: misbehaviour and errors only
dnas node -logjson | jq 'select(.event=="accepted block")'
dnas node -printconfig                   # the effective settings, then exit
```

`-printconfig` matters because settings arrive from a config file AND flags, and
several are then resolved by the node (`-regtest` rewrites the network, a network
supplies a default netkey, an unset `-nodekey` becomes a path beside the chain).
It prints the merged result without starting anything.

A node started in a terminal also drops into a console, which is the only way to
look at one whose HTTP API is unreachable — and `-console` forces it on so a
script can drive it:

```sh
printf 'info\nsupply\nprune\nhealth\nquit\n' | dnas node -console -db chain.db
```

It covers the same ground the API does, read straight out of the running node:
`info`, `balance`, `peers`, `mempool [list]`, `tx <hash>`, `assets [id]`,
`supply`, `stats`, `health`, `reorgs`, `prune`, `webhooks`, `mine [on|off]`,
`generate [n]`, and a `send` that takes the options the API takes:

```
dnas> send dnasabc… 2.5 fee=0.001 expiry=+20 memo="two coffees"
```

## 8d9b. Getting told about a payment (webhooks)

`/events` streams blocks and transactions to anything holding a connection open.
A service that wants to be *called* instead — a shop backend, a bot, a cron job —
gives the node a URL:

```sh
dnas node -webhook https://shop.example/dnas-hook -api :8080 &
curl -s localhost:8080/webhooks     # sent / failed / dropped / queued
```

Every delivery is a POST carrying the event plus the network and the node's
height, so a receiver can tell a regtest event from a mainnet one and can notice
a gap. Delivery never blocks the node: a failed POST is retried a few times with
a growing delay, and a receiver far enough behind to fill the queue loses events
rather than the node growing a backlog for it. Anything that must not miss a
payment should reconcile against `/chain` or `/address/{addr}/history` rather
than trust the stream.

## 8d9c. Running a node that does not grow (pruning)

A node keeps its blocks in memory, so a long chain costs its whole size in RAM.
Everything needed to validate the next block is the account state, the headers,
and the last few bodies:

```sh
dnas node -prune 1000 -api :8080 &      # keep 1000 recent bodies
curl -s localhost:8080/info | jq '{pruned, prune_keep, body_height, pruned_bodies}'
```

The floor is 132 — above `MaxReorgDepth`, because a reorg replays the bodies it
disconnects, and a node that cannot reorg is not a node. A smaller `-prune` is
raised to it and says so.

What a pruning node gives up is serving what it no longer holds, and it is
explicit about that rather than answering "not found":

```sh
curl -si localhost:8080/block/1   | head -1   # HTTP/1.1 410 Gone
curl -si localhost:8080/cfilter/1 | head -1   # HTTP/1.1 410 Gone
```

That second one matters most: a compact filter built from a missing body is a
valid *empty* filter, and an empty filter is a proof of **absence** — serving one
would tell a light client its address is provably not in a block the node cannot
even read. The filter-*header* chain is cached as blocks connect, so a node that
followed the chain from genesis keeps serving all of it even after pruning.

## 8d10. A node's identity is not its wallet

A node proves itself to peers with an Ed25519 key and sends the **public** key in
the handshake — and a DNAS address is a hash of a public key. So a node that used
its wallet key for both would hand every peer the address holding its coin.

The identity therefore lives in its own file, created on first run:

```sh
dnas node -api :8080 -mine          # creates nodekey.json beside chain.db
dnas wallet address -o nodekey.json # a different address from the wallet's
dnas node -nodekey /secure/id.json  # or put it where you like
```

It holds no coin and needs no backup: losing it costs the node its accumulated
peer reputation and nothing else.

## 8d11. Fee-bumping and cancelling a stuck payment

Replace-by-fee is a consensus rule, and now the light wallet can use it. Both
operations reuse the original's **nonce** — that is what makes them a replacement
rather than a second payment:

```sh
dnas spv -api localhost:8080 wallet -key lw.json bump   <txhash> [fee]
dnas spv -api localhost:8080 wallet -key lw.json cancel <txhash> [fee]
```

`bump` re-sends the same transfer at a higher fee. `cancel` replaces it with a
payment to yourself, spending the nonce so the original can never be mined —
there is no "delete a transaction" in an account ledger; the nonce is the slot.
Both refuse if the transaction has already confirmed, rather than paying the
recipient twice.

## 8d12. Spending FROM a multisig account

M-of-N multisig has been in consensus from early on, and four separate surfaces
would derive an address for you. Nothing could spend one — so a funded 2-of-3
address was a hole to put money in. The spend travels between the members as a
file, gaining one signature per stop:

```sh
dnas wallet pubkey -o m1.json        # each member's public key
dnas multisig address -threshold 2 -pubkeys $A,$B,$C     # the address to fund

dnas multisig propose -threshold 2 -pubkeys $A,$B,$C \
    -to <addr> -amount all -o spend.json                 # "all" sweeps it
dnas multisig sign -wallet m1.json -in spend.json        # member 1
dnas multisig sign -wallet m3.json -in spend.json        # member 3 — now complete
dnas multisig inspect -in spend.json                     # who has signed, and what it does
dnas multisig submit -in spend.json
```

The file is not a bearer instrument at any stage: the recipient, the amount and
the nonce are covered by every signature already on it, so a later signer can
only agree to the same transfer or refuse. Signing is entirely **offline** — the
file records which network it is for, because a signature commits to the network
id and a member signing on the wrong chain produces one that is worthless.

The two mistakes the tool catches before the node does, because at the node they
both look like "not enough signatures": a non-member signing, and the same member
signing twice (consensus counts M *distinct* members).

## 8d13. Escrow: a 2-of-3 with names on the members

The oldest use of multisig, and the reason plain 2-of-3 is worth having. A buyer
pays into an account that needs two of {buyer, seller, arbiter}:

```sh
dnas escrow new -buyer $B -seller $S -arbiter $A -terms "one bicycle"
#   → the address to fund, recorded with which key is whose role

dnas escrow release -in escrow.json -o payout.json   # a spend paying the SELLER
dnas escrow refund  -in escrow.json -o refund.json   # a spend paying the BUYER
dnas multisig sign -wallet buyer.json -in payout.json
dnas multisig sign -wallet seller.json -in payout.json
dnas multisig submit -in payout.json
```

The happy path is buyer + seller and the arbiter never appears; a delivery that
never happened is buyer + arbiter; a buyer who will not release is seller +
arbiter. **No party can move the coin alone, including the arbiter** — whose
power is only to break a tie. Two roles sharing a key would quietly make it a
1-of-2, so that is refused.

## 8d14. Timestamping a file on the chain (anchoring)

A chain with proof of work is a timestamp service, and that is useful for things
with nothing to do with money — a contract draft, a dataset, a build artifact:

```sh
dnas anchor add    -file report.pdf -key wallet.json   # publishes sha256(file)
dnas anchor verify -file report.pdf                    # re-hashes, then proves it
```

`verify` proves the file's hash is in a specific block of a proof-of-work chain
the client checked itself, so the file cannot have been written after that block.
It says nothing about *who* made it, and a chain's timestamps are only as good as
the network's — the honest reading is "before block N".

Change one byte and the claim collapses, which is the point.

## 8d15. Asking to be paid, and verifying that you were

```sh
dnas invoice new   -amount 2.5 -memo "two coffees" -key shop.json
#   → invoice.json plus a dnas:ADDRESS?amount=…&memo=… URI to hand to the payer
dnas invoice watch -in invoice.json -wait     # exits 0 when it is settled
dnas invoice pay   -in invoice.json -key mine.json    # the payer's side
```

`watch` is the part worth being careful about, because it is where money is at
stake: it verifies the header chain's proof of work itself, uses compact filters
to find the blocks touching the address, authenticates those bodies against their
headers, and only then counts what arrived. It waits for confirmations before
saying yes — a payment in the tip block alone can still be reorganized away, and
a merchant shipping on one confirmation has been paid reversibly.

The limitation it states out loud: an address can be paid more than once, so two
invoices for the same amount at the same address cannot be told apart. Use a
fresh key per invoice.

## 8d16. Proving you control an address, without spending

```sh
dnas wallet sign   -o mine.json -m "I control this address" -out sig.json
dnas wallet verify -in sig.json -address <addr>
dnas wallet sign   -o mine.json -file report.pdf -out fsig.json   # or over a file
```

Message signatures are **domain-separated** from transaction signatures. That is
not decoration: a wallet that signs whatever bytes it is handed can be asked to
"just sign this to prove it's you" with the serialization of a transaction, and
the answer is a valid transfer. Neither form can be replayed as the other.

`verify` checks the address the signature *proves* against the one the file
*claims* — a file claiming an address whose key did not sign it is exactly what a
forgery looks like, and reporting the claim would endorse it.

## 8d17. Changing a passphrase, and backing up what cannot be re-synced

Encryption at rest used to be one-way: `DNAS_WALLET_PASSPHRASE` decided how a
file was written when it was created and there was no way to change it, so a
passphrase that had been somewhere it should not could never be replaced.

```sh
DNAS_WALLET_PASSPHRASE=old dnas wallet passphrase -o wallet.json   # asks for the new one twice
DNAS_WALLET_PASSPHRASE=old dnas wallet passphrase -o wallet.json -remove   # back to plaintext
```

It writes, then reopens and compares the address before reporting success: this
rewrites the only copy of a key, and a re-encryption that silently produced an
unopenable file would destroy it.

```sh
export DNAS_BACKUP_PASSPHRASE='…'
dnas backup save -o bundle.json          # wallet.json, nodekey.json, spvwallet.json, escrow.json
dnas backup list    -in bundle.json
dnas backup restore -in bundle.json -d ./restored
```

The chain is deliberately **not** in the bundle: it is public, every peer has it,
and a node re-downloads it. What cannot be re-derived is a few kilobytes of key
material and local records — which is why this is one encrypted file rather than
an archive format. A restore refuses to overwrite anything unless told to, and
restored key files come back `0600`.

Key files are also found by *content*, not only by the default names: `-wallet
mine.json` is perfectly ordinary, and a backup that quietly omitted somebody's
actual wallet would be worse than no backup, because it would be trusted.

## 8d18. Reading a transaction before agreeing to it

Half of this tooling passes transactions around as JSON files, and every one of
them is something a person is being asked to agree to:

```sh
dnas tx inspect -in spend.json      # or -hash <txhash> for one the node knows
dnas tx verify  -in spend.json      # exits non-zero if it would be refused
```

It reports what the JSON does not: whether the signatures actually hold, the fee
*rate* against the relay floor, which of the three silent rejections applies (an
expired window, a spent nonce, a fee under the floor), what kind of account it
spends from, and for a multisig file which members have signed.

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

A contract can hold a **native asset** as happily as coin, which is enough for an
on-chain asset-for-coin trade: two contracts sharing one hash, with the parties
swapped and the coin leg timing out first. `dnas htlc swap` derives both legs and
prints the steps in order, so neither side has to get that right by hand:

```sh
dnas htlc swap -hash H -asset-owner ALICE_PUB -coin-owner BOB_PUB \
  -asset <asset-id> -asset-amount 500 -coin-amount 10 \
  -coin-timeout 100 -asset-timeout 200
```

Both parties should derive the addresses themselves and check they match before
funding anything. Fees are always paid in coin, so an asset contract must be
funded with a little coin too or nobody can spend it — the printed plan does that.
Run `./scripts/swap-demo.sh` to watch a full swap settle.

## 8f. Lock down the API

By default the API is fully open (a localhost toy). Set a token to require it on
the **write** endpoints (`/send`, `/tx`, `/mine`, `/generate`, `/submitblock`);
reads stay open, and all bundled clients send it automatically from the same env
var:

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
#   keys: s=send  v=verify  w=watch a tx  m=toggle mining  x=multisig  h=HD wallet
#         c=stop watching  r=refresh  q=quit
#   it also draws a fee-rate histogram of the mempool, so you can see what the
#   queue is actually paying before choosing a fee
#   or: ./dnas-tui -spawn      # launches a local node for you

# Desktop GUI (Python / PyQt6)
python3 gui/dnas_gui.py --api localhost:8080
#   includes a "Wallet tools" panel for multisig addresses and HD/BIP39 wallets
```

Both surface the multisig and HD/BIP39 wallet helpers (previously CLI/API-only).

By default a payment from either client is `POST /send` — the *node* signs with
the *node's* wallet. Pass a key file and they sign locally instead, so the node
only ever receives a signed transaction:

```sh
./dnas-tui -api localhost:8080 -key mine.json -dnas ./dnas
python3 gui/dnas_gui.py --api localhost:8080 --key mine.json --dnas ./dnas
```

Both delegate the signing to the `dnas` binary rather than implementing the
transaction encoding a second (and third) time — that encoding is what
signatures cover, and a hand-written copy of it in a UI is how a client comes to
produce signatures a node rejects.

## 10. Files, persistence, reset

**Key material — the only files that cannot be re-derived.**

- `wallet.json` — your Ed25519 keys (encrypt with `DNAS_WALLET_PASSPHRASE`, and
  change that passphrase later with `dnas wallet passphrase`).
- `nodekey.json` — the node's **network identity** key, created on first run
  beside `-db`. It holds no coin, but it is still a private key: peers are sent
  its public half, and an address is a hash of a public key (§8d10).
- `dnas-backup.json` — whatever `dnas backup save` wrote: an encrypted bundle of
  the two above plus the light wallet's state (§8d17).

**Chain and derived state — all of it re-obtainable.**

- `chain.db` — the append-only chain; it survives restarts, and a node re-syncs
  anything it's missing from peers.
- `peers.json`, `bans.json`, `mempool.json` — soft state written beside `chain.db`
  on graceful shutdown and reloaded on start, so a restart resumes warm. The chain
  stays authoritative; these are conveniences (re-learned from peers if lost).
- `spvwallet.json` and `spvheaders.json` / `<wallet>.headers` — a light wallet's
  scanned state and its verified header cache. Deleting them costs a rescan, not
  money — though `spvwallet.json` also holds your private labels and notes, and
  is written in the clear.

**Working files the multi-party flows leave behind.** `multisig-spend.json`,
`escrow.json`, `escrow-spend.json`, `sponsored-tx.json`, `invoice.json` and the
`*.dnasanchor` receipts are local: a half-signed spend somebody is still
collecting signatures for, or a claim about a document. They are not secret, and
a half-signed spend is not a bearer instrument (§8d12), but they are nobody
else's business.

Everything on this page is in the repository's `.gitignore` (and
`.dockerignore`), including a catch-all for `*.json` at the checkout root —
because this guide walks you through creating key files *by name* in the
directory you cloned into, and a committed `nodekey.json` publishes the identity
your peers know you by. If you ever add a JSON file to the root that *should* be
tracked, add a `!name.json` exception rather than removing the rule.

To start clean, stop the node and delete `chain.db` (keep `wallet.json` to keep
your address). Different networks are separated by `-netkey`. Note: the block
header format evolves as the project gains features, so a `chain.db` from an older
build may be rejected as incompatible — delete it to start a fresh chain.
