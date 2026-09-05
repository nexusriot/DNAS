# cmd

Module `github.com/nexusriot/DNAS/cmd` — the `dnas` command-line tool. The main
package lives in `cmd/dnas`.

```sh
dnas node   [flags]              run a full node (default; -config for a JSON file)
                                 (-network mainnet|testnet|regtest picks the chain;
                                  -minrelayfee sets the base dynamic relay fee;
                                  -addrindex serves /address/{a}/history;
                                  -faucet hands out coin on testnet/regtest;
                                  -regtest mines on demand via POST /generate)
dnas wallet new|address [-o F]   create / show a key file
dnas wallet pubkey [-o F]        print a wallet's public key (for multisig/HTLC)
dnas wallet mnemonic|restore|addresses   BIP39 backup + HD addresses
dnas wallet multisig -threshold M -pubkeys a,b,c   M-of-N multisig address
dnas htlc new|address|claim|refund [-asset ID]     hash-time-locked contracts (coin or a native asset)
dnas htlc swap -hash H -asset-owner A -coin-owner B ...   derive both legs of an asset-for-coin swap
dnas vault address|spend                           time-delayed vault: cold key now, hot key from a height
dnas spv [-api URL] sync|verify <txhash>|scan|balance|history <address>   light client (headers/proof/filters/state)
dnas spv [-api URL] wallet [-f FILE] add|update|status|list|forget          persistent light wallet (incremental, -watch)
dnas spv [-api URL] wallet -key FILE new|send <to> <amount>                 self-custodial light wallet (signs locally)
dnas spv [-api URL] wallet -key FILE issue <ticker> <supply>                mint a native asset (token)
dnas spv [-api URL] wallet -key FILE -asset ID send <to> <amount>           transfer a native asset
dnas fastsync [-api URL] [-checkpoint H:HASH] [addr...]                     bootstrap state from a verified snapshot
dnas spv [-api URL] wallet label|note|export|import                          private labels, notes, and a key-free watch list
dnas miner -api URL -address ADDR [-once] [-shares] [-longpoll=false]       external miner (template -> mine -> submit)
dnas faucet [-api URL] [-address ADDR]                                      ask a testnet/regtest node for coin
dnas sponsor request -key F -to ADDR -amount A -payer ADDR -o tx.json        have someone else pay the fee (sender's half)
dnas sponsor pay -wallet F -in tx.json [-submit]                             counter-sign it as the fee payer
dnas db info|verify|export|import [-db FILE] [-network NAME]                inspect, check and move a chain store
dnas peers [list|bans|unban KEY|add ADDR|drop PEER] [-api URL]              inspect and manage peers and bans
dnas stats [-api URL] [-window N]                                          hashrate, block timing, fee flow, miners
dnas reorgs [-api URL]                                                     chain switches this node has lived through
dnas health [-api URL]                                                     readiness; exits non-zero when not ready
dnas spv [-api URL] wallet -key F bump|cancel <txhash> [fee]               fee-bump or void a stuck payment
dnas supply [-api URL]           minted / burned / circulating + the conservation check
dnas multisig address|propose|sign|submit|inspect                          SPEND from an M-of-N account
dnas escrow new|show|release|refund                                        2-of-3 buyer/seller/arbiter escrow
dnas anchor hash|add|verify -file F                                        timestamp a file's hash on the chain
dnas invoice new|show|watch|pay                                            ask to be paid, and verify that you were
dnas assets [show ID] [-ticker T]                                          what assets exist, and who holds them
dnas tx inspect|verify -in FILE | -hash HASH                               decode and check a transaction
dnas wallet sign|verify                                                    prove control of an address, off-chain
dnas wallet passphrase [-remove]                                           re-encrypt a key file under a new passphrase
dnas backup save|list|restore                                              encrypt the files a re-sync cannot replace
dnas version                     print the build version (stamped via -ldflags)
```

Running a node with an interactive terminal — or with `-console`, which forces it
on so a script can drive it — also starts a **console** (`repl.go`) covering the
same ground the HTTP API does, read straight out of the running node: `info`,
`balance` (with assets), `peers`, `mempool [list]`, `tx`, `assets`, `supply`,
`stats`, `health`, `reorgs`, `prune`, `webhooks`, `mine`, `generate`, and a
`send` that takes `fee=`, `expiry=`, `lock=` and `memo=` words (quoted runs stay
together, so a memo can contain spaces). It is the only way to look at a node
whose API is unreachable, which is when it is most wanted. The commands are a
table, so `help` cannot drift from what exists. It shuts down cleanly on
SIGINT/SIGTERM. The `spv` subcommand (`spv.go`) is a standalone light client
that verifies proof-of-work headers and merkle proofs — it trusts no full node;
`spv scan <address>` adds compact-filter scanning that reports matching blocks and
proves non-inclusion for the rest; `spv balance <address>` proves an address's
balance against the header state root; and `spv history <address>` is a light
wallet that uses the compact filters to find the blocks touching an address,
downloads only those (`GET /block`), authenticates each against its PoW-verified
header, reconstructs the transfers, and cross-checks the net against a state
proof. `spv wallet` (`spvwallet.go`) makes that light wallet **persistent**: it
watches addresses across runs, stores its scanned height and reconstructed
balances in a JSON file, syncs incrementally (only new filter-flagged blocks),
detects reorgs, and with `-watch` follows the `/events` stream. With a `-key`
file it is also **self-custodial** (`spv wallet send`): it proves the balance and
nonce trustlessly, signs the transaction locally (the key never leaves the
client), and submits only the signed transaction. `dnas fastsync` (`fastsync.go`)
bootstraps a node's state from a peer's `/snapshot` without replaying the chain:
it PoW-verifies the headers, checks the account snapshot against the header's
committed state root (and an optional `-checkpoint`), seeds a chain, then fully
validates only the blocks above the snapshot. `dnas miner` (`miner.go`) is a
standalone external miner: it fetches a block template over the API, searches for
a winning nonce locally, and submits the mined block — so mining runs off-node. It
**long polls** by default, so its next template request hangs until the tip
actually moves rather than returning a candidate that is already dead, and with
`-shares` it also submits the hashes that clear the node's easier share target
(pool accounting; `GET /shares` reports the ledger). `dnas db` (`db.go`) reads a
chain store without a node: `info` summarizes it, `verify` replays every block
through full validation and names the first that fails, and `export`/`import`
move a chain as a portable JSON file. `info`/`verify` open the file read-only —
inspection must not modify what it inspects, so a torn trailing record is
*reported* rather than repaired (repairing it is the owning node's job, and doing
it from a tool would destroy a block a live node had just appended);
`export`/`import` do open the store, so the node must be stopped. `dnas vault` (`vault.go`) derives and
spends time-delayed vaults, `dnas faucet` (`faucet.go`) asks a testnet or regtest
node for coin, and `dnas sponsor` (`sponsor.go`) runs the two-party fee-sponsorship
flow: `request` builds and sender-signs a transfer naming who pays, `pay`
counter-signs it as that party and submits.

`dnas peers`, `stats`, `reorgs` and `health` (`peers.go`) are the operator's view
of a running node: everything they show was already known and unreachable. `peers`
pulls `-api` out from **any** position, because these subcommands take a
positional argument and Go's flag package stops parsing at the first one — a
`-api` written afterwards would silently target the default node, and an `unban`
sent to the wrong node is not a mistake worth allowing quietly. `health` exits
non-zero when the node is not ready, so it works in a supervisor or CI check.

`dnas multisig` (`multisig.go`) is the missing half of multisig: consensus has
checked M-of-N signatures from the start and four surfaces would derive an
address, but nothing could spend one. The spend travels between the members as a
file, gaining a signature per stop, and the file records its network because
signing is offline and a signature commits to one chain. `dnas escrow`
(`escrow.go`) puts role names on a 2-of-3 — buyer, seller, arbiter — refuses two
roles sharing a key (which would quietly be a 1-of-2), and emits ordinary
multisig spend files so there is one signing flow rather than two.

`dnas anchor` (`anchor.go`) publishes `sha256(file)` in a zero-value self-payment
and later proves it: PoW-verified headers, a merkle path, and then *reading the
transaction* to see what the proven txid commits to. `dnas invoice`
(`invoice.go`) states what is wanted, prints a `dnas:` URI, and verifies
settlement with confirmations — a payment in the tip block alone can still be
reorganized away. `dnas assets` (`assets.go`) reads the chain's asset registry,
since an asset id is a hash and says nothing on its own.

`dnas tx inspect|verify` (`tx.go`) decodes a transaction file (or one the node
holds) and reports what the JSON does not: whether the signatures hold, the fee
*rate* against the relay floor, which of the silent rejections applies (an
expired window, a spent nonce, a fee below the floor), and for a multisig file
which members have signed. `verify` exits non-zero when a transaction would be
refused, so a script can gate on it.

`dnas wallet sign|verify` and `dnas wallet passphrase` (`walletcmds.go`) prove
control of an address without spending from it — domain-separated, so such a
signature can never be replayed as a transfer — and re-encrypt a key file under a
new passphrase, reopening it to compare the address before reporting success.
`dnas backup` bundles the files a re-sync cannot replace (keys, the node
identity, watch lists — deliberately *not* the chain) into one encrypted file.
Besides the default names it finds key files by CONTENT, so a wallet under
another name (`-wallet mine.json` is ordinary) is not silently omitted from a
backup that would then be trusted; a previous bundle is told apart by its `kind`
and skipped rather than nested.

`dnas spv wallet bump|cancel` (`bump.go`) finally makes replace-by-fee reachable:
both reuse the original's nonce, `bump` re-sending the same transfer at a higher
fee and `cancel` spending the nonce on a self-payment so the original can never
be mined. Both refuse a transaction that has already confirmed rather than paying
its recipient twice.

`dnas spv` keeps its verified headers between runs (`headercache.go`, `-cache`),
so a command downloads only what is new. The cache is trustless — a batch must
link onto it and carry valid proof of work, and a reorg below its tip discards it
— and each `spv wallet` keeps its own beside its state file.

`dnas node -printconfig` (`printconfig.go`) prints the effective configuration:
flags merged over `-config` with the defaults resolved, then exits without
touching the chain. `-loglevel` and `-logjson` set the volume and the format of
everything the node prints.

Every command that signs, verifies a genesis, or recomputes a transaction hash
first reads the network from the node it is pointed at (`network.go`). That is not
convenience: the network id is part of both the signing preimage and a
transaction's hash, so a client on the wrong network produces signatures the node
rejects and merkle roots that do not match.
The self-custodial wallet also mints and moves **native assets** (`wallet issue`,
`wallet -asset ID send`), keeps private **labels** on addresses and **notes** on
transactions (`wallet label`, `wallet note`), and can `export` a **watch-only**
file — addresses and labels, no key material of any kind — for another machine to
`import` and track without being able to spend. `dnas htlc`
(`new`, `address`, `claim`,
`refund`) mints a preimage+hash and builds, signs, and submits HTLC spends —
`claim` reveals the preimage, `refund` is valid only past the timeout height —
sweeping the contract balance minus fee to `-to`. `dnas htlc swap` (`swap.go`) derives both legs of an asset-for-coin atomic swap
and prints the steps in order, refusing any timeout ordering that would let the
party holding the preimage take both sides. `dnas node -regtest` (now shorthand
for `-network regtest`) mines on demand via `POST /generate`; each network has its
own genesis and its own signatures, so a regtest node cannot peer with — or replay
a signature onto — a devnet. A node now persists its peers,
bans, and mempool beside the `-db` file and honors `DNAS_API_TOKEN`; the spend
tools send that token automatically when the env var is set. See the root README
for the full flag list. Depends on all other modules.
