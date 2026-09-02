# Click-Dog Dashboards

Pre-built dashboard templates for Datadog.

## Dashboards

| File | Dashboard | Data source |
|------|-----------|-------------|
| `datadog-query-analysis.json` | **Click-Dog: Application Query Analysis** | Live APM spans from `system.opentelemetry_span_log` |
| `datadog-user-activity.json` | **Click-Dog: Exported User Activity** | Enriched live root-query spans |
| `datadog-clickdog-health.json` | **Click-Dog: Health** | OTLP self-metrics |

## Import

### Option 1: click-dog command (recommended)

```bash
DD_API_KEY=... DD_APP_KEY=... click-dog create-dashboards
```

Creates all three dashboards. Use `--dashboard query`, `--dashboard activity`,
or `--dashboard health` for just one.

**Re-running is safe.** Each dashboard is stamped with a version marker (the
click-dog version + a content hash) appended to its description: it shows as a
small `<!-- clickdog: … -->` line at the end of the description in Datadog, and
lets a later run know whether the live dashboard matches this build:

- not present → created;
- present and identical → left untouched ("already up to date");
- present but a different version → you're asked to **overwrite** it in place,
  create a **new** copy, or **skip**.

On a terminal you get a prompt; for scripted/CI use, force the action with
`--on-exists=skip|overwrite|new` (default `skip` when not a terminal, so a
re-run never hangs or silently overwrites). If several dashboards share a title
(e.g. earlier duplicate imports), pass `--id <dashboard_id>` to choose one.

```bash
# headless update of the query dashboard, overwriting in place
DD_API_KEY=... DD_APP_KEY=... click-dog create-dashboards \
  --dashboard query --on-exists overwrite
```

> Selectively merging only the *new* widgets into a dashboard you've customized
> is a separate, not-yet-implemented capability. The private development
> repository carries the dashboard-update design spec.

### Option 2: Datadog API

From the repository root, import each shipped JSON definition:

```bash
for dashboard in \
  datadog-query-analysis.json \
  datadog-user-activity.json \
  datadog-clickdog-health.json
do
  curl -X POST "https://api.datadoghq.com/api/v1/dashboard" \
    -H "Content-Type: application/json" \
    -H "DD-API-KEY: ${DD_API_KEY}" \
    -H "DD-APPLICATION-KEY: ${DD_APP_KEY}" \
    -d "@dashboards/${dashboard}"
done
```

> [!WARNING]
> The Application Query Analysis and Exported User Activity dashboards filter on
> `@click_dog.source:span_log`. Importing either with click-dog older than
> `v26.03.1` will show no live-span data until click-dog is upgraded. If you
> cannot upgrade immediately, remove `@click_dog.source:span_log` from widget
> queries after import to restore the previous `resource_name:query`-only
> matching behavior.

## Application Query Analysis

This dashboard is intentionally live-span focused. It filters to root query
spans from `system.opentelemetry_span_log` using
`resource_name:query @click_dog.source:span_log`. The dashboard is laid out so
the first screen is an out-of-box key-metrics summary, automatic live-span
`query_log.*` enrichment sections come next, and `log_comment.*` enhancement
sections come last. Template variables: `env`, `app` (log_comment), `user` and
`client` (live-span `query_log.*` enrichment).

> [!NOTE]
> Count/volume widgets (e.g. *Exported queries (1h)*, *Exported queries per
> minute*, *Top apps by exported query volume*) reflect the **exported/qualified
> query stream**, not total ClickHouse traffic. In scheduled mode click-dog
> exports only traces selected by `monitor.min_trace_duration_ms` and any other
> query/operation/IP filters, so with the quickstart default
> `min_trace_duration_ms: 1000` a fresh dashboard shows qualifying slow-query
> traces. For total ClickHouse query volume, use the official Datadog ClickHouse
> integration.

Historical backfill is a separate data contract: backfill spans come from
`system.query_log`, are named `clickhouse.query`, carry
`click_dog.source=query_log`, and use `db.*` attributes such as `db.user`,
`db.read_rows`, and `db.memory_usage`. Use Datadog trace search or custom
widgets with those backfill fields for historical workflows; this shipped
dashboard will not populate from backfill-only imports.

Compatibility: the `@click_dog.source:span_log` filter requires click-dog
`v26.03.1` or newer, or any build that emits the `click_dog.source` attribute.
Older binaries can still export spans, but this dashboard's source filter will
not match them until click-dog is upgraded. If you cannot upgrade immediately,
remove `@click_dog.source:span_log` from widget queries after import to restore
the previous `resource_name:query`-only matching behavior.

The higher-level query-family rollup API groups aggregated exact normalized-query groups for offline/dashboard-analysis callers. The shipped Datadog dashboard does not consume those rollups yet because it only reads span attributes; it continues to show exact normalized families until a dedicated rollup export or storage surface is added.

| Section | Widgets | Out of box? |
|---------|---------|-------------|
| **Key metrics** | Exported queries (1h), p95/p99 latency (with sparkline), slow query count, top user, top client | Yes: `query_log.*` enrichment + duration |
| **Query overview** | Exported queries/min, p50/p95/p99 latency | Yes |
| **Failures** | Failed query count and top failing query families | Yes: `query_log.exception_code` (blank until a query fails) |
| **By ClickHouse user & client** | Top users by exported query volume and p95 latency, top client libraries | Yes: `query_log.user`, `query_log.client_name` |
| **Per-host** | Exported-query distribution across ClickHouse nodes | Yes: `hostname` |
| **Resource usage** | Top query families by total query time, rows read and peak memory, plus tables/databases accessed | Yes: `@duration` plus numeric `query_log.read_rows` / `query_log.memory_usage`, plus `query_log.tables` and `query_log.databases` (family widgets need `query_log.normalized_query_hash`) |
| **Exact query families** | Slowest query shapes grouped deterministically by `query_log.normalized_query_hash` with `query_log.normalized_query` preview | Yes: preferred over raw SQL grouping |
| **By application** | Top apps by exported query volume, latency, slow query count | No: requires `log_comment.app` tagging |
| **By named query** | Top named queries by p95 latency and exported volume | No: requires `log_comment.query_name` tagging |

### Out-of-box vs. `log_comment` enhancement

Everything above the **By application** section works on a fresh install with no client-side changes: click-dog's `query_log` enrichment populates the user, client, host, table, rows-read, memory, total-query-time, and exception dimensions automatically. Normalized-query dimensions appear when `click_dog.normalized_query_supported` is `1`; in cluster query mode that means every replica passed the startup compatibility probe. The key-metrics row gives a single-screen "are query traces flowing, what does the current p95/p99 look like, who's the top contributor right now" answer the moment the dashboard imports.

The **By application** and **By named query** sections require the client to set
`log_comment` (for example, `SET log_comment='{"app":"my-app","query_name":"home_dashboard"}'`).
Those widgets remain blank until tagging is in place.

### Tuning latency thresholds (optional)

The **p95 latency** and **p99 latency** tiles ship with a sparkline but **no
color thresholds**: what counts as "slow" depends entirely on your workload,
and the dashboard makes no assumption about your target. If you have an SLO,
add a `conditional_formats` block to those tiles to color them.

`@duration` is stored in **nanoseconds**, so thresholds are `seconds × 1e9`
(e.g. `2s = 2000000000`). Datadog applies the **first** matching rule, so order
green → yellow → red as `< low`, `< high`, `>= high` (the same convention the
Health dashboard uses). Example for p95 (green < 2s, yellow 2–5s, red ≥ 5s):

```json
"conditional_formats": [
  {"comparator": "<",  "value": 2000000000, "palette": "white_on_green"},
  {"comparator": "<",  "value": 5000000000, "palette": "white_on_yellow"},
  {"comparator": ">=", "value": 5000000000, "palette": "white_on_red"}
]
```

Paste it into the `requests[0]` object of the p95/p99 tile (alongside
`"response_format": "scalar"`), in the dashboard JSON or via **Edit** on the
widget in Datadog, and adjust the second/third values to your target.

## Exported User Activity

This live-span dashboard uses automatic `query_log.*` enrichment to answer four
related questions without displaying raw SQL:

| Searchable list | Facets |
|---|---|
| **Users and access types** | `query_log.user`, `query_log.access_type` |
| **User → database** | `query_log.user`, `query_log.databases`, `query_log.access_type` |
| **User → table** | `query_log.user`, `query_log.tables`, `query_log.operation` |
| **User → operation** | `query_log.user`, `query_log.operation`, `query_log.access_type` |

Each relationship table keeps its search bar visible. Dashboard template
variables provide searchable filters for user, database, table, operation,
access type, service, environment, and host; a selection applies across the
whole dashboard.

`query_log.operation` comes from ClickHouse's `system.query_log.query_kind`.
Click-dog normalizes it to lowercase and derives `query_log.access_type` as
`read`, `write`, `ddl`, `admin`, or `other`. In cluster query mode the operation
column is enabled only when every replica passes the startup capability probe.
Use `click_dog.query_operation_supported` to distinguish an unsupported
`query_kind` capability from a time range with no exported activity.

> [!WARNING]
> This is an **exported activity** view, not an audit log. Scheduled mode only
> exports traces selected by `monitor.min_trace_duration_ms` and the configured
> query/operation/IP/user filters. Use a dedicated, retention-controlled
> `system.query_log` pipeline when complete activity history is required.

## Health

OTLP self-monitoring for the click-dog exporter. Enable
`metrics.otlp.enabled: true`; by default click-dog reuses
`exporters.otel[0]` and pushes self-metrics to the same collector as spans. Set
`metrics.otlp.host` in containers if you want a stable `host.name` dashboard
variable instead of the pod/container hostname.

Standalone `metrics.otlp` connections currently expose plaintext/TLS settings
only; use `inherit_otel_connection: true` when the collector requires the
span exporter's CA or mTLS client certificate settings.

In the default one-sidecar-per-ClickHouse-node deployment the cockpit tiles
aggregate worst-case across active exporters: `max` circuit breaker state,
`max` backoff interval, and oldest (`min`) `role:active` last-success
timestamp, so a single unhealthy active sidecar is never hidden behind
healthy peers (averaging would also yield fractional breaker values that never
match the `0`/`1`/`2` conditional formats). The **Per-host health** table
directly below ranks sidecars worst-first so the degraded active host is
immediately identifiable; leader-gated standbys stay visible through the
leader column without contributing to stale-export age.

| Section | Widgets |
|---------|---------|
| **Operational cockpit** | Last success age, circuit breaker state (`0=closed`, `1=half-open`, `2=open`), current error rate, export throughput, backoff interval: fleet worst-case |
| **Per-host health** | Per-sidecar circuit breaker, last-success age, and backoff, ranked worst-first by breaker state |
| **Current trends** | Exported/filtered/duplicate rates, cycle and error rates, backoff and circuit breaker timelines |
| **Last cycle** | Most recent cycle duration and exported/filtered/duplicate span counts |
| **Lifetime counters** | Uptime, cycles total, errors total |
| **Outcome breakdown** | Cycle results by outcome (`success`, `error`, `skipped`) |

### Legacy Datadog Agent OpenMetrics scrape config

Prometheus-only users can still scrape `/metrics`. Drop this in
`/etc/datadog-agent/conf.d/openmetrics.d/conf.yaml` and restart the Agent. The
explicit `metrics:` rename list is required for the scrape path; a generic
wildcard such as `- click_dog_*` will not produce usable Datadog names.

```yaml
init_config:

instances:
  - openmetrics_endpoint: http://localhost:9090/metrics
    namespace: click_dog
    metrics:
      # Counters: Datadog appends `.count` and submits as a monotonic rate.
      - click_dog_spans_exported_total: spans_exported
      - click_dog_spans_filtered_total: spans_filtered
      - click_dog_spans_duplicates_total: spans_duplicates
      - click_dog_export_attempts_total: export.attempts
      - click_dog_export_accepted_total: export.accepted
      - click_dog_export_errors_total: export.errors
      - click_dog_cycle_results_total: cycle_results
      # Gauges: submitted as-is under the namespace.
      - click_dog_circuit_breaker_state: circuit_breaker.state
      - click_dog_leader: leader
      - click_dog_backoff_interval_seconds: backoff_interval.seconds
      - click_dog_last_success_timestamp_seconds: last_success_timestamp.seconds
      - click_dog_uptime_seconds: uptime.seconds
      - click_dog_last_cycle_duration_seconds: last_cycle.duration_seconds
      - click_dog_last_cycle_exported_spans: last_cycle.exported_spans
      - click_dog_last_cycle_filtered_spans: last_cycle.filtered_spans
      - click_dog_last_cycle_duplicate_spans: last_cycle.duplicate_spans
      # ClickHouse data-plane health (#183).
      - click_dog_span_log_last_poll_timestamp_seconds: span_log.last_poll_timestamp.seconds
      - click_dog_span_log_newest_row_age_seconds: span_log.newest_row_age.seconds
      - click_dog_span_log_rows_last_cycle: span_log.rows_last_cycle
      - click_dog_query_log_enrichment_attempts_total: query_log.enrichment.attempts
      - click_dog_query_log_enrichment_successes_total: query_log.enrichment.successes
      - click_dog_query_log_enrichment_failures_total: query_log.enrichment.failures
      - click_dog_query_log_enrichment_match_ratio: query_log.enrichment.match_ratio
      - click_dog_spans_with_query_id_ratio: spans_with_query_id_ratio
      - click_dog_normalized_query_supported: normalized_query_supported
      - click_dog_query_operation_supported: query_operation_supported
      # Topology self-audit: detects the sidecar + use_cluster_queries anti-pattern.
      - click_dog_topology_warning: topology_warning
```

That mapping turns each Prometheus metric exposed at `/metrics` into the
legacy Datadog scrape-path name. The shipped Health dashboard queries the OTLP
default names directly; Datadog appends `.count` to OTLP monotonic sums.

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
| `click_dog_query_operation_supported`         | gauge   | `click_dog.query_operation_supported`     |
| `click_dog_topology_warning`                  | gauge   | `click_dog.topology_warning` (tag: `reason`) |

The bottom block carries ClickHouse data-plane health signals (#183). See
[Datadog Self-Monitoring: ClickHouse data-plane health signals](https://click-dog.com/integrations/datadog/self-monitoring/#clickhouse-data-plane-health-signals).

### Recommended monitors

Once the Health dashboard is rendering data, the canonical alerting tiers
(stale exports, circuit breaker not closed, cycle error ratio, OTLP self-metrics
no-data, plus the optional webhook events) are documented in
[Datadog: Recommended Monitors](https://click-dog.com/integrations/datadog/monitors/).

### Troubleshooting

If health-dashboard widgets show "No data" after import:

1. Confirm `metrics.otlp.enabled: true`.
2. Confirm the Datadog Agent or collector OTLP gRPC receiver is reachable from
   click-dog.
3. In containers, set `metrics.otlp.host` to the stable node/instance identity
   you want to use for the `host.name` template variable.
4. On the legacy scrape path, search Metrics Explorer for
   `click_dog.click_dog_spans_exported_total`; that means the explicit
   OpenMetrics `metrics:` rename list above is not being applied.

## Prerequisites

- Click-dog running and exporting spans
- Datadog Agent receiving OTLP traces (for query analysis)
- `metrics.otlp.enabled: true` for the health dashboard
- Legacy scrape users only: Datadog Agent OpenMetrics check. See the explicit
  rename list above

## Other Platforms

Dashboard templates for other platforms coming soon:
- Honeycomb
- Grafana (Tempo)
