# dnas-tui

A terminal client for a DNAS node, built with
[bubbletea](https://github.com/charmbracelet/bubbletea) + lipgloss. It talks only
to the node's HTTP API, so it works against any local or remote node.

This is a **standalone module** (it has external dependencies and imports no
DNAS module), so it has its own `go.work` and is built from this directory —
independent of the repo's internal-module workspace:

```sh
cd tui
go build -o dnas-tui .
./dnas-tui -api localhost:8080     # connect to a running node
./dnas-tui -spawn                  # launch a local node and connect to it
                                   #   (-dnas <path> to point at the binary)
```

Keys: `s` send · `v` verify a transaction (SPV) · `m` toggle mining · `x` derive
a multisig address · `h` generate/restore an HD (BIP39) wallet · `r` refresh ·
`q` quit. The dashboard shows live chain status (including the current dynamic
`minfee`), recent blocks, the mempool, and the node wallet's balance; `v`
performs a full light-client check (header proof-of-work + merkle proof fold) in
the client; `x`/`h` call the node's stateless wallet helpers and show the result
in a panel.

- `client.go` — the HTTP API client (unit-tested against httptest).
- `main.go` — the bubbletea model/update/view and local-node launcher.
