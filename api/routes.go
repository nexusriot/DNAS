package api

import (
	"net/http"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/node"
)

// The API's route table.
//
// Registration used to be forty-odd mux.HandleFunc lines with a trailing comment
// each. That is readable and it is not a description a machine can act on, so
// every client in this repo — the CLI, the TUI, the PyQt app, the explorer —
// hand-rolls its own idea of what the endpoints are and what they return, and
// nothing catches the day one of them drifts.
//
// This table is the single description. It registers the handlers AND generates
// the OpenAPI document (openapi.go), so the two cannot disagree: a route added
// without an entry here is not served, and an entry whose response type changes
// changes the published schema in the same commit. A test walks the table and
// fails on anything undocumented (routes_test.go).

// param is one path or query parameter.
type param struct {
	Name     string
	In       string // "path" or "query"
	Desc     string
	Required bool
	Type     string // JSON Schema primitive: string | integer | boolean
}

// route is one endpoint: how it is served, and what it is.
type route struct {
	// Pattern is the http.ServeMux pattern. A trailing slash makes it a prefix
	// match, which is how this API expresses path parameters.
	Pattern string
	// Path is the OpenAPI path, with parameters named: "/tx/{hash}".
	Path string
	// Methods a client may use. The handlers themselves are lenient about this;
	// the spec is not, because a generated client should not offer a POST to a
	// read endpoint.
	Methods []string
	Summary string
	Desc    string
	// Auth marks an endpoint that requires the bearer token when one is set.
	Auth   bool
	Params []param
	// Request and Response are zero values of the types carried in each
	// direction; their JSON Schema is derived from the Go structs by reflection,
	// so it cannot drift from what the handler actually encodes. A nil Response
	// needs ContentType set, for the endpoints that do not answer with JSON.
	Request  any
	Response any
	// ContentType overrides application/json for the non-JSON endpoints (the
	// metrics scrape, the SSE stream, the explorer page).
	ContentType string
	handler     http.HandlerFunc
}

// get/post are small constructors so the table below reads as a list of
// endpoints rather than a list of struct literals.
func get(pattern, summary string) route {
	return route{Pattern: pattern, Path: pattern, Methods: []string{"GET"}, Summary: summary}
}

func post(pattern, summary string) route {
	return route{Pattern: pattern, Path: pattern, Methods: []string{"POST"}, Summary: summary, Auth: true}
}

func (r route) at(path string, params ...param) route {
	r.Path, r.Params = path, params
	return r
}

func (r route) query(params ...param) route {
	r.Params = append(r.Params, params...)
	return r
}

func (r route) in(v any) route  { r.Request = v; return r }
func (r route) out(v any) route { r.Response = v; return r }

func (r route) raw(contentType string) route {
	r.ContentType = contentType
	return r
}

func (r route) describe(d string) route {
	r.Desc = d
	return r
}

// open marks a POST endpoint that does NOT require the token.
func (r route) open() route {
	r.Auth = false
	return r
}

func (r route) does(h http.HandlerFunc) route {
	r.handler = h
	return r
}

func pathParam(name, typ, desc string) param {
	return param{Name: name, In: "path", Type: typ, Desc: desc, Required: true}
}

func queryParam(name, typ, desc string) param {
	return param{Name: name, In: "query", Type: typ, Desc: desc}
}

// routes is every endpoint this API serves.
func (s *Server) routes() []route {
	return []route{
		get("/info", "Node status: chain tip, difficulty, peers, and what this node can serve").
			out(InfoResponse{}).
			does(s.info),
		get("/health", "Readiness check for a supervisor; 503 when a reason is listed").
			describe("Fails on a stale tip, no peers, being behind, or a REFUSED REORG — " +
				"the last of which nothing else here would notice, because a diverged node " +
				"has peers, a fresh tip, and by its own reckoning is not behind.").
			out(HealthResponse{}).
			does(s.health),
		get("/chain", "Blocks, paged").
			query(queryParam("from", "integer", "first height to return"),
				queryParam("last", "integer", "return the newest N instead of paging from a height"),
				queryParam("limit", "integer", "how many blocks (bounded server-side)")).
			out([]core.Block{}).
			does(s.chain),
		get("/balance/", "Coin balance of one address").
			at("/balance/{address}", pathParam("address", "string", "the address to look up")).
			out(BalanceResponse{}).
			does(s.balance),
		get("/account/", "Full account: coin balance, nonce and asset holdings").
			at("/account/{address}", pathParam("address", "string", "the address to look up")).
			out(core.Account{}).
			does(s.account),
		get("/mempool", "Every pending transaction").
			out([]core.Transaction{}).
			does(s.mempool),
		get("/mempool/stats", "Fee-rate distribution of the pending queue").
			describe("A single pending count cannot tell a sender whether their fee will be " +
				"picked next block or sit behind a wall of higher bidders; a distribution can.").
			out(MempoolStatsResponse{}).
			does(s.mempoolStats),
		get("/peers", "Connected peers in detail: identity, version, caps, ban score").
			out([]node.PeerInfo{}).
			does(s.peers),
		get("/bans", "Scored and banned peer keys").
			out(BansResponse{}).
			does(s.bans),
		post("/unban", "Clear a peer's ban score").
			in(unbanRequest{}).out(UnbanResponse{}).
			does(s.guard(s.unban)),
		post("/addpeer", "Dial a peer now").
			in(addPeerRequest{}).out(AddPeerResponse{}).
			does(s.guard(s.addPeer)),
		post("/droppeer", "Close a peer connection").
			in(dropPeerRequest{}).out(DropPeerResponse{}).
			does(s.guard(s.dropPeer)),
		get("/series", "Per-height difficulty, interval, base fee and fee flow — for charts").
			describe("ChainStats summarizes a window into single numbers, which cannot show a "+
				"quantity MOVING: a median interval of 60s is the same number whether every "+
				"block took 60s or half took 5s and half took 115s.").
			query(queryParam("from", "integer", "first height"),
				queryParam("last", "integer", "return the newest N instead"),
				queryParam("limit", "integer", "how many points (bounded server-side)")).
			out([]core.ChainPoint{}).
			does(s.series),
		get("/chainstats", "Hashrate, block intervals and fee flow over a window").
			query(queryParam("window", "integer", "how many recent blocks to summarize")).
			out(core.ChainStats{}).
			does(s.chainStats),
		get("/reorgs", "Reorgs this node has lived through, and the ones it REFUSED").
			out(node.ReorgReport{}).
			does(s.reorgs),
		get("/richlist", "The largest coin holders, ranked").
			describe("Needs no index: the answer is already in the account state the chain " +
				"keeps resident. Bounded to a top-N so one request cannot sort a whole ledger.").
			query(queryParam("limit", "integer", "how many holders to return")).
			out(core.RichList{}).
			does(s.richList),
		get("/supply", "Coin supply: minted, burned, circulating, and whether they agree").
			out(core.Supply{}).
			does(s.supply),
		get("/deployments", "BIP9 miner votes and where the chain has taken each").
			out([]core.DeploymentStatus{}).
			does(s.deployments),
		get("/pool", "PPLNS payout accounting: who has a claim on the next block found").
			out(node.PoolReport{}).
			does(s.pool),
		get("/shares", "The share ledger: what each miner has submitted").
			out(node.ShareReport{}).
			does(s.shares),
		get("/assets", "Every issued native asset").
			query(queryParam("ticker", "string", "return only assets with this ticker")).
			out([]core.AssetInfo{}).
			does(s.assets),
		get("/asset/", "One asset and who holds it").
			at("/asset/{id}", pathParam("id", "string", "the asset id")).
			out(AssetResponse{}).
			does(s.asset),
		get("/address", "This node's own wallet address").
			out(AddressResponse{}).
			does(s.address),
		get("/address/", "Every transaction that touched an address (needs -addrindex)").
			at("/address/{address}/history", pathParam("address", "string", "the address to look up")).
			query(queryParam("from", "integer", "first height to return"),
				queryParam("limit", "integer", "how many entries")).
			out(AddressHistoryResponse{}).
			does(s.addressHistory),
		post("/tx", "Submit a fully signed transaction").
			in(core.Transaction{}).out(TxSubmitResponse{}).
			does(s.guard(s.submitTx)),
		get("/tx/", "One transaction, confirmed or pending").
			at("/tx/{hash}", pathParam("hash", "string", "the transaction id")).
			out(core.Transaction{}).
			does(s.txByHash),
		post("/send", "Build, sign and broadcast a payment from the node's wallet").
			in(sendRequest{}).out(SendResponse{}).
			does(s.guard(s.send)),
		post("/mine", "Turn the built-in miner on or off").
			in(mineRequest{}).out(MineResponse{}).
			does(s.guard(s.mine)),
		post("/generate", "Mine N blocks on demand (regtest only)").
			in(generateRequest{}).out(GenerateResponse{}).
			does(s.guard(s.generate)),
		get("/blocktemplate", "A candidate block for an external miner").
			query(queryParam("address", "string", "address the coinbase should pay"),
				queryParam("longpoll", "string", "tip hash to wait for a change from")).
			out(blockTemplateResponse{}).
			does(s.blockTemplate),
		post("/submitblock", "Submit a mined block").
			in(core.Block{}).out(SubmitBlockResponse{}).
			does(s.guard(s.submitBlock)),
		post("/submitshare", "Submit a candidate that met the easier SHARE target").
			in(core.Block{}).out(node.ShareResult{}).
			does(s.guard(s.submitShare)),
		post("/faucet", "Give coin away (testnet/regtest only)").
			in(faucetRequest{}).out(FaucetResponse{}).
			does(s.guard(s.faucet)),
		get("/estimatefee", "Recommended fee PER BYTE for confirmation within N blocks").
			query(queryParam("blocks", "integer", "target confirmation depth")).
			out(EstimateFeeResponse{}).
			does(s.estimateFee),
		get("/webhooks", "Delivery counters for the configured webhooks").
			out(WebhooksResponse{}).
			does(s.webhooks),
		post("/multisig/address", "Derive an M-of-N multisig address (stateless)").
			open().in(multisigRequest{}).out(MultisigAddressResponse{}).
			does(s.multisigAddress),
		post("/htlc/address", "Derive a hash-time-locked contract address (stateless)").
			open().in(htlcRequest{}).out(HTLCAddressResponse{}).
			does(s.htlcAddress),
		post("/vault/address", "Derive a time-delayed vault address (stateless)").
			open().in(vaultRequest{}).out(VaultAddressResponse{}).
			does(s.vaultAddress),
		post("/wallet/hd", "Derive HD addresses from a mnemonic (stateless)").
			open().in(walletHDRequest{}).out(WalletHDResponse{}).
			does(s.walletHD),
		get("/headers", "Block headers, paged — all a light client needs").
			query(queryParam("from", "integer", "first height"),
				queryParam("last", "integer", "return the newest N instead"),
				queryParam("limit", "integer", "how many (bounded server-side)")).
			out([]core.Header{}).
			does(s.headers),
		get("/header/", "One block header by height").
			at("/header/{height}", pathParam("height", "integer", "block height")).
			out(core.Header{}).
			does(s.header),
		get("/block/", "One full block body by height").
			at("/block/{height}", pathParam("height", "integer", "block height")).
			out(core.Block{}).
			does(s.block),
		get("/snapshot/", "The whole account state at a height, for fast sync").
			at("/snapshot/{height}", pathParam("height", "integer", "block height")).
			out(core.Snapshot{}).
			does(s.snapshot),
		get("/proof/", "Merkle inclusion proof for a transaction").
			at("/proof/{hash}", pathParam("hash", "string", "the transaction id")).
			out(core.TxProof{}).
			does(s.proof),
		get("/stateproof/", "Balance and nonce proof against the header's state root").
			describe("Answers for an address it has never seen: the state root is a TRIE root, "+
				"so arriving at an empty slot is itself the proof that nothing is there.").
			at("/stateproof/{address}", pathParam("address", "string", "the address to prove")).
			out(core.AccountProof{}).
			does(s.stateProof),
		get("/cfilters", "Compact block filters, paged").
			query(queryParam("from", "integer", "first height"),
				queryParam("last", "integer", "return the newest N instead"),
				queryParam("limit", "integer", "how many (bounded server-side)")).
			out([]core.BlockFilter{}).
			does(s.cfilters),
		get("/cfilter/", "One compact block filter by height").
			at("/cfilter/{height}", pathParam("height", "integer", "block height")).
			out(core.BlockFilter{}).
			does(s.cfilter),
		get("/cfheaders", "The filter-header chain, paged").
			query(queryParam("from", "integer", "first height"),
				queryParam("last", "integer", "return the newest N instead"),
				queryParam("limit", "integer", "how many (bounded server-side)")).
			out([]string{}).
			does(s.cfheaders),
		get("/openapi.json", "This API's OpenAPI 3.1 document").
			describe("Generated from the same route table that registers the handlers, so it " +
				"cannot describe an endpoint that is not served or miss one that is.").
			raw("application/json").
			does(s.openAPI),
		get("/metrics", "Prometheus exposition of the node's metrics").
			raw("text/plain; version=0.0.4").
			does(s.metrics),
		get("/events", "Server-Sent Events: a live block and transaction stream").
			raw("text/event-stream").
			does(s.events),
		get("/", "The web block explorer").
			raw("text/html").
			does(s.explorer),
	}
}
