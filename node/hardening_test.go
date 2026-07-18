package node

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// TestBannableIP confirms loopback (and unparseable) addresses are exempt from
// bans so many local nodes sharing 127.0.0.1 don't ban one another.
func TestBannableIP(t *testing.T) {
	if bannableIP("127.0.0.1") || bannableIP("::1") {
		t.Fatal("loopback must not be bannable")
	}
	if bannableIP("not-an-ip") {
		t.Fatal("an unparseable address must not be bannable")
	}
	if !bannableIP("203.0.113.7") {
		t.Fatal("a public IP should be bannable")
	}
}

// TestInvalidBlockBansPeer feeds a peer blocks that fail their own proof-of-work
// (malformed regardless of our chain) and checks the peer is banned by identity.
func TestInvalidBlockBansPeer(t *testing.T) {
	n, _, _ := testNode(t)
	p, _ := bufPeer() // p.id == "peer-id"

	rounds := (banThreshold + banInvalidBlock - 1) / banInvalidBlock
	for i := 0; i < rounds; i++ {
		bad := core.Block{
			Index:        1,
			Bits:         core.GenesisBits,
			Hash:         "not-a-valid-pow-" + string(rune('a'+i)), // distinct so the seen-set doesn't dedup
			Transactions: []core.Transaction{core.NewCoinbase("dnasx", core.BlockReward(1))},
		}
		n.handleMessage(p, Message{Type: MsgBlock, Block: &bad})
	}
	if !n.bans.banned(p.id) {
		t.Fatalf("a peer serving invalid blocks should be banned (score %d)", n.bans.scoreOf(p.id))
	}
}

// TestForkBlockNotBanned confirms a well-formed block that simply doesn't link to
// our tip (an honest fork) is not penalised — it triggers a re-sync instead.
func TestForkBlockNotBanned(t *testing.T) {
	n, _, _ := testNode(t)
	p, buf := bufPeer()

	// A self-valid block (real PoW + merkle) built on a parent we don't have.
	bc := core.NewBlockchain()
	miner, _ := wallet.New()
	tip := bc.Tip()
	b := core.Block{
		Index:        1,
		Timestamp:    tip.Timestamp + 1,
		Transactions: []core.Transaction{core.NewCoinbase(miner.Address(), core.BlockReward(1))},
		PrevHash:     "a-parent-we-do-not-have",
		BaseFee:      bc.NextBaseFee(),
		Bits:         bc.NextBits(),
	}
	b.StateRoot, _ = bc.NextStateRoot(b)
	mined, ok := core.Mine(b, nil)
	if !ok {
		t.Fatal("mining aborted")
	}
	if err := mined.SelfValid(); err != nil {
		t.Fatalf("the fork block should be self-valid: %v", err)
	}

	n.handleMessage(p, Message{Type: MsgBlock, Block: &mined})

	if n.bans.scoreOf(p.id) != 0 {
		t.Fatalf("a non-linking (fork) block must not be penalised, score = %d", n.bans.scoreOf(p.id))
	}
	found := false
	for _, m := range sentMsgs(t, buf) {
		if m.Type == MsgGetHeaders {
			found = true
		}
	}
	if !found {
		t.Fatal("a non-linking block should trigger a getheaders re-sync")
	}
}

// TestPingRepliesPong confirms a keepalive ping is answered with a pong.
func TestPingRepliesPong(t *testing.T) {
	n, _, _ := testNode(t)
	p, buf := bufPeer()
	n.handleMessage(p, Message{Type: MsgPing})
	msgs := sentMsgs(t, buf)
	if len(msgs) != 1 || msgs[0].Type != MsgPong {
		t.Fatalf("MsgPing should elicit exactly one MsgPong, got %+v", msgs)
	}
}

// TestNewTxWakesIdleSleep confirms a new transaction interrupts the miner's idle
// sleep, so a submitted tx isn't stuck behind the block interval.
func TestNewTxWakesIdleSleep(t *testing.T) {
	n, _, _ := testNode(t)
	done := make(chan bool, 1)
	go func() { done <- n.sleepInterruptible(2 * time.Second) }()
	time.Sleep(80 * time.Millisecond)
	n.onNewTx()
	select {
	case completed := <-done:
		if completed {
			t.Fatal("the idle sleep should be interrupted by a new tx, not run to completion")
		}
	case <-time.After(time.Second):
		t.Fatal("sleepInterruptible did not return promptly after onNewTx")
	}
}

// TestSubmitTxWakesMiner confirms SubmitTx bumps the wake signal the idle miner
// watches.
func TestSubmitTxWakesMiner(t *testing.T) {
	n, _, w := testNode(t)
	before := atomic.LoadInt64(&n.txGen)
	tx := core.Transaction{From: w.Address(), To: "dnasx", Amount: core.Coin, Nonce: 0}
	if err := tx.Sign(w); err != nil {
		t.Fatal(err)
	}
	if err := n.SubmitTx(tx); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&n.txGen) == before {
		t.Fatal("SubmitTx should bump txGen to wake an idle miner")
	}
}
