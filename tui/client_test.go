package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	mux.HandleFunc("/chain", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"index":0,"hash":"g"},{"index":1,"hash":"h1","difficulty":4,"transactions":[{"from":"COINBASE","to":"x","amount":5000000000}]}]`)
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
	blocks, err := c.Chain()
	if err != nil || len(blocks) != 2 || blocks[1].Transactions[0].From != "COINBASE" {
		t.Fatalf("Chain = %+v err=%v", blocks, err)
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

func TestClientVerifyTx(t *testing.T) {
	// A single-transaction block: the proof is empty and the merkle root is the
	// leaf itself. Use difficulty 0 so the PoW prefix check is trivially met, and
	// set the header hash to its own computed hash.
	txh := "abc123"
	hs := fmt.Sprintf("%d|%d|%s|%s|%d|%d", 1, 0, "p", txh, 0, 0)
	hdrHash := sha(hs)

	mux := http.NewServeMux()
	mux.HandleFunc("/proof/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"found":true,"block_index":1,"confirmations":2,"merkle_root":%q,"proof":[]}`, txh)
	})
	mux.HandleFunc("/header/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"index":1,"timestamp":0,"prev_hash":"p","merkle_root":%q,"difficulty":0,"nonce":0,"hash":%q}`, txh, hdrHash)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := NewClient(srv.URL).VerifyTx(txh)
	if err != nil {
		t.Fatal(err)
	}
	if got := res; got[:6] != "PROVEN" {
		t.Fatalf("VerifyTx = %q, want it to start with PROVEN", got)
	}
}
