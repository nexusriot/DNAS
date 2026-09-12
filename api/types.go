package api

import (
	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/node"
)

// Named response types for every JSON endpoint.
//
// These used to be anonymous map[string]any literals built inside each handler.
// That is convenient to write and it means the response shape exists nowhere a
// tool can read it: the OpenAPI document would have nothing to describe but
// "object", and each of the five clients in this repo has had to guess the keys
// and types from the source. The structs below ARE the description — openapi.go
// derives the published schema from them by reflection, so a field renamed here
// is renamed in the spec in the same commit.
//
// The JSON tags reproduce the previous keys exactly, so the wire format is
// unchanged and every existing client keeps working.

// InfoResponse is GET /info: what this node is and what it can serve.
type InfoResponse struct {
	Network        string   `json:"network"`
	Height         uint64   `json:"height"`
	Tip            string   `json:"tip"`
	NextBits       uint32   `json:"next_bits"`
	NextDifficulty float64  `json:"next_difficulty"`
	Work           string   `json:"work"` // decimal string: cumulative work exceeds uint64
	Mempool        int      `json:"mempool"`
	MinRelayFee    uint64   `json:"min_relay_fee"`
	BaseFee        uint64   `json:"base_fee"`
	Peers          []string `json:"peers"`

	// Addrs is the address manager's tables, so an operator can see how
	// eclipse-resistant this node currently is rather than only how many peers
	// it has.
	Addrs        node.AddrStats `json:"addrs"`
	Mining       bool           `json:"mining"`
	AddressIndex bool           `json:"address_index"`
	Faucet       bool           `json:"faucet"`
	Webhooks     bool           `json:"webhooks"`

	// BodyHeight and FilterBase say what this node can actually serve, so a
	// client can tell "not in the chain" from "not visible from here".
	BodyHeight uint64 `json:"body_height"`
	FilterBase uint64 `json:"filter_base"`
	Pruned     bool   `json:"pruned"`

	Store        core.StoreStats `json:"store"`
	PruneKeep    uint64          `json:"prune_keep"`
	PrunedBodies uint64          `json:"pruned_bodies"`
}

// BalanceResponse is GET /balance/{address}.
type BalanceResponse struct {
	Address    string `json:"address"`
	Balance    uint64 `json:"balance"`
	BalanceFmt string `json:"balance_fmt"`
}

// MempoolStatsResponse is GET /mempool/stats: the fee-rate distribution of the
// pending queue.
type MempoolStatsResponse struct {
	Count      int              `json:"count"`
	Bytes      int              `json:"bytes"`
	MinRate    uint64           `json:"min_rate"`
	MaxRate    uint64           `json:"max_rate"`
	MedianRate uint64           `json:"median_rate"`
	BaseFee    uint64           `json:"base_fee"`
	Buckets    []core.FeeBucket `json:"buckets"`
}

// BansResponse is GET /bans.
type BansResponse struct {
	Threshold int             `json:"threshold"`
	Entries   []node.BanEntry `json:"entries"`
}

// UnbanResponse is POST /unban.
type UnbanResponse struct {
	Unbanned string `json:"unbanned"`
}

// AddPeerResponse is POST /addpeer.
type AddPeerResponse struct {
	Dialing string `json:"dialing"`
}

// DropPeerResponse is POST /droppeer.
type DropPeerResponse struct {
	Dropped     string `json:"dropped"`
	Connections int    `json:"connections"`
}

// HealthResponse is GET /health. A non-empty Reasons is why the check failed,
// and the HTTP status says so too (503), so a supervisor need not parse this.
type HealthResponse struct {
	OK           bool     `json:"ok"`
	Network      string   `json:"network"`
	Height       uint64   `json:"height"`
	TipAge       string   `json:"tip_age"`
	Peers        int      `json:"peers"`
	BlocksBehind uint64   `json:"blocks_behind"`
	Mempool      int      `json:"mempool"`
	Reasons      []string `json:"reasons"`
}

// AddressResponse is GET /address: this node's own wallet address.
type AddressResponse struct {
	Address string `json:"address"`
}

// AddressHistoryEntry is one transaction that touched an address, with the
// confirmation depth worked out against the tip the query saw.
type AddressHistoryEntry struct {
	Height        uint64           `json:"height"`
	Index         int              `json:"index"`
	Hash          string           `json:"hash"`
	Confirmations uint64           `json:"confirmations"`
	Tx            core.Transaction `json:"tx"`
}

// AddressHistoryResponse is GET /address/{address}/history.
type AddressHistoryResponse struct {
	Address string                `json:"address"`
	Total   int                   `json:"total"`
	From    uint64                `json:"from"`
	Count   int                   `json:"count"`
	Entries []AddressHistoryEntry `json:"entries"`
}

// TxSubmitResponse is POST /tx.
type TxSubmitResponse struct {
	Hash string `json:"hash"`
}

// AssetResponse is GET /asset/{id}: one asset and who holds it.
type AssetResponse struct {
	Asset   core.AssetInfo     `json:"asset"`
	Holders []core.AssetHolder `json:"holders"`
	Held    uint64             `json:"held"`
}

// SendResponse is POST /send.
type SendResponse struct {
	Hash  string `json:"hash"`
	Nonce uint64 `json:"nonce"`
}

// MineResponse is POST /mine.
type MineResponse struct {
	Mining bool `json:"mining"`
}

// GenerateResponse is POST /generate (regtest only).
type GenerateResponse struct {
	Mined  int      `json:"mined"`
	Hashes []string `json:"hashes"`
}

// SubmitBlockResponse is POST /submitblock.
type SubmitBlockResponse struct {
	Accepted bool   `json:"accepted"`
	Height   uint64 `json:"height"`
	Hash     string `json:"hash"`
}

// FaucetResponse is POST /faucet (testnet/regtest only).
type FaucetResponse struct {
	Hash      string `json:"hash"`
	To        string `json:"to"`
	Amount    uint64 `json:"amount"`
	AmountFmt string `json:"amount_fmt"`
	Cooldown  string `json:"cooldown"`
}

// EstimateFeeResponse is GET /estimatefee. Every figure is PER BYTE.
type EstimateFeeResponse struct {
	Blocks      int    `json:"blocks"`
	PerByte     bool   `json:"per_byte"`
	BaseFee     uint64 `json:"base_fee"`
	Tip         uint64 `json:"tip"`
	Fee         uint64 `json:"fee"`
	FeeFmt      string `json:"fee_fmt"`
	MinRelayFee uint64 `json:"min_relay_fee"`
}

// WebhooksResponse is GET /webhooks.
type WebhooksResponse struct {
	Enabled bool `json:"enabled"`
	URLs    int  `json:"urls"` // how many webhook URLs are configured
	Sent    int  `json:"sent"`
	Failed  int  `json:"failed"`
	Dropped int  `json:"dropped"` // events discarded because the queue was full
	Queued  int  `json:"queued"`
}

// MultisigAddressResponse is POST /multisig/address.
type MultisigAddressResponse struct {
	Threshold int    `json:"threshold"`
	N         int    `json:"n"`
	Address   string `json:"address"`
}

// HTLCAddressResponse is POST /htlc/address.
type HTLCAddressResponse struct {
	Address string `json:"address"`
	Timeout uint64 `json:"timeout"`
}

// VaultAddressResponse is POST /vault/address.
type VaultAddressResponse struct {
	Address string `json:"address"`
	Unlock  uint64 `json:"unlock"`
}

// WalletHDResponse is POST /wallet/hd.
type WalletHDResponse struct {
	Mnemonic  string   `json:"mnemonic"`
	Addresses []string `json:"addresses"`
}

// ErrorResponse is the body of every failure.
type ErrorResponse struct {
	Error string `json:"error"`
}

// Request bodies. These were anonymous structs declared inside each handler, for
// the same reason the responses were maps and with the same cost: a generated
// client had nothing to generate from. Naming them puts the accepted shape in
// the published spec.

// unbanRequest is POST /unban.
type unbanRequest struct {
	Key string `json:"key"`
}

// addPeerRequest is POST /addpeer.
type addPeerRequest struct {
	Addr string `json:"addr"`
}

// dropPeerRequest is POST /droppeer.
type dropPeerRequest struct {
	Peer string `json:"peer"`
}

// mineRequest is POST /mine.
type mineRequest struct {
	On bool `json:"on"`
}

// generateRequest is POST /generate.
type generateRequest struct {
	N int `json:"n"`
}

// faucetRequest is POST /faucet.
type faucetRequest struct {
	Address string `json:"address"`
}

// sendRequest is POST /send. Nonce is a pointer so that "not supplied" (pick the
// next one) is distinguishable from an explicit zero.
type sendRequest struct {
	To        string        `json:"to"`
	Amount    uint64        `json:"amount"`
	Outputs   []core.Output `json:"outputs"`
	Fee       uint64        `json:"fee"`
	Expiry    uint64        `json:"expiry"`
	LockUntil uint64        `json:"lock_until"`
	Memo      string        `json:"memo"`
	Nonce     *uint64       `json:"nonce"`
}

// multisigRequest is POST /multisig/address.
type multisigRequest struct {
	Threshold int      `json:"threshold"`
	PubKeys   []string `json:"pubkeys"`
}

// htlcRequest is POST /htlc/address.
type htlcRequest struct {
	Hash      string `json:"hash"`
	Recipient string `json:"recipient"`
	Sender    string `json:"sender"`
	Timeout   uint64 `json:"timeout"`
}

// vaultRequest is POST /vault/address.
type vaultRequest struct {
	Hot    string `json:"hot"`
	Cold   string `json:"cold"`
	Unlock uint64 `json:"unlock"`
}

// walletHDRequest is POST /wallet/hd.
type walletHDRequest struct {
	Mnemonic   string `json:"mnemonic"`
	Passphrase string `json:"passphrase"`
	Count      int    `json:"count"`
}
