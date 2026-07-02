# Operating Click-Dog

This is the **operating contract** for the public beta: what Click-Dog
guarantees, where it can lose data, what is actually tested, and how to size
and budget a deployment. It complements the
[Configuration Reference](configuration.md) (every knob) and the
[Install Guide](install.md) (how to roll out) by answering the questions an
operator asks before trusting a fleet: *what happens on restart, on a backend
outage, on a partial multi-sink failure — and how much will this cost?*

!!! info "Beta status"
    Click-Dog is best-effort, **at-least-once** span delivery. It is a
    monitoring sidecar, not a guaranteed-delivery pipeline. The sections below
    spell out the loss windows explicitly so you can decide whether that
    contract fits your use case.

## Delivery model and loss windows

### Deduplication is in-memory and not persisted

Click-Dog tracks already-exported spans in an in-memory LRU cache keyed by the
composite OTLP span identity `(trace_id, span_id)` — `span_id` is only unique
within a trace, so keying on it alone would silently drop sibling spans in
different traces that collide on the 64-bit ID. The cache size is
`monitor.dedup_cache_size` (**default 10,000**).

The cache is **not persisted across restarts** (`main.go`, the LRU setup in
`runScheduledMode`). On startup it is empty, so any span still inside the
lookback window is **re-exported once**. This is safe by design:

- OTEL collectors and trace backends can deduplicate by `(trace_id, span_id)`.
  Splunk HEC exports include those same stable IDs, but duplicate collapse
  there depends on downstream Splunk searches or index-time posture.
- The alternative — disk persistence — adds crash-recovery complexity not
  justified for a best-effort sidecar.

The same property makes HA mode safe: during a Keeper partition two instances
may briefly both act as leader and double-export; backends dedup it away.

### The lookback window defines the loss boundary

Each cycle queries spans from the last `lookback_s` seconds. If
`monitor.lookback_s` is unset it defaults to:

```
lookback_s = check_interval_s + lookback_buffer_s
```

with `check_interval_s` defaulting to **30** and `lookback_buffer_s` to **10**
(`internal/config/config.go`). The buffer is the overlap that absorbs clock
skew and a poll cycle that ran long, so spans landing right at a window edge
are not missed.

!!! warning "Backend or sidecar downtime longer than the lookback window is a permanent gap"
    Click-Dog only ever looks back `lookback_s` seconds. If the sidecar is
    down, or every export sink is unreachable, for **longer than the lookback
    window**, the spans that aged out of that window are **never re-queried** —
    they are permanently lost from Click-Dog's perspective (the rows usually
    still exist in ClickHouse, but Click-Dog will not go back for them in
    scheduled mode). To recover a known outage window, run a one-shot
    [backfill](modes.md) over the affected time range.

    Practical implication: set `lookback_s` to comfortably exceed your worst
    expected restart/redeploy gap. A rolling deploy that takes 90s per node
    while `lookback_s` is 40 will lose ~50s of spans on each node unless you
    raise it.

### Multi-sink failure semantics

When more than one exporter is configured, Click-Dog fans out to every sink
sequentially under the default **`PolicyAllRequired`** contract
(`internal/export/multi.go`; wired with no policy override in `main.go`):

- A span is marked "seen" in the dedup cache **only if every sink accepts it**
  (`internal/processor/live.go` marks only the accepted-by-all intersection).
- **Any** sink failure — partial or total — makes the cycle a real export
  error. That marks the cycle as an error (`click_dog_cycle_results_total{result="error"}`),
  feeds the **circuit breaker** and **adaptive backoff**, and surfaces as the
  last error on `/status` and `/readyz`.
- Because nothing from a failed batch is marked seen, the next cycle
  **re-delivers the whole batch to every sink** — including sinks that already
  succeeded. They see the same `(trace_id, span_id)` again; this is the
  intentional at-least-once trade-off, with stable IDs for downstream dedup.

Per-sink visibility comes from these counters (see [Observability](observability.md)):

| Metric | Meaning |
|--------|---------|
| `click_dog_export_attempts_total{sink}` | Items submitted to the sink, including retries |
| `click_dog_export_accepted_total{sink}` | Items the sink accepted |
| `click_dog_export_errors_total{sink}` | Export attempts that returned an error |

A dead sink therefore cannot silently drop out of the fan-out — its
`export_errors_total` climbs and `export_accepted_total` stalls.

!!! note "`PolicyAnySuccess` is not operator-configurable"
    An alternate `PolicyAnySuccess` policy exists in the code (mark a span seen
    as soon as *one* sink accepts it). It is **not exposed via YAML** today, so
    every production deployment runs the strict `PolicyAllRequired` default. Do
    not plan around any-success behavior during the beta.

For the configuration-side description of the same contract, see
[Multiple Export Backends](configuration.md#multiple-export-backends).

## Compatibility matrix

"Tested" means exercised in CI or the Docker-based integration suite. "Expected"
means designed and documented for, but not continuously tested — treat it as
likely-to-work, verify in your environment. "Unknown" means unverified or out
of scope; do not assume support.

| Component | Version / target | Status | Source of truth |
|-----------|------------------|--------|-----------------|
| ClickHouse | **24.1** (3-node cluster + embedded Keeper) | **Tested** | `testing/docker-compose.integration.yml` |
| ClickHouse | Other 23.x / 24.x / 25.x exposing `system.opentelemetry_span_log` with the expected schema | Expected | — |
| OTEL Collector (contrib) | **0.91.0** | **Tested** | `testing/docker-compose.integration.yml` |
| Go toolchain | **1.26.4** (build + unit tests) | **Tested** | `.github/workflows/ci.yml` |
| Go (compile floor) | `go` directive `1.25.0` | Build requirement | `go.mod` |
| OS / arch | **linux/amd64** (build + tests on `ubuntu-latest`) | **Tested** | `.github/workflows/ci.yml` |
| OS / arch | **linux/arm64** (cross-built and released; runtime logic identical, unit tests execute on amd64) | Built & released | `.goreleaser.yaml` |
| OS / arch | macOS, Windows | **Unknown / unsupported** — no release binaries are produced | `.goreleaser.yaml` (linux only) |
| Datadog Agent | **v7.35+** via OTLP gRPC | Expected | [Datadog integration](integrations/datadog.md) |
| Honeycomb | via OTEL Collector → `api.honeycomb.io:443` | Expected | [Honeycomb integration](integrations/honeycomb.md) |
| Generic OTLP backend | OTLP over **gRPC** | Expected | [Generic OTLP](integrations/generic-otlp.md) |
| Splunk | HEC endpoint | Expected | [Configuration](configuration.md#otel-exporter) |
| Kubernetes | Centralized Deployment (single replica, cluster() reads via the ClickHouse Service) | Expected — manifests shipped, no e2e in CI | `deploy/kubernetes/`, [Install · Kubernetes](install.md#kubernetes) |
| OTLP over HTTP | — | **Unknown / unsupported** — the OTLP exporter is gRPC-only | `internal/export/` |

## Performance tuning method

The knobs interact; tune them as a set, not in isolation.

| Knob | Default | What it controls |
|------|---------|------------------|
| `monitor.check_interval_s` | `30` | Poll period. **Must be > 0** when `monitor.enabled` (rejected at config-load otherwise — it drives the ticker). |
| `monitor.lookback_s` | `check_interval_s + lookback_buffer_s` | How far back each cycle queries. Defines the loss boundary (above). |
| `monitor.lookback_buffer_s` | `10` | Overlap added to the default lookback to absorb skew / long cycles. |
| `monitor.max_spans_per_cycle` | `1000` | SQL `LIMIT` on the spans query — caps **spans**, not traces, per cycle (post-#91). |
| `monitor.dedup_cache_size` | `10000` | LRU entries for dedup. ~80–100 B/entry; 10k ≈ 1 MiB (see [Monitor](configuration.md#monitor)). |
| `monitor.export_timeout_s` | `30` | Per-call deadline applied to **every** sink in the fan-out. `0` disables the deadline. |

### How they interact

- **Lookback ≥ interval, always.** The default guarantees this. If you set
  `lookback_s` manually, keep it ≥ `check_interval_s + a few seconds`, or a
  slow cycle leaves an uncovered gap between windows.
- **Cache must exceed spans-per-lookback-window.** The dedup cache has to hold
  every span key that can appear within one lookback window, or entries evict
  before the overlapping next cycle reads them and you re-export (harmless, but
  wasteful) — or worse, churn defeats dedup entirely. Rough floor:
  `dedup_cache_size ≳ peak_spans_per_cycle × (lookback_s / check_interval_s)`.
  With the defaults that is `1000 × (40/30) ≈ 1,333`, comfortably under 10,000.
- **`max_spans_per_cycle` is a backpressure valve, not a quota.** It bounds one
  cycle's fetch; spans beyond the limit are picked up on subsequent cycles via
  the lookback overlap + dedup cache, as long as your inflow rate stays below
  `max_spans_per_cycle / check_interval_s`. If inflow exceeds that sustained
  rate, the backlog grows and the oldest spans eventually age out of the
  lookback window and are lost — raise the limit or shorten the interval.

!!! note "`max_spans_per_cycle` ceiling is 100,000"
    The spans query would otherwise silently cap the per-cycle SQL `LIMIT` at
    **100,000** (`MaxSpanQueryLimit` in `internal/clickhouse/reader.go`), so
    config validation **rejects any larger value at load**
    (`internal/config/config.go`) with an explicit error — Click-Dog refuses to
    start on a setting that can't take effect rather than silently capping it.

- **`export_timeout_s` bounds tail latency, not the cycle.** A slow backend
  hits this deadline and the cycle records an export error → breaker/backoff.
  Set it below `check_interval_s` so a stuck sink can't stall polling.

A safe starting point for most fleets is the defaults. Increase
`check_interval_s` (and proportionally `lookback_s`) to reduce backend pressure;
raise `max_spans_per_cycle` only when you observe sustained per-cycle
saturation. See [Install · Scaling Considerations](install.md#scaling-considerations)
for collector-capacity sizing.

## Cost estimation

Most managed tracing backends bill on **volume**, so estimate spans/day before
you point a fleet at one. Per sidecar:

```
spans_per_day ≈ qualifying_traces_per_cycle
              × spans_per_trace
              × (86400 / check_interval_s)

# capped per cycle at min(max_spans_per_cycle, 100000)
```

Multiply by the number of sidecars. In the default **one-sidecar-per-node**
model that is `× N nodes`; in `use_cluster_queries` mode it is a **single**
instance (see below), so do **not** multiply by node count.

### Worked example

Assume per node: 50 qualifying traces per 30s cycle, ~8 spans each,
`check_interval_s = 30` (→ 2,880 cycles/day):

```
per node : 50 × 8 × 2880          = 1,152,000 spans/day   (< the 1000/cycle cap, so uncapped)
50 nodes : 1,152,000 × 50          ≈ 57.6 M spans/day
per month: 57.6 M × 30             ≈ 1.73 B spans/month
```

Map that monthly span volume onto each backend's billing unit:

=== "Datadog"

    Datadog APM bills separately for **ingested** vs **indexed** spans.

    - **Ingested spans** = everything Click-Dog sends — the full ~1.73 B/month.
      This is the number to plug into Datadog's ingested-spans pricing tier.
    - **Indexed (retained) spans** = the subset kept for search, after your
      retention filters drop the rest. If you index, say, 10% of ingested
      traffic, that is ~173 M indexed spans/month — usually the dominant cost.

    Lever: lower ingest with Click-Dog's `min_trace_duration_ms` /
    [filters](filtering.md) (fewer qualifying traces), and tune index retention
    filters Datadog-side.

=== "Honeycomb"

    Honeycomb bills on **events/month**, and each exported span is one event.
    The ~1.73 B spans/month maps directly to ~1.73 B events/month — pick the
    plan tier that covers it, or reduce volume with duration/operation filters
    and Collector-side sampling.

!!! note
    These are order-of-magnitude estimates to size a plan, not a quote. Validate
    against a single-node run first: read `click_dog_export_accepted_total` over
    an hour and extrapolate. Click-Dog's filters (`min_trace_duration_ms`,
    operation/query filters) are your primary volume lever — apply them before
    paying to dedup or sample downstream.

## Cluster topology and sidecar placement

Click-Dog has **two topologies** — and `single` is just `sidecar` with n=1
(see [Configuration · Cluster Mode](configuration.md#cluster-mode)):

- **sidecar** (default): one instance per ClickHouse node, each reading its
  **local** `system.opentelemetry_span_log`. Partitions are disjoint by host, so
  this is the topology that distributes the cluster-wide read — linearly, with no
  coordination. No Keeper.
- **cluster** (`use_cluster_queries: true`): one instance reads the **whole
  cluster** via `cluster('name', system.opentelemetry_span_log)`. It is **always
  leader-gated** — run 1 (leader of one) or 2–3 for automatic failover. Extra
  instances **stand by** behind a single active exporter; they do not divvy up
  the read.

**Cluster mode is always leader-gated.** The fetch/export cycle runs only on the
election leader (export iff no election is active, or this instance is the
leader). Consequences:

- A single cluster reader with **no Keeper** always exports — no regression for
  existing `use_cluster_queries: true` deployments.
- With Keeper, only the leader exports; standbys record skipped cycles (their
  last-success gauge stays at zero by design). Failover promotes a standby within
  ~one session TTL (≤10s) and re-reads `now - lookback`, so the boundary is a
  **deduped overlap**, never a gap.
- **Keeper disruptions fail open.** Once an instance has joined, any later
  disruption — session expiry, reconnect backoff, a partition, or a watch-path
  error — drops its candidate state so it keeps **exporting** (never stalls as a
  gated standby), and the election goroutine retries until it rejoins. The
  duplicate window is therefore **bounded** and recovers to a single exporter
  when Keeper returns. Click-Dog does **not** fail closed.
- If Keeper is unreachable **at startup** (before any join), the instance runs
  standalone and exports; it does not auto-join later, so restart it once Keeper
  is reachable to enter the election. With 2–3 such instances the duplicate
  exports are absorbed by downstream `(trace_id, span_id)` dedup. The guarantee
  under a healthy Keeper is "no **steady-state** duplication," not "never
  duplicates."
- A **manual flush on a standby** (`click-dog flush`, or `SIGUSR1`) runs through
  the same leader-gated cycle, so on a non-leader it records a **skipped cycle**
  and exports nothing — by design, not an error. Flush from the leader, or wait
  for failover, to force an immediate export. (Backfill — `-backfill-start/-end`
  — is the exception: it is an explicit one-shot historical export and is **not**
  leader-gated.)

!!! danger "Pick one topology per fleet — don't run per-node sidecars with `use_cluster_queries: true`"
    `clickhouse.use_cluster_queries` wraps the spans query in
    `cluster('name', system.opentelemetry_span_log)`, which fetches across all
    shards from a single instance. Leader-gating prevents steady-state
    duplication **when those instances share an election**, but sidecars that are
    **not** sharing an election (different `base_path`, or no Keeper) would each
    go cluster-wide and duplicate. For whole-cluster reads, run **cluster** mode
    with a shared Keeper; for per-node reads, leave `use_cluster_queries` off.
    This mirrors the note in
    [Configuration · Cluster Mode](configuration.md#cluster-mode).

    **This is no longer only documented.** When `use_cluster_queries: true`,
    click-dog self-detects this misconfiguration with an in-process **topology
    self-audit** and surfaces it via the `click_dog_topology_warning{reason}`
    metric, the `/status` `topology_warning` field, a throttled WARN log, and the
    **Topology** tile (plus a per-host column) on the Datadog health dashboard.
    Detection is debounced and lands roughly `interval_s × debounce_count`
    (~10 min at defaults) after the misconfig appears. It is observability-only:
    it never gates `/readyz`, restarts, or de-rotates a node — a config that was
    already running keeps running. The `reason` distinguishes co-located sidecars
    (`sidecar_cluster_queries`) from separate readers not sharing an election
    (`multi_instance_cluster_queries`). Tune it under
    [`monitor.topology_audit`](configuration.md#topology-self-audit). A
    **gray / no-data** tile is *not* healthy — it means the metric isn't being
    scraped.

Use **sidecar** for large clusters (locality, linear scaling); use **cluster**
for small clusters where a reader per node is overkill (it needs cross-node query
permissions and reachability, plus a `cluster:` name).

Either way, **`readonly=2` is always enforced** on every ClickHouse connection
(`internal/clickhouse/reader.go`) — Click-Dog can change session settings but
can never write.

## Config management at scale

Treat the YAML as code and roll it out from one source of truth.

- **Ansible** (`deploy/ansible/`): `playbook.yaml` uses the same sidecar
  lifecycle shape as `install.sh` (config file, systemd unit, validation before
  restart) and handles inventory, bounded SSH concurrency, rolling restarts and
  updates; `playbook-canary.yaml` stages rollouts so you can pause after a small
  subset upgrades. See
  [Install · Multi-Node Deploy](install.md#multi-node-deploy).
- **Kubernetes** (`deploy/kubernetes/`): a single-replica **Deployment** reads
  the cluster through the ClickHouse Service (`use_cluster_queries`), with config
  in a ConfigMap and secrets in a Secret; liveness/readiness probe the dedicated
  health listener. For node-local reading, co-locate click-dog as a sidecar in
  the ClickHouse pod. See
  [Install · Kubernetes](install.md#kubernetes).

### Detecting config drift

- **Validate before deploy.** `click-dog -validate -config <path>` loads,
  applies defaults, and runs full validation without connecting — wire it into
  CI/pre-deploy so a malformed or out-of-policy config fails fast.
- **Validate config, connectivity *and* the data plane.** `click-dog check`
  tests ClickHouse and exporter reachability against the target environment and
  then probes the readiness checks Datadog dashboards depend on —
  `system.opentelemetry_span_log` and `system.query_log` readability and grants,
  recent spans, the `min_trace_duration_ms` threshold versus observed durations,
  `clickhouse.query_id` presence on spans and query-log enrichment join
  viability (probed only when enrichment or user filters are configured), and
  `normalized_query_hash` support. It fails on missing required tables or
  grants and warns on empty/recent-data gaps. Tune the window with `--lookback`
  (default `24h`).
- **Pin against the canonical profiles.** `docs/examples/*.yaml`
  (`click-dog-minimal.yaml`, `click-dog-production.yaml`,
  `click-dog-paranoid.yaml`) are the parity-tested reference outputs of
  `click-dog init`. Diff a node's live config against the profile it claims to
  follow to catch hand-edits and drift.
