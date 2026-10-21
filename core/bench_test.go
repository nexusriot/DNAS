package core

import (
	"fmt"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// Performance benchmarks for the consensus hot paths.
//
// There were already benchmarks here — mempool selection, the store codec, a
// single trie update — but nothing that varied the size of the LEDGER, and no
// recorded baseline to compare a run against. That is how a state root costing
// 2.25 seconds on a 100k-account ledger stayed invisible: every test chain has
// a handful of accounts, so a term that grows with the ledger never appears.
// The benchmarks below are parameterised by ledger size precisely so that a
// cost scaling with the wrong thing shows up as a shape rather than a number.
//
// `make bench` records them; `make bench-check` compares a run against the
// recorded baseline and fails on a regression. See scripts/bench.sh.

// ledger builds a synthetic account set of the given size.
func ledger(n int) map[string]Account {
	state := make(map[string]Account, n)
	for i := 0; i < n; i++ {
		state[fmt.Sprintf("dnas%040x", i)] = Account{Balance: uint64(i) * Coin, Nonce: uint64(i)}
	}
	return state
}

var benchLedgerSizes = []int{100, 1000, 10000}

// BenchmarkStateRootRebuild is the OLD cost of committing a block: a fold over
// every account, paid once per block and once per state proof.
func BenchmarkStateRootRebuild(b *testing.B) {
	for _, n := range benchLedgerSizes {
		state := ledger(n)
		b.Run(fmt.Sprintf("accounts=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				_ = stateRoot(memStateOf(state))
			}
		})
	}
}

// BenchmarkStateCommitIncremental is the NEW cost of the same commitment: the
// path from one changed account to the root. It should be flat in the ledger
// size, which is the entire point — compare the curve against the rebuild.
func BenchmarkStateCommitIncremental(b *testing.B) {
	for _, n := range benchLedgerSizes {
		store := NewBoundedNodeStore(0)
		state := ledger(n)
		root := stateRootInto(store, memStateOf(state))
		addr := fmt.Sprintf("dnas%040x", 0)
		b.Run(fmt.Sprintf("accounts=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				acc := state[addr]
				acc.Balance++
				state[addr] = acc
				sc := &stateCommitter{store: store, root: root}
				root = sc.rootAfter(memStateOf(state), []undoEntry{{addr: addr, prev: acc, existed: true}})
			}
		})
	}
}

// BenchmarkProveAccount is what an unauthenticated GET /stateproof costs the
// node serving it.
func BenchmarkProveAccount(b *testing.B) {
	for _, n := range benchLedgerSizes {
		store := NewBoundedNodeStore(0)
		state := ledger(n)
		root := stateRootInto(store, memStateOf(state))
		key := trieKeyFor(fmt.Sprintf("dnas%040x", n/2))
		b.Run(fmt.Sprintf("accounts=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := NewTrie(store, root).Prove(key); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkAddBlock measures end-to-end block validation: structure, proof of
// work, signatures, state application and the state-root commitment.
func BenchmarkAddBlock(b *testing.B) {
	for _, txCount := range []int{0, 10, 100} {
		b.Run(fmt.Sprintf("txs=%d", txCount), func(b *testing.B) {
			b.StopTimer()
			bc := NewBlockchain()
			miner, _ := wallet.New()
			mustAdd(b, bc, mineOn(b, bc, miner.Address(), nil))
			matureCoinbase(b, bc)

			nonce := uint64(0)
			for i := 0; i < b.N; i++ {
				txs := make([]Transaction, txCount)
				for j := range txs {
					to, _ := wallet.New()
					txs[j] = signedTx(b, miner, to.Address(), Coin/1000, testFee, nonce)
					nonce++
				}
				block := mineOn(b, bc, miner.Address(), txs)
				b.StartTimer()
				if err := bc.AddBlock(block); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
			}
		})
	}
}

// BenchmarkTransactionVerify is the per-signature cost that dominates initial
// sync, and BenchmarkTransactionHash the canonical encoding underneath it.
func BenchmarkTransactionVerify(b *testing.B) {
	w, _ := wallet.New()
	to, _ := wallet.New()
	tx := Transaction{From: w.Address(), To: to.Address(), Amount: Coin, Fee: testFee}
	if err := tx.Sign(w); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := tx.VerifySignature(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTransactionHash(b *testing.B) {
	w, _ := wallet.New()
	to, _ := wallet.New()
	tx := Transaction{From: w.Address(), To: to.Address(), Amount: Coin, Fee: testFee}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = tx.Hash()
	}
}

// BenchmarkMerkleRoot and BenchmarkBuildBlockFilter are the two per-block folds
// a miner pays before it may start hashing.
func BenchmarkMerkleRoot(b *testing.B) {
	w, _ := wallet.New()
	txs := make([]Transaction, 500)
	for i := range txs {
		to, _ := wallet.New()
		txs[i] = Transaction{From: w.Address(), To: to.Address(), Amount: Coin, Nonce: uint64(i)}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MerkleRoot(txs)
	}
}

func BenchmarkBuildBlockFilter(b *testing.B) {
	w, _ := wallet.New()
	txs := make([]Transaction, 500)
	for i := range txs {
		to, _ := wallet.New()
		txs[i] = Transaction{From: w.Address(), To: to.Address(), Amount: Coin, Nonce: uint64(i)}
	}
	block := Block{Index: 1, Transactions: txs, Hash: "beef"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = BuildBlockFilter(block)
	}
}
