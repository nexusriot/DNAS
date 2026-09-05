package api_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/core"
)

// The bulk read endpoints must never serialize a whole chain into one response:
// that is a memory-amplification attack anyone can run with curl, and it is what
// made the light client re-download everything on every command.
func TestBulkReadsArePaged(t *testing.T) {
	srv, chain, w := testServer(t)
	for i := 0; i < 6; i++ {
		mineOnto(t, chain, w.Address(), nil)
	}
	// 8 blocks: genesis + the one testServer mines + 6 here.
	for _, path := range []string{"/chain", "/headers", "/cfilters", "/cfheaders"} {
		all := getArr(t, srv.URL+path)
		if len(all) != 8 {
			t.Fatalf("%s returned %d entries, want the whole 8-block chain by default", path, len(all))
		}
		page := getArr(t, srv.URL+path+"?from=3&limit=2")
		if len(page) != 2 {
			t.Fatalf("%s?from=3&limit=2 returned %d entries, want 2", path, len(page))
		}
		if empty := getArr(t, srv.URL+path+"?from=99"); len(empty) != 0 {
			t.Fatalf("%s past the tip returned %d entries, want none", path, len(empty))
		}
		// A malformed page request is refused rather than silently serving page one.
		for _, bad := range []string{"?from=abc", "?limit=0", "?limit=xyz"} {
			resp, err := http.Get(srv.URL + path + bad)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s%s status = %d, want 400", path, bad, resp.StatusCode)
			}
		}
	}
}

// Paging must line up with the chain: page N of headers must be the headers at
// those heights, not an arbitrary window.
func TestHeaderPagesAreContiguous(t *testing.T) {
	srv, chain, w := testServer(t)
	for i := 0; i < 5; i++ {
		mineOnto(t, chain, w.Address(), nil)
	}
	page := getArr(t, srv.URL+"/headers?from=2&limit=3")
	if len(page) != 3 {
		t.Fatalf("page = %d entries, want 3", len(page))
	}
	for i, entry := range page {
		h := entry.(map[string]any)
		if got := uint64(h["index"].(float64)); got != uint64(2+i) {
			t.Fatalf("entry %d has index %d, want %d", i, got, 2+i)
		}
	}
}

func TestPeersEndpointReportsDetail(t *testing.T) {
	srv, _, _ := testServer(t)
	// No peers in a unit-test node, but the shape must be an array rather than
	// null so a client can iterate it unconditionally.
	if peers := getArr(t, srv.URL+"/peers"); len(peers) != 0 {
		t.Fatalf("expected no peers, got %d", len(peers))
	}
}

func TestBansEndpointAndUnban(t *testing.T) {
	srv, _, _ := testServer(t)
	bans := getObj(t, srv.URL+"/bans")
	if bans["threshold"].(float64) == 0 {
		t.Fatal("/bans does not report the threshold, so a score has no context")
	}
	// Unbanning a key that was never scored must fail loudly.
	if _, code := postObj(t, srv.URL+"/unban", map[string]string{"key": "never-seen"}); code != http.StatusBadRequest {
		t.Fatalf("unban of an unknown key = %d, want 400", code)
	}
}

func TestAddAndDropPeerEndpoints(t *testing.T) {
	srv, _, _ := testServer(t)
	if _, code := postObj(t, srv.URL+"/addpeer", map[string]string{"addr": ""}); code != http.StatusBadRequest {
		t.Fatalf("addpeer with no address = %d, want 400", code)
	}
	if _, code := postObj(t, srv.URL+"/droppeer", map[string]string{"peer": "nobody"}); code != http.StatusBadRequest {
		t.Fatalf("droppeer of an unknown peer = %d, want 400", code)
	}
	// A real dial is accepted even though nothing is listening: the node starts a
	// dial loop and reports that, rather than blocking on a connection.
	if _, code := postObj(t, srv.URL+"/addpeer", map[string]string{"addr": "127.0.0.1:1"}); code != http.StatusOK {
		t.Fatalf("addpeer of a plausible address = %d, want 200", code)
	}
}

func TestChainStatsEndpoint(t *testing.T) {
	srv, chain, w := testServer(t)
	for i := 0; i < 4; i++ {
		mineOnto(t, chain, w.Address(), nil)
	}
	st := getObj(t, srv.URL+"/chainstats")
	if st["window"].(float64) < 2 {
		t.Fatalf("window = %v, want the whole short chain", st["window"])
	}
	if st["target_interval"].(float64) != float64(core.TargetBlockTime) {
		t.Fatalf("target_interval = %v, want %d", st["target_interval"], core.TargetBlockTime)
	}
	miners := st["miners"].([]any)
	if len(miners) != 1 || miners[0].(map[string]any)["address"] != w.Address() {
		t.Fatalf("miners = %v, want one row for the only miner", miners)
	}
	if windowed := getObj(t, srv.URL+"/chainstats?window=2"); windowed["window"].(float64) != 2 {
		t.Fatalf("explicit window = %v, want 2", windowed["window"])
	}
	// A window of one block cannot yield an interval, so it is refused.
	resp, err := http.Get(srv.URL + "/chainstats?window=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("window=1 status = %d, want 400", resp.StatusCode)
	}
}

func TestReorgsEndpoint(t *testing.T) {
	srv, _, _ := testServer(t)
	rep := getObj(t, srv.URL+"/reorgs")
	if rep["total"].(float64) != 0 {
		t.Fatalf("a fresh node reports %v reorgs", rep["total"])
	}
	if rep["max_depth"].(float64) != float64(core.MaxReorgDepth) {
		t.Fatalf("max_depth = %v, want %d", rep["max_depth"], core.MaxReorgDepth)
	}
	if _, ok := rep["orphans"]; !ok {
		t.Fatal("the report should include the orphan-pool depth")
	}
}

// /health must answer 503 when a node is not usable, which is the whole reason
// it exists separately from /info — /info always answers 200.
func TestHealthDistinguishesReadyFromNot(t *testing.T) {
	srv, _, _ := testServer(t)
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// A unit-test node has no peers, so it is deliberately NOT ready.
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for a node with no peers", resp.StatusCode)
	}
	body := getObj(t, srv.URL+"/health")
	if body["ok"] != false {
		t.Fatalf("ok = %v, want false", body["ok"])
	}
	reasons, _ := body["reasons"].([]any)
	if len(reasons) == 0 {
		t.Fatal("an unhealthy node must say why")
	}
	for _, key := range []string{"network", "height", "tip_age", "peers", "blocks_behind", "mempool"} {
		if _, ok := body[key]; !ok {
			t.Errorf("/health is missing %q", key)
		}
	}
}

// `?last=N` exists because paging the bulk reads quietly changed what an
// unparameterized request means. Every client that draws a chain view wants the
// TAIL — the newest blocks — and each of them was fetching the whole chain and
// slicing. Once /chain became paged, "no parameters" started meaning "the OLDEST
// page", so the explorer, the TUI and the GUI would all have shown genesis
// forever on a long chain. Asking for the tail must be one request that does not
// depend on knowing the height first.
func TestLastParamReturnsTheTail(t *testing.T) {
	srv, chain, w := testServer(t)
	for i := 0; i < 6; i++ {
		mineOnto(t, chain, w.Address(), nil)
	}
	tipHeight := chain.Height() // genesis + 1 (testServer) + 6

	for _, path := range []string{"/chain", "/headers", "/cfilters", "/cfheaders"} {
		tail := getArr(t, srv.URL+path+"?last=3")
		if len(tail) != 3 {
			t.Fatalf("%s?last=3 returned %d entries, want 3", path, len(tail))
		}
		// It must be the tail, not the head: the last entry is the tip.
		if idx, ok := entryHeight(tail[len(tail)-1]); ok && idx != tipHeight {
			t.Fatalf("%s?last=3 ends at height %d, want the tip %d", path, idx, tipHeight)
		}
		if idx, ok := entryHeight(tail[0]); ok && idx != tipHeight-2 {
			t.Fatalf("%s?last=3 starts at height %d, want %d", path, idx, tipHeight-2)
		}

		// Asking for more than there is clamps at genesis rather than under-running it.
		all := getArr(t, srv.URL+path+"?last=99999")
		if uint64(len(all)) != tipHeight+1 {
			t.Fatalf("%s?last=99999 returned %d entries, want the whole %d-block chain",
				path, len(all), tipHeight+1)
		}
		// `from` and `last` are two different questions; answering one while the
		// caller asked both would silently ignore half the request.
		for _, bad := range []string{"?last=0", "?last=-1", "?last=abc", "?from=2&last=2"} {
			resp, err := http.Get(srv.URL + path + bad)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s%s status = %d, want 400", path, bad, resp.StatusCode)
			}
		}
	}
}

// entryHeight reads the height out of a chain/header entry, where it is present.
// The filter endpoints return entries keyed differently, so the bool reports
// whether this entry carries a height at all.
func entryHeight(entry any) (uint64, bool) {
	obj, ok := entry.(map[string]any)
	if !ok {
		return 0, false
	}
	for _, key := range []string{"index", "height"} {
		if v, ok := obj[key].(float64); ok {
			return uint64(v), true
		}
	}
	return 0, false
}

// A webhook that is silently failing is invisible from outside the node: the
// receiver sees nothing, and a quiet chain also produces nothing.
func TestWebhooksEndpointReportsDeliveryState(t *testing.T) {
	srv, _, _ := testServer(t)
	rep := getObj(t, srv.URL+"/webhooks")
	if rep["enabled"] != false {
		t.Fatalf("enabled = %v on a node with no webhooks", rep["enabled"])
	}
	for _, key := range []string{"urls", "sent", "failed", "dropped", "queued"} {
		if _, ok := rep[key]; !ok {
			t.Errorf("/webhooks is missing %q", key)
		}
	}
	// /info advertises the feature, so a client can tell whether to expect calls.
	info := getObj(t, srv.URL+"/info")
	if _, ok := info["webhooks"]; !ok {
		t.Error("/info does not report whether webhooks are configured")
	}
}

// A pruning node has to be honest about what it cannot serve: a client told
// "not found" for a pruned body would conclude the chain is shorter than it is,
// and one served an EMPTY filter would conclude its address is provably absent.
func TestPrunedNodeDistinguishesGoneFromMissing(t *testing.T) {
	srv, chain, w := testServer(t)
	for i := 0; i < core.MinPruneKeep+5; i++ {
		mineOnto(t, chain, w.Address(), nil)
	}
	chain.EnablePruning(core.MinPruneKeep)
	if chain.PrunedCount() == 0 {
		t.Fatal("nothing was pruned")
	}
	low := uint64(1)
	if chain.HasBody(low) {
		t.Fatal("height 1 was not pruned")
	}

	// 410 Gone, not 404: the height exists and this node cannot serve it.
	if code := statusOf(t, srv.URL+fmt.Sprintf("/block/%d", low)); code != http.StatusGone {
		t.Fatalf("a pruned body returned %d, want 410", code)
	}
	if code := statusOf(t, srv.URL+fmt.Sprintf("/cfilter/%d", low)); code != http.StatusGone {
		t.Fatalf("a pruned block's filter returned %d, want 410", code)
	}
	// A height that never existed is still a 404.
	if code := statusOf(t, srv.URL+"/block/999999"); code != http.StatusNotFound {
		t.Fatalf("a nonexistent height returned %d, want 404", code)
	}
	// A kept body is served as normal.
	if code := statusOf(t, srv.URL+fmt.Sprintf("/block/%d", chain.Height())); code != http.StatusOK {
		t.Fatalf("the tip body returned %d", code)
	}
	// The paged filter list must not include pruned heights at all.
	for _, entry := range getArr(t, srv.URL+"/cfilters?from=0&limit=2000") {
		f := entry.(map[string]any)
		if h := uint64(f["index"].(float64)); !chain.HasBody(h) {
			t.Fatalf("/cfilters served a filter for pruned height %d", h)
		}
	}

	// And /info says where its data starts, so a client can ask elsewhere.
	info := getObj(t, srv.URL+"/info")
	if info["pruned"] != true {
		t.Fatalf("pruned = %v on a pruning node", info["pruned"])
	}
	if info["body_height"].(float64) <= 1 {
		t.Fatalf("body_height = %v on a pruning node", info["body_height"])
	}
	if info["prune_keep"].(float64) != float64(core.MinPruneKeep) {
		t.Fatalf("prune_keep = %v", info["prune_keep"])
	}
	// A node that followed the chain from genesis still serves the whole
	// filter-header chain, because it is cached rather than recomputed.
	if info["filter_base"].(float64) != 0 {
		t.Fatalf("filter_base = %v on a node that pruned but never fast-synced", info["filter_base"])
	}
	if code := statusOf(t, srv.URL+"/cfheaders?from=0&limit=5"); code != http.StatusOK {
		t.Fatalf("cfheaders from genesis returned %d on a pruning node", code)
	}
}

// The explorer is a static page, so what a test can check is that it actually
// reads the surfaces the node grew — and that every endpoint it fetches exists.
// A panel wired to a URL that 404s renders as silence, which is the failure mode
// this catches.
func TestExplorerReadsTheEndpointsItReferences(t *testing.T) {
	srv, chain, _ := testServer(t)
	mature(t, chain)
	page := getPage(t, srv.URL+"/")

	// The panels that had no representation on the page at all.
	for _, want := range []string{
		`id="search"`,   // one box for a height, a hash or an address
		`id="assets"`,   // issued assets were invisible
		`id="nodeinfo"`, // health, hashrate, reorgs, what this node can serve
		"doSearch",
		"renderAssets",
		"renderNodeInfo",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the explorer is missing %q", want)
		}
	}
	// The transaction line must be able to describe every form the ledger allows,
	// or it will render a multi-output payment as a transfer of zero to nobody.
	for _, want := range []string{"tx.outputs", "tx.issue", "tx.asset_id", "tx.memo",
		"tx.lock_until", "tx.multisig", "tx.htlc", "tx.vault", "tx.fee_payer"} {
		if !strings.Contains(page, want) {
			t.Errorf("the transaction line ignores %s", want)
		}
	}
	// A memo is arbitrary data chosen by a stranger, so it must be escaped.
	if !strings.Contains(page, "esc(tx.memo)") {
		t.Error("the explorer renders a memo without escaping it")
	}

	// Every endpoint the page fetches must be a route that EXISTS. The status
	// itself varies legitimately — /health answers 503 on a peerless node, /send
	// is a POST, and the paths fetched with an argument appended answer 400 or 404
	// when asked bare — so what is checked is that nothing 404s in the way an
	// unrouted path would: with the catch-all's "not found" for a page.
	paths := explorerEndpoints(page)
	if len(paths) < 6 {
		t.Fatalf("only found %d fetched endpoints in the page: %v", len(paths), paths)
	}
	for _, path := range paths {
		code := statusOf(t, srv.URL+path)
		switch code {
		case http.StatusOK, http.StatusBadRequest, http.StatusServiceUnavailable,
			http.StatusMethodNotAllowed, http.StatusGone:
			// A routed endpoint answering on its own terms.
		case http.StatusNotFound:
			// Only acceptable for a prefix route fetched without its argument.
			if !strings.HasSuffix(path, "/") {
				t.Errorf("the explorer fetches %s, which is not a route", path)
			}
		default:
			t.Errorf("the explorer fetches %s, which answers %d", path, code)
		}
	}
}

// explorerEndpoints extracts the API paths the page fetches through j("…").
func explorerEndpoints(page string) []string {
	var out []string
	for _, part := range strings.Split(page, `j("`)[1:] {
		end := strings.Index(part, `"`)
		if end <= 0 {
			continue
		}
		path := part[:end]
		if strings.HasPrefix(path, "/") {
			out = append(out, path)
		}
	}
	return out
}

// getPage fetches a whole text response.
func getPage(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
