package api_test

import (
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// /metrics used to export only the handful of gauges that were cheap to reach
// from the tip. Everything else the node counts — reorgs, orphans, ban scores,
// block timing, hashrate, supply, webhook deliveries — existed only as JSON on
// five other endpoints, which is the wrong shape for a monitoring system.

func fetchMetrics(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// metricValues parses the exposition format into name -> value, and fails on any
// line that is not a comment, a blank, or a well-formed sample. Prometheus
// silently drops a malformed scrape, so a metric that never parses is worse than
// one that is missing: it looks present and is not.
func metricValues(t *testing.T, body string) map[string]float64 {
	t.Helper()
	sample := regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_]*) (.+)$`)
	out := map[string]float64{}
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := sample.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("unparseable metrics line: %q", line)
		}
		v, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			t.Fatalf("metric %s has non-numeric value %q", m[1], m[2])
		}
		out[m[1]] = v
	}
	return out
}

func TestMetricsExposesNodeCounters(t *testing.T) {
	srv, _, _ := testServer(t)
	body := fetchMetrics(t, srv.URL)
	vals := metricValues(t, body)

	// The series that previously existed only as JSON elsewhere.
	for _, name := range []string{
		"dnas_reorgs_total",
		"dnas_reorg_deepest",
		"dnas_orphan_blocks",
		"dnas_peers_scored",
		"dnas_peers_banned",
		"dnas_peer_worst_ban_score",
		"dnas_ban_threshold",
		"dnas_hashrate",
		"dnas_block_interval_mean",
		"dnas_block_interval_median",
		"dnas_block_interval_target",
		"dnas_window_fees",
		"dnas_window_burned",
		"dnas_supply_minted",
		"dnas_supply_burned",
		"dnas_supply_circulating",
		"dnas_tip_age_seconds",
		"dnas_blocks_behind",
		"dnas_mempool_bytes",
		"dnas_mempool_max_bytes",
		"dnas_webhook_sent",
		"dnas_webhook_failed",
		"dnas_webhook_dropped",
		"dnas_webhook_queued",
		"dnas_addrs_new",
		"dnas_addrs_tried",
		"dnas_addr_groups",
		"dnas_outbound_groups",
		"dnas_outbound_dialed",
		"dnas_reorgs_refused_total",
		"dnas_reorg_refused_deepest",
		"dnas_store_bytes",
		"dnas_store_bytes_saved",
	} {
		if _, ok := vals[name]; !ok {
			t.Errorf("metric %s is missing from /metrics", name)
		}
	}

	// And the ones that were already there must not have been lost.
	for _, name := range []string{
		"dnas_height", "dnas_difficulty", "dnas_mempool_size",
		"dnas_min_relay_fee", "dnas_base_fee", "dnas_peers", "dnas_mining",
		"dnas_shares_submitted", "dnas_share_difficulty",
	} {
		if _, ok := vals[name]; !ok {
			t.Errorf("pre-existing metric %s disappeared from /metrics", name)
		}
	}
}

// TestMetricsDeclaresEveryMetric guards the exposition format itself: every
// sample needs its HELP and TYPE lines, and a scraper rejects a duplicate name.
func TestMetricsDeclaresEveryMetric(t *testing.T) {
	srv, _, _ := testServer(t)
	body := fetchMetrics(t, srv.URL)

	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := strings.Fields(line)[0]
		if seen[name] {
			t.Errorf("metric %s is exported twice", name)
		}
		seen[name] = true
		if !strings.Contains(body, "# HELP "+name+" ") {
			t.Errorf("metric %s has no HELP line", name)
		}
		if !strings.Contains(body, "# TYPE "+name+" ") {
			t.Errorf("metric %s has no TYPE line", name)
		}
	}
	if len(seen) < 30 {
		t.Errorf("only %d metrics exported; expected the full set", len(seen))
	}
}

// TestMetricsReflectChainState checks the values are actually read from the node
// rather than hard-coded zeros — the failure mode a presence-only test misses.
func TestMetricsReflectChainState(t *testing.T) {
	srv, chain, w := fundedServer(t)

	before := metricValues(t, fetchMetrics(t, srv.URL))
	mineOnto(t, chain, w.Address(), nil)
	after := metricValues(t, fetchMetrics(t, srv.URL))

	if after["dnas_height"] != before["dnas_height"]+1 {
		t.Errorf("height %v -> %v after mining one block", before["dnas_height"], after["dnas_height"])
	}
	if after["dnas_supply_minted"] <= before["dnas_supply_minted"] {
		t.Errorf("minted supply did not grow with a new coinbase: %v -> %v",
			before["dnas_supply_minted"], after["dnas_supply_minted"])
	}
	// A chain with blocks has a real circulating supply and a real ban threshold.
	if after["dnas_supply_circulating"] <= 0 {
		t.Error("circulating supply should be positive on a mined chain")
	}
	if after["dnas_ban_threshold"] <= 0 {
		t.Error("ban threshold should be exported as its real value, not zero")
	}
}

// /reorgs must expose the refusal counters, not only the adopted ones. Whether a
// refusal actually flips /health is asserted in the node package
// (TestRefusedReorgIsCountedAndReported), which can drive a refusal directly;
// from out here the interesting thing is that the fields exist and are wired to
// the same report rather than defaulting silently.
func TestReorgsEndpointExposesRefusalCounters(t *testing.T) {
	srv, _, _ := testServer(t)
	rep := getObj(t, srv.URL+"/reorgs")
	for _, field := range []string{"refused", "refused_deepest", "total", "deepest", "max_depth"} {
		if _, ok := rep[field]; !ok {
			t.Errorf("/reorgs is missing %q", field)
		}
	}
	// A fresh node has refused nothing, and must say so as a number rather than
	// omitting the field — a missing counter reads as "no data", not "zero".
	if got, ok := rep["refused"].(float64); !ok || got != 0 {
		t.Errorf("refused = %v on a fresh node, want 0", rep["refused"])
	}
}
