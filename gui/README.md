# dnas-gui

A desktop client for a DNAS node, built with **PyQt6**. It drives the node's
HTTP API over the Python standard library (`urllib`) — no `pip` dependencies
beyond PyQt6 itself.

```sh
python3 gui/dnas_gui.py --api localhost:8080     # connect to a running node
python3 gui/dnas_gui.py --dnas ./dnas            # then "Launch local node" in the UI

# spend YOUR key instead of the node's: signed locally by the dnas binary
python3 gui/dnas_gui.py --api localhost:8080 --key mine.json --dnas ./dnas
```

Features: live overview (**network**, height, difficulty, work, mempool, **min
fee**, peers, mining, **estimated hashrate**) with a **readiness line** that says
why a node is not usable rather than only that it is running, the node wallet's
address + balance, a **Toggle mining** button, a send form (amount/fee in DNAS,
optional memo) that expands a pasted `dnas:` payment URI into its address, amount
and memo, recent-blocks / mempool / **peers** tables, an **SPV verifier**
(header proof-of-work and the merkle path, checked by running
`dnas spv -cache "" verify` through the `--dnas` binary), a **Wallet tools** panel (M-of-N multisig address, HD/BIP39 wallet)
and a **Node tools** panel (ask the faucet, look up an address's history). It can
also launch a local node so mining works out of the box.

The SPV verifier **silently rotted twice**. First it used the header format from
before the chain moved to a 256-bit nBits target and a committed state root, so
it threw on every call (and the blocks table read a `difficulty` field blocks no
longer carry). That was fixed with Python copies of the header preimage and the
target check — and those drifted the same way when BIP9 put `Version` at the
front of the header, so the verifier reported FAILED for every valid proof. So
verification now goes where signing already went: to the one implementation, in
the `dnas` binary. The only target arithmetic left in `dnas_gui.py` is
`compact_to_big`, and it is cosmetic: it labels the difficulty column.

**Self-custodial mode** (`--key`). Without it a payment is `POST /send` — the
*node* signs with the *node's* wallet, which against a shared node spends
somebody else's coin. With it the transaction is signed locally and the wallet
panel is titled "Your wallet (signed locally)", showing that key's balance rather
than the node's.

The signing is delegated to the `dnas` binary rather than implemented in Python.
Doing it here would mean re-implementing Ed25519 signing *and* the canonical
transaction encoding — and the SPV rot below is exactly what a hand-written copy
of consensus-critical code in a UI turns into. A broken setup is reported before
the window opens.

Polling runs on a background thread and updates the UI via a Qt signal, so a slow
or down node never freezes the window.

Tests (headless): `cd gui && QT_QPA_PLATFORM=offscreen python3 -m unittest test_dnas_gui -v`.
