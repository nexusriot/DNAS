# cmd

Module `github.com/nexusriot/DNAS/cmd` — the `dnas` command-line tool. The main
package lives in `cmd/dnas`.

```sh
dnas node   [flags]              run a full node (default; -config for a JSON file)
                                 (-minrelayfee sets the base dynamic relay fee)
dnas wallet new|address [-o F]   create / show a key file
dnas wallet mnemonic|restore|addresses   BIP39 backup + HD addresses
dnas wallet multisig -threshold M -pubkeys a,b,c   M-of-N multisig address
dnas spv [-api URL] sync|verify <txhash>           light client (headers + proof)
dnas version                     print the build version (stamped via -ldflags)
```

Running a node with an interactive terminal also starts a REPL
(`send`, `balance`, `address`, `info`, `peers`, `mempool`); it shuts down cleanly
on SIGINT/SIGTERM. The `spv` subcommand (`spv.go`) is a standalone light client
that verifies proof-of-work headers and merkle proofs — it trusts no full node.
See the root README for the full flag list. Depends on all other modules.
