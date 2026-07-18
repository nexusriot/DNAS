# cmd

Module `github.com/nexusriot/DNAS/cmd` — the `dnas` command-line tool. The main
package lives in `cmd/dnas`.

```sh
dnas node   [flags]              run a full node (default; -config for a JSON file)
                                 (-minrelayfee sets the base dynamic relay fee;
                                  -regtest mines on demand via POST /generate)
dnas wallet new|address [-o F]   create / show a key file
dnas wallet pubkey [-o F]        print a wallet's public key (for multisig/HTLC)
dnas wallet mnemonic|restore|addresses   BIP39 backup + HD addresses
dnas wallet multisig -threshold M -pubkeys a,b,c   M-of-N multisig address
dnas htlc new|address|claim|refund                 hash-time-locked contracts
dnas spv [-api URL] sync|verify <txhash>|scan|balance|history <address>   light client (headers/proof/filters/state)
dnas spv [-api URL] wallet [-f FILE] add|update|status|list|forget          persistent light wallet (incremental, -watch)
dnas spv [-api URL] wallet -key FILE new|send <to> <amount>                 self-custodial light wallet (signs locally)
dnas fastsync [-api URL] [-checkpoint H:HASH] [addr...]                     bootstrap state from a verified snapshot
dnas version                     print the build version (stamped via -ldflags)
```

Running a node with an interactive terminal also starts a REPL
(`send`, `balance`, `address`, `info`, `peers`, `mempool`); it shuts down cleanly
on SIGINT/SIGTERM. The `spv` subcommand (`spv.go`) is a standalone light client
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
validates only the blocks above the snapshot. `dnas htlc`
(`new`, `address`, `claim`,
`refund`) mints a preimage+hash and builds, signs, and submits HTLC spends —
`claim` reveals the preimage, `refund` is valid only past the timeout height —
sweeping the contract balance minus fee to `-to`. `dnas node -regtest` mines on
demand via `POST /generate` and defaults its network key to an isolated value so
a regtest node doesn't peer with a devnet. A node now persists its peers,
bans, and mempool beside the `-db` file and honors `DNAS_API_TOKEN`; the spend
tools send that token automatically when the env var is set. See the root README
for the full flag list. Depends on all other modules.
