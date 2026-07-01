# cmd

Module `github.com/nexusriot/DNAS/cmd` — the `dnas` command-line tool. The main
package lives in `cmd/dnas`.

```sh
dnas node   [flags]        run a full node (default if no subcommand)
dnas wallet new   [-o F]   create a wallet key file
dnas wallet address [-o F] print a wallet's address
```

Running a node with an interactive terminal also starts a REPL
(`send`, `balance`, `address`, `info`, `peers`, `mempool`). See the root README
for the full flag list. Depends on all other modules.
