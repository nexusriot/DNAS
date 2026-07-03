package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/nexusriot/DNAS/core"
)

// runSPV implements `dnas spv ...`, a standalone light client. It trusts only
// block headers (which it proof-of-work-verifies) and compact merkle proofs —
// it never downloads block bodies or trusts the node's balances.
//
//	dnas spv [-api URL] sync            verify the header chain, print tip + work
//	dnas spv [-api URL] verify <txhash> prove a transaction is in the chain
func runSPV(args []string) {
	fs := flag.NewFlagSet("spv", flag.ExitOnError)
	api := fs.String("api", "localhost:8080", "node HTTP API address")
	_ = fs.Parse(args)
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Println("usage: dnas spv [-api URL] <sync | verify <txhash>>")
		return
	}
	base := ensureHTTP(*api)

	switch rest[0] {
	case "sync":
		headers, err := fetchHeaders(base)
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		tip, work, err := verifyHeaderChain(headers)
		if err != nil {
			fmt.Println("header chain INVALID:", err)
			return
		}
		fmt.Printf("✓ header chain verified: %d headers, tip height %d, cumulative work %s\n",
			len(headers), tip, work)
	case "verify":
		if len(rest) < 2 {
			fmt.Println("usage: dnas spv [-api URL] verify <txhash>")
			return
		}
		headers, err := fetchHeaders(base)
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		pr, err := fetchProof(base, rest[1])
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		msg, err := verifySPV(headers, pr, rest[1])
		if err != nil {
			fmt.Println("NOT PROVEN:", err)
			return
		}
		fmt.Println("✓", msg)
	default:
		fmt.Println("unknown spv command:", rest[0])
	}
}

// verifyHeaderChain checks that headers start at the canonical genesis and form
// a contiguous, proof-of-work-valid chain, returning the tip height and the
// chain's cumulative work.
func verifyHeaderChain(headers []core.Header) (uint64, *big.Int, error) {
	if len(headers) == 0 {
		return 0, nil, fmt.Errorf("no headers")
	}
	if headers[0].Hash != core.GenesisBlock().Hash {
		return 0, nil, fmt.Errorf("genesis mismatch")
	}
	if err := core.ValidateHeaderChain(headers[1:], headers[0].Hash, 0); err != nil {
		return 0, nil, err
	}
	work := new(big.Int)
	for _, h := range headers {
		work.Add(work, core.BlockWork(h.Difficulty))
	}
	return headers[len(headers)-1].Index, work, nil
}

// verifySPV performs a full light-client verification of a transaction: the
// header chain's proof-of-work, that the proof's merkle root matches the
// PoW-verified header, and that the merkle path folds to that root.
func verifySPV(headers []core.Header, pr core.TxProof, txHash string) (string, error) {
	tip, _, err := verifyHeaderChain(headers)
	if err != nil {
		return "", fmt.Errorf("header chain invalid: %w", err)
	}
	if !pr.Found {
		return "", fmt.Errorf("transaction not found in the chain")
	}
	if pr.BlockIndex >= uint64(len(headers)) {
		return "", fmt.Errorf("proof references a block beyond the verified chain")
	}
	hdr := headers[pr.BlockIndex]
	if pr.MerkleRoot != hdr.MerkleRoot {
		return "", fmt.Errorf("proof merkle root does not match the verified header")
	}
	if !core.VerifyMerkleProof(txHash, hdr.MerkleRoot, pr.Proof) {
		return "", fmt.Errorf("merkle proof does not fold to the header root")
	}
	confs := tip - pr.BlockIndex + 1
	return fmt.Sprintf("tx %s is in block %d with %d confirmation(s) (verified against a %d-header PoW chain)",
		short(txHash), pr.BlockIndex, confs, len(headers)), nil
}

// --- HTTP helpers -----------------------------------------------------------

var spvHTTP = &http.Client{Timeout: 8 * time.Second}

func ensureHTTP(addr string) string {
	if !strings.HasPrefix(addr, "http") {
		addr = "http://" + addr
	}
	return strings.TrimRight(addr, "/")
}

func fetchHeaders(base string) ([]core.Header, error) {
	var hs []core.Header
	return hs, getJSON(base+"/headers", &hs)
}

func fetchProof(base, txHash string) (core.TxProof, error) {
	var pr core.TxProof
	err := getJSON(base+"/proof/"+txHash, &pr)
	return pr, err
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}

func getJSON(url string, v any) error {
	resp, err := spvHTTP.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// /proof returns 404 for an unknown tx; decode still yields Found=false.
	} else if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
