package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// richChain builds a chain whose accounts hold known, distinct amounts.
func richChain(t *testing.T) (*Blockchain, []*wallet.Wallet) {
	t.Helper()
	bc := NewBlockchain()
	miner, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	matureCoinbase(t, bc)

	ws := make([]*wallet.Wallet, 3)
	for i := range ws {
		ws[i], _ = wallet.New()
	}
	// The miner pays each of them a different amount, largest first.
	for i, w := range ws {
		amount := uint64(len(ws)-i) * Coin
		tx := signedTx(t, miner, w.Address(), amount, testFee, uint64(i))
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{tx}))
	}
	return bc, ws
}

func TestRichListRanksByBalance(t *testing.T) {
	bc, ws := richChain(t)
	rl := bc.RichList(10)

	if rl.Height != bc.Height() {
		t.Errorf("height = %d, want %d", rl.Height, bc.Height())
	}
	if rl.Circulating != bc.Supply().Circulating {
		t.Errorf("circulating = %d, want %d", rl.Circulating, bc.Supply().Circulating)
	}
	if len(rl.Entries) == 0 {
		t.Fatal("empty rich list on a funded chain")
	}
	for i := 1; i < len(rl.Entries); i++ {
		if rl.Entries[i-1].Balance < rl.Entries[i].Balance {
			t.Fatalf("entry %d (%d) sorts above entry %d (%d)",
				i-1, rl.Entries[i-1].Balance, i, rl.Entries[i].Balance)
		}
		if rl.Entries[i].Rank != i+1 {
			t.Errorf("entry %d has rank %d", i, rl.Entries[i].Rank)
		}
	}
	byAddr := map[string]RichListEntry{}
	for _, e := range rl.Entries {
		byAddr[e.Address] = e
	}
	for i, w := range ws {
		want := uint64(len(ws)-i) * Coin
		got, ok := byAddr[w.Address()]
		if !ok {
			t.Fatalf("recipient %d is missing from the rich list", i)
		}
		if got.Balance != want {
			t.Errorf("recipient %d holds %d, want %d", i, got.Balance, want)
		}
	}
}

// The limit is what stops one request from sorting a whole ledger, so it must
// actually bound the result — and still return the LARGEST holders, not an
// arbitrary subset of them.
func TestRichListLimitKeepsTheLargest(t *testing.T) {
	bc, _ := richChain(t)
	full := bc.RichList(MaxRichListLimit)
	if len(full.Entries) < 3 {
		t.Fatalf("need at least 3 holders to test truncation, have %d", len(full.Entries))
	}

	two := bc.RichList(2)
	if len(two.Entries) != 2 {
		t.Fatalf("limit 2 returned %d entries", len(two.Entries))
	}
	for i := range two.Entries {
		if two.Entries[i].Address != full.Entries[i].Address {
			t.Errorf("rank %d is %s under a limit but %s without one",
				i+1, two.Entries[i].Address, full.Entries[i].Address)
		}
	}
	if two.Accounts != full.Accounts {
		t.Errorf("account total changed with the limit: %d vs %d", two.Accounts, full.Accounts)
	}
	if two.TopShare >= full.TopShare && full.TopShare > 0 {
		t.Errorf("top share did not shrink when fewer holders were shown: %v vs %v",
			two.TopShare, full.TopShare)
	}
}

func TestRichListLimitIsClamped(t *testing.T) {
	bc, _ := richChain(t)
	if got := bc.RichList(0).Shown; got == 0 {
		t.Error("a zero limit returned nothing instead of the default")
	}
	if got := len(bc.RichList(MaxRichListLimit * 10).Entries); got > MaxRichListLimit {
		t.Errorf("returned %d entries, cap is %d", got, MaxRichListLimit)
	}
}

// An account can exist with no coin — an asset holder, or one spent to zero.
// Listing it would push a real holder out of the ranking.
func TestRichListSkipsEmptyAccounts(t *testing.T) {
	bc, _ := richChain(t)
	bc.mu.Lock()
	bc.state["dnasempty"] = Account{Balance: 0, Nonce: 4}
	bc.mu.Unlock()

	for _, e := range bc.RichList(MaxRichListLimit).Entries {
		if e.Address == "dnasempty" {
			t.Fatal("an account holding nothing appeared in the rich list")
		}
		if e.Balance == 0 {
			t.Fatalf("%s holds nothing but was listed", e.Address)
		}
	}
}

// Two accounts holding the same amount must not make the ranking depend on Go's
// map iteration order: identical state has to produce an identical list.
func TestRichListIsDeterministicOnTies(t *testing.T) {
	bc := NewBlockchain()
	bc.mu.Lock()
	for _, a := range []string{"dnasa", "dnasb", "dnasc", "dnasd", "dnase"} {
		bc.state[a] = Account{Balance: 1000}
	}
	bc.mu.Unlock()

	first := bc.RichList(3)
	for i := 0; i < 20; i++ {
		again := bc.RichList(3)
		if len(again.Entries) != len(first.Entries) {
			t.Fatalf("length changed between calls: %d then %d", len(first.Entries), len(again.Entries))
		}
		for j := range first.Entries {
			if again.Entries[j].Address != first.Entries[j].Address {
				t.Fatalf("rank %d was %s and is now %s", j+1,
					first.Entries[j].Address, again.Entries[j].Address)
			}
		}
	}
	// And ties resolve on the address, ascending.
	if first.Entries[0].Address != "dnasa" || first.Entries[1].Address != "dnasb" {
		t.Errorf("ties did not break on the address: %s, %s",
			first.Entries[0].Address, first.Entries[1].Address)
	}
}

func TestRichListPercentagesAddUp(t *testing.T) {
	bc, _ := richChain(t)
	rl := bc.RichList(MaxRichListLimit)

	var sum float64
	for _, e := range rl.Entries {
		sum += e.Percent
	}
	// Every holder is shown, so the percentages must account for the whole supply.
	if diff := sum - 100; diff > 0.001 || diff < -0.001 {
		t.Errorf("percentages sum to %v, want 100", sum)
	}
	if diff := rl.TopShare - 100; diff > 0.001 || diff < -0.001 {
		t.Errorf("top share = %v with every holder shown, want 100", rl.TopShare)
	}
}

func TestRichListOnAnEmptyChain(t *testing.T) {
	rl := NewBlockchain().RichList(10)
	if rl.Shown != 0 || len(rl.Entries) != 0 {
		t.Fatalf("genesis-only chain listed %d holders", rl.Shown)
	}
	if rl.Circulating != 0 || rl.TopShare != 0 {
		t.Errorf("genesis-only chain reports circulating=%d top_share=%v", rl.Circulating, rl.TopShare)
	}
}
