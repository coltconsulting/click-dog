# Datadog Integration

Click-Dog's packaged Datadog path includes OTLP trace export, the
**Application Query Analysis** dashboard, the **Click-Dog: Health** dashboard,
and recommended monitors for exporter health and query regressions. You can send
traces directly to the Datadog Agent or through an OpenTelemetry Collector.

## Fast Path

1. Enable OTLP gRPC on the Datadog Agent.
2. Install Click-Dog with the [guided installer](../getting-started.md).
3. Run the readiness check:
   ```bash
   click-dog check -config /etc/click-dog/click-dog.yaml
   ```
4. Provision dashboards:
   ```bash
   DD_API_KEY=... DD_APP_KEY=... click-dog create-dashboards
   ```
5. In Datadog, open **Click-Dog: Application Query Analysis** and
   **Click-Dog: Health**.

## Option 1: Datadog Agent (Recommended)

The Datadog Agent (v7.35+) can ingest OTLP traces directly.

### Configure the Datadog Agent

Edit `datadog.yaml` or use environment variables:

**datadog.yaml:**
```yaml
otlp_config:
  receiver:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
```

**Environment variables (e.g., in Docker/Kubernetes):**
```bash
DD_OTLP_CONFIG_RECEIVER_PROTOCOLS_GRPC_ENDPOINT=0.0.0.0:4317
```

Restart the Datadog Agent after making changes.

### Configure Click-Dog

Point click-dog at the Datadog Agent's OTLP endpoint:

```yaml
exporters:
  otel:
    - collector_address: localhost:4317
      service_name: click-dog-monitor
```

If the Datadog Agent is on a different host:
```yaml
exporters:
  otel:
    - collector_address: datadog-agent.internal:4317
      service_name: click-dog-monitor
```

### With TLS

If your Datadog Agent requires TLS:

```yaml
exporters:
  otel:
    - collector_address: datadog-agent.internal:4317
      service_name: click-dog-monitor
      secure: true
      ca_cert: /path/to/ca.pem
```

### Verify

Before sending traffic, confirm the ClickHouse data plane these dashboards rely
on is actually ready:

```bash
click-dog check -config /etc/click-dog/click-dog.yaml
```

This validates that `system.opentelemetry_span_log` and `system.query_log` are
readable and granted, that recent spans exist, that `min_trace_duration_ms`
isn't filtering everything out, that spans carry `clickhouse.query_id` (so
query-log enrichment and user filters work), and that `normalized_query_hash`
is available (so the query-family widgets populate). Warn/fail lines print the
exact fix.

To confirm a span actually reaches Datadog end-to-end, send a synthetic one:

```bash
click-dog test-span -config /etc/click-dog/click-dog.yaml
```

It goes to every configured exporter and is tagged `click_dog.test=true` so it
is easy to find (and filter out) in the backend.

Then, in Datadog, navigate to **APM > Traces** and search for:
- Service: `click-dog-monitor`
- Look for spans with `db.statement`, `duration_ms`, and `hostname` attributes

---

## Option 2: OpenTelemetry Collector

If you're already running an OTEL Collector, you can route click-dog traces through it.

### OTEL Collector Configuration

```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317

exporters:
  datadog:
    api:
      key: ${DD_API_KEY}
      site: datadoghq.com      # or datadoghq.eu, etc.

service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [datadog]
```

### Click-Dog Configuration

```yaml
exporters:
  otel:
    - collector_address: otel-collector.internal:4317
      service_name: click-dog-monitor
```

---

## Datadog-Specific Attributes

Click-Dog spans appear in Datadog with these attributes:

### From Scheduled Mode (span log)

| Datadog Attribute | Source |
|---|---|
| Service | `exporters.otel[].service_name` config |
| Operation | `operation_name` from span |
| Duration | Computed from span start/finish timestamps |
| `hostname` | ClickHouse server hostname |
| `duration_ms` | Duration in milliseconds (numeric, queryable) |
| `db.statement` | SQL query text |
| `client.address` | Client IP |
| `kind` | Span kind (INTERNAL, SERVER, CLIENT, etc.) |
| `query_log.query_duration_ms` | Query duration from live query_log enrichment (numeric) |
| `query_log.read_rows` / `query_log.read_bytes` | Data read from live query_log enrichment (numeric) |
| `query_log.written_rows` / `query_log.written_bytes` | Data written from live query_log enrichment (numeric) |
| `query_log.result_rows` / `query_log.result_bytes` | Result size from live query_log enrichment (numeric) |
| `query_log.memory_usage` | Peak memory use from live query_log enrichment (numeric) |
| `query_log.exception_code` | ClickHouse exception code from live query_log enrichment (numeric, when non-zero) |
| `query_log.normalized_query_hash` | Query-family grouping hash (intentionally a **string**, not numeric — it's a dimension, not a measure) |
| `query_log.databases` | Databases accessed from query_log enrichment (array facet) |
| `query_log.tables` | Tables accessed from query_log enrichment (array facet) |

The numeric `query_log.*` fields above are emitted as OTLP integer measures.
ClickHouse stores `read_bytes` / `written_bytes` / `memory_usage` as `UInt64`,
so a value above `2^63` (≈9.2 EB / 9.2×10¹⁸) is emitted as a **string** instead
(OTLP integers are signed 64-bit) — rare in practice, but a measure widget over
these fields should tolerate the occasional string-typed point.

If you previously created Datadog facets for `query_log.databases` or
`query_log.tables` while they were joined strings, recreate those facets after
upgrading so new spans are indexed as multi-value array facets. Historical spans
keep the old joined-string shape.

### From Backfill Mode (query log)

| Datadog Attribute | Source |
|---|---|
| Service | `exporters.otel[].service_name` config |
| Operation | `clickhouse.query` |
| `db.system` | `clickhouse` |
| `db.statement` | SQL query text |
| `db.query_duration_ms` | Query duration in ms |
| `db.user` | ClickHouse user |
| `client.address` | Client IP |
| `db.databases` | Databases accessed |
| `db.tables` | Tables accessed |
| `db.read_rows` / `db.read_bytes` | Data read |
| `db.memory_usage` | Memory used |
| `error` | `true` if query failed |

### log_comment Attributes (Scheduled Mode)

If your ClickHouse clients set the `log_comment` query setting with JSON, click-dog
extracts the keys as span attributes. For example, a query with
`log_comment={"app": "Dash8", "query_name": "what_watched"}` produces:

| Attribute | Value |
|---|---|
| `@log_comment.app` | `Dash8` |
| `@log_comment.query_name` | `what_watched` |

These appear as filterable facets in Datadog APM. See [Span Attributes](../span-attributes.md#log_comment-extraction).

### Dashboards & Monitors

Use these attributes to build Datadog dashboards and monitors:

**Slow query monitor:**
```
avg:trace.duration{service:click-dog-monitor} > 5000000000
```

**Query by duration_ms attribute:**
```
@duration_ms:>5000
```

**Filter by ClickHouse host:**
```
@hostname:clickhouse-prod-01
```

**Filter by app (via log_comment):**
```
@log_comment.app:Dash8
```

---

## Dashboards

Click-dog ships two Datadog dashboards:

- **Click-Dog: Application Query Analysis** ([`datadog-query-analysis.json`](https://github.com/coltconsulting/click-dog/blob/master/dashboards/datadog-query-analysis.json)) — live-span dashboard for `system.opentelemetry_span_log` data, filtered with `resource_name:query @click_dog.source:span_log` and grouped by `log_comment.*` plus live `query_log.*` enrichment
- **Click-Dog: Health** ([`datadog-clickdog-health.json`](https://github.com/coltconsulting/click-dog/blob/master/dashboards/datadog-clickdog-health.json)) — self-monitoring metrics for current export state, error/export rates, backoff, circuit breaker, and lifetime counters

### Option 1: click-dog command

```bash
DD_API_KEY=... DD_APP_KEY=... click-dog create-dashboards
```

### Option 2: Datadog API

```bash
curl -X POST "https://api.datadoghq.com/api/v1/dashboard" \
  -H "Content-Type: application/json" \
  -H "DD-API-KEY: ${DD_API_KEY}" \
  -H "DD-APPLICATION-KEY: ${DD_APP_KEY}" \
  -d @dashboards/datadog-query-analysis.json
```

The Application Query Analysis dashboard requires click-dog `v26.03.1` or
newer for its `@click_dog.source:span_log` filter. If you cannot upgrade
immediately, remove that filter from widget queries after import.

For EU or other regions, replace `datadoghq.com` with your site (e.g. `datadoghq.eu`).

!!! tip "Running on Kubernetes?"
    The dashboards only show data once ClickHouse is actually emitting spans
    and click-dog is exporting them. For the operator-based ClickHouse setup
    (enabling `system.opentelemetry_span_log` cluster-wide, the read-only
    monitoring user, sidecar vs centralized topology, and OTLP/Agent wiring),
    follow [Kubernetes — ClickHouse Setup](../kubernetes-clickhouse.md).

### Query dashboard data-source contract

The shipped **Application Query Analysis** dashboard is live-span-only. It is
designed for click-dog's scheduled mode, where ClickHouse native spans from
`system.opentelemetry_span_log` arrive in Datadog as `click_dog.source=span_log`
with `resource_name:query`. Its filters and template variables use live-span
fields:

- `@log_comment.app` and `@log_comment.query_name` from ClickHouse client
  tagging
- `@query_log.user`, `@query_log.client_name`,
  `@query_log.normalized_query_hash`, and related `query_log.*` enrichment
  joined onto live spans
- `@query_log.read_rows`, `@query_log.memory_usage`, and other live
  query_log stats as numeric OTLP attributes for measure-style widgets

Compatibility: the `@click_dog.source:span_log` filter requires click-dog
`v26.03.1` or newer, or any build that emits the `click_dog.source` attribute.
Older binaries can still export spans, but this dashboard's source filter will
not match them until click-dog is upgraded. If you cannot upgrade immediately,
remove `@click_dog.source:span_log` from widget queries after import to restore
the previous `resource_name:query`-only matching behavior.

Backfill mode is intentionally separate. Backfill reads `system.query_log` and
exports single-span traces named `clickhouse.query` with
`click_dog.source=query_log` and `db.*` attributes (`db.user`,
`db.read_rows`, `db.memory_usage`, `db.normalized_query_hash`, and so on). If
you import only historical backfill data, the shipped query dashboard may show
little or no data. For backfill analysis, use Datadog trace search or custom
widgets with a query such as:

```
service:click-dog-monitor resource_name:clickhouse.query @click_dog.source:query_log
```

Group those views by backfill fields such as `@db.user`, `@client.name`,
`@db.normalized_query_hash`, `@db.tables`, or `@hostname`.

### Query dashboard layout

```
Row 1: Overview
  [ Exported queries/min ] [ p50/p95/p99 latency ]

Row 2: Application identity
  [ Top apps by exported volume ] [ Top apps by p95 latency ] [ Slow queries by app ]

Row 3: Query identity
  [ Named query latency ] [ Named queries by exported volume ] [ Exact query families ]

Row 4: Live query_log enrichment
  [ Users ] [ Client libraries ] [ Rows read ] [ Peak memory ] [ Tables ] [ Databases ]

Row 5: Distribution
  [ Exported queries by host ]
```

Volume/count widgets show the **exported/qualified query stream**, not total
ClickHouse query volume. Scheduled mode exports only traces selected by
`monitor.min_trace_duration_ms` and any other query/operation/IP filters, so
with the quickstart default `min_trace_duration_ms: 1000` these counts reflect
qualifying slow-query traces. For total ClickHouse query volume, use the
official Datadog ClickHouse integration.

---

## Click-Dog Self-Monitoring

Click-dog can push its own low-cardinality exporter health metrics over OTLP:
cycle counts and outcomes, export totals, errors, circuit-breaker state,
backoff interval, last-success timestamp, and last-cycle duration / spans.
Enable `metrics.otlp.enabled: true`; by default click-dog reuses
`exporters.otel[0]` and sends self-metrics to the same collector as spans. The
`production` init profile (the default for `click-dog init`) now writes this
block for you, so a guided install already feeds the Health dashboard — if you
generated your config that way it's on already; delete the block to opt out.
Set `metrics.otlp.host` in containers if you want a stable `host.name` dashboard
variable instead of the pod/container hostname.

Standalone `metrics.otlp` connections currently expose plaintext/TLS settings
only; use `inherit_otel_connection: true` when the collector requires the
span exporter's CA or mTLS client certificate settings.

The Prometheus/OpenMetrics-compatible `/metrics` endpoint is still available
(default `:9090/metrics`, opt-in via `metrics.enabled: true` — see
[Observability](../observability.md)) for Prometheus, Grafana Alloy, the OTel
Collector's prometheus receiver, or Datadog users who cannot enable OTLP
self-metrics.

The positioning is intentional: traces (Option 1 / Option 2 above) carry the
rich query analytics — `db.statement`, latency, user, app, normalized query
family — and that's what the **Click-Dog: Application Query Analysis**
dashboard reads. Self-metrics carry the orthogonal exporter-health surface —
"is click-dog itself running, scraping, and exporting?" — and that's what the
**Click-Dog: Health** dashboard reads (with current action state first: last
success age, breaker state, current error rate, export throughput, and backoff
interval; trends and lifetime counters follow).

### Legacy Datadog Agent OpenMetrics scrape config

Prometheus-only users can still scrape `/metrics` with Datadog. Drop this in
`/etc/datadog-agent/conf.d/openmetrics.d/conf.yaml` and restart the Agent. The
explicit `metrics:` rename list is required for the scrape path; a generic
wildcard such as `- click_dog_*` will not produce usable Datadog names.
`dashboards/README.md` and `dashboards/datadog-clickdog-health.json` document
the same legacy mapping.

```yaml
init_config:

instances:
  - openmetrics_endpoint: http://localhost:9090/metrics
    namespace: click_dog
    metrics:
      # Counters — Datadog appends `.count` and submits as a monotonic rate.
      - click_dog_spans_exported_total: spans_exported
      - click_dog_spans_filtered_total: spans_filtered
      - click_dog_spans_duplicates_total: spans_duplicates
      - click_dog_export_attempts_total: export.attempts
      - click_dog_export_accepted_total: export.accepted
      - click_dog_export_errors_total: export.errors
      - click_dog_cycle_results_total: cycle_results
      # Gauges — submitted as-is under the namespace.
      - click_dog_circuit_breaker_state: circuit_breaker.state
      - click_dog_leader: leader
      - click_dog_backoff_interval_seconds: backoff_interval.seconds
      - click_dog_last_success_timestamp_seconds: last_success_timestamp.seconds
      - click_dog_uptime_seconds: uptime.seconds
      - click_dog_last_cycle_duration_seconds: last_cycle.duration_seconds
      - click_dog_last_cycle_exported_spans: last_cycle.exported_spans
      - click_dog_last_cycle_filtered_spans: last_cycle.filtered_spans
      - click_dog_last_cycle_duplicate_spans: last_cycle.duplicate_spans
      # ClickHouse data-plane health (#183) — span_log freshness, enrichment
      # health, and query_id coverage. Lets an operator distinguish
      # "click-dog is down" from "ClickHouse is not emitting useful data".
      - click_dog_span_log_last_poll_timestamp_seconds: span_log.last_poll_timestamp.seconds
      - click_dog_span_log_newest_row_age_seconds: span_log.newest_row_age.seconds
      - click_dog_span_log_rows_last_cycle: span_log.rows_last_cycle
      - click_dog_query_log_enrichment_attempts_total: query_log.enrichment.attempts
      - click_dog_query_log_enrichment_successes_total: query_log.enrichment.successes
      - click_dog_query_log_enrichment_failures_total: query_log.enrichment.failures
      - click_dog_query_log_enrichment_match_ratio: query_log.enrichment.match_ratio
      - click_dog_spans_with_query_id_ratio: spans_with_query_id_ratio
      - click_dog_normalized_query_supported: normalized_query_supported
      # Topology self-audit — detects the sidecar + use_cluster_queries anti-pattern.
      - click_dog_topology_warning: topology_warning
```

### Mapped metric names

| Prometheus metric (`/metrics`)                | Type    | Datadog metric                            |
|-----------------------------------------------|---------|-------------------------------------------|
| `click_dog_spans_exported_total`              | counter | `click_dog.spans_exported.count`          |
| `click_dog_spans_filtered_total`              | counter | `click_dog.spans_filtered.count`          |
| `click_dog_spans_duplicates_total`            | counter | `click_dog.spans_duplicates.count`        |
| `click_dog_export_attempts_total`             | counter | `click_dog.export.attempts.count` (tag: `sink`) |
| `click_dog_export_accepted_total`             | counter | `click_dog.export.accepted.count` (tag: `sink`) |
| `click_dog_export_errors_total`               | counter | `click_dog.export.errors.count` (tag: `sink`) |
| `click_dog_cycle_results_total`               | counter | `click_dog.cycle_results.count` (tag: `result`) |
| `click_dog_circuit_breaker_state`             | gauge   | `click_dog.circuit_breaker.state`         |
| `click_dog_leader`                            | gauge   | `click_dog.leader`                        |
| `click_dog_backoff_interval_seconds`          | gauge   | `click_dog.backoff_interval.seconds`      |
| `click_dog_last_success_timestamp_seconds`    | gauge   | `click_dog.last_success_timestamp.seconds` (tag: `role`) |
| `click_dog_uptime_seconds`                    | gauge   | `click_dog.uptime.seconds`                |
| `click_dog_last_cycle_duration_seconds`       | gauge   | `click_dog.last_cycle.duration_seconds`   |
| `click_dog_last_cycle_exported_spans`         | gauge   | `click_dog.last_cycle.exported_spans`     |
| `click_dog_last_cycle_filtered_spans`         | gauge   | `click_dog.last_cycle.filtered_spans`     |
| `click_dog_last_cycle_duplicate_spans`        | gauge   | `click_dog.last_cycle.duplicate_spans`    |
| `click_dog_span_log_last_poll_timestamp_seconds` | gauge | `click_dog.span_log.last_poll_timestamp.seconds` |
| `click_dog_span_log_newest_row_age_seconds`   | gauge   | `click_dog.span_log.newest_row_age.seconds` |
| `click_dog_span_log_rows_last_cycle`          | gauge   | `click_dog.span_log.rows_last_cycle`      |
| `click_dog_query_log_enrichment_attempts_total`  | counter | `click_dog.query_log.enrichment.attempts.count`  |
| `click_dog_query_log_enrichment_successes_total` | counter | `click_dog.query_log.enrichment.successes.count` |
| `click_dog_query_log_enrichment_failures_total`  | counter | `click_dog.query_log.enrichment.failures.count`  |
| `click_dog_query_log_enrichment_match_ratio`  | gauge   | `click_dog.query_log.enrichment.match_ratio` |
| `click_dog_spans_with_query_id_ratio`         | gauge   | `click_dog.spans_with_query_id_ratio`     |
| `click_dog_normalized_query_supported`        | gauge   | `click_dog.normalized_query_supported`    |
| `click_dog_topology_warning`                  | gauge   | `click_dog.topology_warning` (tag: `reason`) |

### ClickHouse data-plane health signals (#183)

The metrics in the bottom block of the table above carry **ClickHouse
data-plane health** — orthogonal to "is click-dog exporting" and answering
"is ClickHouse producing the data click-dog needs?" Useful for the
"dashboards are empty but click-dog looks healthy" failure mode:

- `click_dog_span_log_last_poll_timestamp_seconds` — Unix timestamp of the
  most recent span-log fetch that completed without error. An empty fetch
  (zero rows returned) still advances this gauge; absence of the metric
  family from `/metrics` means click-dog is down.
- `click_dog_span_log_newest_row_age_seconds` — age in seconds of the
  newest span observed in the most recent non-empty fetch. Empty cycles do
  NOT reset this; the gauge keeps growing while no new spans arrive, so
  monotonic growth is the staleness signal. `0` means no observation yet.
- `click_dog_span_log_rows_last_cycle` — raw row count returned by the
  most recent span-log fetch, before click-dog's in-process filter/dedup.
- `click_dog_query_log_enrichment_{attempts,successes,failures}_total` —
  cycle-level counters tracking query_log join health. An attempt is
  recorded only when at least one span carried a `clickhouse.query_id` to
  look up, so non-enrichment configurations cost nothing.
- `click_dog_query_log_enrichment_match_ratio` — last cycle's matched /
  requested ratio. Persists across failed attempts (a failed lookup tells
  us nothing about how well a future join would match). `0` before the
  first successful enrichment.
- `click_dog_spans_with_query_id_ratio` — last cycle's fraction of fetched
  spans carrying `clickhouse.query_id`. Counted on the raw (pre-filter)
  span set so the gauge reflects ClickHouse's output, not click-dog's
  choices. A value below 1.0 means downstream query-analysis dashboards
  lose join context for some spans.
- `click_dog_normalized_query_supported` — `1` if
  `system.query_log.normalized_query_hash` is available on the connected
  ClickHouse and click-dog is using it; `0` otherwise. In cluster query
  mode, `1` means every replica passed the `clusterAllReplicas(...,
  system.columns)` compatibility probe. Set once at startup from the
  capability probe in `internal/clickhouse/reader.go`.
- `click_dog_topology_warning` — `1` when the topology self-audit detects more
  than one instance running whole-cluster span reads (`use_cluster_queries`),
  which duplicates exports; `0` when clean. Emitted per `reason`
  (`sidecar_cluster_queries` | `multi_instance_cluster_queries`) and always
  present for both reasons, so a green `0` is distinguishable from no-data.
  Observability-only — it never affects `/readyz`. A red `1` can also reflect
  legitimate, bounded duplication — a sustained Keeper outage (instances fail
  open and export by design) or a long-running backfill from another host —
  and clears on its own once resolved. The gauge is always emitted
  (stable `0` for both reasons from startup, even when the auditor is skipped);
  the auditor only runs its `system.query_log` probe — and can raise the gauge to
  `1` — when this instance has `use_cluster_queries: true`. See
  [Operating · Cluster topology](../operating.md#cluster-topology-and-sidecar-placement).

The recommended pairing on the Health dashboard:

| Operator question                               | Read these together                                              |
|-------------------------------------------------|------------------------------------------------------------------|
| "Is click-dog alive?"                           | `uptime_seconds` (cockpit) + `span_log_last_poll_timestamp_seconds` |
| "Is ClickHouse emitting spans?"                 | `span_log_rows_last_cycle` + `span_log_newest_row_age_seconds`   |
| "Are query_log joins still working?"            | `query_log_enrichment_failures.count` rate + `query_log_enrichment_match_ratio` |
| "Are downstream query dashboards complete?"     | `spans_with_query_id_ratio` + `normalized_query_supported`       |
| "Is exactly one reader doing cluster reads?"    | `topology_warning` (per `reason`) + the Per-host health table    |

### Troubleshooting "No data" on the health dashboard

1. Confirm `metrics.otlp.enabled: true`.
2. Confirm the Datadog Agent or collector OTLP gRPC receiver is reachable from
   click-dog.
3. In containers, set `metrics.otlp.host` to the stable node/instance identity
   you want to use for the `host.name` template variable.
4. On the legacy scrape path, search Metrics Explorer for
   `click_dog.click_dog_spans_exported_total`; that means the explicit
   OpenMetrics `metrics:` rename list above is not being applied.

### Health endpoints (HTTP / Synthetic check alternative)

Alongside `/metrics`, click-dog exposes dedicated HTTP endpoints suitable for
Datadog HTTP checks, Synthetic checks, or Kubernetes probes. Full reference:
[Observability — Health Endpoints](../observability.md#health-endpoints).

| Endpoint | Default port | Use from Datadog |
|----------|--------------|------------------|
| `GET /healthz`  | `:8686` | HTTP check — liveness; always `200` while the process is up |
| `GET /readyz`   | `:8686` | HTTP check — readiness; `200` only when ClickHouse is reachable AND the breaker is closed |
| `GET /status`   | `:8686` | Synthetic / scripted check — JSON body with `version`, `clickhouse_healthy`, `circuit_breaker`, `backoff_interval_s`, `last_cycle.{exported,filtered,duplicates,duration_ms,at,error}`. Always returns `200` — assert on the JSON body, not the HTTP status |
| `GET /clusterz` | `:8686` (leader only, HA) | Aggregate `/readyz` across configured peers. Only the leader serves this path; followers return `404 {"status":"not_leader"}`. Returns `200` only when every node is healthy; `503` when any node is degraded or unreachable |

These overlap with Tier 1 metrics on purpose. Pick endpoints when you want a
probe Datadog already speaks (HTTP / Synthetic) without standing up an
OpenMetrics scrape; pick metrics when you want trends, ratios, and dashboard
widgets. Health endpoints are opt-in via the `health:` block — see the
Observability doc for the binding rules and the `/clusterz` HA configuration.

---

## Recommended Monitors

The canonical setup is the **Click-Dog: Health** dashboard (above) wired to
OTLP self-metrics. Build alerts on top of it in this order:

### Tier 1: Exporter health (canonical)

These monitors fire on click-dog itself and use the default OTLP self-metric
names (Datadog adds `.count` to monotonic sums). They are the primary
signal that click-dog is doing its job.

1. **Stale exports** — last successful cycle is too old. The single most
   important alert: if click-dog stops, you lose visibility into ClickHouse.
   Configure this as a *formula* metric monitor (Datadog → Monitors →
   New Monitor → Metric → "Use a formula"), not a single-metric monitor:

   - Query `a`: `min:click_dog.last_success_timestamp_seconds{role:active}`
   - Formula: `now() - a`
   - Alert threshold: `> 600`

   `last_success_timestamp_seconds` is absent until the first successful
   cycle, so a fresh or failed-start instance reads as no-data rather than
   epoch zero. In HA mode, the `role:active` filter excludes leader-gated
   standbys from stale-export paging while keeping the active exporter covered.
   The `min:` aggregation is intentional: it pages when any active exporter is
   stale, matching the Health dashboard's worst-case cockpit tile.
   `now()` is only available in the formula editor; the basic
   single-metric monitor will reject it. The `≥ 600` threshold is the
   conservative floor — `2–3× monitor.backoff.max_interval_s`, where
   the default is `300` s. Adaptive backoff widens the gap between
   successful cycles during sustained errors, so anything tighter than
   the maximum backoff will alert on a healthy exporter that is simply
   pacing itself. If you also wire Tier 1 #2 (breaker not closed) to
   catch the sustained-error case, a tighter threshold like
   `5–10× check_interval_s` (default `30` s, so `150–300` s) detects
   total click-dog death much faster — the breaker monitor backstops
   the false-positive risk that the wider floor exists to avoid.

2. **Circuit breaker not closed** — exporter is shedding load against
   ClickHouse.
   ```
   max:click_dog.circuit_breaker_state{*} >= 1
   ```
   `0=closed`, `1=half_open`, `2=open`. Half-open is a normal-but-noisy
   recovery state; tighten to `>= 2` if you only want hard-open pages.

3. **Cycle error ratio** — sustained errors even before the breaker trips.
   `cycle_results` tags every cycle as exactly one of `success`, `error`,
   `skipped`, so this is a clean ratio against the summed total.
   ```
   sum:click_dog.cycle_results.count{result:error}.as_rate() /
     sum:click_dog.cycle_results.count{*}.as_rate() > 0.1
   ```
   `as_rate()` resolves per evaluation bucket, so a single noisy bucket
   can page. Evaluate over a multi-bucket window (e.g. ≥ 10 min) with
   an at-least-N-of-M condition (e.g. 3 of 5 buckets over threshold) to
   avoid single-bucket flap.

4. **OTLP self-metrics are missing** — backstop in case the self-metrics
   exporter or collector path stops running (and therefore Tier 1 #1–#3 stop firing). Create a
   metric monitor on `avg:click_dog.cycle_results.count{*}` and enable
   *"Notify if data is missing for the last 5 minutes"* in the monitor
   options panel — `no data` is a monitor condition, not a query
   function, so it lives in the options checkbox rather than the query
   field.

5. **Stale span_log source** — click-dog is exporting, but ClickHouse
   has stopped producing spans for click-dog to read. This is the
   "click-dog looks healthy, dashboards are empty" failure mode that
   Tier 1 #1 misses: `last_success_timestamp` keeps advancing on every
   poll (including zero-row polls), so the staleness signal lives on
   the data-plane gauge introduced for #183:
   ```
   max:click_dog.span_log_newest_row_age_seconds{*} > 600
   ```
   Pair with #1 (stale exports) to triangulate: #1 alone means click-dog
   stopped; #5 alone means ClickHouse stopped emitting spans; both
   firing means click-dog crashed while ClickHouse kept producing.
   Threshold is `> 600 s` to match the dashboard tile's red band; tune
   for your span retention window. Note `0` is ambiguous on this gauge
   ("no observation yet" OR "the newest span arrived just now"), so
   always alert on `>` rather than `==` (see
   [Observability — Available Metrics](../observability.md#available-metrics)).

6. **(Optional) push-side webhook events** — if the `webhook` block is
   configured (see [Observability — Webhook Notifications](../observability.md#webhook-notifications)),
   click-dog itself fires `circuit_breaker_opened`,
   `circuit_breaker_closed`, `backfill_failed`, `error_spike`, `startup`,
   and `shutdown` straight to Slack / PagerDuty / a Datadog webhook
   integration without waiting on a scrape window.

### Tier 2: Query analytics (from traces)

These monitors fire on the traces click-dog exports. They catch
ClickHouse-side regressions, not click-dog-side problems.

The shape of each monitor below is illustrative — the metric and tag
names are the load-bearing part. Tune your team's exact monitor query
(window, threshold, group-by, anomaly bounds) against them; what's worth
paging on differs by workload, and Tier 1 already covers exporter
health, so Tier 2 deliberately stays at the "here is the right signal"
level rather than prescribing thresholds.

1. **Slow query spike** — P95 query duration regression.
   ```
   avg:trace.duration{service:click-dog-monitor} > 5000000000  # 5e9 ns = 5 s
   ```

2. **Trace error rate** (backfill mode only) — alert when >5% of spans have
   `error:true`. The `error` attribute only exists on backfill spans from
   `query_log`; scheduled-mode spans from `opentelemetry_span_log` always
   have status OK.

3. **Memory hog queries** (backfill mode only) — `db.memory_usage` exceeds
   threshold.

4. **Read amplification** (backfill mode only) — P95 of `db.read_bytes`
   spikes.

5. **Unusual query volume** — anomaly detection on span count; catches both
   traffic spikes and unexpected drops.

6. **Top-N queries by duration** — weekly digest of the slowest query
   families, grouped by `@query_log.normalized_query_hash`. Useful for
   regression hunting without an alerting threshold.

7. **Per-user query patterns** — track which users generate load, grouped
   by `@query_log.user`. Useful for capacity planning and noisy-neighbour
   detection.

8. **Cross-host skew** — alert if one ClickHouse node's P95 latency drifts
   well above its peers, grouped by `@hostname`. Catches single-node
   degradation that aggregate latency monitors mask.

### Tier 3: Fallback signals

Complementary backstops for setups where Tier 1 and Tier 2 aren't fully
wired yet, or for catching failures the higher tiers can't observe:

- **Log-based monitors** — ship click-dog logs to Datadog Logs and alert on
  `[ERROR]` lines or circuit-breaker warnings. Useful when `/metrics` is
  not (yet) scraped.
- **Process check** — Datadog Agent `process` check on the `click-dog`
  binary. The one failure mode `/metrics` cannot report on is the binary
  crashing — because the process serving `/metrics` is the one that died.
- **No data on traces** — fires if no spans land in the trace pipeline.
  Less precise than Tier 1 #1 because it conflates "click-dog is down"
  with "ClickHouse has no traffic", but it works without any extra wiring.

---

## Example: Full Production Setup

```yaml
clickhouse:
  host: ${CLICKHOUSE_HOST}
  port: 9440
  database: default
  username: ${CLICKHOUSE_USERNAME}
  password: ${CLICKHOUSE_PASSWORD}
  secure: true
  ca_cert: /etc/ssl/clickhouse-ca.pem

exporters:
  otel:
    - collector_address: ${DD_AGENT_HOST}:4317
      service_name: click-dog-monitor
      secure: true
      ca_cert: /etc/ssl/dd-ca.pem

monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30
  max_spans_per_cycle: 500
  batch_size: 50
  batch_delay_ms: 100
  circuit_breaker:
    enabled: true
    failure_threshold: 5
  backoff:
    enabled: true

filters:
  blacklist_queries:
    - "^SELECT \\* FROM system\\."
    - "(?i)healthcheck"

log_level: info
```
