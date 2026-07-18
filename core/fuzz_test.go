package core

import (
	"encoding/json"
	"testing"
)

// FuzzTransactionDecode ensures arbitrary JSON never panics the transaction
// paths (decode, Hash, IsExpiredAt, VerifySignature).
func FuzzTransactionDecode(f *testing.F) {
	f.Add([]byte(`{"from":"dnasx","to":"dnasy","amount":5,"nonce":1}`))
	f.Add([]byte(`{"from":"COINBASE","to":"dnasy","amount":50}`))
	f.Add([]byte(`{"multisig":{"threshold":2,"pubkeys":["a","b"]},"signatures":["x"]}`))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var tx Transaction
		if json.Unmarshal(data, &tx) != nil {
			return
		}
		_ = tx.Hash()
		_ = tx.IsExpiredAt(10)
		_ = tx.IsLockedAt(10)
		_ = tx.VerifySignature() // must not panic on adversarial input
	})
}

// FuzzHeaderPoW ensures header proof-of-work checking never panics.
func FuzzHeaderPoW(f *testing.F) {
	f.Add([]byte(`{"index":1,"bits":486604799,"hash":"0000abcd"}`))
	f.Add([]byte(`{"bits":0}`))
	f.Add([]byte(`{"bits":4294967295}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var h Header
		if json.Unmarshal(data, &h) != nil {
			return
		}
		// Bits is a compact uint32 target; the target comparison handles any value
		// without a huge allocation, so no guard is needed.
		_ = h.HasValidPoW()
		_ = h.ComputeHash()
	})
}

// FuzzVerifyMerkleProof ensures proof verification is panic-free for any inputs.
func FuzzVerifyMerkleProof(f *testing.F) {
	f.Add("leaf", "root", "sib", true)
	f.Fuzz(func(t *testing.T, leaf, root, sib string, right bool) {
		_ = VerifyMerkleProof(leaf, root, []MerkleProofStep{{Hash: sib, Right: right}})
	})
}

// FuzzParseAmount ensures amount parsing never panics.
func FuzzParseAmount(f *testing.F) {
	f.Add("1.5")
	f.Add("999999999999999999999999")
	f.Add("-3")
	f.Add("..")
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = ParseAmount(s)
	})
}
