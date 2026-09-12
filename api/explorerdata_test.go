package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/nexusriot/DNAS/core"
)

// getJSON fetches a URL and decodes it into v.
func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
}

// The explorer's charts are fed by /series, so the endpoint has to answer with
// the NEWEST points for ?last= — paging from height zero would leave the charts
// showing the distant past on any chain long enough to matter.
func TestSeriesEndpointServesTheNewestPoints(t *testing.T) {
	srv, chain, w := testServer(t)
	for i := 0; i < 5; i++ {
		mineOnto(t, chain, w.Address(), nil)
	}

	var pts []core.ChainPoint
	getJSON(t, srv.URL+"/series?last=3", &pts)
	if len(pts) != 3 {
		t.Fatalf("got %d points, want 3", len(pts))
	}
	if last := pts[len(pts)-1].Height; last != chain.Height() {
		t.Errorf("newest point is height %d, tip is %d", last, chain.Height())
	}
	for i := 1; i < len(pts); i++ {
		if pts[i].Height != pts[i-1].Height+1 {
			t.Errorf("points are not consecutive: %d then %d", pts[i-1].Height, pts[i].Height)
		}
	}
	if pts[0].Difficulty <= 0 {
		t.Errorf("difficulty = %v, want a positive value", pts[0].Difficulty)
	}
}

func TestRichListEndpoint(t *testing.T) {
	srv, chain, w := testServer(t)
	mineOnto(t, chain, w.Address(), nil)

	var rl core.RichList
	getJSON(t, srv.URL+"/richlist", &rl)
	if rl.Shown == 0 {
		t.Fatal("rich list is empty on a chain with a funded miner")
	}
	if rl.Entries[0].Address != w.Address() {
		t.Errorf("top holder is %s, want the miner %s", rl.Entries[0].Address, w.Address())
	}
	if rl.Circulating != chain.Supply().Circulating {
		t.Errorf("circulating = %d, want %d", rl.Circulating, chain.Supply().Circulating)
	}

	var limited core.RichList
	getJSON(t, srv.URL+"/richlist?limit=1", &limited)
	if limited.Shown != 1 {
		t.Errorf("limit=1 returned %d entries", limited.Shown)
	}

	// A limit that is not a positive integer is a client mistake, answered as one.
	resp, err := http.Get(srv.URL + "/richlist?limit=nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("limit=nope answered %d, want 400", resp.StatusCode)
	}
}
