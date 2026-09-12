package node

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// fakePeer is a peer whose writes land in a buffer instead of on a socket, so the
// relay decisions (announce vs. push, what is requested, what is served) can be
// asserted directly rather than inferred from two nodes converging.
func fakePeer(caps ...string) (*peer, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	p := &peer{enc: json.NewEncoder(buf), caps: map[string]bool{}, addr: "fake"}
	for _, c := range caps {
		p.caps[c] = true
	}
	return p, buf
}

// sent decodes everything a fake peer was sent.
func sent(t *testing.T, buf *bytes.Buffer) []Message {
	t.Helper()
	var out []Message
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for {
		var m Message
		if err := dec.Decode(&m); err != nil {
			return out
		}
		out = append(out, m)
	}
}

func attach(n *Node, ps ...*peer) {
	n.peersMu.Lock()
	defer n.peersMu.Unlock()
	for _, p := range ps {
		n.peers[p] = true
	}
}

// The whole point of the change: a capable peer is told the id, not handed the
// body. A peer that has never heard of the capability keeps getting the body, so
// rolling this out cannot silently stop propagating transactions to older nodes.
func TestAnnounceSendsHashesToCapablePeersAndBodiesToOthers(t *testing.T) {
	n, _, w := fundedNode(t)
	modern, modernBuf := fakePeer(CapTxInv)
	legacy, legacyBuf := fakePeer()
	attach(n, modern, legacy)

	tx := core.Transaction{From: w.Address(), To: w.Address(), Amount: 1, Fee: 1, Nonce: 0}
	if err := tx.Sign(w); err != nil {
		t.Fatal(err)
	}
	n.announceTx(tx, nil)

	got := sent(t, modernBuf)
	if len(got) != 1 || got[0].Type != MsgTxInv {
		t.Fatalf("capable peer got %+v, want one %s", got, MsgTxInv)
	}
	if len(got[0].Hashes) != 1 || got[0].Hashes[0] != tx.Hash() {
		t.Errorf("announcement carried %v, want [%s]", got[0].Hashes, tx.Hash())
	}
	if got[0].Tx != nil {
		t.Error("the announcement carried the body too, which defeats the purpose")
	}

	got = sent(t, legacyBuf)
	if len(got) != 1 || got[0].Type != MsgTx || got[0].Tx == nil {
		t.Fatalf("legacy peer got %+v, want one %s carrying the body", got, MsgTx)
	}
}

func TestAnnounceSkipsTheSender(t *testing.T) {
	n, _, w := fundedNode(t)
	from, fromBuf := fakePeer(CapTxInv)
	other, otherBuf := fakePeer(CapTxInv)
	attach(n, from, other)

	tx := core.Transaction{From: w.Address(), To: w.Address(), Amount: 1, Fee: 1, Nonce: 0}
	if err := tx.Sign(w); err != nil {
		t.Fatal(err)
	}
	n.announceTx(tx, from)

	if got := sent(t, fromBuf); len(got) != 0 {
		t.Errorf("announced back to the peer it came from: %+v", got)
	}
	if got := sent(t, otherBuf); len(got) != 1 {
		t.Errorf("other peer got %d messages, want 1", len(got))
	}
}

// An announcement must NOT be recorded as seen. If it were, a peer that
// announced and then never delivered would make the transaction permanently
// unfetchable: every later announcement, from anyone, would look like a
// duplicate and be dropped.
func TestAnnouncementDoesNotMarkTheTransactionSeen(t *testing.T) {
	n, _, _ := fundedNode(t)
	p, buf := fakePeer(CapTxInv)
	attach(n, p)

	h := strings.Repeat("ab", 32)
	n.onTxInv(p, []string{h})

	if n.seenTx.has(h) {
		t.Fatal("an announced-but-never-received transaction was marked seen")
	}
	got := sent(t, buf)
	if len(got) != 1 || got[0].Type != MsgGetTx || len(got[0].Hashes) != 1 {
		t.Fatalf("got %+v, want one %s asking for the id", got, MsgGetTx)
	}
}

// ...and the in-flight table is what stops eight peers announcing the same id
// from producing eight downloads of the same body.
func TestSecondAnnouncementOfTheSameIdIsNotRequestedAgain(t *testing.T) {
	n, _, _ := fundedNode(t)
	first, firstBuf := fakePeer(CapTxInv)
	second, secondBuf := fakePeer(CapTxInv)
	attach(n, first, second)

	h := strings.Repeat("cd", 32)
	n.onTxInv(first, []string{h})
	n.onTxInv(second, []string{h})

	if len(sent(t, firstBuf)) != 1 {
		t.Error("the first announcement was not acted on")
	}
	if got := sent(t, secondBuf); len(got) != 0 {
		t.Errorf("the same body was requested from a second peer as well: %+v", got)
	}
}

// A transaction already held is not requested again, however often it is announced.
func TestAnnouncementOfAHeldTransactionIsIgnored(t *testing.T) {
	n, mp, w := fundedNode(t)
	tx := core.Transaction{From: w.Address(), To: w.Address(), Amount: core.Coin,
		Fee: core.DefaultMinRelayFee * 1000, Nonce: n.NextNonce(w.Address())}
	if err := tx.Sign(w); err != nil {
		t.Fatal(err)
	}
	if added, err := mp.Add(tx); err != nil || !added {
		t.Fatalf("seed the pool: added=%v err=%v", added, err)
	}

	p, buf := fakePeer(CapTxInv)
	attach(n, p)
	n.onTxInv(p, []string{tx.Hash()})
	if got := sent(t, buf); len(got) != 0 {
		t.Errorf("requested a transaction already in the pool: %+v", got)
	}
}

// Announced ids are attacker-supplied, so anything that is not a txid is dropped
// before it can be echoed back or held in the in-flight table.
func TestMalformedAnnouncedIdsAreDropped(t *testing.T) {
	n, _, _ := fundedNode(t)
	p, buf := fakePeer(CapTxInv)
	attach(n, p)

	n.onTxInv(p, []string{"", "zz", strings.Repeat("zz", 32), strings.Repeat("ab", 40), "../../etc/passwd"})
	if got := sent(t, buf); len(got) != 0 {
		t.Errorf("malformed ids produced a request: %+v", got)
	}
	if n.txReq.len() != 0 {
		t.Errorf("malformed ids were recorded in flight (%d entries)", n.txReq.len())
	}
}

func TestAnnouncementBatchIsBounded(t *testing.T) {
	n, _, _ := fundedNode(t)
	p, buf := fakePeer(CapTxInv)
	attach(n, p)

	hashes := make([]string, maxTxInvBatch+500)
	for i := range hashes {
		hashes[i] = core.NewCoinbase("addr", uint64(i)).Hash()
	}
	n.onTxInv(p, hashes)

	got := sent(t, buf)
	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1", len(got))
	}
	if len(got[0].Hashes) > maxTxInvBatch {
		t.Errorf("requested %d ids from one announcement, cap is %d", len(got[0].Hashes), maxTxInvBatch)
	}
}

// Serving a request returns the bodies held and silently omits the rest: a
// transaction can be mined out of the pool between announcement and request,
// which is ordinary, not misbehaviour.
func TestServingARequestReturnsOnlyWhatIsHeld(t *testing.T) {
	n, mp, w := fundedNode(t)
	tx := core.Transaction{From: w.Address(), To: w.Address(), Amount: core.Coin,
		Fee: core.DefaultMinRelayFee * 1000, Nonce: n.NextNonce(w.Address())}
	if err := tx.Sign(w); err != nil {
		t.Fatal(err)
	}
	if added, err := mp.Add(tx); err != nil || !added {
		t.Fatalf("seed the pool: added=%v err=%v", added, err)
	}

	p, buf := fakePeer(CapTxInv)
	attach(n, p)
	n.onGetTx(p, []string{tx.Hash(), strings.Repeat("ef", 32)})

	got := sent(t, buf)
	if len(got) != 1 || got[0].Type != MsgTxs {
		t.Fatalf("got %+v, want one %s", got, MsgTxs)
	}
	if len(got[0].Txs) != 1 || got[0].Txs[0].Hash() != tx.Hash() {
		t.Fatalf("served %d transactions, want just the one held", len(got[0].Txs))
	}
}

func TestRequestClaimExpiresSoAStalledPeerCannotBlockATransaction(t *testing.T) {
	r := newTxRequests()
	h := strings.Repeat("ab", 32)
	now := time.Now()

	if !r.claim(h, now) {
		t.Fatal("the first claim was refused")
	}
	if r.claim(h, now.Add(txRequestTimeout-time.Second)) {
		t.Error("a second claim inside the timeout was allowed")
	}
	if !r.claim(h, now.Add(txRequestTimeout+time.Second)) {
		t.Error("the claim never expired, so a peer that went quiet blocks the transaction forever")
	}

	// Once the body arrives the claim is forgotten, so a re-broadcast is fetched
	// at once rather than waiting out the timeout.
	r.done(h)
	if r.len() != 0 {
		t.Errorf("done left %d entries", r.len())
	}
	if !r.claim(h, now.Add(txRequestTimeout+2*time.Second)) {
		t.Error("a forgotten claim was still treated as in flight")
	}
}

func TestInFlightTableIsBounded(t *testing.T) {
	r := newTxRequests()
	now := time.Now()
	for i := 0; i < maxTxInFlight+1000; i++ {
		r.claim(core.NewCoinbase("addr", uint64(i)).Hash(), now)
	}
	if r.len() > maxTxInFlight {
		t.Fatalf("in-flight table grew to %d, cap is %d", r.len(), maxTxInFlight)
	}
}

// End to end over real sockets: a transaction submitted on one node reaches the
// other's pool. With both peers announcing rather than pushing, this exercises
// the whole inv → getdata → bodies round trip.
func TestTransactionPropagatesByAnnouncement(t *testing.T) {
	if testing.Short() {
		t.Skip("networked integration test")
	}
	addrA := freeAddr(t)
	a := fundedNetNode(t, addrA, nil)
	b := startTestNode(t, freeAddr(t), []string{addrA}, false)

	mustSoon(t, 10*time.Second, "the peers to negotiate capabilities", func() bool {
		for _, p := range a.Peers() {
			for _, c := range p.Caps {
				if c == CapTxInv {
					return true
				}
			}
		}
		return false
	})

	recipient, _ := wallet.New()
	tx := core.Transaction{
		From:   a.Wallet().Address(),
		To:     recipient.Address(),
		Amount: core.Coin,
		Fee:    core.DefaultMinRelayFee * 1000,
		Nonce:  a.NextNonce(a.Wallet().Address()),
	}
	if err := tx.Sign(a.Wallet()); err != nil {
		t.Fatal(err)
	}
	if err := a.SubmitTx(tx); err != nil {
		t.Fatalf("submit: %v", err)
	}
	mustSoon(t, 20*time.Second, "the announced transaction to be pulled by the peer", func() bool {
		_, ok := b.Mempool().Get(tx.Hash())
		return ok
	})
}
