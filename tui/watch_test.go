package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMempoolStatsClient(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/mempool/stats", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"count":3,"bytes":600,"min_rate":10,"max_rate":90,"median_rate":40,"base_fee":10,
			"buckets":[{"from_rate":10,"to_rate":24,"count":1,"bytes":200},{"from_rate":25,"to_rate":0,"count":2,"bytes":400}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st, err := NewClient(srv.URL).MempoolStats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Count != 3 || st.MedianRate != 40 || st.Bytes != 600 {
		t.Fatalf("stats = %+v", st)
	}
	if len(st.Buckets) != 2 || st.Buckets[1].Count != 2 {
		t.Fatalf("buckets = %+v", st.Buckets)
	}
}

// A transaction the node has never seen is a STATUS, not an error: a wallet
// polling right after submitting will see exactly that for a moment, and an
// error would look like the node is broken.
func TestTxStatusTreatsUnknownAsAStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/tx/pending1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"pending","hash":"pending1","confirmations":0}`)
	})
	mux.HandleFunc("/tx/mined1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"confirmed","hash":"mined1","height":12,"confirmations":3}`)
	})
	mux.HandleFunc("/tx/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClient(srv.URL)

	pending, err := c.TxStatus("pending1")
	if err != nil || pending.Status != "pending" {
		t.Fatalf("pending = %+v, err %v", pending, err)
	}
	mined, err := c.TxStatus("mined1")
	if err != nil || mined.Status != "confirmed" || mined.Confirmations != 3 || mined.Height != 12 {
		t.Fatalf("confirmed = %+v, err %v", mined, err)
	}
	unknown, err := c.TxStatus("nosuchtx")
	if err != nil {
		t.Fatalf("an unknown transaction should not be an error: %v", err)
	}
	if unknown.Status != "unknown" {
		t.Fatalf("unknown status = %q, want %q", unknown.Status, "unknown")
	}
}

func TestBarScalesWithinItsWidth(t *testing.T) {
	if got := bar(0, 10, 20); got != "" {
		t.Fatalf("an empty bucket should render nothing, got %q", got)
	}
	if got := bar(10, 10, 20); len([]rune(got)) != 20 {
		t.Fatalf("a full bucket = %d chars, want 20", len([]rune(got)))
	}
	// A bucket too small to round up to one character still shows: an invisible
	// non-empty bucket reads as an empty one.
	if got := bar(1, 1000, 20); got == "" {
		t.Fatal("a tiny but non-empty bucket rendered nothing")
	}
	if got := bar(5, 10, 20); len([]rune(got)) != 10 {
		t.Fatalf("a half bucket = %d chars, want 10", len([]rune(got)))
	}
}

// The watch panel must say something useful in each of a transaction's states.
func TestWatchPanelRendersEachState(t *testing.T) {
	base := model{c: NewClient("localhost:0")}
	for _, tc := range []struct {
		state watchState
		want  string
	}{
		{watchState{hash: "abc123", status: "confirmed", height: 9, confs: 2}, "block 9"},
		{watchState{hash: "abc123", status: "pending"}, "mempool"},
		{watchState{hash: "abc123", status: "unknown"}, "never seen"},
		{watchState{hash: "abc123", status: "error: boom"}, "boom"},
	} {
		m := base
		st := tc.state
		m.watch = &st
		out := m.View()
		if !strings.Contains(out, "watching") {
			t.Fatalf("status %q: the watch panel is missing", tc.state.status)
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("status %q: view does not mention %q", tc.state.status, tc.want)
		}
	}
	// With nothing being watched the panel is absent entirely.
	if strings.Contains(base.View(), "watching") {
		t.Error("the watch panel is shown when nothing is being watched")
	}
}
