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

# spend YOUR key instead of the node's: signed locally, the node only ever
# receives a signed transaction
./dnas-tui -api localhost:8080 -key mine.json -dnas ./dnas
```

Keys: `s` send · `v` verify a transaction (SPV) · `w` watch a transaction to
confirmation · `c` stop watching · `m` toggle mining · `x` derive a multisig
address · `h` generate/restore an HD (BIP39) wallet · `r` refresh · `q` quit.

The send prompt accepts a pasted `dnas:` payment URI in place of
`<to> <amount>`: it carries the amount and the memo, so neither is retyped — and
neither, more importantly, is the address. An amount typed alongside a URI that
asks for a different one is refused rather than silently overridden.

The dashboard shows live chain status (including the current dynamic `minfee`),
recent blocks, the mempool, and the node wallet's balance. `v` performs a full
light-client check (header proof-of-work + merkle proof fold) in the client, and
`x`/`h` call the node's stateless wallet helpers and show the result in a panel.

Two things go beyond a status readout. A **fee-rate histogram** (from
`/mempool/stats`) shows what the pending queue is actually paying per byte — a
queue depth alone cannot tell you whether your fee lands in the next block or sits
behind a wall of higher bidders. And `w` **watches one transaction** from
submission to confirmation, updating on the same event/poll signal as everything
else, so you can see a payment go pending → confirmed without re-running a lookup
by hand.

**Self-custodial mode** (`-key`). Without it a payment is `POST /send`, which
asks the *node* to sign with the *node's* wallet — fine for a private node you
own, and against a shared one it spends somebody else's coin. With it the wallet
panel shows your address and the send prompt says "signed locally".

The signing is delegated to the `dnas` binary (`dnas spv wallet -key … send`),
not implemented here. This module deliberately imports no DNAS package, so
signing locally would mean a second hand-written copy of the canonical
transaction encoding — which is what signatures cover, and exactly how a client
comes to produce signatures a node rejects. The GUI's SPV verifier had already
rotted that way once. A broken setup (missing key file, wrong binary) is reported
at startup rather than in the middle of a payment.

- `client.go` — the HTTP API client (unit-tested against httptest). It asks for
  the chain's TAIL (`/chain?last=N`): the bulk reads are paged, so an
  unparameterized request returns the OLDEST page and the panel would sit at
  genesis on a long chain.
- `selfcustody.go` — the delegation to the binary, keeping stdout (the result)
  and stderr (the log lines) apart, because a merged stream has no reliable last
  line — and reading the outcome from the output, because a refusal the CLI
  handles itself is printed with a zero exit status.
- `main.go` — the bubbletea model/update/view and local-node launcher.
