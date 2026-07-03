"""Tests for the DNAS PyQt client's new wallet tools (multisig + HD).

Run headless from the gui/ directory:

    QT_QPA_PLATFORM=offscreen python3 -m unittest test_dnas_gui -v

A stub HTTP server stands in for a node so the tests need no running chain.
"""
import json
import os
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

import dnas_gui  # noqa: E402  (imported after choosing the Qt platform)


class _Stub(BaseHTTPRequestHandler):
    def log_message(self, *_a):  # keep test output clean
        pass

    def _json(self, obj, code=200):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):  # noqa: N802 (BaseHTTPRequestHandler API)
        p = self.path
        if p == "/info":
            self._json({"height": 1, "next_difficulty": 4, "work": "1",
                        "mempool": 0, "min_relay_fee": 10000, "peers": [], "mining": False})
        elif p == "/address":
            self._json({"address": "dnasnode"})
        elif p.startswith("/balance/"):
            self._json({"balance_fmt": "0.00000000 DNAS"})
        elif p in ("/chain", "/mempool"):
            self._json([])
        else:
            self._json({"error": "not found"}, 404)

    def do_POST(self):  # noqa: N802
        n = int(self.headers.get("Content-Length", 0))
        req = json.loads(self.rfile.read(n) or b"{}")
        if self.path == "/multisig/address":
            if req.get("threshold", 0) > len(req.get("pubkeys", [])):
                self._json({"error": "threshold too high"}, 400)
                return
            self._json({"threshold": req["threshold"], "n": len(req["pubkeys"]),
                        "address": "dnasms_" + "_".join(req["pubkeys"])})
        elif self.path == "/wallet/hd":
            if req.get("mnemonic") == "bad":
                self._json({"error": "invalid mnemonic"}, 400)
                return
            phrase = req.get("mnemonic") or "alpha bravo charlie"
            count = int(req.get("count", 5))
            self._json({"mnemonic": phrase, "addresses": ["dnas%d" % i for i in range(count)]})
        else:
            self._json({"error": "not found"}, 404)


def _serve():
    srv = ThreadingHTTPServer(("127.0.0.1", 0), _Stub)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


class ApiTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = _serve()
        cls.base = "127.0.0.1:%d" % cls.srv.server_address[1]

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def test_multisig_address(self):
        api = dnas_gui.Api(self.base)
        self.assertEqual(api.multisig_address(2, ["aa", "bb", "cc"]), "dnasms_aa_bb_cc")
        with self.assertRaises(RuntimeError):
            api.multisig_address(9, ["aa"])

    def test_hd_wallet(self):
        api = dnas_gui.Api(self.base)
        phrase, addrs = api.hd_wallet("", 3)
        self.assertEqual(phrase, "alpha bravo charlie")
        self.assertEqual(addrs, ["dnas0", "dnas1", "dnas2"])
        phrase2, _ = api.hd_wallet("my words", 2)  # restore echoes the mnemonic
        self.assertEqual(phrase2, "my words")
        with self.assertRaises(RuntimeError):
            api.hd_wallet("bad", 1)


class WidgetTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = _serve()
        cls.base = "127.0.0.1:%d" % cls.srv.server_address[1]
        from PyQt6.QtWidgets import QApplication
        cls.app = QApplication.instance() or QApplication([])

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def _win(self):
        w = dnas_gui.Main(self.base, "dnas")
        w._stop_poller()  # no background polling during the test
        return w

    def test_multisig_button_sets_result(self):
        w = self._win()
        w.ms_threshold.setText("2")
        w.ms_pubkeys.setText("aa, bb, cc")
        w._multisig()
        self.assertIn("dnasms_aa_bb_cc", w.ms_result.text())

    def test_hd_button_reveals_phrase_and_addresses(self):
        w = self._win()
        w.hd_mnemonic.setText("")  # blank -> generate
        w._hd()
        self.assertIn("alpha bravo charlie", w.hd_result.text())
        self.assertIn("[0] dnas0", w.hd_result.text())
        self.assertEqual(w.hd_mnemonic.text(), "alpha bravo charlie")


if __name__ == "__main__":
    unittest.main()
