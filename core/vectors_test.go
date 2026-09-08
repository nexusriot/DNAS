package core

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// Golden consensus vectors.
//
// The canonical codec makes an implementation-independent spec *possible*; it
// does not make one *true*. Until a second client exists, the only thing that
// can catch "the spec drifted" is a fixed corpus of inputs with their expected
// hashes, checked on every run. That is this file, and the artifact it writes
// (testdata/consensus_vectors.json) is deliberately language-neutral so a client
// written in anything can consume it — see testdata/README.md.
//
// Two directions, one corpus:
//
//   - Regenerate:  go test ./core -run TestConsensusVectors -update
//   - Verify:      go test ./core -run TestConsensusVectors
//
// A verify failure means a consensus-visible value changed. That is sometimes
// intended (a deliberate fork) and sometimes a bug; either way it must be a
// decision, not a surprise, which is exactly what a golden file forces.

var updateVectors = flag.Bool("update", false, "rewrite testdata/consensus_vectors.json from the current code")

const vectorsPath = "testdata/consensus_vectors.json"

// vectorMnemonic seeds every key in the corpus. Fixed, so the whole file is
// reproducible: Ed25519 signing is deterministic (RFC 8032), so the same key
// over the same message yields the same signature on any implementation.
const vectorMnemonic = "pony excite absent genuine fish ice catalog side energy useful fiscal hip"

// ---------------------------------------------------------------------------
// The document
// ---------------------------------------------------------------------------

type vectorFile struct {
	Format      int                 `json:"format"`
	Description string              `json:"description"`
	Mnemonic    string              `json:"mnemonic"`
	Params      map[string]any      `json:"params"`
	Amounts     []amountVector      `json:"amounts"`
	Addresses   []addressVector     `json:"addresses"`
	Scripts     []scriptVector      `json:"script_addresses"`
	AssetIDs    []assetIDVector     `json:"asset_ids"`
	Rewards     []rewardVector      `json:"block_rewards"`
	Targets     []targetVector      `json:"targets"`
	Merkle      []merkleVector      `json:"merkle_roots"`
	StateRoots  []stateRootVector   `json:"state_roots"`
	Headers     []headerVector      `json:"headers"`
	Txs         []txVector          `json:"transactions"`
	FeeSplits   []feeSplitVector    `json:"fee_splits"`
	Filters     []blockFilterVector `json:"block_filters"`
}

type amountVector struct {
	Text  string `json:"text"`
	Units uint64 `json:"units"`
	// Formatted is what FormatAmount renders Units as; it is not always Text
	// (Text may be a shorthand like "2.5" that formats back as "2.50000000").
	Formatted string `json:"formatted"`
}

type addressVector struct {
	Index     uint32 `json:"hd_index"`
	PubKeyHex string `json:"pubkey_hex"`
	Address   string `json:"address"`
}

type scriptVector struct {
	Kind    string   `json:"kind"` // multisig | htlc | vault
	Address string   `json:"address"`
	Args    []string `json:"args"`
}

type assetIDVector struct {
	Issuer  string `json:"issuer"`
	Ticker  string `json:"ticker"`
	Nonce   uint64 `json:"nonce"`
	AssetID string `json:"asset_id"`
}

type rewardVector struct {
	Height uint64 `json:"height"`
	Reward uint64 `json:"reward"`
}

type targetVector struct {
	Bits       uint32  `json:"bits"`
	TargetHex  string  `json:"target_hex"`
	Difficulty float64 `json:"difficulty"`
	Roundtrip  uint32  `json:"roundtrip_bits"`
}

type merkleVector struct {
	Leaves []string `json:"leaves"`
	Root   string   `json:"root"`
}

type stateRootVector struct {
	Name     string             `json:"name"`
	Accounts map[string]Account `json:"accounts"`
	Root     string             `json:"root"`
}

type headerVector struct {
	Name     string `json:"name"`
	Header   Header `json:"header"`
	Preimage string `json:"preimage"`
	Hash     string `json:"hash"`
}

type txVector struct {
	Name    string      `json:"name"`
	Network string      `json:"network"`
	Tx      Transaction `json:"tx"`
	// SigningPreimageHex is what the SENDER signs; CanonicalHex is the full
	// encoding the txid is taken over. They differ by the authorization fields.
	SigningPreimageHex string `json:"signing_preimage_hex"`
	CanonicalHex       string `json:"canonical_hex"`
	TxID               string `json:"txid"`
	Size               int    `json:"size"`
	VerifyOps          int    `json:"verify_ops"`
	// Sanity is "" when CheckTxSanity accepts the transaction, else its error.
	Sanity string `json:"sanity"`
}

type feeSplitVector struct {
	BaseFee      uint64 `json:"base_fee"`
	TxSize       int    `json:"tx_size"`
	Fee          uint64 `json:"fee"`
	BurnedPart   uint64 `json:"burned"`
	MinerTip     uint64 `json:"tip"`
	BelowMinimum bool   `json:"below_minimum"`
}

type blockFilterVector struct {
	Name    string   `json:"name"`
	Entries []string `json:"entries"`
	Matches []string `json:"matches_probed"`
}

// ---------------------------------------------------------------------------
// Building the corpus
// ---------------------------------------------------------------------------

func vectorWallets(t *testing.T) []*wallet.Wallet {
	t.Helper()
	hd, err := wallet.HDFromMnemonic(vectorMnemonic, "")
	if err != nil {
		t.Fatalf("derive HD wallet from the fixed mnemonic: %v", err)
	}
	ws := make([]*wallet.Wallet, 4)
	for i := range ws {
		ws[i] = hd.Derive(uint32(i))
	}
	return ws
}

// buildVectors derives the whole corpus from the current code. Anything it
// computes is, by construction, what this build believes consensus to be.
func buildVectors(t *testing.T) vectorFile {
	t.Helper()
	ws := vectorWallets(t)
	alice, bob, carol := ws[0], ws[1], ws[2]

	v := vectorFile{
		Format: 1,
		Description: "DNAS golden consensus vectors. Every value here is derived from the " +
			"canonical encoding; an implementation that reproduces all of them agrees " +
			"with this one on transaction ids, block hashes, state roots and validity. " +
			"See README.md in this directory.",
		Mnemonic: vectorMnemonic,
		Params: map[string]any{
			"ticker":                          Ticker,
			"coin":                            Coin,
			"initial_block_reward":            InitialBlockReward,
			"halving_interval":                HalvingInterval,
			"target_block_time":               TargetBlockTime,
			"coinbase_maturity":               CoinbaseMaturity,
			"max_reorg_depth":                 MaxReorgDepth,
			"max_block_txs":                   MaxBlockTxs,
			"max_block_bytes":                 MaxBlockBytes,
			"max_block_verify_ops":            MaxBlockVerifyOps,
			"max_tx_outputs":                  MaxTxOutputs,
			"max_memo_bytes":                  MaxMemoBytes,
			"max_address_bytes":               MaxAddressBytes,
			"max_coinbase_bytes":              MaxCoinbaseBytes,
			"max_multisig_keys":               wallet.MaxMultisigKeys,
			"max_ticker_len":                  MaxTickerLen,
			"max_asset_supply":                uint64(MaxAssetSupply),
			"dust_threshold":                  DustThreshold,
			"max_future_drift":                MaxFutureDrift,
			"initial_base_fee":                InitialBaseFee,
			"min_base_fee":                    MinBaseFee,
			"base_fee_target_txs":             BaseFeeTargetTxs,
			"base_fee_max_change_denominator": BaseFeeMaxChangeDenominator,
			"genesis_timestamp":               GenesisTimestamp,
			"genesis_bits":                    GenesisBits,
			"pow_limit_bits":                  PowLimitBits,
			"tx_codec_version":                txCodecVersion,
			"coinbase_sender":                 CoinbaseSender,
			"empty_state_root":                stateRoot(map[string]Account{}),
			"genesis_hash_mainnet":            GenesisBlock().Hash,
		},
	}

	// Amounts: the decimal <-> base-unit conversion, including the truncation
	// rule for more than eight fractional digits.
	for _, tc := range []struct {
		text  string
		units uint64
	}{
		{"0", 0},
		{"1", Coin},
		{"1.5", 150_000_000},
		{"2.50000000", 250_000_000},
		{"0.00000001", 1},
		{"0.000000019", 1}, // 9th digit truncated, not rounded
		{"21000000", 21_000_000 * Coin},
		{"50", InitialBlockReward},
	} {
		got, err := ParseAmount(tc.text)
		if err != nil {
			t.Fatalf("ParseAmount(%q): %v", tc.text, err)
		}
		v.Amounts = append(v.Amounts, amountVector{Text: tc.text, Units: got, Formatted: FormatAmount(got)})
	}

	// Addresses: the pubkey -> address derivation, which is what a wrong
	// checksum implementation would silently get wrong.
	for i, w := range ws {
		v.Addresses = append(v.Addresses, addressVector{
			Index: uint32(i), PubKeyHex: w.PublicKeyHex(), Address: w.Address(),
		})
	}

	// Script addresses: multisig, HTLC and vault all hash their parameters into
	// an address, and each hash is domain-separated from the others.
	ms, err := wallet.MultisigAddress(2, []string{alice.PublicKeyHex(), bob.PublicKeyHex(), carol.PublicKeyHex()})
	if err != nil {
		t.Fatalf("multisig address: %v", err)
	}
	v.Scripts = append(v.Scripts, scriptVector{
		Kind: "multisig", Address: ms,
		Args: []string{"2", alice.PublicKeyHex(), bob.PublicKeyHex(), carol.PublicKeyHex()},
	})
	const preimageHash = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" // sha256("test")
	// HTLC and vault addresses fold PUBLIC KEYS, not addresses: the spend has to
	// verify a signature against them, which an address hash cannot do.
	htlc, err := wallet.HTLCAddress(preimageHash, bob.PublicKeyHex(), alice.PublicKeyHex(), 500)
	if err != nil {
		t.Fatalf("htlc address: %v", err)
	}
	v.Scripts = append(v.Scripts, scriptVector{
		Kind: "htlc", Address: htlc,
		Args: []string{preimageHash, bob.PublicKeyHex(), alice.PublicKeyHex(), "500"},
	})
	vault, err := wallet.VaultAddress(alice.PublicKeyHex(), bob.PublicKeyHex(), 1000)
	if err != nil {
		t.Fatalf("vault address: %v", err)
	}
	v.Scripts = append(v.Scripts, scriptVector{
		Kind: "vault", Address: vault,
		Args: []string{alice.PublicKeyHex(), bob.PublicKeyHex(), "1000"},
	})

	// Asset ids bind issuer + ticker + nonce, so two issuers can both mint "GOLD".
	for _, tc := range []struct {
		issuer string
		ticker string
		nonce  uint64
	}{
		{alice.Address(), "GOLD", 0},
		{alice.Address(), "GOLD", 1},
		{bob.Address(), "GOLD", 0},
	} {
		v.AssetIDs = append(v.AssetIDs, assetIDVector{
			Issuer: tc.issuer, Ticker: tc.ticker, Nonce: tc.nonce,
			AssetID: AssetID(tc.issuer, tc.ticker, tc.nonce),
		})
	}

	// The halving schedule, including the point where it reaches zero.
	for _, h := range []uint64{0, 1, HalvingInterval - 1, HalvingInterval, 2 * HalvingInterval,
		10 * HalvingInterval, 63 * HalvingInterval, 64 * HalvingInterval} {
		v.Rewards = append(v.Rewards, rewardVector{Height: h, Reward: BlockReward(h)})
	}

	// Compact target (nBits) encoding, the piece an independent miner must agree
	// on exactly or it mines against the wrong difficulty.
	for _, bits := range []uint32{GenesisBits, PowLimitBits, 0x1d00ffff, 0x1b0404cb} {
		target := CompactToBig(bits)
		v.Targets = append(v.Targets, targetVector{
			Bits:       bits,
			TargetHex:  target.Text(16),
			Difficulty: TargetDifficulty(bits),
			Roundtrip:  BigToCompact(target),
		})
	}

	// Merkle roots, including the odd-count case where the last node is
	// duplicated — the classic place two implementations disagree.
	for _, leaves := range [][]string{
		{},
		{"aa"},
		{"aa", "bb"},
		{"aa", "bb", "cc"},
		{"aa", "bb", "cc", "dd", "ee"},
	} {
		v.Merkle = append(v.Merkle, merkleVector{Leaves: leaves, Root: merkleRootOf(leaves)})
	}

	// State roots, including the empty sentinel and an account carrying assets.
	goldID := AssetID(alice.Address(), "GOLD", 0)
	for _, tc := range []struct {
		name  string
		state map[string]Account
	}{
		{"empty", map[string]Account{}},
		{"one-account", map[string]Account{alice.Address(): {Balance: 5 * Coin, Nonce: 1}}},
		{"two-accounts", map[string]Account{
			alice.Address(): {Balance: 5 * Coin, Nonce: 1},
			bob.Address():   {Balance: 7, Nonce: 0},
		}},
		{"with-assets", map[string]Account{
			alice.Address(): {Balance: Coin, Nonce: 2, Assets: map[string]uint64{goldID: 42}},
		}},
	} {
		v.StateRoots = append(v.StateRoots, stateRootVector{
			Name: tc.name, Accounts: tc.state, Root: stateRoot(tc.state),
		})
	}

	// Header hashing: the preimage string is part of consensus, so it is pinned
	// verbatim rather than only through its hash.
	genesis := GenesisBlock()
	hdrs := []struct {
		name string
		h    Header
	}{
		{"genesis", genesis.Header()},
		{"synthetic", Header{
			Index: 7, Timestamp: 1735689700, PrevHash: genesis.Hash,
			MerkleRoot: merkleRootOf([]string{"aa", "bb"}),
			StateRoot:  stateRoot(map[string]Account{alice.Address(): {Balance: Coin, Nonce: 1}}),
			BaseFee:    12, Bits: GenesisBits, Nonce: 99,
		}},
	}
	for _, tc := range hdrs {
		h := tc.h
		h.Hash = h.ComputeHash()
		v.Headers = append(v.Headers, headerVector{
			Name: tc.name, Header: h, Preimage: h.headerString(), Hash: h.Hash,
		})
	}

	// The fee split: how much of a fee is burned and how much the miner keeps.
	for _, tc := range []struct {
		baseFee uint64
		size    int
		fee     uint64
	}{
		{0, 100, 500},
		{10, 100, 500},
		{10, 100, 1000},
		{10, 100, 999}, // below base fee x size: invalid, contributes no tip
	} {
		tx := Transaction{From: alice.Address(), To: bob.Address(), Fee: tc.fee}
		burned := tc.baseFee * uint64(tc.size)
		var tip uint64
		if tc.fee > burned {
			tip = tc.fee - burned
		}
		_ = tx
		v.FeeSplits = append(v.FeeSplits, feeSplitVector{
			BaseFee: tc.baseFee, TxSize: tc.size, Fee: tc.fee,
			BurnedPart: burned, MinerTip: tip, BelowMinimum: tc.fee < burned,
		})
	}

	// Transactions across every optional block in the codec, on each network.
	// The network matters: its id goes into the signing preimage, so the same
	// transfer signed on testnet must NOT verify on mainnet.
	v.Txs = buildTxVectors(t, ws)

	// Compact block filters: what a light client matches against.
	v.Filters = buildFilterVectors(t, ws)

	return v
}

// txCase is one transaction shape, built from the fixed wallets.
type txCase struct {
	name  string
	build func(ws []*wallet.Wallet) Transaction
	// sign, when set, signs with this wallet index after the fields are set.
	sign int
}

func txVectorCases() []txCase {
	return []txCase{
		{name: "plain-transfer", sign: 0, build: func(ws []*wallet.Wallet) Transaction {
			return Transaction{From: ws[0].Address(), To: ws[1].Address(), Amount: 3 * Coin, Fee: 1000, Nonce: 0}
		}},
		{name: "with-memo-and-window", sign: 0, build: func(ws []*wallet.Wallet) Transaction {
			return Transaction{From: ws[0].Address(), To: ws[1].Address(), Amount: Coin, Fee: 2000,
				Nonce: 4, Expiry: 900, LockUntil: 800, Memo: "two coffees"}
		}},
		{name: "multi-output", sign: 0, build: func(ws []*wallet.Wallet) Transaction {
			return Transaction{From: ws[0].Address(), Fee: 1500, Nonce: 1, Outputs: []Output{
				{To: ws[1].Address(), Amount: Coin},
				{To: ws[2].Address(), Amount: 2 * Coin},
			}}
		}},
		{name: "asset-issue", sign: 0, build: func(ws []*wallet.Wallet) Transaction {
			return Transaction{From: ws[0].Address(), Fee: 3000, Nonce: 2,
				Issue: &AssetIssue{Ticker: "GOLD", Supply: 1_000_000}}
		}},
		{name: "asset-transfer", sign: 0, build: func(ws []*wallet.Wallet) Transaction {
			return Transaction{From: ws[0].Address(), To: ws[1].Address(), Amount: 25, Fee: 1200,
				Nonce: 3, AssetID: AssetID(ws[0].Address(), "GOLD", 2)}
		}},
		{name: "fee-sponsored", sign: 0, build: func(ws []*wallet.Wallet) Transaction {
			return Transaction{From: ws[0].Address(), To: ws[1].Address(), Amount: Coin, Fee: 900,
				Nonce: 5, FeePayer: ws[2].Address()}
		}},
		{name: "coinbase", sign: -1, build: func(ws []*wallet.Wallet) Transaction {
			return NewCoinbase(ws[0].Address(), InitialBlockReward)
		}},
		{name: "unsigned-plain", sign: -1, build: func(ws []*wallet.Wallet) Transaction {
			return Transaction{From: ws[0].Address(), To: ws[1].Address(), Amount: Coin, Fee: 10, Nonce: 0}
		}},
	}
}

func buildTxVectors(t *testing.T, ws []*wallet.Wallet) []txVector {
	t.Helper()
	var out []txVector
	for _, netName := range []string{MainNet, TestNet, RegTest} {
		prev := NetworkName()
		if err := SetNetwork(netName); err != nil {
			t.Fatalf("set network %s: %v", netName, err)
		}
		for _, c := range txVectorCases() {
			tx := c.build(ws)
			if c.sign >= 0 {
				if err := tx.Sign(ws[c.sign]); err != nil {
					t.Fatalf("%s/%s: sign: %v", netName, c.name, err)
				}
			}
			sanity := ""
			if err := CheckTxSanity(tx); err != nil {
				sanity = err.Error()
			}
			out = append(out, txVector{
				Name: c.name, Network: netName, Tx: tx,
				SigningPreimageHex: hex.EncodeToString(tx.canonicalSigningBytes()),
				CanonicalHex:       hex.EncodeToString(tx.canonicalBytes()),
				TxID:               tx.Hash(),
				Size:               tx.Size(),
				VerifyOps:          VerifyOps(tx),
				Sanity:             sanity,
			})
		}
		if err := SetNetwork(prev); err != nil {
			t.Fatalf("restore network %s: %v", prev, err)
		}
	}
	return out
}

func buildFilterVectors(t *testing.T, ws []*wallet.Wallet) []blockFilterVector {
	t.Helper()
	tx := Transaction{From: ws[0].Address(), To: ws[1].Address(), Amount: Coin, Fee: 100, Nonce: 0}
	if err := tx.Sign(ws[0]); err != nil {
		t.Fatalf("sign filter tx: %v", err)
	}
	blk := Block{Index: 1, Timestamp: GenesisTimestamp + 1, Transactions: []Transaction{
		NewCoinbase(ws[2].Address(), InitialBlockReward), tx,
	}}
	f := BuildBlockFilter(blk)
	var matched []string
	for _, probe := range []string{ws[0].Address(), ws[1].Address(), ws[2].Address(), ws[3].Address()} {
		if f.Match(probe) {
			matched = append(matched, probe)
		}
	}
	return []blockFilterVector{{
		Name:    "coinbase-plus-transfer",
		Entries: []string{ws[0].Address(), ws[1].Address(), ws[2].Address()},
		Matches: matched,
	}}
}

// ---------------------------------------------------------------------------
// Generate / verify
// ---------------------------------------------------------------------------

func TestConsensusVectors(t *testing.T) {
	built := buildVectors(t)

	if *updateVectors {
		data, err := json.MarshalIndent(built, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(vectorsPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(vectorsPath, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d transactions, %d state roots)", vectorsPath, len(built.Txs), len(built.StateRoots))
		return
	}

	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatalf("read %s (regenerate with `go test ./core -run TestConsensusVectors -update`): %v",
			vectorsPath, err)
	}
	var want vectorFile
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("parse %s: %v", vectorsPath, err)
	}

	// Compare field by field so a failure names WHAT diverged. A single
	// reflect.DeepEqual on the whole document would say only "they differ",
	// which for a consensus change is the least useful thing it could say.
	if want.Format != built.Format {
		t.Fatalf("vector format is %d, this build writes %d", want.Format, built.Format)
	}
	if want.Mnemonic != built.Mnemonic {
		t.Fatalf("the corpus mnemonic changed; every key in the file derives from it")
	}
	compareVectorMaps(t, "params", want.Params, built.Params)
	compareVectorSlices(t, "amounts", want.Amounts, built.Amounts, func(a amountVector) string { return a.Text })
	compareVectorSlices(t, "addresses", want.Addresses, built.Addresses, func(a addressVector) string { return a.PubKeyHex })
	compareVectorSlices(t, "script_addresses", want.Scripts, built.Scripts, func(s scriptVector) string { return s.Kind })
	compareVectorSlices(t, "asset_ids", want.AssetIDs, built.AssetIDs, func(a assetIDVector) string {
		return a.Issuer + "/" + a.Ticker
	})
	compareVectorSlices(t, "block_rewards", want.Rewards, built.Rewards, func(r rewardVector) string {
		return FormatAmount(r.Height)
	})
	compareVectorSlices(t, "targets", want.Targets, built.Targets, func(x targetVector) string { return x.TargetHex })
	compareVectorSlices(t, "merkle_roots", want.Merkle, built.Merkle, func(m merkleVector) string { return m.Root })
	compareVectorSlices(t, "state_roots", want.StateRoots, built.StateRoots, func(s stateRootVector) string { return s.Name })
	compareVectorSlices(t, "headers", want.Headers, built.Headers, func(h headerVector) string { return h.Name })
	compareVectorSlices(t, "transactions", want.Txs, built.Txs, func(x txVector) string {
		return x.Network + "/" + x.Name
	})
	compareVectorSlices(t, "fee_splits", want.FeeSplits, built.FeeSplits, func(f feeSplitVector) string {
		return FormatAmount(f.Fee)
	})
	compareVectorSlices(t, "block_filters", want.Filters, built.Filters, func(f blockFilterVector) string { return f.Name })
}

func compareVectorMaps(t *testing.T, section string, want, got map[string]any) {
	t.Helper()
	// Round-trip the built map through JSON so numeric types match what was read
	// back from the file (encoding/json decodes every number as float64).
	norm, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var gotNorm map[string]any
	if err := json.Unmarshal(norm, &gotNorm); err != nil {
		t.Fatal(err)
	}
	for k, w := range want {
		g, ok := gotNorm[k]
		if !ok {
			t.Errorf("%s: %q is in the vectors but this build no longer produces it", section, k)
			continue
		}
		if !reflect.DeepEqual(w, g) {
			t.Errorf("%s[%q]: vectors say %v, this build says %v", section, k, w, g)
		}
	}
	for k := range gotNorm {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: %q is new in this build and absent from the vectors (regenerate with -update)", section, k)
		}
	}
}

// maxReportedDivergences caps the noise. A consensus change usually moves every
// vector at once (a codec bump moves all 24 transactions), and printing each in
// full buries the one line that says WHICH field moved.
const maxReportedDivergences = 4

func compareVectorSlices[T any](t *testing.T, section string, want, got []T, label func(T) string) {
	t.Helper()
	if len(want) != len(got) {
		t.Errorf("%s: vectors hold %d entries, this build produces %d (regenerate with -update)",
			section, len(want), len(got))
	}
	n := len(want)
	if len(got) < n {
		n = len(got)
	}
	shown := 0
	diverged := 0
	for i := 0; i < n; i++ {
		if reflect.DeepEqual(want[i], got[i]) {
			continue
		}
		diverged++
		if shown >= maxReportedDivergences {
			continue
		}
		shown++
		t.Errorf("%s[%d] (%s) diverged:\n%s", section, i, label(want[i]), diffFields(want[i], got[i]))
	}
	if diverged > shown {
		t.Errorf("%s: %d more entries diverged (only the first %d are shown)",
			section, diverged-shown, shown)
	}
}

// diffFields reports only the struct fields that actually differ, so a failure
// says "txid moved" rather than reprinting two 900-character JSON objects.
func diffFields(want, got any) string {
	wv, gv := reflect.ValueOf(want), reflect.ValueOf(got)
	if wv.Kind() != reflect.Struct {
		return fmt.Sprintf("    vectors:    %s\n    this build: %s", brief(want), brief(got))
	}
	var b strings.Builder
	ty := wv.Type()
	for i := 0; i < wv.NumField(); i++ {
		if !ty.Field(i).IsExported() {
			continue
		}
		w, g := wv.Field(i).Interface(), gv.Field(i).Interface()
		if reflect.DeepEqual(w, g) {
			continue
		}
		fmt.Fprintf(&b, "    %s:\n      vectors:    %s\n      this build: %s\n",
			ty.Field(i).Name, brief(w), brief(g))
	}
	if b.Len() == 0 {
		return "    (no exported field differs — an unexported field or map ordering changed)"
	}
	return strings.TrimRight(b.String(), "\n")
}

// brief renders a value for a failure message, truncating the long hex strings
// that dominate this corpus down to something a person can compare by eye.
func brief(v any) string {
	s := fmt.Sprintf("%v", v)
	if b, err := json.Marshal(v); err == nil {
		s = string(b)
	}
	const max = 96
	if len(s) > max {
		return s[:max/2] + "…(" + fmt.Sprint(len(s)) + " chars)…" + s[len(s)-max/4:]
	}
	return s
}

// TestConsensusVectorsAreSelfConsistent re-derives the corpus from the FILE
// rather than from the builder, so the committed artifact is checked as a
// standalone document — which is how a second implementation will read it. A
// value that is internally inconsistent (a txid that is not the hash of the
// canonical bytes beside it) would otherwise be invisible.
func TestConsensusVectorsAreSelfConsistent(t *testing.T) {
	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Skipf("no vectors file yet: %v", err)
	}
	var v vectorFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse %s: %v", vectorsPath, err)
	}

	for _, tx := range v.Txs {
		canon, err := hex.DecodeString(tx.CanonicalHex)
		if err != nil {
			t.Errorf("%s/%s: canonical_hex is not hex: %v", tx.Network, tx.Name, err)
			continue
		}
		// The txid must be sha256 over exactly the bytes the file records.
		if got := hashBytes(canon); got != tx.TxID {
			t.Errorf("%s/%s: txid is %s but sha256(canonical_hex) is %s", tx.Network, tx.Name, tx.TxID, got)
		}
		if len(canon) != tx.Size {
			t.Errorf("%s/%s: size is %d but canonical_hex is %d bytes", tx.Network, tx.Name, tx.Size, len(canon))
		}
		// The signing preimage must be a prefix-compatible encoding of the same
		// transaction: it shares the version byte and the signed fields.
		sig, err := hex.DecodeString(tx.SigningPreimageHex)
		if err != nil {
			t.Errorf("%s/%s: signing_preimage_hex is not hex: %v", tx.Network, tx.Name, err)
			continue
		}
		if len(sig) == 0 || sig[0] != canon[0] {
			t.Errorf("%s/%s: signing preimage and canonical bytes disagree on the codec version",
				tx.Network, tx.Name)
		}
	}

	for _, h := range v.Headers {
		if got := hashBytes([]byte(h.Preimage)); got != h.Hash {
			t.Errorf("header %q: hash is %s but sha256(preimage) is %s", h.Name, h.Hash, got)
		}
	}
	for _, a := range v.Amounts {
		if got := FormatAmount(a.Units); got != a.Formatted {
			t.Errorf("amount %q: formatted is %q, recomputed %q", a.Text, a.Formatted, got)
		}
	}
	for _, a := range v.Addresses {
		if err := wallet.ValidateAddress(a.Address); err != nil {
			t.Errorf("address %s does not validate: %v", a.Address, err)
		}
		derived, err := wallet.AddressFromPubKeyHex(a.PubKeyHex)
		if err != nil {
			t.Errorf("pubkey %s: %v", a.PubKeyHex, err)
			continue
		}
		if derived != a.Address {
			t.Errorf("pubkey %s derives %s, file says %s", a.PubKeyHex, derived, a.Address)
		}
	}
	for _, s := range v.Scripts {
		if err := wallet.ValidateAddress(s.Address); err != nil {
			t.Errorf("%s address %s does not validate: %v", s.Kind, s.Address, err)
		}
	}
	for _, tv := range v.Targets {
		if tv.Roundtrip != tv.Bits {
			t.Errorf("target %#x does not round-trip through the compact encoding (got %#x)",
				tv.Bits, tv.Roundtrip)
		}
	}
}
