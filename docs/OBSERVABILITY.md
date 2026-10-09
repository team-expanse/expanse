# Observability and alerting

How to point a real Prometheus and Grafana at an Expanse cluster, and how to read the in-cluster
UI's own health view when you have neither running. See `.plan/ARCHITECTURE.md` §9 (A37–A38) for
the design rationale; this document is the operator-facing companion.

## 1. The design, in one paragraph

Expanse does not write its own metrics TSDB or dashboarding tool — it adopts **Prometheus** (via
`prometheus/client_golang`, `.plan/ARCHITECTURE.md` A37) and **Grafana**, both brought up by the
operator, not bundled or auto-started. Every node serves a `/metrics` endpoint (mTLS + bearer
token, its own dedicated listener) exporting the same node/resource(including block)/volume/quorum
health every other surface already computes — no second health-detection mechanism anywhere in
this phase. Expanse ships a default alert-rule file and a default Grafana dashboard as the tested
path, not just documentation prose. Independent of any of that, every node's own web UI has a live
`/health` page showing the same signals and deriving the same critical alerts, so an operator with
no Prometheus/Grafana configured at all still sees degradation in real time.

## 2. Point a Prometheus at a node

Mint a bearer token and fetch the cluster CA once:

```sh
expanse ctl metrics set-token --token '<a-strong-random-token>'
cp /persist/expanse/ca/ca.pem /etc/prometheus/expanse-ca.pem
```

Scrape config:

```yaml
scrape_configs:
  - job_name: expanse
    scheme: https
    scrape_interval: 5s      # see the cadence note below -- do not use 10-15s
    tls_config:
      ca_file: /etc/prometheus/expanse-ca.pem
      server_name: <node-name>   # node certs carry DNS-name SANs, not IP SANs
    bearer_token_file: /etc/prometheus/expanse-metrics-token
    static_configs:
      - targets: ['<node-address>:7447']
```

Repeat per node (each node's `/metrics` reports its own local view; there is no cluster-wide
aggregation endpoint). A missing or wrong bearer token is rejected with 401; TLS is the cluster's
own CA, not self-signed.

**Scrape/evaluation cadence — use ≤5s, not the commonly-assumed 10–15s.** This phase's release
blocker (`ROADMAP.md`'s exit line: a node failure raises an alert and is visible within 30s,
`.plan/PHASE-09-TASKS.md` X5/D6) leaves less headroom than it first appears: `nodelc`'s own
leader-side failure detector (§4.8) only marks a silent node unreachable after its own ~15s
threshold, evaluated on a 5s tick, against a status write that is itself on a 10s period — so real
detection alone can take up to ~20s before Prometheus ever scrapes. A 10–15s scrape interval on top
of that risks the 30s budget; a real measured run at a 2s/2s cadence closed the whole pipeline
(alert firing → UI → Grafana) in 11.5–11.7s. ≤5s leaves comfortable margin without materially
increasing scrape load (Stream A's own measurement: ~3.6 KB per scrape response at this project's
scale, negligible against `ARCHITECTURE.md` §8's control-plane budget).

## 3. The shipped alert rules

Point Prometheus's `rule_files:` at `deploy/prometheus/expanse-alerts.rules.yml`, shipped and
tested unmodified (`promtool test rules`, plus a live nixosTest against real degraded fixtures,
`.plan/PHASE-09-TASKS.md` X2). Every rule's `severity` label tells you what it's for:

| Severity | Meaning | `for:` |
|---|---|---|
| `critical` | Fires immediately on the first true evaluation — no pending window. These seven rules are what the 30s alert-visibility budget is about (`ExpanseNodeUnhealthy`, `ExpanseResourceUnhealthy`, `ExpanseVolumeFailed`, `ExpanseVolumeReadOnly`, `ExpanseQuorumNoLeader`, `ExpanseQuorumDegraded`, `ExpanseNodeUnreachable`) |
| `warning` | Requires the condition to hold for a sustained window (2–5 min) before firing, to avoid flapping on a transient blip (`ExpanseNodeDegraded`, `ExpanseResourceDegraded`, `ExpanseVolumeDegraded`, `ExpanseQuorumNodeDegraded`) — deliberately outside the 30s budget |

`ExpanseNodeUnreachable` (added by Stream E, X5) deserves a specific note: it is the *only* rule
that fires when a single, otherwise-uninvolved node simply goes silent — a lone follower's death in
a healthy cluster loses neither quorum nor produces a self-report, so nothing else here would
notice it.

Alertmanager routing (email, webhook, pager) is intentionally **not** configured by Expanse — that
remains real, operator-specific configuration outside this project's scope (`.plan/PHASE-09-TASKS.md`
D4). Point your own Alertmanager's `rule_files`/receivers at the same rules.

## 4. The shipped Grafana dashboard

Point Grafana's provisioning at `deploy/grafana/provisioning/` (a datasource pointed at your
Prometheus, plus a file-backed dashboard provider) and drop
`deploy/grafana/dashboards/expanse-cluster-health.json` at the path
`provisioning/dashboards/dashboards.yaml` names. The shipped dashboard (`expanse-health`) has six
panels: Node Health, Resource Health, Volume Health, Quorum, Per-node degraded flag, and Per-node
unreachable flag (the last panel reflects `ExpanseNodeUnreachable`'s own signal, §3 above) — proven
provisioned and rendering real data through Grafana's own `/api/ds/query`, not just "the JSON
exists" (`.plan/PHASE-09-TASKS.md` X3).

## 5. Or run them as blocks

`monitor/prometheus` and `monitor/grafana` run the same pieces inside the cluster. The Prometheus
block scrapes every voter's `/metrics` (one job per node, `server_name` set to the node ID, the
cluster CA bundle handed over by the bridge and updated through a CA rotation) and node-exporter on
every node, and evaluates the shipped alert rules unmodified. The Grafana block provisions the
`expanse-prometheus` datasource and the shipped dashboard.

```yaml
apiVersion: expanse.io/v1
kind: Block
metadata: { name: prometheus, namespace: default }
spec:
  type: monitor/prometheus
  replicas: 1
  strategy: { kind: SINGLETON }
  storage:
    - { name: tsdb, size: 10Gi, replication: 3, mountPath: /var/lib/prometheus }
  config:
    metricsToken: <the token from `expanse ctl metrics set-token`>
    scrapeInterval: 5s        # the default; keep it at or below 5s (section 2)
    retention: 15d
    nodeExporterPort: 9100    # where your monitor/node-exporter daemonset listens
---
apiVersion: expanse.io/v1
kind: Block
metadata: { name: grafana, namespace: default }
spec:
  type: monitor/grafana
  replicas: 1
  strategy: { kind: SINGLETON }
  storage:
    - { name: data, size: 1Gi, replication: 3, mountPath: /var/lib/grafana }
  config:
    prometheusUrl: http://<prometheus block address>:9090
    adminPassword: <at least 8 characters>
```

Without `metricsToken` the Prometheus block scrapes only node-exporter and itself, since the
cluster stores the token only as a hash. The token sits in the block's config the same way
`db/postgres` passwords do. Give the Prometheus block a VIP (`expose: EXPOSE_VIP`) so Grafana's
`prometheusUrl` survives a failover. Grafana applies `adminPassword` only when it first creates
its database; change it later in Grafana itself. Neither block configures Alertmanager (section 3).
Like every binary-backed block, the workload execs the upstream binary from the system PATH, so
add `pkgs.prometheus` and `pkgs.grafana` to `environment.systemPackages` on nodes that may run them.
Grafana 13.2 no longer includes its Prometheus datasource, so also add Expanse's
`packages.grafana-plugins` (`nix/grafana-plugins.nix`, which ships `grafanaPlugins.prometheus`);
the Grafana block links it into Grafana's plugins on every start.

## 6. The in-cluster UI, with no Prometheus or Grafana at all

Every node's own web UI has a live `/health` page (reached the same way as `/cluster`, `/blocks`,
`/volumes`) showing node checks, resource/block health, volume health, quorum, and the same set of
currently-firing critical alerts by the same names as §3's table — pushed live over the UI's
existing SSE mechanism, refreshing within a few seconds of a real change. This reads the exact same
signals `/metrics` exports, through the same in-process calls, so it can never disagree with what
Prometheus would show — it is simply available with zero external tooling configured
(`.plan/PHASE-09-TASKS.md` X4).

## 7. What is not covered

Alertmanager notification routing (§3). TPM-backed credential storage and OIDC for the web UI are
Phase 10 scope, not this phase's. The `warning`-severity rules' pending windows are deliberately
untested against a real stopwatch — they exist specifically to avoid flapping over a period the
30s release-blocking budget does not apply to.
