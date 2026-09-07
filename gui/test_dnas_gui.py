"""Tests for the DNAS PyQt client.

Covers the wallet tools (multisig + HD), the light-client SPV check — which had
silently rotted when the chain moved to an nBits target and a state root, since
nothing exercised it — and the node surfaces the client grew later (network,
readiness, hashrate, peers, faucet, address history).

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
        p = self.path.split("?", 1)[0]  # routes match the path, not the query
        if p == "/info":
            self._json({"height": 1, "next_difficulty": 4, "work": "1",
                        "mempool": 0, "min_relay_fee": 10000, "peers": [], "mining": False})
        elif p == "/address":
            self._json({"address": "dnasnode"})
        elif p.startswith("/balance/"):
            self._json({"balance_fmt": "0.00000000 DNAS"})
        elif p == "/chain":
            self._json([{"index": 1, "hash": "h1", "bits": STUB_BITS, "transactions": []}])
        elif p == "/mempool":
            self._json([])
        elif p == "/chainstats":
            self._json({"hashrate_fmt": "1.5 kH/s", "window": 2, "median_interval": 5})
        elif p == "/health":
            self._json({"ok": False, "reasons": ["no peers connected"], "tip_age": "3s",
                        "blocks_behind": 0, "network": "regtest"}, 503)
        elif p == "/peers":
            self._json([{"addr": "10.0.0.1:3000", "ip": "10.0.0.1", "identity": "abc",
                         "version": 2, "inbound": False, "ban_score": 7, "connected": "1m"}])
        elif p.startswith("/proof/"):
            self._json(STUB_PROOF)
        elif p.startswith("/header/"):
            self._json(STUB_HEADER)
        elif p.startswith("/address/") and p.endswith("/history"):
            self._json({"address": "dnasx", "total": 1, "count": 1,
                        "entries": [{"height": 3, "hash": "abc123", "confirmations": 2}]})
        elif p.startswith("/address/"):
            self._json({"error": "not found"}, 404)
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
        elif self.path == "/faucet":
            if not req.get("address"):
                self._json({"error": "invalid address"}, 400)
                return
            self._json({"hash": "faucettx", "to": req["address"], "amount_fmt": "10.00000000 DNAS"})
        elif self.path == "/vault/address":
            self._json({"address": "dnasvault", "unlock": req.get("unlock", 0)})
        elif self.path == "/wallet/hd":
            if req.get("mnemonic") == "bad":
                self._json({"error": "invalid mnemonic"}, 400)
                return
            phrase = req.get("mnemonic") or "alpha bravo charlie"
            count = int(req.get("count", 5))
            self._json({"mnemonic": phrase, "addresses": ["dnas%d" % i for i in range(count)]})
        else:
            self._json({"error": "not found"}, 404)


# A header whose hash the client must recompute itself, and a one-step merkle
# proof folding to its root. Built here rather than hardcoded so the fixture
# cannot drift from the client's own hashing.
def _make_header_and_proof():
    import hashlib

    def sha(x):
        return hashlib.sha256(x.encode()).hexdigest()

    tx = "a" * 64
    sibling = "b" * 64
    root = sha(tx + sibling)  # the sibling is on the right
    hdr = {"index": 3, "timestamp": 1735689600, "prev_hash": "0", "merkle_root": root,
           "state_root": "s", "base_fee": 10, "bits": STUB_BITS, "nonce": 0}
    # An easy target (bits with a large exponent) so any hash passes: this test
    # is about the client's arithmetic, not about mining.
    hdr["hash"] = sha(dnas_gui.header_string(hdr))
    proof = {"block_index": 3, "confirmations": 2, "merkle_root": root,
             "proof": [{"hash": sibling, "right": True}]}
    return hdr, proof, tx


STUB_BITS = 0x20FFFFFF  # exponent 0x20 -> a target near 2^255, so anything meets it
STUB_HEADER, STUB_PROOF, STUB_TX = _make_header_and_proof()


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


class PureHelpersTest(unittest.TestCase):
    """The client folds proofs and checks proof of work itself, so its copies of
    the header preimage and the nBits decoding have to match core exactly."""

    def test_header_string_has_every_committed_field(self):
        hs = dnas_gui.header_string(STUB_HEADER)
        # Eight fields, pipe-separated, in the order core.Header.headerString
        # writes them: index, timestamp, prev, merkle, state, base fee, bits, nonce.
        self.assertEqual(len(hs.split("|")), 8, hs)
        self.assertIn(STUB_HEADER["state_root"], hs)
        self.assertIn(str(STUB_HEADER["bits"]), hs)

    def test_compact_to_big_and_meets_target(self):
        # 0x03123456 -> mantissa 0x123456 with exponent 3, i.e. the mantissa itself.
        self.assertEqual(dnas_gui.compact_to_big(0x03123456), 0x123456)
        # A hash below the target passes; one above it does not.
        self.assertTrue(dnas_gui.meets_target("00" + "f" * 62, 0x20FFFFFF))
        self.assertFalse(dnas_gui.meets_target("f" * 64, 0x03000001))
        self.assertFalse(dnas_gui.meets_target("not-hex", 0x20FFFFFF))

    def test_difficulty_of_reads_bits(self):
        self.assertNotEqual(dnas_gui.difficulty_of(STUB_BITS), "—")
        self.assertEqual(dnas_gui.difficulty_of(0), "—")


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

    def test_spv_verify_proves_a_payment(self):
        """This is the regression the GUI most needed: the verifier used the
        pre-nBits header format and would have thrown on every call."""
        api = dnas_gui.Api(self.base)
        self.assertIn("PROVEN", api.verify(STUB_TX))
        # A transaction that is not the proof's leaf must not verify.
        self.assertIn("FAILED", api.verify("c" * 64))

    def test_overview_shows_network_health_and_peers(self):
        w = self._win()
        w.apply_state({
            "info": {"height": 3, "network": "regtest", "next_difficulty": 16, "work": "1",
                     "mempool": 0, "min_relay_fee": 10, "peers": ["a"], "mining": False},
            "addr": "dnasnode", "balance": "1.00000000 DNAS",
            "chain": [{"index": 1, "hash": "h1", "bits": STUB_BITS, "transactions": []}],
            "mempool": [],
            "stats": {"hashrate_fmt": "1.5 kH/s"},
            "health": {"ok": False, "reasons": ["no peers connected"]},
            "peers": [{"addr": "10.0.0.1:3000", "inbound": False, "version": 2,
                       "connected": "1m", "ban_score": 7}],
        })
        self.assertEqual(w.lbl["network"].text(), "regtest")
        self.assertEqual(w.lbl["hashrate"].text(), "1.5 kH/s")
        self.assertIn("NOT READY", w.health_lbl.text())
        self.assertIn("no peers connected", w.health_lbl.text())
        self.assertEqual(w.peers.rowCount(), 1)
        self.assertEqual(w.peers.item(0, 0).text(), "10.0.0.1:3000")
        self.assertEqual(w.peers.item(0, 4).text(), "7")
        # The blocks table must render a difficulty from `bits`; it used to read a
        # `difficulty` field that no longer exists and showed "None".
        self.assertNotIn("None", w.blocks.item(0, 3).text())

    def test_faucet_and_history_buttons(self):
        w = self._win()
        w.faucet_addr.setText("dnasbob")
        w._faucet()
        self.assertIn("faucet sent", w.node_result.text())
        w.hist_addr.setText("dnasbob")
        w._history()
        self.assertIn("block 3", w.node_result.text())
        # An empty lookup is refused rather than querying nothing.
        w.hist_addr.setText("")
        w._history()
        self.assertIn("give an address", w.node_result.text())

    def test_hd_button_reveals_phrase_and_addresses(self):
        w = self._win()
        w.hd_mnemonic.setText("")  # blank -> generate
        w._hd()
        self.assertIn("alpha bravo charlie", w.hd_result.text())
        self.assertIn("[0] dnas0", w.hd_result.text())
        self.assertEqual(w.hd_mnemonic.text(), "alpha bravo charlie")


if __name__ == "__main__":
    unittest.main()


class SelfCustodyTest(unittest.TestCase):
    """Spending your own key from the GUI.

    The GUI cannot sign: doing so would mean a second, hand-written copy of the
    canonical transaction encoding in Python, which is how this client's SPV
    verifier silently rotted once already. So it delegates to the `dnas` binary,
    and what these tests check is that the delegation is right — the arguments,
    and reading the outcome out of the output rather than assuming it.
    """

    @classmethod
    def setUpClass(cls):
        # Constructing a window needs a QApplication, and unittest gives no
        # ordering guarantee that another test class made one first.
        from PyQt6.QtWidgets import QApplication
        cls.app = QApplication.instance() or QApplication([])

    def _fake_dnas(self, script):
        import stat
        import tempfile
        d = tempfile.mkdtemp()
        path = os.path.join(d, "dnas")
        with open(path, "w") as f:
            f.write("#!/bin/sh\n" + script)
        os.chmod(path, os.stat(path).st_mode | stat.S_IEXEC)
        return path

    def test_resolve_reports_the_address(self):
        bin_path = self._fake_dnas(
            'if [ "$1" = "wallet" ]; then '
            'echo dnasaaaabbbbccccddddeeeeffff00001111222233334444; '
            'echo "a log line written after the result" >&2; exit 0; fi\nexit 9\n')
        w = dnas_gui.LocalWallet(bin_path, "k.json")
        self.assertTrue(w.enabled())
        self.assertEqual(w.resolve(), "dnasaaaabbbbccccddddeeeeffff00001111222233334444")

        # A broken setup must be reported up front, not during a payment.
        with self.assertRaises(RuntimeError):
            dnas_gui.LocalWallet(self._fake_dnas("exit 3"), "k.json").resolve()
        with self.assertRaises(RuntimeError):
            dnas_gui.LocalWallet(self._fake_dnas("echo not-an-address"), "k.json").resolve()
        # No key file means node-signed, and nothing to resolve.
        self.assertFalse(dnas_gui.LocalWallet(bin_path, "").enabled())

    def test_send_passes_the_right_arguments(self):
        import tempfile
        args_file = os.path.join(tempfile.mkdtemp(), "args")
        bin_path = self._fake_dnas(
            'echo "$@" > %s\n'
            'echo "submitted abc123 -> dnasx  1.50000000 DNAS (fee 0.00010000 DNAS, nonce 0)"\n'
            'echo "10:00:00 a log line written after the result" >&2\n' % args_file)
        w = dnas_gui.LocalWallet(bin_path, "k.json", "state.json")
        line = w.send("http://localhost:1", "dnasdest", "1.5", "0.001", "rent")
        self.assertTrue(line.startswith("submitted"), line)
        with open(args_file) as f:
            got = f.read()
        for want in ("spv", "-api http://localhost:1", "-f state.json", "-key k.json",
                     "-memo rent", "send dnasdest 1.5 0.001"):
            self.assertIn(want, got)

    def test_send_reads_the_outcome_not_the_exit_code(self):
        # The CLI reports a refusal it handled itself on stdout with a ZERO exit
        # status, so a send that failed would otherwise be shown as submitted.
        quiet_failure = self._fake_dnas('echo "insufficient proven balance: have 0.1, need 5"\n')
        w = dnas_gui.LocalWallet(quiet_failure, "k.json")
        with self.assertRaises(RuntimeError) as cm:
            w.send("http://localhost:1", "dnasdest", "5")
        self.assertIn("insufficient", str(cm.exception))

        loud_failure = self._fake_dnas('echo "key error: no such file" >&2\nexit 1\n')
        w = dnas_gui.LocalWallet(loud_failure, "k.json")
        with self.assertRaises(RuntimeError) as cm:
            w.send("http://localhost:1", "dnasdest", "5")
        self.assertIn("key error", str(cm.exception))

    def test_last_line_skips_logs_and_blanks(self):
        for text, want in (("one", "one"), ("log\nresult", "result"),
                           ("log\nresult\n\n  \n", "result"), ("", ""), ("\n\n", "")):
            self.assertEqual(dnas_gui.last_line(text), want)

    def test_wallet_panel_names_whose_key_it_is(self):
        # Which key a payment comes from is not a detail, so the panel says.
        bin_path = self._fake_dnas('echo dnasaaaabbbbccccddddeeeeffff00001111222233334444\n')
        custodial = dnas_gui.Main("localhost:1", "dnas")
        self.assertFalse(custodial.wallet.enabled())
        own = dnas_gui.Main("localhost:1", bin_path, "k.json")
        self.assertTrue(own.wallet.enabled())
        self.assertEqual(own.wallet.address, "dnasaaaabbbbccccddddeeeeffff00001111222233334444")
        for win in (custodial, own):
            win._stop_poller()


class PaymentURITest(unittest.TestCase):
    """A `dnas:` URI is what a payee hands over, and the send form is where it
    gets pasted. Until it was parsed, the payer still retyped the address — the
    field a typo silently burns coin into, since consensus does not check
    recipient checksums.
    """

    ADDR = "dnas906a1032a67ad2230b32f6d57c76d677c55cf9fd54d3fc31"

    @classmethod
    def setUpClass(cls):
        # Constructing a window needs a QApplication, and unittest gives no
        # ordering guarantee that another test class made one first.
        from PyQt6.QtWidgets import QApplication
        cls.app = QApplication.instance() or QApplication([])

    def test_parses_the_forms_people_paste(self):
        for text in ("dnas:" + self.ADDR,
                     "dnas://" + self.ADDR,
                     "DNAS:" + self.ADDR,
                     "  dnas:" + self.ADDR + "  "):
            got = dnas_gui.parse_payment_uri(text)
            self.assertIsNotNone(got, text)
            self.assertEqual(got["address"], self.ADDR)

    def test_reads_amount_and_memo(self):
        got = dnas_gui.parse_payment_uri(
            "dnas:" + self.ADDR + "?amount=2.5&memo=two+coffees&ref=r1")
        self.assertEqual(got["address"], self.ADDR)
        # The amount stays as WRITTEN: the CLI's parser is the only thing that
        # turns it into base units, so the GUI cannot round it differently.
        self.assertEqual(got["amount"], "2.5")
        self.assertEqual(got["memo"], "two coffees")

    def test_escaped_memo(self):
        got = dnas_gui.parse_payment_uri("dnas:" + self.ADDR + "?memo=a+%26+b")
        self.assertEqual(got["memo"], "a & b")

    def test_unknown_parameters_are_ignored(self):
        got = dnas_gui.parse_payment_uri("dnas:" + self.ADDR + "?amount=1&future=x")
        self.assertEqual(got["amount"], "1")

    def test_non_uris_return_none(self):
        for text in (self.ADDR, "", None, "bitcoin:" + self.ADDR, "dnas:", "dnas:?amount=1"):
            self.assertIsNone(dnas_gui.parse_payment_uri(text), repr(text))

    def test_send_form_expands_a_pasted_uri(self):
        win = dnas_gui.Main("localhost:1", "dnas")
        try:
            win.to_edit.setText("dnas:" + self.ADDR + "?amount=2.5&memo=two+coffees")
            win._apply_payment_uri()
            self.assertEqual(win.to_edit.text(), self.ADDR)
            self.assertEqual(win.amt_edit.text(), "2.5")
            self.assertEqual(win.memo_edit.text(), "two coffees")
        finally:
            win._stop_poller()

    def test_a_plain_address_is_left_alone(self):
        win = dnas_gui.Main("localhost:1", "dnas")
        try:
            win.to_edit.setText(self.ADDR)
            win.amt_edit.setText("7")
            win._apply_payment_uri()
            self.assertEqual(win.to_edit.text(), self.ADDR)
            self.assertEqual(win.amt_edit.text(), "7")
        finally:
            win._stop_poller()

    def test_a_uri_without_an_amount_keeps_the_typed_one(self):
        win = dnas_gui.Main("localhost:1", "dnas")
        try:
            win.amt_edit.setText("3")
            win.to_edit.setText("dnas:" + self.ADDR)
            win._apply_payment_uri()
            self.assertEqual(win.to_edit.text(), self.ADDR)
            self.assertEqual(win.amt_edit.text(), "3")
        finally:
            win._stop_poller()
