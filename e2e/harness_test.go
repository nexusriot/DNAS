//go:build e2e

// Package e2e drives the shipped `dnas` binary the way a user does — start a
// node, talk to its HTTP API, run the CLI against it — and asserts on what comes
// back. Nothing here imports DNAS packages: the suite is deliberately a black-box
// consumer of the binary and the wire formats, so it fails if the *product*
// breaks even when every unit test still passes.
//
// Run it with the `e2e` build tag (see the Makefile) or, hermetically, inside the
// container built from e2e/Dockerfile:
//
//	make e2e          # against a binary built from the working tree
//	make e2e-docker   # same suite, isolated in a container
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Coin is the number of base units in one DNAS. Duplicated rather than imported
// so the suite stays a black-box client with no DNAS dependencies; if consensus
// ever changed it, that is exactly the kind of break e2e should catch loudly.
const Coin = 100_000_000

// readyTimeout bounds how long a freshly started node may take to serve /info.
const readyTimeout = 30 * time.Second

var (
	buildOnce sync.Once
	binary    string
	buildErr  error
)

// TestMain builds the binary once for the whole suite.
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

// dnasBin returns the path to the dnas binary, building it on first use. A
// prebuilt binary can be supplied with DNAS_BIN (the container does this, so the
// image's build stage is reused instead of compiling inside the test run).
func dnasBin(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		if pre := os.Getenv("DNAS_BIN"); pre != "" {
			if _, err := os.Stat(pre); err == nil {
				binary = pre
				return
			}
			buildErr = fmt.Errorf("DNAS_BIN=%s does not exist", pre)
			return
		}
		out, err := os.MkdirTemp("", "dnas-e2e-bin")
		if err != nil {
			buildErr = err
			return
		}
		binary = filepath.Join(out, "dnas")
		cmd := exec.Command("go", "build", "-o", binary, "./cmd/dnas")
		cmd.Dir = ".."
		if b, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("build dnas: %v\n%s", err, b)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binary
}

// freePort asks the kernel for an unused TCP port and hands it back. There is an
// unavoidable gap between closing the probe and the node binding, which is why
// every port is drawn fresh per node rather than reused across tests.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe for a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// node is a running dnas process plus everything needed to talk to it.
type node struct {
	t       *testing.T
	name    string
	dir     string
	apiAddr string // host:port of the HTTP API
	p2pAddr string // host:port of the P2P listener
	token   string // API bearer token, empty when the node is open
	cmd     *exec.Cmd
	log     *lockedBuffer
	stopped bool
}

// lockedBuffer collects a node's output without racing the test goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// nodeOpts tunes how a node is started.
type nodeOpts struct {
	name  string   // label used in failure messages and the data directory
	peers []string // seed peers to dial
	token string   // when set, the node requires this API bearer token for writes
	dir   string   // reuse an existing data directory (for restart tests)
	extra []string // any additional flags
}

// startNode launches a regtest node and waits until its API answers. Regtest
// keeps blocks instant (no retargeting) and mines only on demand via /generate,
// so every test drives the chain deterministically instead of racing a miner.
func startNode(t *testing.T, opts nodeOpts) *node {
	t.Helper()
	if opts.name == "" {
		opts.name = "node"
	}
	dir := opts.dir
	if dir == "" {
		dir = filepath.Join(t.TempDir(), opts.name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	n := &node{
		t:       t,
		name:    opts.name,
		dir:     dir,
		apiAddr: fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		p2pAddr: fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		token:   opts.token,
		log:     &lockedBuffer{},
	}

	args := []string{
		"node", "-regtest",
		"-listen", n.p2pAddr,
		"-advertise", n.p2pAddr,
		"-api", n.apiAddr,
		"-db", filepath.Join(dir, "chain.db"),
		"-wallet", filepath.Join(dir, "wallet.json"),
	}
	if len(opts.peers) > 0 {
		args = append(args, "-peers", strings.Join(opts.peers, ","))
	}
	args = append(args, opts.extra...)

	cmd := exec.Command(dnasBin(t), args...)
	cmd.Dir = dir
	cmd.Stdout = n.log
	cmd.Stderr = n.log
	cmd.Env = append(os.Environ(), "DNAS_API_TOKEN="+opts.token)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", n.name, err)
	}
	n.cmd = cmd
	t.Cleanup(n.stop)

	n.waitReady()
	return n
}

// waitReady blocks until the node serves /info, failing the test with its log if
// it never does.
func (n *node) waitReady() {
	n.t.Helper()
	deadline := time.Now().Add(readyTimeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(n.url("/info"))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if n.cmd.ProcessState != nil && n.cmd.ProcessState.Exited() {
			n.t.Fatalf("%s exited before becoming ready:\n%s", n.name, n.log.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	n.t.Fatalf("%s did not become ready within %s:\n%s", n.name, readyTimeout, n.log.String())
}

// stop shuts the node down the way an operator would (SIGTERM), so its graceful
// path — flushing the store and persisting peers/bans/mempool — is exercised.
func (n *node) stop() {
	if n.stopped || n.cmd == nil || n.cmd.Process == nil {
		return
	}
	n.stopped = true
	_ = n.cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _, _ = n.cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = n.cmd.Process.Kill()
		<-done
	}
}

// forgetPeers deletes the persisted peer book, so a restarted node does not
// re-dial everyone it used to know. Call it while the node is stopped; tests that
// need two nodes to build competing branches rely on staying disconnected.
func (n *node) forgetPeers() {
	n.t.Helper()
	if err := os.Remove(filepath.Join(n.dir, "peers.json")); err != nil && !os.IsNotExist(err) {
		n.t.Fatalf("forget peers: %v", err)
	}
}

func (n *node) url(path string) string { return "http://" + n.apiAddr + path }

// getJSON fetches a JSON document, failing on any non-200.
func (n *node) getJSON(path string, v any) {
	n.t.Helper()
	resp, err := http.Get(n.url(path))
	if err != nil {
		n.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		n.t.Fatalf("GET %s: status %d: %s", path, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, v); err != nil {
		n.t.Fatalf("GET %s: decode %q: %v", path, body, err)
	}
}

// get returns the raw status and body, for tests that assert on failure codes.
func (n *node) get(path string) (int, string) {
	n.t.Helper()
	resp, err := http.Get(n.url(path))
	if err != nil {
		n.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// post sends a JSON body (attaching the bearer token when the node has one) and
// returns the status and decoded response.
func (n *node) post(path string, body any) (int, map[string]any) {
	n.t.Helper()
	return n.postWithToken(path, body, n.token)
}

func (n *node) postWithToken(path string, body any, token string) (int, map[string]any) {
	n.t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		n.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, n.url(path), bytes.NewReader(data))
	if err != nil {
		n.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		n.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// generate mines n blocks on demand (regtest) and fails if the node refuses.
func (n *node) generate(count int) {
	n.t.Helper()
	status, body := n.post("/generate", map[string]int{"n": count})
	if status != http.StatusOK {
		n.t.Fatalf("generate %d: status %d: %v\n%s", count, status, body, n.log.String())
	}
	if mined, _ := body["mined"].(float64); int(mined) != count {
		n.t.Fatalf("generate %d: node mined %v", count, body["mined"])
	}
}

// height reads the current chain height.
func (n *node) height() uint64 {
	n.t.Helper()
	var info struct {
		Height uint64 `json:"height"`
	}
	n.getJSON("/info", &info)
	return info.Height
}

// tip reads the current tip hash.
func (n *node) tip() string {
	n.t.Helper()
	var info struct {
		Tip string `json:"tip"`
	}
	n.getJSON("/info", &info)
	return info.Tip
}

// balance reads a confirmed coin balance in base units.
func (n *node) balance(addr string) uint64 {
	n.t.Helper()
	var b struct {
		Balance uint64 `json:"balance"`
	}
	n.getJSON("/balance/"+addr, &b)
	return b.Balance
}

// address returns the node wallet's address.
func (n *node) address() string {
	n.t.Helper()
	var a struct {
		Address string `json:"address"`
	}
	n.getJSON("/address", &a)
	return a.Address
}

// send asks the node wallet to pay an address, returning the transaction hash.
func (n *node) send(to string, amount, fee uint64) string {
	n.t.Helper()
	status, body := n.post("/send", map[string]any{"to": to, "amount": amount, "fee": fee})
	if status != http.StatusOK {
		n.t.Fatalf("send %d to %s: status %d: %v", amount, to, status, body)
	}
	hash, _ := body["hash"].(string)
	if hash == "" {
		n.t.Fatalf("send returned no transaction hash: %v", body)
	}
	return hash
}

// cli runs the dnas CLI and returns its combined output. The CLI reports most
// failures on stdout and still exits 0, so callers assert on the text.
func (n *node) cli(args ...string) string {
	n.t.Helper()
	return runCLI(n.t, n.dir, args...)
}

func runCLI(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(dnasBin(t), args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dnas %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// newWallet creates a wallet file in dir and returns its address.
func newWallet(t *testing.T, dir, file string) string {
	t.Helper()
	out := runCLI(t, dir, "wallet", "new", "-o", file)
	addr := scanAddress(out)
	if addr == "" {
		t.Fatalf("wallet new printed no address:\n%s", out)
	}
	return addr
}

// scanAddress pulls the first dnas… address out of CLI output.
func scanAddress(out string) string {
	for _, f := range strings.Fields(out) {
		f = strings.Trim(f, "\"',")
		if strings.HasPrefix(f, "dnas") && len(f) == 52 {
			return f
		}
	}
	return ""
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// mustContain fails with the full output when a marker is missing, which is what
// makes a CLI failure readable (the commands print errors and exit 0).
func mustContain(t *testing.T, out, want, what string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Fatalf("%s: expected output to contain %q, got:\n%s", what, want, out)
	}
}
