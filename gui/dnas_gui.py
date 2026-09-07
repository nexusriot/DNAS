#!/usr/bin/env python3
"""DNAS desktop client (PyQt6).

A GUI over a DNAS node's HTTP API: live chain status and wallet balance, a send
form, blocks/mempool tables, an SPV transaction verifier, and a mining toggle.
It can also launch a local node so mining works out of the box.

    python3 gui/dnas_gui.py [--api localhost:8080] [--dnas ./dnas]

Only the Python standard library + PyQt6 are required (HTTP via urllib).
"""
import argparse
import hashlib
import json
import os
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

from PyQt6.QtCore import QObject, Qt, pyqtSignal
from PyQt6.QtWidgets import (
    QApplication, QGridLayout, QGroupBox, QHBoxLayout, QHeaderView, QLabel,
    QLineEdit, QMessageBox, QPushButton, QTableWidget, QTableWidgetItem,
    QVBoxLayout, QWidget,
)

COIN = 100_000_000


def header_string(hdr) -> str:
    """The exact preimage a block header hashes over. It must match
    core.Header.headerString byte for byte, or every proof-of-work check here is
    meaningless — this client fell out of step with it once already, when the
    chain moved from a leading-zero difficulty to a 256-bit nBits target and
    gained a state root."""
    return "%d|%d|%s|%s|%s|%d|%d|%d" % (
        hdr["index"], hdr["timestamp"], hdr["prev_hash"], hdr["merkle_root"],
        hdr["state_root"], hdr["base_fee"], hdr["bits"], hdr["nonce"])


def compact_to_big(bits: int) -> int:
    """Decode a compact nBits target into the 256-bit integer it represents
    (mantissa x 256^(exponent-3)), mirroring core.CompactToBig."""
    mantissa = bits & 0x007FFFFF
    exponent = bits >> 24
    if exponent <= 3:
        return mantissa >> (8 * (3 - exponent))
    return mantissa << (8 * (exponent - 3))


def difficulty_of(bits: int) -> str:
    """Human-readable difficulty (PowLimit / target), the display-only ratio
    core.TargetDifficulty computes. Blocks carry `bits`, not a difficulty."""
    target = compact_to_big(bits)
    if target <= 0:
        return "—"
    pow_limit = (1 << 244) - 1
    return "%.2f" % (pow_limit / target)


def meets_target(block_hash: str, bits: int) -> bool:
    """Proof of work is a target comparison, not a count of leading zeros: the
    hash read as a big-endian integer must be <= the target."""
    try:
        return int(block_hash, 16) <= compact_to_big(bits)
    except ValueError:
        return False


def sha(s: str) -> str:
    return hashlib.sha256(s.encode()).hexdigest()


def parse_payment_uri(text):
    """Parse "dnas:ADDRESS?amount=DECIMAL&memo=TEXT&ref=TOKEN".

    Returns a dict with address/amount/memo, or None if `text` is not a URI.
    The amount is kept as WRITTEN so the CLI's parser stays the only thing that
    turns it into base units. The address is not checksum-checked here: the node
    does that on the way in, and the GUI has no wallet library to do it with.
    """
    text = (text or "").strip()
    if not text.lower().startswith("dnas:"):
        return None
    rest = text[len("dnas:"):]
    if rest.startswith("//"):
        rest = rest[2:]
    addr, _, query = rest.partition("?")
    addr = urllib.parse.unquote(addr).strip()
    if not addr:
        return None
    params = urllib.parse.parse_qs(query)
    return {
        "address": addr,
        "amount": (params.get("amount", [""])[0] or "").strip(),
        "memo": params.get("memo", [""])[0] or "",
    }


class Api:
    """Thin HTTP client for a node's API (stdlib urllib)."""

    def __init__(self, base: str):
        if not base.startswith("http"):
            base = "http://" + base
        self.base = base.rstrip("/")
        # Bearer token for a locked-down node (write endpoints); reads stay open.
        self.token = os.environ.get("DNAS_API_TOKEN", "")

    def _get(self, path):
        with urllib.request.urlopen(self.base + path, timeout=2) as r:
            return json.load(r)

    def _post(self, path, body):
        headers = {"Content-Type": "application/json"}
        if self.token:
            headers["Authorization"] = "Bearer " + self.token
        req = urllib.request.Request(
            self.base + path, data=json.dumps(body).encode(),
            headers=headers, method="POST")
        try:
            with urllib.request.urlopen(req, timeout=3) as r:
                return json.load(r)
        except urllib.error.HTTPError as e:
            try:
                raise RuntimeError(json.load(e).get("error", str(e)))
            except (ValueError, json.JSONDecodeError):
                raise RuntimeError(str(e))

    def info(self):
        return self._get("/info")

    def address(self):
        return self._get("/address").get("address", "")

    def balance(self, addr):
        return self._get("/balance/" + addr).get("balance_fmt", "")

    def chain(self, last=12):
        """The NEWEST `last` blocks. The bulk reads are paged, so a plain
        /chain returns the first page — the oldest blocks once a chain passes
        the page limit, which would freeze this view in the past."""
        return self._get("/chain?last=%d" % last)

    def mempool(self):
        return self._get("/mempool")

    def set_mining(self, on: bool):
        return self._post("/mine", {"on": on})

    def send(self, to, amount, fee, memo=""):
        body = {"to": to, "amount": amount, "fee": fee}
        if memo:
            body["memo"] = memo
        return self._post("/send", body).get("hash", "")

    def multisig_address(self, threshold, pubkeys):
        """Derive an M-of-N multisig address from member public keys (hex)."""
        r = self._post("/multisig/address", {"threshold": threshold, "pubkeys": pubkeys})
        return r.get("address", "")

    def hd_wallet(self, mnemonic="", count=5):
        """Generate (blank mnemonic) or restore a BIP39 HD wallet; return the
        mnemonic and its first `count` derived addresses."""
        r = self._post("/wallet/hd", {"mnemonic": mnemonic, "count": count})
        return r.get("mnemonic", ""), r.get("addresses", [])

    def verify(self, txh: str) -> str:
        """Light-client check, done here rather than trusted: recompute the
        header hash, check it meets the 256-bit target the header commits to,
        then fold the merkle proof up to the header's root."""
        pr = self._get("/proof/" + txh)
        hdr = self._get("/header/%d" % pr["block_index"])
        pow_ok = sha(header_string(hdr)) == hdr["hash"] and meets_target(hdr["hash"], hdr["bits"])
        h = txh
        for step in pr["proof"]:
            h = sha(h + step["hash"]) if step["right"] else sha(step["hash"] + h)
        inc = h == hdr["merkle_root"]
        if pow_ok and inc:
            return "PROVEN in block %d (%d confirmations)" % (pr["block_index"], pr["confirmations"])
        return "FAILED (pow=%s inclusion=%s)" % (pow_ok, inc)

    # --- the surfaces the node grew and this client had not caught up with ---

    def peers_detail(self):
        return self._get("/peers")

    def chain_stats(self, window=0):
        return self._get("/chainstats" + ("?window=%d" % window if window else ""))

    def mempool_stats(self):
        return self._get("/mempool/stats")

    def health(self):
        """Readiness, which /info cannot report: it answers 200 while syncing,
        un-peered or sitting on a stale tip. A 503 body carries the reasons."""
        try:
            return self._get("/health")
        except urllib.error.HTTPError as e:
            try:
                return json.load(e)
            except (ValueError, json.JSONDecodeError):
                raise
        except urllib.error.URLError:
            raise

    def address_history(self, addr, limit=25):
        return self._get("/address/%s/history?limit=%d" % (addr, limit))

    def faucet(self, addr):
        return self._post("/faucet", {"address": addr})

    def vault_address(self, hot, cold, unlock):
        r = self._post("/vault/address", {"hot": hot, "cold": cold, "unlock": unlock})
        return r.get("address", "")


class LocalWallet:
    """Spending your OWN key from the GUI.

    Every payment this GUI could make went through POST /send, which asks the
    NODE to sign with the node's wallet: fine for a private node you own, and
    useless otherwise — against a shared node you would be spending somebody
    else's coin, and to spend your own you would have to hand them your key.

    Signing locally here would mean re-implementing Ed25519 signing AND the
    canonical transaction encoding in Python. That encoding is consensus
    critical, and a second hand-written copy of it in a UI is exactly how a
    client comes to produce signatures a node rejects — this GUI has already had
    that bug once, in its SPV header format. So the signing is delegated to the
    `dnas` binary, which holds the one implementation:

        dnas spv -api URL wallet -f STATE -key KEY [-memo M] send <to> <amount> [fee]

    The key file never leaves the machine and the node only ever receives a
    signed transaction.
    """

    def __init__(self, dnas_bin: str, key_file: str, state_file: str = "spvwallet.json"):
        self.dnas_bin = dnas_bin
        self.key_file = key_file
        self.state_file = state_file
        self.address = ""

    def enabled(self) -> bool:
        return bool(self.key_file)

    def _run(self, args, timeout=60):
        """Run the binary, returning (code, stdout, stderr) SEPARATELY.

        Keeping them apart matters: the CLI writes its log lines to stderr and
        its result to stdout, so a merged stream has no reliable last line — the
        log would be read as the outcome.
        """
        out = subprocess.run([self.dnas_bin] + args, capture_output=True, text=True, timeout=timeout)
        return out.returncode, (out.stdout or "").strip(), (out.stderr or "").strip()

    def resolve(self) -> str:
        """Return the key file's address, raising if the setup does not work.

        Done before any payment, so a missing key file or a wrong binary is
        reported up front rather than in the middle of sending money.
        """
        code, out, err = self._run(["wallet", "address", "-o", self.key_file], timeout=20)
        addr = last_line(out)
        if code != 0 or not addr.startswith("dnas"):
            raise RuntimeError(last_line(err) or out or "no output from " + self.dnas_bin)
        self.address = addr
        return addr

    def send(self, api: str, to: str, amount: str, fee: str = "", memo: str = "") -> str:
        args = ["spv", "-api", api, "wallet", "-f", self.state_file, "-key", self.key_file]
        if memo:
            args += ["-memo", memo]
        args += ["send", to, amount]
        if fee:
            args.append(fee)
        code, out, err = self._run(args)
        line = last_line(out)
        # A refusal the CLI handles itself (an insufficient balance, a rejected
        # transaction) is printed with a ZERO exit status, so the outcome has to be
        # read rather than inferred from the exit code.
        if code != 0 or not line.startswith("submitted"):
            raise RuntimeError(line or last_line(err) or "send failed")
        return line


def last_line(text: str) -> str:
    """The final non-empty line of some output: the CLI's result, after any logs."""
    for line in reversed((text or "").strip().splitlines()):
        if line.strip():
            return line.strip()
    return ""


class Poller(QObject):
    """Background poller; emits state to the GUI thread via a Qt signal."""
    updated = pyqtSignal(dict)
    failed = pyqtSignal(str)

    def __init__(self, api: Api, own_address: str = ""):
        super().__init__()
        self.api = api
        # The address whose balance the wallet panel should show. Empty means the
        # node's own, which is the right answer only in node-signed mode.
        self.own_address = own_address
        self._stop = False

    def run(self):
        while not self._stop:
            try:
                # In self-custodial mode the wallet panel is about the LOCAL key;
                # showing the node's own balance there would be showing money the
                # user cannot spend.
                addr = self.own_address or self.api.address()
                data = {
                    "info": self.api.info(),
                    "addr": addr,
                    "balance": self.api.balance(addr) if addr else "",
                    "chain": self.api.chain(),
                    "mempool": self.api.mempool(),
                }
                # The rest are newer node surfaces; a node that predates any of
                # them still polls fine, so each is best-effort rather than
                # fatal to the whole snapshot.
                for key, fetch in (("stats", self.api.chain_stats),
                                   ("health", self.api.health),
                                   ("peers", self.api.peers_detail)):
                    try:
                        data[key] = fetch()
                    except Exception:  # noqa: BLE001 (optional surface)
                        pass
                self.updated.emit(data)
            except Exception as e:  # noqa: BLE001 (report any connectivity error)
                self.failed.emit(str(e))
            time.sleep(1.5)

    def stop(self):
        self._stop = True


class Main(QWidget):
    def __init__(self, api_addr: str, dnas_bin: str, key_file: str = "", state_file: str = "spvwallet.json"):
        super().__init__()
        self.dnas_bin = dnas_bin
        self.wallet = LocalWallet(dnas_bin, key_file, state_file)
        if self.wallet.enabled() and not self.wallet.address:
            # main() has normally resolved this already (and exits if it cannot);
            # doing it here too keeps a directly-constructed window working.
            try:
                self.wallet.resolve()
            except Exception:  # noqa: BLE001 (reported by main(); the panel just shows nothing)
                pass
        self.node_proc = None
        self.mining = False
        self.setWindowTitle("DNAS")
        self.resize(880, 720)
        self._build()
        self._connect(api_addr)

    def _build(self):
        root = QVBoxLayout(self)

        # connection bar
        bar = QHBoxLayout()
        self.api_edit = QLineEdit()
        self.status = QLabel("—")
        connect_btn = QPushButton("Connect")
        connect_btn.clicked.connect(lambda: self._connect(self.api_edit.text()))
        launch_btn = QPushButton("Launch local node")
        launch_btn.clicked.connect(self._launch_node)
        bar.addWidget(QLabel("API")); bar.addWidget(self.api_edit, 1)
        bar.addWidget(connect_btn); bar.addWidget(launch_btn)
        root.addLayout(bar)
        root.addWidget(self.status)

        # overview
        ov = QGroupBox("Overview")
        g = QGridLayout(ov)
        self.lbl = {}
        for i, k in enumerate(["network", "height", "difficulty", "work", "mempool",
                               "min fee", "peers", "mining", "hashrate"]):
            g.addWidget(QLabel(k + ":"), i // 3, (i % 3) * 2)
            self.lbl[k] = QLabel("—")
            g.addWidget(self.lbl[k], i // 3, (i % 3) * 2 + 1)
        # Readiness, which the overview numbers cannot express: a node answers
        # /info happily while syncing, un-peered, or sitting on a stale tip.
        self.health_lbl = QLabel("—")
        self.health_lbl.setWordWrap(True)
        g.addWidget(self.health_lbl, 3, 0, 1, 6)
        self.mine_btn = QPushButton("Toggle mining")
        self.mine_btn.clicked.connect(self._toggle_mining)
        g.addWidget(self.mine_btn, 4, 0, 1, 6)
        root.addWidget(ov)

        # wallet
        # Which key a payment comes from is not a detail: node-signed spends the
        # node's coin, self-custodial spends yours. The panel title says which.
        wal = QGroupBox("Your wallet (signed locally)" if self.wallet.enabled() else "This node's wallet")
        wl = QVBoxLayout(wal)
        self.addr_lbl = QLabel("—")
        self.addr_lbl.setTextInteractionFlags(Qt.TextInteractionFlag.TextSelectableByMouse)
        self.bal_lbl = QLabel("—")
        wl.addWidget(self.addr_lbl); wl.addWidget(self.bal_lbl)
        root.addWidget(wal)

        # send
        snd = QGroupBox("Send")
        sg = QGridLayout(snd)
        self.to_edit = QLineEdit(); self.amt_edit = QLineEdit("1")
        self.fee_edit = QLineEdit("0.1"); self.memo_edit = QLineEdit()
        for i, (lab, w) in enumerate([("to", self.to_edit), ("amount (DNAS)", self.amt_edit),
                                      ("fee (DNAS)", self.fee_edit), ("memo", self.memo_edit)]):
            sg.addWidget(QLabel(lab), i, 0); sg.addWidget(w, i, 1)
        send_btn = QPushButton("Send")
        send_btn.clicked.connect(self._send)
        sg.addWidget(send_btn, 4, 0, 1, 2)
        self.send_result = QLabel("")
        sg.addWidget(self.send_result, 5, 0, 1, 2)
        root.addWidget(snd)

        # SPV verify
        spv = QGroupBox("Verify a transaction (SPV)")
        sv = QHBoxLayout(spv)
        self.tx_edit = QLineEdit()
        verify_btn = QPushButton("Verify")
        verify_btn.clicked.connect(self._verify)
        self.verify_result = QLabel("")
        sv.addWidget(QLabel("tx hash")); sv.addWidget(self.tx_edit, 1)
        sv.addWidget(verify_btn)
        root.addWidget(spv)
        root.addWidget(self.verify_result)

        # wallet tools: multisig address + HD/BIP39 wallet
        tools = QGroupBox("Wallet tools (multisig / HD)")
        tgl = QGridLayout(tools)
        self.ms_threshold = QLineEdit("2")
        self.ms_pubkeys = QLineEdit()
        self.ms_pubkeys.setPlaceholderText("member public keys (hex), comma- or space-separated")
        ms_btn = QPushButton("Derive multisig address")
        ms_btn.clicked.connect(self._multisig)
        self.ms_result = QLabel("")
        self.ms_result.setWordWrap(True)
        self.ms_result.setTextInteractionFlags(Qt.TextInteractionFlag.TextSelectableByMouse)
        tgl.addWidget(QLabel("threshold (M)"), 0, 0); tgl.addWidget(self.ms_threshold, 0, 1)
        tgl.addWidget(QLabel("pubkeys"), 1, 0); tgl.addWidget(self.ms_pubkeys, 1, 1)
        tgl.addWidget(ms_btn, 2, 0, 1, 2)
        tgl.addWidget(self.ms_result, 3, 0, 1, 2)

        self.hd_mnemonic = QLineEdit()
        self.hd_mnemonic.setPlaceholderText("leave blank to generate a new mnemonic, or paste one to restore")
        hd_btn = QPushButton("Generate / restore HD wallet")
        hd_btn.clicked.connect(self._hd)
        self.hd_result = QLabel("")
        self.hd_result.setWordWrap(True)
        self.hd_result.setTextInteractionFlags(Qt.TextInteractionFlag.TextSelectableByMouse)
        tgl.addWidget(QLabel("mnemonic"), 4, 0); tgl.addWidget(self.hd_mnemonic, 4, 1)
        tgl.addWidget(hd_btn, 5, 0, 1, 2)
        tgl.addWidget(self.hd_result, 6, 0, 1, 2)
        root.addWidget(tools)

        # node tools: the faucet (testnet/regtest only) and address history
        nt = QGroupBox("Node tools (faucet / address history)")
        ng = QGridLayout(nt)
        self.faucet_addr = QLineEdit()
        self.faucet_addr.setPlaceholderText("address to fund (blank = this node's own wallet)")
        faucet_btn = QPushButton("Ask the faucet")
        faucet_btn.clicked.connect(self._faucet)
        self.hist_addr = QLineEdit()
        self.hist_addr.setPlaceholderText("address to look up (needs a node started with -addrindex)")
        hist_btn = QPushButton("Show history")
        hist_btn.clicked.connect(self._history)
        self.node_result = QLabel("")
        self.node_result.setWordWrap(True)
        self.node_result.setTextInteractionFlags(Qt.TextInteractionFlag.TextSelectableByMouse)
        ng.addWidget(QLabel("faucet"), 0, 0); ng.addWidget(self.faucet_addr, 0, 1); ng.addWidget(faucet_btn, 0, 2)
        ng.addWidget(QLabel("history"), 1, 0); ng.addWidget(self.hist_addr, 1, 1); ng.addWidget(hist_btn, 1, 2)
        ng.addWidget(self.node_result, 2, 0, 1, 3)
        root.addWidget(nt)

        # tables
        tabs = QHBoxLayout()
        self.blocks = self._table(["#", "hash", "tx", "diff"])
        self.mp = self._table(["from", "to", "amount", "fee"])
        self.peers = self._table(["peer", "dir", "ver", "up", "score"])
        bg = QGroupBox("Recent blocks"); QVBoxLayout(bg).addWidget(self.blocks)
        mg = QGroupBox("Mempool"); QVBoxLayout(mg).addWidget(self.mp)
        pg = QGroupBox("Peers"); QVBoxLayout(pg).addWidget(self.peers)
        tabs.addWidget(bg); tabs.addWidget(mg); tabs.addWidget(pg)
        root.addLayout(tabs, 1)

    def _table(self, cols):
        t = QTableWidget(0, len(cols))
        t.setHorizontalHeaderLabels(cols)
        t.horizontalHeader().setSectionResizeMode(QHeaderView.ResizeMode.Stretch)
        t.setEditTriggers(QTableWidget.EditTrigger.NoEditTriggers)
        return t

    def _connect(self, addr):
        addr = addr.strip() or "localhost:8080"
        self.api = Api(addr)
        self.api_edit.setText(addr)
        self._stop_poller()
        self.poller = Poller(self.api, self.wallet.address if self.wallet.enabled() else "")
        self.poller.updated.connect(self.apply_state)
        self.poller.failed.connect(lambda e: self.status.setText("● offline: " + e))
        self._thread = threading.Thread(target=self.poller.run, daemon=True)
        self._thread.start()

    def _stop_poller(self):
        p = getattr(self, "poller", None)
        if p:
            p.stop()

    def apply_state(self, d):
        """Update all widgets from a poll snapshot (also used by tests)."""
        info = d.get("info", {})
        self.mining = bool(info.get("mining"))
        self.status.setText("● live — " + self.api.base)
        self.lbl["network"].setText(str(info.get("network", "—")))
        self.lbl["height"].setText(str(info.get("height", "—")))
        self.lbl["difficulty"].setText(str(info.get("next_difficulty", "—")))
        self.lbl["work"].setText(str(info.get("work", "—")))
        self.lbl["mempool"].setText(str(info.get("mempool", "—")))
        self.lbl["min fee"].setText("%.8f" % (info.get("min_relay_fee", 0) / COIN))
        self.lbl["peers"].setText(str(len(info.get("peers") or [])))
        self.lbl["mining"].setText("ON" if self.mining else "off")
        stats = d.get("stats") or {}
        self.lbl["hashrate"].setText(stats.get("hashrate_fmt") or "—")
        self.addr_lbl.setText("address: " + (d.get("addr") or "(none)"))
        self.bal_lbl.setText("balance: " + (d.get("balance") or "—"))

        health = d.get("health") or {}
        if health:
            if health.get("ok"):
                self.health_lbl.setText("ready — tip %s old, %d block(s) behind" % (
                    health.get("tip_age", "?"), health.get("blocks_behind", 0)))
            else:
                self.health_lbl.setText("NOT READY: " + "; ".join(health.get("reasons") or ["unknown"]))

        blocks = d.get("chain") or []
        recent = list(reversed(blocks))[:12]
        self.blocks.setRowCount(len(recent))
        for r, b in enumerate(recent):
            # Blocks commit a compact nBits target, not the old integer
            # difficulty this column used to read (which rendered as "None").
            vals = [str(b.get("index")), (b.get("hash") or "")[:14],
                    str(len(b.get("transactions") or [])), difficulty_of(b.get("bits", 0))]
            for c, v in enumerate(vals):
                self.blocks.setItem(r, c, QTableWidgetItem(v))

        self._fill_peers(d.get("peers") or [])

        mp = d.get("mempool") or []
        self.mp.setRowCount(len(mp))
        for r, tx in enumerate(mp):
            vals = [(tx.get("from") or "")[:12], (tx.get("to") or "")[:12],
                    "%.8f" % (tx.get("amount", 0) / COIN), "%.8f" % (tx.get("fee", 0) / COIN)]
            for c, v in enumerate(vals):
                self.mp.setItem(r, c, QTableWidgetItem(v))

    def _fill_peers(self, peers):
        self.peers.setRowCount(len(peers))
        for r, p in enumerate(peers):
            addr = p.get("addr") or (p.get("ip", "") + " (no hello)")
            vals = [addr, "in" if p.get("inbound") else "out", str(p.get("version", "")),
                    p.get("connected", ""), str(p.get("ban_score", 0))]
            for c, v in enumerate(vals):
                self.peers.setItem(r, c, QTableWidgetItem(v))

    def _faucet(self):
        """Ask the node's faucet for coin. It only exists on testnet/regtest and
        only when the operator enabled it, so a refusal here is normal."""
        addr = self.faucet_addr.text().strip() or (self.addr_lbl.text().split(": ", 1)[-1])
        try:
            r = self.api.faucet(addr)
            self.node_result.setText("faucet sent %s to %s (%s)" % (
                r.get("amount_fmt", "?"), r.get("to", addr), (r.get("hash") or "")[:12]))
        except Exception as e:  # noqa: BLE001
            self.node_result.setText("faucet: " + str(e))

    def _history(self):
        """Show an address's history from the node's index, if it keeps one."""
        addr = self.hist_addr.text().strip()
        if not addr:
            self.node_result.setText("give an address to look up")
            return
        try:
            r = self.api.address_history(addr)
            lines = ["%d entr%s (total %d):" % (r.get("count", 0),
                                                "y" if r.get("count") == 1 else "ies",
                                                r.get("total", 0))]
            for e in r.get("entries") or []:
                lines.append("  block %s  %s  (%s confs)" % (
                    e.get("height"), (e.get("hash") or "")[:12], e.get("confirmations")))
            self.node_result.setText("\n".join(lines))
        except Exception as e:  # noqa: BLE001
            self.node_result.setText("history: " + str(e) +
                                     "  (the node needs -addrindex)")

    def _toggle_mining(self):
        try:
            self.api.set_mining(not self.mining)
        except Exception as e:  # noqa: BLE001
            QMessageBox.warning(self, "mining", str(e))

    def _apply_payment_uri(self):
        """Expand a `dnas:` payment URI pasted into the "to" field, in place.

        A URI is what a payee hands over, and this is where it gets pasted. It
        carries the amount and the memo, so neither has to be retyped -- and
        retyping the ADDRESS is the dangerous part, since consensus does not
        check recipient checksums.
        """
        uri = parse_payment_uri(self.to_edit.text())
        if uri is None:
            return
        self.to_edit.setText(uri["address"])
        if uri["amount"]:
            self.amt_edit.setText(uri["amount"])
        if uri["memo"]:
            self.memo_edit.setText(uri["memo"])
        self.send_result.setText("read payment URI - check the amount before sending")

    def _send(self):
        try:
            # Paste-then-Send without leaving the field still works.
            self._apply_payment_uri()
            if self.wallet.enabled():
                # Signed locally. The amount and fee are passed through as written,
                # so the CLI's parser is the only thing that interprets them.
                line = self.wallet.send(
                    self.api.base, self.to_edit.text().strip(),
                    self.amt_edit.text().strip(), self.fee_edit.text().strip(),
                    self.memo_edit.text().strip())
                self.send_result.setText(line)
                return
            amount = round(float(self.amt_edit.text()) * COIN)
            fee = round(float(self.fee_edit.text() or "0") * COIN)
            h = self.api.send(self.to_edit.text().strip(), amount, fee, self.memo_edit.text().strip())
            self.send_result.setText("submitted " + h[:16] + "…")
        except Exception as e:  # noqa: BLE001
            self.send_result.setText("rejected: " + str(e))

    def _verify(self):
        try:
            self.verify_result.setText(self.api.verify(self.tx_edit.text().strip()))
        except Exception as e:  # noqa: BLE001
            self.verify_result.setText("error: " + str(e))

    def _multisig(self):
        try:
            threshold = int(self.ms_threshold.text().strip() or "0")
            pubkeys = self.ms_pubkeys.text().replace(",", " ").split()
            addr = self.api.multisig_address(threshold, pubkeys)
            self.ms_result.setText("%d-of-%d address: %s" % (threshold, len(pubkeys), addr))
        except Exception as e:  # noqa: BLE001
            self.ms_result.setText("error: " + str(e))

    def _hd(self):
        try:
            phrase, addrs = self.api.hd_wallet(self.hd_mnemonic.text().strip(), 5)
            lines = ["backup phrase (write it down): " + phrase, ""]
            lines += ["[%d] %s" % (i, a) for i, a in enumerate(addrs)]
            self.hd_result.setText("\n".join(lines))
            self.hd_mnemonic.setText(phrase)  # reveal the generated phrase for copying
        except Exception as e:  # noqa: BLE001
            self.hd_result.setText("error: " + str(e))

    def _launch_node(self):
        if self.node_proc and self.node_proc.poll() is None:
            QMessageBox.information(self, "node", "a local node is already running")
            return
        d = tempfile.mkdtemp(prefix="dnas-gui-node-")
        logf = open(os.path.join(d, "node.log"), "w")
        try:
            self.node_proc = subprocess.Popen(
                [self.dnas_bin, "node", "-api", ":18080", "-listen", ":18060",
                 "-db", os.path.join(d, "chain.db"), "-wallet", os.path.join(d, "wallet.json")],
                stdout=logf, stderr=logf, stdin=subprocess.DEVNULL)
        except FileNotFoundError:
            QMessageBox.warning(self, "node", "dnas binary not found: " + self.dnas_bin)
            return
        time.sleep(0.6)
        self._connect("localhost:18080")

    def closeEvent(self, e):
        self._stop_poller()
        if self.node_proc and self.node_proc.poll() is None:
            self.node_proc.terminate()
        e.accept()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--api", default="localhost:8080")
    ap.add_argument("--dnas", default="dnas", help="path to the dnas binary (for Launch local node and --key)")
    ap.add_argument("--key", default="", help="sign payments locally with this key file (self-custodial; the node never sees it)")
    ap.add_argument("--wallet", default="spvwallet.json", help="light-wallet state file used with --key")
    args = ap.parse_args()
    app = QApplication(sys.argv)
    # A broken self-custodial setup is reported before the window opens, rather
    # than when somebody presses Send.
    if args.key:
        probe = LocalWallet(args.dnas, args.key, args.wallet)
        try:
            print("signing locally as", probe.resolve())
        except Exception as e:  # noqa: BLE001
            print("self-custodial mode:", e, file=sys.stderr)
            sys.exit(1)
    win = Main(args.api, args.dnas, args.key, args.wallet)
    win.show()
    sys.exit(app.exec())


if __name__ == "__main__":
    main()
