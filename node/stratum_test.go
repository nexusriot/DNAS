package node

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// stratumClient is the smallest thing that can talk to the server: write a line,
// read lines, and tell a reply from a push.
type stratumClient struct {
	t    *testing.T
	conn net.Conn
	rd   *bufio.Scanner
}

func dialStratum(t *testing.T, n *Node) *stratumClient {
	t.Helper()
	conn, err := net.Dial("tcp", n.StratumAddr())
	if err != nil {
		t.Fatalf("dial stratum: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	return &stratumClient{t: t, conn: conn, rd: bufio.NewScanner(conn)}
}

func (c *stratumClient) call(id int, method string, params ...any) {
	c.t.Helper()
	line, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.conn.Write(append(line, '\n')); err != nil {
		c.t.Fatalf("write %s: %v", method, err)
	}
}

// next reads one message, decoded loosely so a reply and a push are both readable.
func (c *stratumClient) next() map[string]any {
	c.t.Helper()
	if !c.rd.Scan() {
		c.t.Fatalf("stratum connection closed: %v", c.rd.Err())
	}
	var m map[string]any
	if err := json.Unmarshal(c.rd.Bytes(), &m); err != nil {
		c.t.Fatalf("decode %q: %v", c.rd.Text(), err)
	}
	return m
}

// nextNotify reads until a push of the given method arrives, returning its params.
func (c *stratumClient) nextNotify(method string) []any {
	c.t.Helper()
	for i := 0; i < 20; i++ {
		m := c.next()
		if m["method"] == method {
			params, _ := m["params"].([]any)
			return params
		}
	}
	c.t.Fatalf("no %s within 20 messages", method)
	return nil
}

// jobFrom re-decodes a mining.notify job into its parts.
func jobFrom(t *testing.T, params []any) (jobID string, blk core.Block, shareBits uint32) {
	t.Helper()
	if len(params) < 4 {
		t.Fatalf("mining.notify carried %d params, want 4", len(params))
	}
	jobID, _ = params[0].(string)
	raw, err := json.Marshal(params[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &blk); err != nil {
		t.Fatalf("job is not a block: %v", err)
	}
	bits, _ := params[2].(float64)
	return jobID, blk, uint32(bits)
}

// testNodeIn is testNode with a state directory, so the persisted files can be
// written by one node and read back by another.
func testNodeIn(t *testing.T, dir string) (*Node, *core.Mempool, *wallet.Wallet) {
	t.Helper()
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	mp := core.NewMempool()
	return New(Config{ListenAddr: ":0", StateDir: dir}, core.NewBlockchain(), mp, w), mp, w
}

func startStratumNode(t *testing.T) *Node {
	t.Helper()
	n, _, _ := fundedNode(t)
	if err := n.StartStratum("127.0.0.1:0"); err != nil {
		t.Fatalf("start stratum: %v", err)
	}
	t.Cleanup(n.stratum.Stop)
	return n
}

// The whole session: subscribe, authorize, receive a job, find a nonce that meets
// the share target, submit it, and be credited.
func TestStratumSessionEarnsAShare(t *testing.T) {
	n := startStratumNode(t)
	miner, _ := wallet.New()
	c := dialStratum(t, n)

	c.call(1, "mining.subscribe", "dnas-test/1")
	if got := c.next(); got["result"] == nil {
		t.Fatalf("subscribe was refused: %+v", got)
	}

	c.call(2, "mining.authorize", miner.Address()+".rig1", "x")
	if got := c.next(); got["result"] != true {
		t.Fatalf("authorize was refused: %+v", got)
	}

	jobID, blk, shareBits := jobFrom(t, c.nextNotify("mining.notify"))
	if jobID == "" {
		t.Fatal("job has no id")
	}
	if blk.Index != n.Chain().Height()+1 {
		t.Fatalf("job is for height %d, tip is %d", blk.Index, n.Chain().Height())
	}

	// Search for a nonce, exactly as a miner would.
	var nonce uint64
	for ; nonce < 5_000_000; nonce++ {
		blk.Nonce = nonce
		if core.MeetsShareTarget(blk.ComputeHash(), shareBits) {
			break
		}
	}
	if nonce == 5_000_000 {
		t.Fatal("no share found; the share target is implausibly hard for a test")
	}

	c.call(3, "mining.submit", miner.Address()+".rig1", jobID, fmt.Sprintf("%x", nonce))
	for i := 0; i < 20; i++ {
		m := c.next()
		if m["id"] == nil {
			continue // a push (difficulty or a fresh job), not our answer
		}
		if m["result"] != true {
			t.Fatalf("share refused: %+v", m)
		}
		break
	}

	// It must show up as work the pool owes for.
	report := n.Pool()
	if report.Window == 0 {
		t.Fatal("the accepted share did not reach the payout window")
	}
	if report.Payouts[0].Address != miner.Address() {
		t.Fatalf("credited %s, want the authorized address %s",
			report.Payouts[0].Address, miner.Address())
	}
	if n.Shares().Accepted == 0 {
		t.Error("the share ledger recorded nothing")
	}
}

// The address a worker authorizes with is its accounting key, so a typo must be
// refused at the door rather than accruing credit nobody can be paid.
func TestStratumRefusesAnInvalidPayoutAddress(t *testing.T) {
	n := startStratumNode(t)
	c := dialStratum(t, n)

	c.call(1, "mining.subscribe", "dnas-test/1")
	c.next()
	c.call(2, "mining.authorize", "not-a-dnas-address", "x")
	got := c.next()
	if got["result"] == true {
		t.Fatal("an unpayable address was authorized")
	}
	if got["error"] == nil {
		t.Fatal("refusal carried no error")
	}
}

func TestStratumRefusesSubmitBeforeAuthorize(t *testing.T) {
	n := startStratumNode(t)
	c := dialStratum(t, n)

	c.call(1, "mining.subscribe", "dnas-test/1")
	c.next()
	c.call(2, "mining.submit", "whoever", "some-job", "00")
	got := c.next()
	if got["result"] == true {
		t.Fatal("an unauthorized connection was allowed to submit")
	}
}

func TestStratumRejectsAnUnknownJob(t *testing.T) {
	n := startStratumNode(t)
	miner, _ := wallet.New()
	c := dialStratum(t, n)

	c.call(1, "mining.subscribe", "dnas-test/1")
	c.next()
	c.call(2, "mining.authorize", miner.Address(), "x")
	c.next()
	c.nextNotify("mining.notify")

	c.call(3, "mining.submit", miner.Address(), "no-such-job", "ff")
	for i := 0; i < 20; i++ {
		m := c.next()
		if m["id"] == nil {
			continue
		}
		if m["result"] == true {
			t.Fatal("a share against an unknown job was accepted")
		}
		return
	}
	t.Fatal("no answer to the submission")
}

// Two miners on the same tip must not search the same space. The extranonce goes
// into the coinbase memo, which changes the merkle root and therefore every hash
// each miner computes.
func TestExtranonceGivesMinersDistinctSearchSpaces(t *testing.T) {
	n, _, _ := fundedNode(t)
	a, err := n.stratumTemplate("aaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	b, err := n.stratumTemplate("bbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	if a.MerkleRoot == b.MerkleRoot {
		t.Fatal("two extranonces produced the same merkle root, so both miners race over identical nonces")
	}
	if a.StateRoot != b.StateRoot {
		t.Error("the extranonce changed the state root, which it must not: no balance moved")
	}
	if !strings.HasPrefix(a.Transactions[0].Memo, "xn") {
		t.Errorf("extranonce not written to the coinbase memo: %q", a.Transactions[0].Memo)
	}
	// And the result must still be a block consensus would accept.
	mined, ok := core.Mine(a, nil)
	if !ok {
		t.Fatal("mining aborted")
	}
	if err := n.Chain().AddBlock(mined); err != nil {
		t.Fatalf("a template carrying an extranonce was rejected: %v", err)
	}
}

// A pool's accounting is the thing a restart must not lose: the shares were real
// work, and a miner that is not paid for them has been robbed by a process
// restart.
func TestPoolAccountingSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	n, _, _ := testNodeIn(t, dir)
	n.shares.record("alice", 7, false)
	n.window.add(PoolShare{Address: "alice", Weight: 5, Height: 7})
	n.reorgs.record(Reorg{Height: 7, Depth: 2})
	n.saveState()

	restarted, _, _ := testNodeIn(t, dir)
	restarted.loadState()

	if got := restarted.Shares().Accepted; got != 1 {
		t.Fatalf("share ledger lost its counters: accepted = %d, want 1", got)
	}
	if miners := restarted.Shares().Miners; len(miners) != 1 || miners[0].Address != "alice" {
		t.Fatalf("share ledger lost its rows: %+v", miners)
	}
	if r := restarted.Pool(); r.Window != 1 || r.Weight != 5 {
		t.Fatalf("payout window lost: %d shares / weight %v", r.Window, r.Weight)
	}
	if h := restarted.Reorgs().Reorgs; len(h) != 1 || h[0].Height != 7 {
		t.Fatalf("reorg history lost: %+v", h)
	}
	// The counters that mean "since this node started" are deliberately NOT
	// restored: a refused reorg from a previous run must not read as a live one.
	if restarted.Reorgs().Total != 0 {
		t.Errorf("reorg counters were restored; they describe this run, not the last one")
	}
}
