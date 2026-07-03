package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a thin wrapper over a DNAS node's HTTP API.
type Client struct {
	base string
	http *http.Client
}

func NewClient(base string) *Client {
	if !strings.HasPrefix(base, "http") {
		base = "http://" + base
	}
	return &Client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 4 * time.Second}}
}

// Info mirrors GET /info.
type Info struct {
	Height         int      `json:"height"`
	Tip            string   `json:"tip"`
	NextDifficulty int      `json:"next_difficulty"`
	Work           string   `json:"work"`
	Mempool        int      `json:"mempool"`
	MinRelayFee    uint64   `json:"min_relay_fee"`
	Peers          []string `json:"peers"`
	Mining         bool     `json:"mining"`
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
	Difficulty   int    `json:"difficulty"`
	Timestamp    int64  `json:"timestamp"`
	Transactions []Tx   `json:"transactions"`
}

type header struct {
	Index      uint64 `json:"index"`
	Timestamp  int64  `json:"timestamp"`
	PrevHash   string `json:"prev_hash"`
	MerkleRoot string `json:"merkle_root"`
	Difficulty int    `json:"difficulty"`
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
	resp, err := c.http.Post(c.base+path, "application/json", bytes.NewReader(data))
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

func (c *Client) Chain() ([]Block, error) { var b []Block; return b, c.getJSON("/chain", &b) }
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

// VerifyTx performs a light-client (SPV) check: fetch the proof and the header
// it commits to, verify the header's proof-of-work, and fold the merkle proof.
func (c *Client) VerifyTx(txHash string) (string, error) {
	var pr proof
	if err := c.getJSON("/proof/"+txHash, &pr); err != nil {
		return "", err
	}
	var hdr header
	if err := c.getJSON(fmt.Sprintf("/header/%d", pr.BlockIndex), &hdr); err != nil {
		return "", err
	}
	hs := fmt.Sprintf("%d|%d|%s|%s|%d|%d", hdr.Index, hdr.Timestamp, hdr.PrevHash, hdr.MerkleRoot, hdr.Difficulty, hdr.Nonce)
	powOK := sha(hs) == hdr.Hash && strings.HasPrefix(hdr.Hash, strings.Repeat("0", hdr.Difficulty))
	h := txHash
	for _, s := range pr.Proof {
		if s.Right {
			h = sha(h + s.Hash)
		} else {
			h = sha(s.Hash + h)
		}
	}
	if powOK && h == hdr.MerkleRoot {
		return fmt.Sprintf("PROVEN in block %d (%d confirmations)", pr.BlockIndex, pr.Confirmations), nil
	}
	return fmt.Sprintf("FAILED (pow=%v inclusion=%v)", powOK, h == hdr.MerkleRoot), nil
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
