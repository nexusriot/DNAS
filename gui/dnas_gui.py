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
import urllib.request

from PyQt6.QtCore import QObject, Qt, pyqtSignal
from PyQt6.QtWidgets import (
    QApplication, QGridLayout, QGroupBox, QHBoxLayout, QHeaderView, QLabel,
    QLineEdit, QMessageBox, QPushButton, QTableWidget, QTableWidgetItem,
    QVBoxLayout, QWidget,
)

COIN = 100_000_000


def sha(s: str) -> str:
    return hashlib.sha256(s.encode()).hexdigest()


class Api:
    """Thin HTTP client for a node's API (stdlib urllib)."""

    def __init__(self, base: str):
        if not base.startswith("http"):
            base = "http://" + base
        self.base = base.rstrip("/")

    def _get(self, path):
        with urllib.request.urlopen(self.base + path, timeout=2) as r:
            return json.load(r)

    def _post(self, path, body):
        req = urllib.request.Request(
            self.base + path, data=json.dumps(body).encode(),
            headers={"Content-Type": "application/json"}, method="POST")
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

    def chain(self):
        return self._get("/chain")

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
        pr = self._get("/proof/" + txh)
        hdr = self._get("/header/%d" % pr["block_index"])
        hs = "%d|%d|%s|%s|%d|%d" % (hdr["index"], hdr["timestamp"], hdr["prev_hash"],
                                    hdr["merkle_root"], hdr["difficulty"], hdr["nonce"])
        pow_ok = sha(hs) == hdr["hash"] and hdr["hash"].startswith("0" * hdr["difficulty"])
        h = txh
        for step in pr["proof"]:
            h = sha(h + step["hash"]) if step["right"] else sha(step["hash"] + h)
        inc = h == hdr["merkle_root"]
        if pow_ok and inc:
            return "PROVEN in block %d (%d confirmations)" % (pr["block_index"], pr["confirmations"])
        return "FAILED (pow=%s inclusion=%s)" % (pow_ok, inc)


class Poller(QObject):
    """Background poller; emits state to the GUI thread via a Qt signal."""
    updated = pyqtSignal(dict)
    failed = pyqtSignal(str)

    def __init__(self, api: Api):
        super().__init__()
        self.api = api
        self._stop = False

    def run(self):
        while not self._stop:
            try:
                addr = self.api.address()
                data = {
                    "info": self.api.info(),
                    "addr": addr,
                    "balance": self.api.balance(addr) if addr else "",
                    "chain": self.api.chain(),
                    "mempool": self.api.mempool(),
                }
                self.updated.emit(data)
            except Exception as e:  # noqa: BLE001 (report any connectivity error)
                self.failed.emit(str(e))
            time.sleep(1.5)

    def stop(self):
        self._stop = True


class Main(QWidget):
    def __init__(self, api_addr: str, dnas_bin: str):
        super().__init__()
        self.dnas_bin = dnas_bin
        self.node_proc = None
        self.mining = False
        self.setWindowTitle("DNAS")
        self.resize(880, 720)
        self._build()
        self._connect(api_addr)

    # --- layout -------------------------------------------------------------
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
        for i, k in enumerate(["height", "difficulty", "work", "mempool", "min fee", "peers", "mining"]):
            g.addWidget(QLabel(k + ":"), i // 3, (i % 3) * 2)
            self.lbl[k] = QLabel("—")
            g.addWidget(self.lbl[k], i // 3, (i % 3) * 2 + 1)
        self.mine_btn = QPushButton("Toggle mining")
        self.mine_btn.clicked.connect(self._toggle_mining)
        g.addWidget(self.mine_btn, 3, 0, 1, 6)
        root.addWidget(ov)

        # wallet
        wal = QGroupBox("This node's wallet")
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

        # tables
        tabs = QHBoxLayout()
        self.blocks = self._table(["#", "hash", "tx", "diff"])
        self.mp = self._table(["from", "to", "amount", "fee"])
        bg = QGroupBox("Recent blocks"); QVBoxLayout(bg).addWidget(self.blocks)
        mg = QGroupBox("Mempool"); QVBoxLayout(mg).addWidget(self.mp)
        tabs.addWidget(bg); tabs.addWidget(mg)
        root.addLayout(tabs, 1)

    def _table(self, cols):
        t = QTableWidget(0, len(cols))
        t.setHorizontalHeaderLabels(cols)
        t.horizontalHeader().setSectionResizeMode(QHeaderView.ResizeMode.Stretch)
        t.setEditTriggers(QTableWidget.EditTrigger.NoEditTriggers)
        return t

    # --- connection / polling ----------------------------------------------
    def _connect(self, addr):
        addr = addr.strip() or "localhost:8080"
        self.api = Api(addr)
        self.api_edit.setText(addr)
        self._stop_poller()
        self.poller = Poller(self.api)
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
        self.lbl["height"].setText(str(info.get("height", "—")))
        self.lbl["difficulty"].setText(str(info.get("next_difficulty", "—")))
        self.lbl["work"].setText(str(info.get("work", "—")))
        self.lbl["mempool"].setText(str(info.get("mempool", "—")))
        self.lbl["min fee"].setText("%.8f" % (info.get("min_relay_fee", 0) / COIN))
        self.lbl["peers"].setText(str(len(info.get("peers") or [])))
        self.lbl["mining"].setText("ON" if self.mining else "off")
        self.addr_lbl.setText("address: " + (d.get("addr") or "(none)"))
        self.bal_lbl.setText("balance: " + (d.get("balance") or "—"))

        blocks = d.get("chain") or []
        recent = list(reversed(blocks))[:12]
        self.blocks.setRowCount(len(recent))
        for r, b in enumerate(recent):
            vals = [str(b.get("index")), (b.get("hash") or "")[:14],
                    str(len(b.get("transactions") or [])), str(b.get("difficulty"))]
            for c, v in enumerate(vals):
                self.blocks.setItem(r, c, QTableWidgetItem(v))

        mp = d.get("mempool") or []
        self.mp.setRowCount(len(mp))
        for r, tx in enumerate(mp):
            vals = [(tx.get("from") or "")[:12], (tx.get("to") or "")[:12],
                    "%.8f" % (tx.get("amount", 0) / COIN), "%.8f" % (tx.get("fee", 0) / COIN)]
            for c, v in enumerate(vals):
                self.mp.setItem(r, c, QTableWidgetItem(v))

    # --- actions ------------------------------------------------------------
    def _toggle_mining(self):
        try:
            self.api.set_mining(not self.mining)
        except Exception as e:  # noqa: BLE001
            QMessageBox.warning(self, "mining", str(e))

    def _send(self):
        try:
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
    ap.add_argument("--dnas", default="dnas", help="path to the dnas binary (for Launch local node)")
    args = ap.parse_args()
    app = QApplication(sys.argv)
    win = Main(args.api, args.dnas)
    win.show()
    sys.exit(app.exec())


if __name__ == "__main__":
    main()
