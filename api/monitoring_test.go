package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The shipped dashboard and alert rules are only worth anything if every metric
// they name is a metric the node actually exports. A renamed series turns a
// dashboard panel into a flat line and an alert into one that can never fire —
// both of which look exactly like "nothing is wrong".
//
// So these tests read the files in scripts/monitoring/ and check them against a
// live /metrics scrape.

const (
	dashboardPath = "../scripts/monitoring/grafana-dashboard.json"
	alertsPath    = "../scripts/monitoring/prometheus-alerts.yml"
)

var metricName = regexp.MustCompile(`dnas_[a-z0-9_]+`)

// exportedMetrics scrapes a node and returns the set of series it emits.
func exportedMetrics(t *testing.T) map[string]bool {
	t.Helper()
	srv, chain, w := testServer(t)
	// A block with a payment in it, so the fee and supply series are non-trivial.
	mineOnto(t, chain, w.Address(), nil)

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, _, _ := strings.Cut(line, " ")
		name, _, _ = strings.Cut(name, "{") // a labelled series
		out[name] = true
	}
	if len(out) < 20 {
		t.Fatalf("only %d series scraped; the export looks broken", len(out))
	}
	return out
}

// namesIn pulls every dnas_* token out of a file.
func namesIn(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	seen := map[string]bool{}
	for _, m := range metricName.FindAllString(string(data), -1) {
		seen[m] = true
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func TestDashboardOnlyNamesMetricsTheNodeExports(t *testing.T) {
	exported := exportedMetrics(t)
	names := namesIn(t, dashboardPath)
	if len(names) < 20 {
		t.Fatalf("the dashboard names only %d metrics; is it still populated?", len(names))
	}
	for _, name := range names {
		if !exported[name] {
			t.Errorf("dashboard charts %s, which /metrics does not export", name)
		}
	}
}

func TestAlertRulesOnlyNameMetricsTheNodeExports(t *testing.T) {
	exported := exportedMetrics(t)
	names := namesIn(t, alertsPath)
	if len(names) < 10 {
		t.Fatalf("the alert rules name only %d metrics; are they still there?", len(names))
	}
	for _, name := range names {
		if !exported[name] {
			t.Errorf("an alert rule fires on %s, which /metrics does not export", name)
		}
	}
}

// The findings this project learned the hard way must each have an alert. It is
// deliberately a list of NAMES rather than a count: adding an alert should not
// make this pass, and deleting one of these should not.
func TestTheAlertsThatMatterExist(t *testing.T) {
	data, err := os.ReadFile(alertsPath)
	if err != nil {
		t.Fatal(err)
	}
	rules := string(data)
	for _, alert := range []string{
		"DnasReorgRefused",       // a node that has silently stopped converging
		"DnasSupplyNotConserved", // coin appearing or vanishing
		"DnasNoPeers",
		"DnasTipStale",
		"DnasOutboundConcentrated", // the /16-not-ASN eclipse caveat
		"DnasWebhookEventsDropped", // at-most-once delivery losing a payment
		"DnasCompactBlocksMissing", // compact relay costing rather than saving
	} {
		if !strings.Contains(rules, "alert: "+alert) {
			t.Errorf("no alert named %s", alert)
		}
	}
}

func TestDashboardIsValidAndDescribesItself(t *testing.T) {
	data, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatal(err)
	}
	var dash struct {
		Title  string `json:"title"`
		UID    string `json:"uid"`
		Panels []struct {
			Title   string `json:"title"`
			Type    string `json:"type"`
			Desc    string `json:"description"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(data, &dash); err != nil {
		t.Fatalf("the dashboard is not valid JSON: %v", err)
	}
	if dash.Title == "" || dash.UID == "" {
		t.Error("dashboard has no title or uid, so Grafana cannot import it cleanly")
	}
	charts := 0
	for _, p := range dash.Panels {
		if p.Type == "row" {
			continue
		}
		charts++
		if p.Title == "" {
			t.Error("a panel has no title")
		}
		if len(p.Targets) == 0 {
			t.Errorf("panel %q queries nothing", p.Title)
		}
		for _, tg := range p.Targets {
			if strings.TrimSpace(tg.Expr) == "" {
				t.Errorf("panel %q has an empty query", p.Title)
			}
		}
		// A panel whose meaning needs explaining is most of them here: the numbers
		// are only useful with the reason they matter attached.
		if p.Desc == "" {
			t.Errorf("panel %q has no description", p.Title)
		}
	}
	if charts < 15 {
		t.Errorf("dashboard has only %d panels", charts)
	}
}
