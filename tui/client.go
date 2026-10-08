package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client is a thin wrapper over a DNAS node's HTTP API.
type Client struct {
	base string
	http *http.Client
	// bin is the dnas binary this client delegates verification to (see
	// VerifyTx). It defaults to "dnas" on PATH and is overridden by the -dnas
	// flag, the same binary -spawn and -key already use.
	bin string
}

func NewClient(base string) *Client {
	if !strings.HasPrefix(base, "http") {
		base = "http://" + base
	}
	return &Client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 4 * time.Second}, bin: "dnas"}
}

// UseBinary points the client at a particular dnas binary for the operations
// it delegates.
func (c *Client) UseBinary(path string) {
	if path != "" {
		c.bin = path
	}
}

// Info mirrors GET /info.
type Info struct {
	Height         int      `json:"height"`
	Tip            string   `json:"tip"`
	NextDifficulty float64  `json:"next_difficulty"`
	Work           string   `json:"work"`
	Mempool        int      `json:"mempool"`
	MinRelayFee    uint64   `json:"min_relay_fee"`
	BaseFee        uint64   `json:"base_fee"`
	Peers          []string `json:"peers"`
	Mining         bool     `json:"mining"`
}

// FeeBucket is one band of the mempool's fee-rate distribution (GET
// /mempool/stats). The node computes it because it needs each transaction's
// canonical size, which a client cannot derive from the JSON it is served.
type FeeBucket struct {
	From  uint64 `json:"from_rate"`
	To    uint64 `json:"to_rate"` // 0 = "and above"
	Count int    `json:"count"`
	Bytes int    `json:"bytes"`
}

// MempoolStats mirrors GET /mempool/stats.
type MempoolStats struct {
	Count      int         `json:"count"`
	Bytes      int         `json:"bytes"`
	MinRate    uint64      `json:"min_rate"`
	MaxRate    uint64      `json:"max_rate"`
	MedianRate uint64      `json:"median_rate"`
	BaseFee    uint64      `json:"base_fee"`
	Buckets    []FeeBucket `json:"buckets"`
}

// TxStatus mirrors GET /tx/{hash}: a transaction's position in its life, from
// pending through confirmed.
type TxStatus struct {
	Status        string `json:"status"`
	Hash          string `json:"hash"`
	Height        uint64 `json:"height"`
	Confirmations uint64 `json:"confirmations"`
	Error         string `json:"error"`
}

// Tx is a transaction as returned by /chain and /mempool.
type Tx struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Amount uint64 `json:"amount"`
	Fee    uint64 `json:"fee"`
	Nonce  uint64 `json:"nonce"`
	Memo   string `json:"memo"`
}

// Block is a block as returned by /chain.
type Block struct {
	Index        uint64 `json:"index"`
	Hash         string `json:"hash"`
	Bits         uint32 `json:"bits"`
	Timestamp    int64  `json:"timestamp"`
	Transactions []Tx   `json:"transactions"`
}

type header struct {
	Index      uint64 `json:"index"`
	Timestamp  int64  `json:"timestamp"`
	PrevHash   string `json:"prev_hash"`
	MerkleRoot string `json:"merkle_root"`
	StateRoot  string `json:"state_root"`
	BaseFee    uint64 `json:"base_fee"`
	Bits       uint32 `json:"bits"`
	Nonce      uint64 `json:"nonce"`
	Hash       string `json:"hash"`
}

type proofStep struct {
	Hash  string `json:"hash"`
	Right bool   `json:"right"`
}

type proof struct {
	Found         bool        `json:"found"`
	BlockIndex    uint64      `json:"block_index"`
	Confirmations uint64      `json:"confirmations"`
	MerkleRoot    string      `json:"merkle_root"`
	Proof         []proofStep `json:"proof"`
}

func (c *Client) getJSON(path string, v any) error {
	resp, err := c.http.Get(c.base + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func (c *Client) postJSON(path string, body, v any) error {
	data, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := os.Getenv("DNAS_API_TOKEN"); tok != "" { // attach token if the node requires one
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e); e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("%s", resp.Status)
	}
	if v != nil {
		return json.Unmarshal(raw, v)
	}
	return nil
}

func (c *Client) Info() (Info, error) { var i Info; return i, c.getJSON("/info", &i) }

func (c *Client) Address() (string, error) {
	var m map[string]string
	err := c.getJSON("/address", &m)
	return m["address"], err
}

func (c *Client) BalanceFmt(addr string) (string, error) {
	var m map[string]any
	if err := c.getJSON("/balance/"+addr, &m); err != nil {
		return "", err
	}
	s, _ := m["balance_fmt"].(string)
	return s, nil
}

// Subscribe opens the node's Server-Sent Events stream and returns a channel that
// receives the type of each event ("block", "reorg", "tx") plus a cancel func.
// The channel closes when the stream ends or cancel is called. It uses its own
// client with no timeout, since the stream is long-lived. This lets the UI
// refresh the instant something changes instead of only polling.
func (c *Client) Subscribe() (<-chan string, func(), error) {
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/events", nil)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return nil, nil, fmt.Errorf("events: %s", resp.Status)
	}
	ch := make(chan string, 16)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "event:") {
				continue
			}
			select {
			case ch <- strings.TrimSpace(strings.TrimPrefix(line, "event:")):
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, cancel, nil
}

// RecentBlocks fetches the newest `n` blocks. It must ask for `last` rather than
// GET /chain and keep the tail: the bulk reads are paged, so an unparameterized
// request returns the FIRST page — which past the page limit is the oldest
// blocks, not the newest, and looks like a chain frozen in the distant past.
func (c *Client) RecentBlocks(n int) ([]Block, error) {
	var b []Block
	return b, c.getJSON(fmt.Sprintf("/chain?last=%d", n), &b)
}

// MempoolStats fetches the pending queue's fee-rate distribution.
func (c *Client) MempoolStats() (MempoolStats, error) {
	var st MempoolStats
	return st, c.getJSON("/mempool/stats", &st)
}

// TxStatus looks one transaction up, whether it is pending or confirmed. A
// transaction the node has never seen is not an error here — it is a status,
// because a watcher polling for one it just submitted will see exactly that for
// a moment.
func (c *Client) TxStatus(hash string) (TxStatus, error) {
	var st TxStatus
	if err := c.getJSON("/tx/"+hash, &st); err != nil {
		if strings.Contains(err.Error(), "404") {
			return TxStatus{Status: "unknown", Hash: hash}, nil
		}
		return TxStatus{}, err
	}
	return st, nil
}
func (c *Client) Mempool() ([]Tx, error)  { var t []Tx; return t, c.getJSON("/mempool", &t) }
func (c *Client) SetMining(on bool) error { return c.postJSON("/mine", map[string]bool{"on": on}, nil) }

// MultisigAddress asks the node to derive an M-of-N multisig address from member
// public keys (hex). It is a stateless helper — no keys are stored on the node.
func (c *Client) MultisigAddress(threshold int, pubkeys []string) (string, error) {
	var r map[string]any
	err := c.postJSON("/multisig/address", map[string]any{"threshold": threshold, "pubkeys": pubkeys}, &r)
	if err != nil {
		return "", err
	}
	addr, _ := r["address"].(string)
	return addr, nil
}

// HDWallet generates (empty mnemonic) or restores a BIP39 HD wallet and returns
// the mnemonic plus the first `count` derived addresses.
func (c *Client) HDWallet(mnemonic string, count int) (string, []string, error) {
	var r struct {
		Mnemonic  string   `json:"mnemonic"`
		Addresses []string `json:"addresses"`
	}
	err := c.postJSON("/wallet/hd", map[string]any{"mnemonic": mnemonic, "count": count}, &r)
	return r.Mnemonic, r.Addresses, err
}

// Send builds a transfer (amounts in base units) via the node wallet.
func (c *Client) Send(to string, amount, fee uint64, memo string) (string, error) {
	req := map[string]any{"to": to, "amount": amount, "fee": fee}
	if memo != "" {
		req["memo"] = memo
	}
	var r map[string]any
	if err := c.postJSON("/send", req, &r); err != nil {
		return "", err
	}
	h, _ := r["hash"].(string)
	return h, nil
}

// VerifyTx performs a light-client (SPV) check by DELEGATING to the `dnas`
// binary, which holds the one implementation of the verification rules.
//
// It used to do the check here, with a hand-written copy of the header
// preimage and the compact-target comparison. That copy was wrong. It hashed
// `index|timestamp|prev|merkle|state|base_fee|bits|nonce`, which stopped being
// the real preimage when BIP9 signaling put Version at the front — so this
// client had been answering FAILED for every valid proof, and would have
// answered it just as confidently for an invalid one. Three other copies in
// this repo had drifted in exactly the same way at exactly the same moment.
//
// The same reasoning that put signing behind the binary (selfcustody.go)
// applies with more force to verification: a signer that is wrong produces a
// transaction a node rejects, and you find out. A verifier that is wrong tells
// you a payment is proven, and you do not.
func (c *Client) VerifyTx(txHash string) (string, error) {
	bin := c.bin
	if bin == "" {
		bin = "dnas"
	}
	out, errOut, err := run(bin, "spv", "-api", c.base, "-cache", "", "verify", txHash)
	if err != nil {
		if msg := lastLine(errOut); msg != "" {
			return "", fmt.Errorf("%s", msg)
		}
		return "", fmt.Errorf("%s spv verify: %w", bin, err)
	}
	// stdout carries the result and stderr the log lines, so the last line of
	// stdout is the verdict. The CLI reports a refusal it handled itself with
	// exit status 0, so the outcome has to be read from the text.
	line := lastLine(out)
	if line == "" {
		return "", fmt.Errorf("%s spv verify said nothing", bin)
	}
	return line, nil
}

// difficultyOf renders a compact target as a difficulty ratio.
//
// This is the one piece of target arithmetic the client still does, and it is
// kept because it is COSMETIC: it labels a row in a list. Nothing is believed
// on the strength of it, so a drift here misdraws a number rather than
// asserting that a payment happened. Everything load-bearing goes through the
// binary (VerifyTx) — see the note there for what happened when this file
// tried to be clever about proof of work.
func difficultyOf(bits uint32) float64 {
	mantissa := bits & 0x007fffff
	exponent := bits >> 24
	target := new(big.Int).SetUint64(uint64(mantissa))
	if exponent <= 3 {
		target.Rsh(target, 8*(3-uint(exponent)))
	} else {
		target.Lsh(target, 8*(uint(exponent)-3))
	}
	if target.Sign() <= 0 {
		return 0
	}
	powLimit := new(big.Int).Lsh(big.NewInt(0x0fffff), 8*(0x1f-3))
	f, _ := new(big.Rat).SetFrac(powLimit, target).Float64()
	return f
}
