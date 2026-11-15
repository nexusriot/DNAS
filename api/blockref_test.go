package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/api"
	"github.com/nexusriot/DNAS/core"
)

// /block/{ref} and /header/{ref} take a block hash as well as a height, resolved
// through the chain's hash index.
func TestBlockAndHeaderByHash(t *testing.T) {
	srv, _, _ := testServer(t)
	genesis := core.GenesisBlock().Hash

	for _, path := range []string{"/block/", "/header/"} {
		for _, ref := range []string{genesis, strings.ToUpper(genesis)} {
			resp, err := http.Get(srv.URL + path + ref)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Hash  string `json:"hash"`
				Index uint64 `json:"index"`
			}
			err = json.NewDecoder(resp.Body).Decode(&got)
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK || got.Hash != genesis || got.Index != 0 {
				t.Fatalf("%s%s: status %d, %+v, %v", path, ref[:8], resp.StatusCode, got, err)
			}
		}
		unknown := strings.Repeat("ab", 32)
		if r, _ := http.Get(srv.URL + path + unknown); r.StatusCode != http.StatusNotFound {
			t.Errorf("%s<unknown hash> = %d, want 404", path, r.StatusCode)
		}
		if r, _ := http.Get(srv.URL + path + strings.Repeat("zz", 32)); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s<64 non-hex chars> = %d, want 400", path, r.StatusCode)
		}
	}
}

// /info names the node's identity key — the thing another operator pins it by.
// It was readable only from some other node's /peers.
func TestInfoReportsTheNodeIdentity(t *testing.T) {
	srv, _, w := testServer(t)
	var info api.InfoResponse
	getJSON(t, srv.URL+"/info", &info)
	// testServer builds the node without a separate key, so its identity is the
	// wallet's; what matters is that /info reports the key the node proves.
	if info.Identity == "" || info.Identity != w.PublicKeyHex() {
		t.Fatalf("identity %q, want %q", info.Identity, w.PublicKeyHex())
	}
}
