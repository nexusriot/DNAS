package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDNAS(t *testing.T) {
	cases := map[string]uint64{"1": coin, "1.5": coin + coin/2, "0.00000001": 1, "0": 0}
	for in, want := range cases {
		got, err := parseDNAS(in)
		if err != nil || got != want {
			t.Errorf("parseDNAS(%q) = %d,%v want %d", in, got, err, want)
		}
	}
	if _, err := parseDNAS("abc"); err == nil {
		t.Error("parseDNAS should reject non-numbers")
	}
}

func TestClientInfoAndParsing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"height":7,"mining":true,"work":"1234","next_difficulty":4,"mempool":2,"peers":["a","b"]}`)
	})
	var lastQuery string
	mux.HandleFunc("/chain", func(w http.ResponseWriter, r *http.Request) {
		lastQuery = r.URL.RawQuery
		fmt.Fprint(w, `[{"index":0,"hash":"g"},{"index":1,"hash":"h1","bits":508715007,"transactions":[{"from":"COINBASE","to":"x","amount":5000000000}]}]`)
	})
	mux.HandleFunc("/mempool", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"from":"a","to":"b","amount":100,"fee":1}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClient(srv.URL)

	info, err := c.Info()
	if err != nil || info.Height != 7 || !info.Mining || len(info.Peers) != 2 {
		t.Fatalf("Info = %+v err=%v", info, err)
	}
	// The dashboard asks for the TAIL of the chain, not the whole thing: /chain is
	// paged, so an unparameterized request would return the OLDEST page and the
	// panel would show genesis forever on a long chain.
	blocks, err := c.RecentBlocks(12)
	if err != nil || len(blocks) != 2 || blocks[1].Transactions[0].From != "COINBASE" {
		t.Fatalf("RecentBlocks = %+v err=%v", blocks, err)
	}
	if lastQuery != "last=12" {
		t.Fatalf("RecentBlocks asked for %q, want last=12", lastQuery)
	}
	mp, err := c.Mempool()
	if err != nil || len(mp) != 1 || mp[0].Amount != 100 {
		t.Fatalf("Mempool = %+v err=%v", mp, err)
	}
}

func TestClientSendSuccessAndError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		if req["to"] == "dnasbad" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid recipient"}`)
			return
		}
		fmt.Fprint(w, `{"hash":"deadbeef","nonce":3}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClient(srv.URL)

	h, err := c.Send("dnasgood", 100, 1, "")
	if err != nil || h != "deadbeef" {
		t.Fatalf("Send ok = %q err=%v", h, err)
	}
	if _, err := c.Send("dnasbad", 100, 1, ""); err == nil || err.Error() != "invalid recipient" {
		t.Fatalf("Send error should surface the API message, got %v", err)
	}
}

func TestClientMultisigAddress(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/multisig/address", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		if req["threshold"].(float64) != 2 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"bad threshold"}`)
			return
		}
		fmt.Fprint(w, `{"threshold":2,"n":3,"address":"dnasmultisig"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClient(srv.URL)

	addr, err := c.MultisigAddress(2, []string{"a", "b", "c"})
	if err != nil || addr != "dnasmultisig" {
		t.Fatalf("MultisigAddress = %q err=%v", addr, err)
	}
	if _, err := c.MultisigAddress(9, []string{"a"}); err == nil || err.Error() != "bad threshold" {
		t.Fatalf("MultisigAddress should surface the API error, got %v", err)
	}
}

func TestClientHDWallet(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/wallet/hd", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		if m, _ := req["mnemonic"].(string); m == "bad" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid mnemonic"}`)
			return
		}
		fmt.Fprint(w, `{"mnemonic":"alpha bravo charlie","addresses":["dnas0","dnas1"]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClient(srv.URL)

	phrase, addrs, err := c.HDWallet("", 2)
	if err != nil || phrase != "alpha bravo charlie" || len(addrs) != 2 || addrs[1] != "dnas1" {
		t.Fatalf("HDWallet = %q %v err=%v", phrase, addrs, err)
	}
	if _, _, err := c.HDWallet("bad", 1); err == nil || err.Error() != "invalid mnemonic" {
		t.Fatalf("HDWallet should surface the API error, got %v", err)
	}
}

// VerifyTx delegates to the dnas binary, so the test drives a stand-in binary
// and checks the delegation itself: the right subcommand, the right node, and
// the binary's verdict surfaced verbatim.
//
// It deliberately does NOT re-derive a header hash here. The previous version
// of this test did, with the same wrong preimage the client used, so the two
// agreed with each other and both disagreed with the chain — which is how a
// verifier stays broken for months with a passing test.
func TestClientVerifyTxDelegatesToTheBinary(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	fake := filepath.Join(dir, "fake-dnas")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\n" +
		"echo 'a log line on stderr' >&2\n" +
		"echo '\u2713 tx abc123 is in block 7 with 2 confirmation(s)'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	c := NewClient("localhost:19098")
	c.UseBinary(fake)
	res, err := c.VerifyTx("abc123")
	if err != nil {
		t.Fatal(err)
	}
	// The verdict comes from stdout's last line; the stderr log must not leak
	// into it, which is the trap a merged stream sets.
	if !strings.Contains(res, "is in block 7") {
		t.Fatalf("VerifyTx = %q, want the binary's verdict", res)
	}
	if strings.Contains(res, "a log line") {
		t.Fatalf("VerifyTx = %q: stderr leaked into the result", res)
	}

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(raw))
	want := []string{"spv", "-api", "http://localhost:19098", "-cache", "", "verify", "abc123"}
	// The empty -cache argument does not survive Fields, so compare what does.
	wantNonEmpty := []string{}
	for _, w := range want {
		if w != "" {
			wantNonEmpty = append(wantNonEmpty, w)
		}
	}
	if len(got) != len(wantNonEmpty) {
		t.Fatalf("called with %v, want %v", got, wantNonEmpty)
	}
	for i := range got {
		if got[i] != wantNonEmpty[i] {
			t.Fatalf("called with %v, want %v", got, wantNonEmpty)
		}
	}
}

// A binary that fails must surface its message rather than a bare exit status.
func TestClientVerifyTxSurfacesBinaryFailure(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-dnas")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 'error: no such transaction' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := NewClient("localhost:19098")
	c.UseBinary(fake)
	if _, err := c.VerifyTx("nope"); err == nil || !strings.Contains(err.Error(), "no such transaction") {
		t.Fatalf("err = %v, want the binary's message", err)
	}
}
