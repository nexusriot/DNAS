# dnas-gui

A desktop client for a DNAS node, built with **PyQt6**. It drives the node's
HTTP API over the Python standard library (`urllib`) — no `pip` dependencies
beyond PyQt6 itself.

```sh
python3 gui/dnas_gui.py --api localhost:8080     # connect to a running node
python3 gui/dnas_gui.py --dnas ./dnas            # then "Launch local node" in the UI
```

Features: live overview (height, difficulty, work, mempool, **min fee**, peers,
mining), the node wallet's address + balance, a **Toggle mining** button, a send
form (amount/fee in DNAS, optional memo), recent-blocks and mempool tables, an
**SPV verifier** that fetches a proof + header and checks header proof-of-work
and the merkle path in the client, and a **Wallet tools** panel that derives an
M-of-N multisig address and generates/restores an HD (BIP39) wallet via the
node's stateless helpers. It can also launch a local node so mining works out of
the box.

Polling runs on a background thread and updates the UI via a Qt signal, so a slow
or down node never freezes the window.

Tests (headless): `cd gui && QT_QPA_PLATFORM=offscreen python3 -m unittest test_dnas_gui -v`.
