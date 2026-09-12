package core

import (
	"encoding/hex"
	"math/rand"
	"reflect"
	"sort"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// Differential fuzzing: check the real implementation against a from-scratch
// oracle that is written to be OBVIOUSLY correct rather than fast.
//
// The existing fuzz targets check that nothing panics and that decoding survives
// arbitrary bytes. That finds crashes, which is worth doing and is not the thing
// most likely to be wrong here: a consensus bug is usually a correct-looking
// function that computes a subtly different answer, and it will not crash.
//
// A differential test needs a second opinion. Each oracle below re-derives the
// same answer by the simplest method available — a linear scan instead of an
// index, a naive sum instead of an incremental total, a full state rebuild
// instead of an undo log — so agreement between the two is evidence the fast
// path is right, and disagreement points at exactly which input broke it.

// --- Oracles ----------------------------------------------------------------

// oracleMerkleRoot folds transaction hashes the slowest, most literal way the
// definition allows: build the layer, duplicate the last node when it is odd,
// hash pairs, repeat.
func oracleMerkleRoot(txs []Transaction) string {
	if len(txs) == 0 {
		// The empty root is a fixed sentinel rather than sha256(""), so that an
		// empty tree is distinguishable from one whose single leaf happens to hash
		// to that value.
		return hashBytes([]byte("empty"))
	}
	layer := make([]string, 0, len(txs))
	for _, tx := range txs {
		layer = append(layer, tx.Hash())
	}
	for len(layer) > 1 {
		if len(layer)%2 == 1 {
			layer = append(layer, layer[len(layer)-1])
		}
		next := make([]string, 0, len(layer)/2)
		for i := 0; i < len(layer); i += 2 {
			next = append(next, hashBytes([]byte(layer[i]+layer[i+1])))
		}
		layer = next
	}
	return layer[0]
}

// oracleStateRoot rebuilds the trie from scratch for every call, with no cache
// and no incremental update.
func oracleStateRoot(state map[string]Account) string {
	t := NewTrie(NewMemNodeStore(), EmptyTrieRoot())
	keys := make([]string, 0, len(state))
	for a := range state {
		keys = append(keys, a)
	}
	// Deliberately the REVERSE order of the real implementation: a root that
	// depends on insertion order would show up here and nowhere else.
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	for _, a := range keys {
		if _, err := t.Update(trieKeyFor(a), stateLeaf(a, state[a])); err != nil {
			panic(err)
		}
	}
	return t.Root()
}

// oracleTips sums the miner's take one transaction at a time, in the most
// literal reading of the rule.
func oracleTips(txs []Transaction, baseFee uint64) uint64 {
	var total uint64
	for _, tx := range txs {
		burn := baseFee * uint64(len(tx.canonicalBytes()))
		if tx.Fee > burn {
			total += tx.Fee - burn
		}
	}
	return total
}

// oracleSubsidy sums block subsidies one height at a time, which is the
// definition CumulativeSubsidy's halving-epoch walk is a shortcut for.
func oracleSubsidy(height uint64) uint64 {
	var total uint64
	for h := uint64(1); h <= height; h++ {
		r := BlockReward(h)
		if r == 0 {
			break
		}
		total += r
	}
	return total
}

// --- Generators -------------------------------------------------------------

// fuzzTx builds a transaction from fuzz bytes. It uses real keys, because a
// transaction whose signature never verifies would never reach most of the code
// being compared.
func fuzzTx(r *rand.Rand, signers []*wallet.Wallet) Transaction {
	from := signers[r.Intn(len(signers))]
	to := signers[r.Intn(len(signers))]
	tx := Transaction{
		From:   from.Address(),
		To:     to.Address(),
		Amount: uint64(r.Int63n(1_000_000)),
		Fee:    uint64(r.Int63n(100_000)),
		Nonce:  uint64(r.Int63n(1000)),
	}
	switch r.Intn(6) {
	case 0:
		tx.Memo = hex.EncodeToString([]byte{byte(r.Intn(256)), byte(r.Intn(256))})
	case 1:
		tx.Expiry = uint64(r.Int63n(10_000))
	case 2:
		tx.LockUntil = uint64(r.Int63n(10_000))
	case 3:
		tx.AssetID = "tok" + hex.EncodeToString([]byte{byte(r.Intn(256))})
	case 4:
		tx.To, tx.Amount = "", 0
		n := 1 + r.Intn(3)
		for i := 0; i < n; i++ {
			tx.Outputs = append(tx.Outputs, Output{
				To: signers[r.Intn(len(signers))].Address(), Amount: uint64(r.Int63n(1000)),
			})
		}
	case 5:
		tx.Issue = &AssetIssue{Ticker: "FUZZ", Supply: uint64(1 + r.Int63n(1000))}
		tx.To, tx.Amount = "", 0
	}
	_ = tx.Sign(from)
	return tx
}

func fuzzSigners(t *testing.T, n int) []*wallet.Wallet {
	t.Helper()
	out := make([]*wallet.Wallet, n)
	for i := range out {
		w, err := wallet.New()
		if err != nil {
			t.Fatal(err)
		}
		out[i] = w
	}
	return out
}

// --- Differential targets ---------------------------------------------------

// The merkle root is what a block commits its transactions with, so a
// disagreement here is a disagreement about which block is which.
func FuzzMerkleRootAgainstOracle(f *testing.F) {
	f.Add(int64(1), 1)
	f.Add(int64(42), 7)
	f.Add(int64(7), 0)
	f.Fuzz(func(t *testing.T, seed int64, count int) {
		if count < 0 || count > 64 {
			t.Skip()
		}
		r := rand.New(rand.NewSource(seed))
		signers := fuzzSigners(t, 3)
		txs := make([]Transaction, 0, count)
		for i := 0; i < count; i++ {
			txs = append(txs, fuzzTx(r, signers))
		}
		if got, want := MerkleRoot(txs), oracleMerkleRoot(txs); got != want {
			t.Fatalf("merkle root disagrees for %d transactions: %s vs oracle %s", count, got, want)
		}
	})
}

// The state root is what makes a balance provable. An insertion-order dependence
// would make two honest nodes commit different roots for identical state, which
// is the failure the reversed-order oracle is looking for.
func FuzzStateRootAgainstOracle(f *testing.F) {
	f.Add(int64(3), 5)
	f.Add(int64(99), 1)
	f.Add(int64(11), 0)
	f.Fuzz(func(t *testing.T, seed int64, accounts int) {
		if accounts < 0 || accounts > 40 {
			t.Skip()
		}
		r := rand.New(rand.NewSource(seed))
		state := map[string]Account{}
		for i := 0; i < accounts; i++ {
			w, err := wallet.New()
			if err != nil {
				t.Fatal(err)
			}
			acc := Account{Balance: uint64(r.Int63n(1 << 40)), Nonce: uint64(r.Int63n(1000))}
			if r.Intn(3) == 0 {
				acc.Assets = map[string]uint64{
					"tok" + hex.EncodeToString([]byte{byte(r.Intn(256))}): uint64(1 + r.Int63n(1000)),
				}
			}
			state[w.Address()] = acc
		}
		if got, want := stateRoot(state), oracleStateRoot(state); got != want {
			t.Fatalf("state root disagrees for %d accounts: %s vs oracle %s", accounts, got, want)
		}
	})
}

// Tips decide what a coinbase may pay, so a disagreement is a disagreement about
// whether a block is valid.
func FuzzTipsAgainstOracle(f *testing.F) {
	f.Add(int64(5), 4, uint64(10))
	f.Add(int64(8), 1, uint64(0))
	f.Fuzz(func(t *testing.T, seed int64, count int, baseFee uint64) {
		if count < 0 || count > 32 || baseFee > 1_000_000 {
			t.Skip()
		}
		r := rand.New(rand.NewSource(seed))
		signers := fuzzSigners(t, 2)
		txs := make([]Transaction, 0, count)
		for i := 0; i < count; i++ {
			txs = append(txs, fuzzTx(r, signers))
		}
		if got, want := Tips(txs, baseFee), oracleTips(txs, baseFee); got != want {
			t.Fatalf("tips disagree: %d vs oracle %d (base fee %d, %d txs)", got, want, baseFee, count)
		}
	})
}

// CumulativeSubsidy walks halving epochs rather than blocks, which is the sort of
// closed form that is right for years and then wrong at a boundary.
func FuzzCumulativeSubsidyAgainstOracle(f *testing.F) {
	f.Add(uint64(0))
	f.Add(uint64(1))
	f.Add(HalvingInterval)
	f.Add(HalvingInterval + 1)
	f.Add(2*HalvingInterval - 1)
	f.Fuzz(func(t *testing.T, height uint64) {
		// The oracle is O(height), so it is only usable over a range a test can
		// afford to walk. The epoch boundaries — where a closed form actually goes
		// wrong — are all inside it.
		if height > 3*HalvingInterval {
			t.Skip()
		}
		if got, want := CumulativeSubsidy(height), oracleSubsidy(height); got != want {
			t.Fatalf("cumulative subsidy at %d: %d vs oracle %d", height, got, want)
		}
	})
}

// The canonical encoding is the consensus definition of a transaction. Round
// tripping it must not change the transaction, its id or its size — a transaction
// whose size changed would pay a different fee for the same bytes.
func FuzzCanonicalRoundTripIsStable(f *testing.F) {
	f.Add(int64(1))
	f.Add(int64(1234))
	f.Fuzz(func(t *testing.T, seed int64) {
		r := rand.New(rand.NewSource(seed))
		signers := fuzzSigners(t, 3)
		tx := fuzzTx(r, signers)

		encoded := tx.canonicalBytes()
		if again := tx.canonicalBytes(); !reflect.DeepEqual(encoded, again) {
			t.Fatal("encoding the same transaction twice produced different bytes")
		}
		if got := tx.Size(); got != len(encoded) {
			t.Fatalf("Size() = %d but the encoding is %d bytes", got, len(encoded))
		}
		if tx.Hash() != hashBytes(encoded) {
			t.Fatal("the txid is not the hash of the canonical encoding")
		}
		// The store codec is a different encoding of the same transaction, and must
		// preserve every field the hash covers.
		blk := Block{Index: 1, Transactions: []Transaction{NewCoinbase("dnasx", 1), tx}}
		decoded, err := decodeStoredBlock(encodeBlockV4(blk))
		if err != nil {
			t.Fatalf("store round trip failed: %v", err)
		}
		if len(decoded.Transactions) != 2 {
			t.Fatalf("store round trip returned %d transactions", len(decoded.Transactions))
		}
		if decoded.Transactions[1].Hash() != tx.Hash() {
			t.Fatalf("store round trip changed the txid:\n got %s\nwant %s",
				decoded.Transactions[1].Hash(), tx.Hash())
		}
	})
}

// The undo log is how a reorg puts state back. Rebuilding the state from scratch
// is the obviously-correct alternative, and the two must agree exactly — an undo
// that is subtly incomplete is a node whose balances quietly drift from everyone
// else's after a fork.
func FuzzUndoMatchesAFullRebuild(f *testing.F) {
	f.Add(int64(1), 3)
	f.Add(int64(77), 6)
	f.Fuzz(func(t *testing.T, seed int64, blocks int) {
		if blocks < 1 || blocks > 8 {
			t.Skip()
		}
		r := rand.New(rand.NewSource(seed))

		bc := NewBlockchain()
		alice, _ := wallet.New()
		bob, _ := wallet.New()
		mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
		matureCoinbase(t, bc)

		// A snapshot of the state to roll back to, and the chain that produced it.
		want := cloneState(bc.stateCopy())
		wantHeight := bc.Height()

		nonce := uint64(0)
		for i := 0; i < blocks; i++ {
			var txs []Transaction
			if r.Intn(2) == 0 && bc.Balance(alice.Address()) > 2*testFee+Coin {
				tx := signedTx(t, alice, bob.Address(), uint64(1+r.Int63n(int64(Coin))), testFee, nonce)
				nonce++
				txs = append(txs, tx)
			}
			mustAdd(t, bc, mineOn(t, bc, alice.Address(), txs))
		}

		// Roll back to the snapshot by replacing the chain with its own prefix plus
		// enough new work to win, which is the path a real reorg takes.
		prefix := bc.Blocks()[:wantHeight+1]
		rival := NewBlockchain()
		for _, b := range prefix[1:] {
			mustAdd(t, rival, b)
		}
		sink, _ := wallet.New()
		for i := 0; i <= blocks; i++ {
			mustAdd(t, rival, mineOn(t, rival, sink.Address(), nil))
		}
		replaced, _, err := bc.ReplaceChain(rival.Blocks())
		if err != nil {
			t.Fatalf("reorg: %v", err)
		}
		if !replaced {
			t.Skip("the rival chain did not win; nothing to compare")
		}

		// Every account that existed at the fork point must be back exactly as it
		// was, apart from what the winning branch itself changed.
		rebuilt := rival.stateCopy()
		got := bc.stateCopy()
		if len(got) != len(rebuilt) {
			t.Fatalf("after the reorg the state holds %d accounts, a rebuild holds %d", len(got), len(rebuilt))
		}
		for addr, acc := range rebuilt {
			mine := got[addr]
			if mine.Balance != acc.Balance || mine.Nonce != acc.Nonce {
				t.Fatalf("%s: after the reorg balance=%d nonce=%d, a rebuild gives balance=%d nonce=%d",
					addr, mine.Balance, mine.Nonce, acc.Balance, acc.Nonce)
			}
		}
		if bc.Tip().StateRoot != rival.Tip().StateRoot {
			t.Fatalf("state root after the reorg is %s, a rebuild gives %s",
				bc.Tip().StateRoot, rival.Tip().StateRoot)
		}
		_ = want
	})
}
