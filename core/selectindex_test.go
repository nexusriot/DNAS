package core

import (
	"fmt"
	"sort"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// Select used to rescan the whole pool once per chosen transaction, recomputing
// every candidate's hash and canonical size on each pass. These tests pin the
// behaviour of the indexed version: it must choose the same transactions the
// straightforward implementation would, and it must do so deterministically.

// selectReference is the obvious O(pool × block) implementation, kept here as an
// oracle. It mirrors what Select did before it was indexed: rescan everything,
// re-derive each candidate, take the best-paying ready one. If the fast path
// ever disagrees with this, the fast path is wrong.
func selectReference(m *Mempool, bc *Blockchain, max int) []Transaction {
	all := m.All()
	mineHeight := bc.Height() + 1
	baseFee := bc.NextBaseFee()

	balance := map[string]uint64{}
	nonce := map[string]uint64{}
	assets := map[string]map[string]uint64{}
	seen := map[string]bool{}
	load := func(addr string) {
		if seen[addr] {
			return
		}
		seen[addr] = true
		acc := bc.Account(addr)
		a := map[string]uint64{}
		for k, v := range acc.Assets {
			a[k] = v
		}
		balance[addr], nonce[addr], assets[addr] = bc.SpendableBalance(addr), acc.Nonce, a
	}

	var selected []Transaction
	used := map[string]bool{}
	weight, ops := 0, 0
	for len(selected) < max {
		var cands []Transaction
		for _, tx := range all {
			if used[tx.Hash()] || tx.Fee < BaseFeeFor(tx, baseFee) {
				continue
			}
			if CheckTxSanity(tx) != nil || checkTxAtHeight(tx, mineHeight) != nil {
				continue
			}
			if weight+tx.Size() > MaxBlockBytes || ops+VerifyOps(tx) > MaxBlockVerifyOps {
				continue
			}
			load(tx.From)
			if tx.Nonce != nonce[tx.From] {
				continue
			}
			if tx.IsSponsored() {
				load(tx.FeePayer)
				if balance[tx.FeePayer] < tx.Fee {
					continue
				}
			}
			ok := false
			switch {
			case tx.IsIssue():
				ok = balance[tx.From] >= senderFee(tx)
			case tx.IsAssetTransfer():
				ok = balance[tx.From] >= senderFee(tx) && assets[tx.From][tx.AssetID] >= tx.Amount
			default:
				ok = balance[tx.From] >= txCoinCost(tx)
			}
			if ok {
				cands = append(cands, tx)
			}
		}
		if len(cands) == 0 {
			break
		}
		// Same tie-break as Select, so the comparison is meaningful rather than
		// a coin flip on map order.
		sort.Slice(cands, func(i, j int) bool {
			ri, rj := txRate(cands[i]), txRate(cands[j])
			if ri != rj {
				return ri > rj
			}
			return cands[i].Hash() < cands[j].Hash()
		})
		pick := cands[0]

		nonce[pick.From]++
		switch {
		case pick.IsIssue():
			balance[pick.From] -= senderFee(pick)
			assets[pick.From][AssetID(pick.From, pick.Issue.Ticker, pick.Nonce)] += pick.Issue.Supply
		case pick.IsAssetTransfer():
			balance[pick.From] -= senderFee(pick)
			assets[pick.From][pick.AssetID] -= pick.Amount
			load(pick.To)
			assets[pick.To][pick.AssetID] += pick.Amount
		default:
			balance[pick.From] -= txCoinCost(pick)
			for _, o := range pick.outputs() {
				load(o.To)
				balance[o.To] += o.Amount
			}
		}
		if pick.IsSponsored() {
			balance[pick.FeePayer] -= pick.Fee
		}
		selected = append(selected, pick)
		used[pick.Hash()] = true
		weight += pick.Size()
		ops += VerifyOps(pick)
	}
	return selected
}

// busyPool builds a chain where every one of `senders` wallets holds spendable
// coin, plus a mempool where each has `perSender` queued payments at ascending
// nonces and varied fees.
func busyPool(t testing.TB, senders, perSender int) (*Blockchain, *Mempool, []*wallet.Wallet) {
	t.Helper()
	bc := NewBlockchain()
	ws := make([]*wallet.Wallet, senders)
	for i := range ws {
		w, err := wallet.New()
		if err != nil {
			t.Fatal(err)
		}
		ws[i] = w
		if err := bc.AddBlock(mineOn(t, bc, w.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}
	matureCoinbase(t, bc)

	mp := NewMempoolWithLimits(senders*perSender+16, DefaultMempoolBytes, 0).UseAccounts(bc)
	for i, w := range ws {
		to := ws[(i+1)%len(ws)].Address()
		for n := 0; n < perSender; n++ {
			// Fees vary across senders and nonces so fee-rate ordering actually
			// has something to order, including exact ties between senders.
			fee := testFee * uint64(1+(i*7+n*3)%11)
			tx := signedTx(t, w, to, Coin/2, fee, uint64(n))
			if ok, err := mp.Add(tx); !ok || err != nil {
				t.Fatalf("add %d/%d: ok=%v err=%v", i, n, ok, err)
			}
		}
	}
	return bc, mp, ws
}

func hashes(txs []Transaction) []string {
	out := make([]string, len(txs))
	for i, tx := range txs {
		out[i] = tx.Hash()
	}
	return out
}

func TestSelectMatchesReferenceImplementation(t *testing.T) {
	for _, tc := range []struct{ senders, per, max int }{
		{1, 1, 10}, {3, 4, 100}, {8, 5, 100}, {12, 3, 7}, {5, 6, 0},
	} {
		t.Run(fmt.Sprintf("%dx%d_max%d", tc.senders, tc.per, tc.max), func(t *testing.T) {
			bc, mp, _ := busyPool(t, tc.senders, tc.per)
			got := hashes(mp.Select(bc, tc.max))
			want := hashes(selectReference(mp, bc, tc.max))
			if len(got) != len(want) {
				t.Fatalf("selected %d, reference selected %d", len(got), len(want))
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("pick %d = %s, reference = %s", i, got[i][:12], want[i][:12])
				}
			}
		})
	}
}

// TestSelectIsDeterministic is the behaviour change: the pool is a map, so the
// old implementation's tie-breaks followed Go's randomized iteration order and
// two nodes could build different templates from identical mempools.
func TestSelectIsDeterministic(t *testing.T) {
	bc, mp, _ := busyPool(t, 10, 4)
	first := hashes(mp.Select(bc, 100))
	if len(first) == 0 {
		t.Fatal("nothing selected; the fixture is not exercising anything")
	}
	for i := 0; i < 12; i++ {
		got := hashes(mp.Select(bc, 100))
		if len(got) != len(first) {
			t.Fatalf("run %d selected %d, first run selected %d", i, len(got), len(first))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("run %d differs at pick %d: %s vs %s", i, j, got[j][:12], first[j][:12])
			}
		}
	}
}

// TestSelectStillOrdersByNonceAndRate guards the two properties the indexing
// could plausibly have broken: a sender's transactions must stay in nonce order,
// and a sender with a gap at its next nonce must contribute nothing.
func TestSelectSkipsSenderWithNonceGap(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	for _, w := range []*wallet.Wallet{alice, bob} {
		if err := bc.AddBlock(mineOn(t, bc, w.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}
	matureCoinbase(t, bc)

	mp := NewMempool().UseAccounts(bc)
	// Bob queues normally. Alice's only queued transaction sits at nonce 1 with a
	// huge fee, with nothing at nonce 0 — it must never be selected however well
	// it pays, and it must not stop bob being selected.
	if ok, err := mp.Add(signedTx(t, bob, alice.Address(), Coin, testFee, 0)); !ok || err != nil {
		t.Fatalf("bob add: ok=%v err=%v", ok, err)
	}
	gap := signedTx(t, alice, bob.Address(), Coin, 500*testFee, 1)
	mp.mu.Lock()
	mp.insertLocked(gap.Hash(), gap) // bypass admission, which rejects the gap
	mp.mu.Unlock()

	sel := mp.Select(bc, 10)
	for _, tx := range sel {
		if tx.From == alice.Address() {
			t.Fatalf("selected alice's nonce-%d transaction across a gap", tx.Nonce)
		}
	}
	if len(sel) != 1 || sel[0].From != bob.Address() {
		t.Fatalf("expected only bob's transaction, got %d", len(sel))
	}
}

// BenchmarkSelect is the reason for the change: a full pool building a full
// block. Run with -benchtime=1x if the reference arm is painfully slow.
func BenchmarkSelect(b *testing.B) {
	bc, mp, _ := busyPool(b, 200, 10)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if len(mp.Select(bc, MaxBlockTxs)) == 0 {
			b.Fatal("selected nothing")
		}
	}
}

func BenchmarkSelectReference(b *testing.B) {
	bc, mp, _ := busyPool(b, 200, 10)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if len(selectReference(mp, bc, MaxBlockTxs)) == 0 {
			b.Fatal("selected nothing")
		}
	}
}
