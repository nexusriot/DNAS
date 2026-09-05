package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

func TestHashFileStreamsAndMatchesKnownDigests(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// The published sha256 of the empty string, so a broken hash cannot pass by
	// merely being self-consistent.
	const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got, err := hashFile(empty); err != nil || got != emptySHA {
		t.Fatalf("hashFile(empty) = %q, %v; want %s", got, err, emptySHA)
	}
	abc := filepath.Join(dir, "abc")
	if err := os.WriteFile(abc, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	const abcSHA = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got, err := hashFile(abc); err != nil || got != abcSHA {
		t.Fatalf("hashFile(abc) = %q, %v; want %s", got, err, abcSHA)
	}
	if _, err := hashFile(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("hashing a missing file succeeded")
	}
}

func TestAnchorMemoRoundTrip(t *testing.T) {
	const digest = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	memo := anchorMemo(strings.ToUpper(digest))
	if len(memo) > core.MaxMemoBytes {
		t.Fatalf("an anchor memo is %d bytes, over the %d-byte consensus limit", len(memo), core.MaxMemoBytes)
	}
	got, ok := digestFromMemo(memo)
	if !ok || got != digest {
		t.Fatalf("round trip gave %q, %v", got, ok)
	}
	// Anything that is not an anchor must be reported as such rather than
	// half-parsed: a memo is arbitrary user data, and most of them are not this.
	for _, memo := range []string{"", "rent", "anchor:", "anchor:xyz", "anchor:" + digest[:63],
		"anchor:" + digest + "extra", "anchor:" + strings.Repeat("z", 64), digest} {
		if _, ok := digestFromMemo(memo); ok {
			t.Errorf("%q was accepted as an anchor", memo)
		}
	}
}

func TestBuildAnchorIsASignedSelfPaymentCarryingTheDigest(t *testing.T) {
	w, _ := wallet.New()
	const digest = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"

	tx, err := buildAnchor(w, strings.ToUpper(digest), 1000, 4)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if tx.From != w.Address() || tx.To != w.Address() || tx.Amount != 0 {
		t.Fatalf("an anchor should be a zero-value self-payment: %+v", tx)
	}
	if got, ok := digestFromMemo(tx.Memo); !ok || got != digest {
		t.Fatalf("memo = %q", tx.Memo)
	}
	if err := tx.VerifySignature(); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := core.CheckTxSanity(tx); err != nil {
		t.Fatalf("consensus rejects an anchor: %v", err)
	}
	// The digest must be covered by the signature, or the anchor proves nothing:
	// a relay could swap in another document's hash.
	altered := tx
	altered.Memo = anchorMemo(strings.Repeat("aa", 32))
	if err := altered.VerifySignature(); err == nil {
		t.Fatal("the anchored digest can be rewritten in flight")
	}

	for _, tc := range []struct {
		name   string
		digest string
		fee    uint64
	}{
		{"short digest", digest[:32], 1000},
		{"not hex", strings.Repeat("z", 64), 1000},
		{"no fee", digest, 0},
	} {
		if _, err := buildAnchor(w, tc.digest, tc.fee, 0); err == nil {
			t.Errorf("%s was accepted", tc.name)
		}
	}
}

// The verification itself, end to end on a real chain: the header chain's proof
// of work, the merkle path, and — the step it would be easy to skip — reading the
// transaction to see what the proven txid actually commits to.
func TestVerifyAnchorProvesInclusionAndTheDigest(t *testing.T) {
	bc := core.NewBlockchain()
	w, _ := wallet.New()
	// Fund the anchoring account, and let the reward mature — a coinbase is not
	// spendable until it has, so anchoring with fresher coin would fail on a rule
	// that has nothing to do with anchoring.
	for i := 0; i <= core.CoinbaseMaturity; i++ {
		mineOntoChain(t, bc, w.Address())
	}

	const digest = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	tx, err := buildAnchor(w, digest, core.DefaultMinRelayFee*2000, 0)
	if err != nil {
		t.Fatal(err)
	}
	mineOntoChainWithTx(t, bc, w.Address(), tx)

	headers := bc.Headers()
	pr, ok := bc.FindTxProof(tx.Hash())
	if !ok {
		t.Fatal("the anchor was not found in the chain")
	}
	block, ok := bc.BlockAt(pr.BlockIndex)
	if !ok {
		t.Fatalf("no block at %d", pr.BlockIndex)
	}

	proven, err := verifyAnchor(headers, pr, block, digest, tx.Hash())
	if err != nil {
		t.Fatalf("a genuine anchor did not verify: %v", err)
	}
	if proven.Digest != digest || proven.Height != pr.BlockIndex {
		t.Fatalf("proof reports %+v", proven)
	}
	if proven.Confirmations != 1 {
		t.Fatalf("confirmations = %d, want 1 for a tip block", proven.Confirmations)
	}

	// Claiming a different file against this proof must fail: the merkle path is
	// genuine, so only reading the memo catches it.
	if _, err := verifyAnchor(headers, pr, block, strings.Repeat("aa", 32), tx.Hash()); err == nil {
		t.Fatal("another file's digest verified against this anchor")
	}
	// A block body that does not hash to the verified header is refused, which is
	// what stops a node from serving a body with the memo it likes.
	tampered := block
	tampered.Transactions = append([]core.Transaction{}, block.Transactions...)
	tampered.Transactions[len(tampered.Transactions)-1].Memo = anchorMemo(strings.Repeat("bb", 32))
	if _, err := verifyAnchor(headers, pr, tampered, digest, tx.Hash()); err == nil {
		t.Fatal("a rewritten block body verified")
	}
	// A proof pointing past the verified header chain proves nothing.
	beyond := pr
	beyond.BlockIndex = uint64(len(headers)) + 5
	if _, err := verifyAnchor(headers, beyond, block, digest, tx.Hash()); err == nil {
		t.Fatal("a proof beyond the header chain verified")
	}
	// And a transaction with an ordinary memo is not an anchor, even when its
	// inclusion proof is perfectly valid.
	plain := core.Transaction{From: w.Address(), To: w.Address(),
		Fee: core.DefaultMinRelayFee * 2000, Nonce: 1, Memo: "rent"}
	if err := plain.Sign(w); err != nil {
		t.Fatal(err)
	}
	mineOntoChainWithTx(t, bc, w.Address(), plain)
	headers = bc.Headers()
	pr2, _ := bc.FindTxProof(plain.Hash())
	block2, _ := bc.BlockAt(pr2.BlockIndex)
	if _, err := verifyAnchor(headers, pr2, block2, digest, plain.Hash()); err == nil {
		t.Fatal("a transaction with an unrelated memo verified as an anchor")
	}
}

func TestAnchorReceiptRoundTripAndRejections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.json")
	want := anchorReceipt{Version: anchorReceiptVersion, Network: "regtest", File: "doc.txt",
		SHA256: strings.Repeat("ab", 32), TxHash: strings.Repeat("cd", 32), Address: "dnasx"}
	if err := writeReceipt(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := readReceipt(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != want {
		t.Fatalf("round trip changed the receipt: %+v", got)
	}

	// A receipt this build does not understand, or one missing what verification
	// needs, must be refused rather than silently half-checked.
	bad := filepath.Join(dir, "bad.json")
	future := want
	future.Version = anchorReceiptVersion + 1
	writeJSON(t, bad, future)
	if _, err := readReceipt(bad); err == nil {
		t.Fatal("a receipt from an unknown format version was accepted")
	}
	noTx := want
	noTx.TxHash = ""
	writeJSON(t, bad, noTx)
	if _, err := readReceipt(bad); err == nil {
		t.Fatal("a receipt naming no transaction was accepted")
	}
	shortDigest := want
	shortDigest.SHA256 = "abcd"
	writeJSON(t, bad, shortDigest)
	if _, err := readReceipt(bad); err == nil {
		t.Fatal("a receipt with a malformed digest was accepted")
	}
	if err := os.WriteFile(bad, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readReceipt(bad); err == nil {
		t.Fatal("a receipt that is not JSON was accepted")
	}
}
