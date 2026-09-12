package node

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// A Stratum-shaped mining server.
//
// `dnas miner` polls GET /blocktemplate and POSTs to /submitshare, which works
// and is not how miners are actually connected to pools. Stratum is: one long
// TCP connection, the server PUSHES a job the moment the tip moves, the miner
// answers with nonces, and the server tells each miner how hard to work.
//
// What this is NOT is Bitcoin-compatible. Stratum V1's job encoding is built
// around Bitcoin's 80-byte header and its coinbase splicing; DNAS has neither, so
// a cgminer pointed at this port would hash the wrong bytes. Rather than pretend
// otherwise, the framing, the method names and the session lifecycle are
// Stratum's — line-delimited JSON-RPC, subscribe/authorize/notify/submit — and
// the job carries a DNAS candidate block. A miner written against this is a
// dozen lines; a miner written against Bitcoin's Stratum is not one of them.
//
// Three details make it a pool rather than a demo:
//
//   - Every connection gets its own EXTRANONCE, written into the coinbase memo.
//     That changes the merkle root, so two miners on the same job search
//     genuinely different spaces instead of racing over identical nonces.
//   - Every connection gets its own DIFFICULTY, retuned as it works (pool.go), so
//     a slow miner's work is still visible and a fast one does not flood us.
//   - Shares are weighted by their own difficulty and paid PPLNS (pool.go), so
//     what a miner earns tracks the work it did.
//
// Miners are authorized by the DNAS address they want paying. The coinbase pays
// the POOL's address — payouts are the pool's job, which is the whole point of
// running one — so the address a worker authorizes with is an accounting key, and
// it is validated as an address so a typo cannot silently accrue unpayable
// credit.

// stratumMaxLine bounds one request. Nothing a miner legitimately sends is large,
// and without a bound a connection could force an unbounded read.
const stratumMaxLine = 64 << 10

// stratumJobTTL is how long a job may be submitted against after it is replaced.
// Some grace matters: a share found microseconds before the tip moved is real
// work, and rejecting it would penalize the miner for the pool's timing.
const stratumJobTTL = 2 * time.Minute

// maxStratumJobs bounds the jobs remembered per connection.
const maxStratumJobs = 16

// stratumRequest is one line from a miner.
type stratumRequest struct {
	ID     *json.RawMessage  `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

// stratumResponse answers one request; stratumNotify is a server push.
type stratumResponse struct {
	ID     *json.RawMessage `json:"id"`
	Result any              `json:"result"`
	Error  any              `json:"error"`
}

type stratumNotify struct {
	ID     *json.RawMessage `json:"id"` // always null for a notification
	Method string           `json:"method"`
	Params []any            `json:"params"`
}

// stratumJob is a candidate handed to one miner.
type stratumJob struct {
	id        string
	block     core.Block
	shareBits uint32
	issued    time.Time
}

// stratumConn is one miner's session.
type stratumConn struct {
	srv        *StratumServer
	conn       net.Conn
	enc        *json.Encoder
	mu         sync.Mutex // serializes writes
	extranonce string
	subscribed bool

	stateMu sync.Mutex
	address string // the DNAS address this worker is credited to
	worker  string
	vd      *vardiff
	jobs    map[string]*stratumJob
	order   []string
	lastJob string
}

// StratumServer accepts miner connections and keeps them supplied with jobs.
type StratumServer struct {
	node *Node
	ln   net.Listener

	mu    sync.Mutex
	conns map[*stratumConn]bool

	quit chan struct{}
	wg   sync.WaitGroup
}

// StartStratum begins serving the Stratum-shaped protocol on addr. The node must
// have a wallet: the coinbase of every job pays it, and the pool distributes from
// there.
func (n *Node) StartStratum(addr string) error {
	if n.wallet == nil {
		return errors.New("stratum needs a wallet to pay the pool's coinbase to")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("stratum listen %s: %w", addr, err)
	}
	s := &StratumServer{node: n, ln: ln, conns: map[*stratumConn]bool{}, quit: make(chan struct{})}
	n.stratum = s
	s.wg.Add(2)
	go s.accept()
	go s.jobLoop()
	Infof("stratum listening", "addr", ln.Addr().String(), "pays", n.wallet.Address())
	return nil
}

// StratumAddr is the address the stratum server is listening on ("" if none).
func (n *Node) StratumAddr() string {
	if n.stratum == nil {
		return ""
	}
	return n.stratum.ln.Addr().String()
}

// Stop closes the listener and every session.
func (s *StratumServer) Stop() {
	select {
	case <-s.quit:
		return
	default:
	}
	close(s.quit)
	_ = s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		_ = c.conn.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *StratumServer) sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

func (s *StratumServer) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return
			default:
			}
			continue
		}
		c := &stratumConn{
			srv: s, conn: conn, enc: json.NewEncoder(conn),
			extranonce: randomExtranonce(),
			vd:         newVardiff(s.node.ShareFactor()),
			jobs:       map[string]*stratumJob{},
		}
		s.mu.Lock()
		s.conns[c] = true
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			c.serve()
		}()
	}
}

// jobLoop pushes a fresh job to every authorized miner whenever the tip moves,
// which is the reason Stratum exists: a miner that learns of a new block by
// polling spends the gap hashing a template that is already dead.
func (s *StratumServer) jobLoop() {
	defer s.wg.Done()
	events, unsub := s.node.Subscribe()
	defer unsub()
	last := s.node.chain.Tip().Hash
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-s.node.quit:
			return
		case _, ok := <-events:
			if !ok {
				return
			}
			if tip := s.node.chain.Tip().Hash; tip != last {
				last = tip
				s.pushJobs(true)
			}
		case <-tick.C:
			// A periodic refresh picks up transactions that arrived without the tip
			// moving, so a miner is not stuck on a job that pays no fees.
			s.pushJobs(false)
		}
	}
}

func (s *StratumServer) pushJobs(clean bool) {
	s.mu.Lock()
	conns := make([]*stratumConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		if c.authorized() {
			c.sendJob(clean)
		}
	}
}

func (s *StratumServer) drop(c *stratumConn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	_ = c.conn.Close()
}

func randomExtranonce() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (c *stratumConn) authorized() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.address != ""
}

func (c *stratumConn) write(v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = c.enc.Encode(v)
}

func (c *stratumConn) reply(id *json.RawMessage, result any) {
	c.write(stratumResponse{ID: id, Result: result})
}

func (c *stratumConn) fail(id *json.RawMessage, code int, msg string) {
	c.write(stratumResponse{ID: id, Result: nil, Error: []any{code, msg, nil}})
}

func (c *stratumConn) notify(method string, params []any) {
	c.write(stratumNotify{Method: method, Params: params})
}

func (c *stratumConn) serve() {
	defer c.srv.drop(c)
	sc := bufio.NewScanner(c.conn)
	sc.Buffer(make([]byte, 0, 4096), stratumMaxLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req stratumRequest
		if err := json.Unmarshal(line, &req); err != nil {
			c.fail(nil, 20, "malformed request")
			continue
		}
		if !c.handle(req) {
			return
		}
	}
}

// handle dispatches one request and reports whether the session continues.
func (c *stratumConn) handle(req stratumRequest) bool {
	switch req.Method {
	case "mining.subscribe":
		c.subscribed = true
		// The shape Stratum clients expect: the subscription list, this session's
		// extranonce, and how many bytes of it the miner may itself vary. DNAS
		// gives the whole extranonce to the server, so that size is 0.
		c.reply(req.ID, []any{
			[]any{[]any{"mining.set_difficulty", c.extranonce}, []any{"mining.notify", c.extranonce}},
			c.extranonce, 0,
		})
		return true

	case "mining.authorize":
		user, _ := stratumString(req.Params, 0)
		addr, worker := splitWorker(user)
		if err := wallet.ValidateAddress(addr); err != nil {
			c.fail(req.ID, 24, "unauthorized: "+err.Error())
			return true
		}
		c.stateMu.Lock()
		c.address, c.worker = addr, worker
		c.stateMu.Unlock()
		c.reply(req.ID, true)
		Infof("stratum worker authorized", "address", short(addr), "worker", worker)
		c.sendDifficulty()
		c.sendJob(true)
		return true

	case "mining.submit":
		return c.onSubmit(req)

	case "mining.extranonce.subscribe":
		c.reply(req.ID, true)
		return true

	default:
		c.fail(req.ID, 20, "unknown method "+req.Method)
		return true
	}
}

func (c *stratumConn) onSubmit(req stratumRequest) bool {
	if !c.authorized() {
		c.fail(req.ID, 24, "unauthorized: authorize before submitting")
		return true
	}
	jobID, _ := stratumString(req.Params, 1)
	nonceStr, _ := stratumString(req.Params, 2)
	nonce, err := strconv.ParseUint(strings.TrimPrefix(nonceStr, "0x"), 16, 64)
	if err != nil {
		c.fail(req.ID, 20, "nonce is not hex")
		return true
	}

	c.stateMu.Lock()
	job := c.jobs[jobID]
	addr, worker := c.address, c.worker
	c.stateMu.Unlock()
	if job == nil {
		c.fail(req.ID, 21, "unknown job")
		return true
	}
	if time.Since(job.issued) > stratumJobTTL {
		c.fail(req.ID, 21, "job expired")
		return true
	}

	b := job.block
	b.Nonce = nonce
	b.Hash = b.ComputeHash()
	res, err := c.srv.node.submitShareAt(b, job.shareBits, addr, worker)
	if err != nil {
		c.fail(req.ID, 23, err.Error())
		return true
	}
	c.reply(req.ID, true)
	if res.Block {
		Infof("stratum miner found a block", "height", res.Height,
			"worker", worker, "address", short(addr))
	}
	// Retune only after the share is answered, so the miner is never told to
	// change difficulty in the middle of accounting for one.
	c.stateMu.Lock()
	next, changed := c.vd.observe(time.Now())
	c.stateMu.Unlock()
	if changed {
		Debugf("stratum difficulty retuned", "worker", worker, "factor", next)
		c.sendDifficulty()
		c.sendJob(false)
	}
	return true
}

func (c *stratumConn) sendDifficulty() {
	c.stateMu.Lock()
	f := c.vd.factor
	c.stateMu.Unlock()
	// Stratum's set_difficulty is "how many times harder than the easiest target";
	// DNAS's share factor is "how many times EASIER than a block", so the number a
	// miner needs in order to compute its own target is the factor itself.
	c.notify("mining.set_difficulty", []any{f})
}

// sendJob builds this connection's next candidate and pushes it. clean tells the
// miner to abandon earlier jobs, which is true whenever the tip moved: work on an
// old tip can no longer become a block.
func (c *stratumConn) sendJob(clean bool) {
	c.stateMu.Lock()
	factor := c.vd.factor
	c.stateMu.Unlock()

	b, err := c.srv.node.stratumTemplate(c.extranonce)
	if err != nil {
		Debugf("stratum job unavailable", "err", err)
		return
	}
	job := &stratumJob{
		id:        strconv.FormatUint(b.Index, 10) + "-" + b.MerkleRoot[:8] + "-" + randomExtranonce(),
		block:     b,
		shareBits: core.ShareBits(b.Bits, factor),
		issued:    time.Now(),
	}

	c.stateMu.Lock()
	if len(c.order) >= maxStratumJobs {
		delete(c.jobs, c.order[0])
		c.order = c.order[1:]
	}
	c.jobs[job.id] = job
	c.order = append(c.order, job.id)
	c.lastJob = job.id
	c.stateMu.Unlock()

	c.notify("mining.notify", []any{job.id, b, job.shareBits, clean})
}

// stratumTemplate builds a candidate paying the pool, with this connection's
// extranonce written into the coinbase memo so two miners on the same tip search
// different spaces rather than racing over identical nonces.
//
// The memo changes the coinbase's txid and therefore the merkle root; it does not
// touch any balance, so the state root the template already committed to stays
// correct and does not need recomputing.
func (n *Node) stratumTemplate(extranonce string) (core.Block, error) {
	b, err := n.BuildTemplate(n.wallet.Address())
	if err != nil {
		return core.Block{}, err
	}
	b.Transactions = append([]core.Transaction(nil), b.Transactions...)
	b.Transactions[0].Memo = "xn" + extranonce
	b.MerkleRoot = core.MerkleRoot(b.Transactions)
	b.Hash = ""
	return b, nil
}

// submitShareAt is SubmitShare against a per-connection share target, crediting
// the pool's PPLNS window as well as the plain share ledger.
func (n *Node) submitShareAt(b core.Block, shareBits uint32, addr, worker string) (ShareResult, error) {
	res, err := n.submitShareChecked(b, shareBits, addr)
	if err != nil {
		return res, err
	}
	n.window.add(PoolShare{
		Address: addr,
		Worker:  worker,
		Weight:  core.TargetDifficulty(shareBits),
		Height:  b.Index,
		At:      time.Now().Unix(),
	})
	return res, nil
}

// stratumString reads one positional parameter as a string.
func stratumString(params []json.RawMessage, i int) (string, bool) {
	if i >= len(params) {
		return "", false
	}
	var s string
	if err := json.Unmarshal(params[i], &s); err != nil {
		return "", false
	}
	return s, true
}

// splitWorker separates "address.worker" into its parts. A bare address is a
// worker named "default", which is what a miner that does not name one gets.
func splitWorker(user string) (address, worker string) {
	user = strings.TrimSpace(user)
	if addr, w, ok := strings.Cut(user, "."); ok {
		if w == "" {
			w = "default"
		}
		return addr, w
	}
	return user, "default"
}
