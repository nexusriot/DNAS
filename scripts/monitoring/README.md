# Monitoring a DNAS node

Two files, meant to be used together:

- **`grafana-dashboard.json`** — 27 panels over the node's `/metrics`.
- **`prometheus-alerts.yml`** — 15 rules, grouped by what has gone wrong.

A dashboard with fifty series and no alerting is a dashboard nobody opens. The
rules are the half that wakes someone up, and they exist because this project
kept finding failure modes that every other signal a node emits reports as
healthy.

## Scraping

```yaml
# prometheus.yml
scrape_configs:
  - job_name: dnas
    static_configs:
      - targets: ["127.0.0.1:8080"]
rule_files:
  - prometheus-alerts.yml
```

`/metrics` is a read endpoint and needs no token. It is also rate limited along
with everything else, so scrape it no faster than the API's budget allows
(`-apirate`, `-apiburst`).

## Importing the dashboard

Grafana → Dashboards → Import → upload `grafana-dashboard.json`, and pick your
Prometheus data source when prompted. The dashboard is filtered by an `instance`
variable, so one copy covers every node you scrape.

## The alerts worth reading twice

**`DnasReorgRefused`** is the reason this directory exists. When fork choice
prefers a chain and the finality guard refuses it, the node may have stopped
following the network's chain — permanently, because the same guard refuses the
same switch every time it is offered. Nothing else here would notice: a diverged
node has peers, a fresh tip, and by its own reckoning is not behind. If it fires,
compare the tip against another node.

**`DnasSupplyNotConserved`** should be impossible: `minted − burned ==
circulating` is enforced per block by consensus. If it fires, the bug is in the
node's accounting rather than in the chain.

**`DnasOutboundConcentrated`** is the one a peer count cannot tell you. Eight
connections into one operator's range is one connection as far as an eclipse is
concerned. Note the honest limit: grouping is by **/16, not by ASN**, so this
raises the cost of an eclipse rather than settling it.

**`DnasWebhookEventsDropped`** means events are gone, not delayed: the node's
webhook delivery is at-most-once behind a bounded queue, by design. A service
that must not miss a payment should reconcile against `/chain` or
`/address/{addr}/history` — or use `dnas invoice serve`, whose delivery is
at-least-once precisely because it keeps its state on disk.

## Keeping this honest

`api/monitoring_test.go` scrapes a live node and fails if either file names a
metric that is not exported. A renamed series would otherwise turn a panel into a
flat line and an alert into one that can never fire, both of which look exactly
like nothing being wrong.

For a one-shot check of a single node rather than continuous monitoring, use
[`dnas doctor`](../../cmd/README.md), which runs the same checks and says what to
do about each one.
