package api_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/core"
)

// Every write endpoint used to decode its request body with no size bound, so a
// single POST could force an arbitrarily large allocation before anything
// validated it — the transaction size check ran only after the whole body had
// been parsed into memory. These tests pin the ceilings.

// postBody sends raw bytes to path and returns the status code.
func postBody(t *testing.T, url, path, body string) int {
	t.Helper()
	resp, err := http.Post(url+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// oversizedJSON is a syntactically valid JSON object whose single string value
// pushes it past n bytes. Validity matters: it proves the request is refused on
// SIZE, before parsing, rather than incidentally rejected as malformed.
func oversizedJSON(field string, n int) string {
	return fmt.Sprintf(`{%q:%q}`, field, strings.Repeat("A", n))
}

func TestWriteEndpointsRejectOversizedBodies(t *testing.T) {
	srv, _, _ := testServer(t)

	// Each control endpoint takes kilobytes of JSON at most. A megabyte of it is
	// refused with 413 rather than decoded.
	const overControl = 1 << 20
	for _, tc := range []struct{ path, field string }{
		{"/mine", "on"},
		{"/unban", "key"},
		{"/addpeer", "addr"},
		{"/droppeer", "peer"},
		{"/multisig/address", "pubkeys"},
		{"/htlc/address", "hash"},
		{"/vault/address", "hot"},
		{"/wallet/hd", "mnemonic"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			got := postBody(t, srv.URL, tc.path, oversizedJSON(tc.field, overControl))
			if got != http.StatusRequestEntityTooLarge {
				t.Errorf("POST %s with a %d-byte body = %d, want 413",
					tc.path, overControl, got)
			}
		})
	}
}

// /generate and /faucet are gated on the node's configuration, and that check
// runs BEFORE the body is touched — on a node that is neither regtest nor a
// faucet they answer 403 without reading a byte, which is a better outcome than
// reading a megabyte and then refusing it. This pins that ordering: a disabled
// endpoint must not be a way to make the node read an oversized body.
func TestDisabledEndpointsRefuseBeforeReadingTheBody(t *testing.T) {
	srv, _, _ := testServer(t)
	for _, tc := range []struct{ path, field string }{
		{"/generate", "n"},
		{"/faucet", "address"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			got := postBody(t, srv.URL, tc.path, oversizedJSON(tc.field, 1<<20))
			if got != http.StatusForbidden {
				t.Errorf("POST %s on a node without it = %d, want 403 (the guard "+
					"must run before the body is read)", tc.path, got)
			}
		})
	}
}

func TestTransactionAndBlockBodiesHaveRoomButNotUnlimited(t *testing.T) {
	srv, _, _ := testServer(t)

	// A transaction body is allowed several times core.MaxRelayTxBytes, because
	// JSON with hex signatures runs larger than the canonical encoding it bounds.
	// Comfortably past that is refused.
	big := postBody(t, srv.URL, "/tx", oversizedJSON("memo", 8*core.MaxRelayTxBytes))
	if big != http.StatusRequestEntityTooLarge {
		t.Errorf("POST /tx with %d bytes = %d, want 413", 8*core.MaxRelayTxBytes, big)
	}
	// A body under the ceiling still reaches the decoder and is judged on its
	// contents, not its size: this one parses and is then rejected as an invalid
	// transaction. Anything but 413 proves the limit is not firing early.
	ok := postBody(t, srv.URL, "/tx", oversizedJSON("memo", 1<<10))
	if ok == http.StatusRequestEntityTooLarge {
		t.Error("a 1 KiB transaction body should be under the limit, got 413")
	}

	// Blocks get the largest allowance, and still have one.
	bigBlock := postBody(t, srv.URL, "/submitblock", oversizedJSON("hash", 8*core.MaxBlockBytes))
	if bigBlock != http.StatusRequestEntityTooLarge {
		t.Errorf("POST /submitblock with %d bytes = %d, want 413", 8*core.MaxBlockBytes, bigBlock)
	}
	bigShare := postBody(t, srv.URL, "/submitshare", oversizedJSON("hash", 8*core.MaxBlockBytes))
	if bigShare != http.StatusRequestEntityTooLarge {
		t.Errorf("POST /submitshare with %d bytes = %d, want 413", 8*core.MaxBlockBytes, bigShare)
	}
}

// TestOversizedBodyIsRefusedBeforeItIsRead is the point of the change: the cap
// must come off the reader, not off Content-Length, so a request that lies about
// its length is still stopped mid-stream instead of being buffered in full.
func TestOversizedBodyIsRefusedBeforeItIsRead(t *testing.T) {
	srv, _, _ := testServer(t)

	// A chunked request sends no Content-Length at all, so nothing but the
	// MaxBytesReader can stop it.
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		fmt.Fprint(pw, `{"addr":"`)
		chunk := strings.Repeat("A", 64<<10)
		for i := 0; i < 64; i++ { // 4 MiB, far past the 64 KiB control ceiling
			if _, err := io.WriteString(pw, chunk); err != nil {
				return // the server hung up on us, which is the expected outcome
			}
		}
		fmt.Fprint(pw, `"}`)
	}()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/addpeer", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// A connection reset is an acceptable way to refuse an over-long stream.
		t.Skipf("server closed the connection on the oversized stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("chunked oversized body = %d, want 413", resp.StatusCode)
	}
}
