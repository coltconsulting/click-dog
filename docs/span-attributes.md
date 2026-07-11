# Span Attributes Reference

Click-Dog exports two types of spans depending on the operation mode. Each type carries different attributes.

## Scheduled Mode — OpenTelemetry Span Log

In scheduled mode, click-dog reads from `system.opentelemetry_span_log` and re-exports spans with their original structure preserved.

### Resource Attributes

| Attribute | Value |
|---|---|
| `service.name` | Configured `exporters.otel[].service_name` (default: `click-dog-monitor`) |

### Span Fields

| Field | Description |
|---|---|
| `trace_id` | Original ClickHouse trace ID (UUID, 16 bytes) |
| `span_id` | Original span ID (uint64, 8 bytes big-endian) |
| `parent_span_id` | Parent span ID (uint64, 0 if root span) |
| `name` | Operation name (e.g., `DB::InterpreterSelectQuery::execute()`) |
| `kind` | One of: INTERNAL, SERVER, CLIENT, PRODUCER, CONSUMER |
| `start_time` | Microsecond precision from ClickHouse |
| `end_time` | Microsecond precision from ClickHouse |
| `status` | Always OK |

### Span Attributes

| Attribute | Type | Description |
|---|---|---|
| `click_dog.source` | string | Always `span_log` in scheduled mode (added by click-dog; distinguishes live spans from backfill's `query_log`) |
| `hostname` | string | ClickHouse server hostname (added by click-dog from the span log's `hostname` column) |
| `duration_ms` | int64 | Computed duration in milliseconds (added by click-dog) |
| `db.statement` | string | SQL query text (from ClickHouse span attributes) |
| `client.address` | string | Client IP address (from ClickHouse span attributes) |
| `log_comment.*` | string | Extracted from `log_comment` query parameter in URI attributes (see below) |
| `log_comment` | string | Raw `log_comment` value when it is not valid JSON |
| *additional* | string | All other attributes from the ClickHouse `attribute` map are passed through as-is |

The `attribute` map in ClickHouse's span log may contain various keys depending on the operation type and ClickHouse version. Click-dog forwards all of them.

### log_comment Extraction

ClickHouse clients can tag queries using the `log_comment` query setting. When this value appears URL-encoded in a span's URI attributes (`http.url`, `http.target`), click-dog extracts it automatically.

If the value is JSON, each key is promoted to a span attribute prefixed with `log_comment.`:

```
log_comment={"app": "Dash8", "query_name": "what_watched"}
```

Produces:

| Attribute | Value |
|---|---|
| `log_comment.app` | `Dash8` |
| `log_comment.query_name` | `what_watched` |

If the value is not JSON, it is set as a single `log_comment` attribute.

This is enabled by default. To disable, set `monitor.extract_log_comment: false` in the config.

### query_log Enrichment

When `monitor.enrich_from_query_log` is enabled (default: `true`), click-dog automatically enriches spans with metadata from `system.query_log`. The join key is `query_id`, which appears as a primary column in `query_log` and as the `clickhouse.query_id` attribute on `opentelemetry_span_log` spans.

For each batch of exported spans, click-dog collects unique `clickhouse.query_id` values, queries `query_log` for matching rows, and adds the following attributes to each span whose query_id matches:

| Attribute | Type | Description |
|---|---|---|
| `query_log.query_id` | string | ClickHouse query ID |
| `query_log.user` | string | ClickHouse user that executed the query |
| `query_log.client_name` | string | Client application/library name |
| `query_log.client_hostname` | string | Client hostname |
| `query_log.client_address` | string | Client IP address |
| `query_log.query_duration_ms` | int64 | Query duration in milliseconds |
| `query_log.normalized_query_hash` | string | Exact ClickHouse normalized query hash for grouping query shapes (only when supported by ClickHouse) |
| `query_log.normalized_query` | string | Truncated `normalizeQuery(query)` preview with literals removed (only when supported by ClickHouse) |
| `query_log.read_rows` | int64 | Number of rows read |
| `query_log.read_bytes` | int64 | Bytes read |
| `query_log.written_rows` | int64 | Number of rows written |
| `query_log.written_bytes` | int64 | Bytes written |
| `query_log.result_rows` | int64 | Number of result rows |
| `query_log.result_bytes` | int64 | Result size in bytes |
| `query_log.memory_usage` | int64 | Peak memory usage in bytes |
| `query_log.exception_code` | int | ClickHouse exception code (only when non-zero) |
| `query_log.databases` | string[] | List of databases accessed (only when non-empty) |
| `query_log.tables` | string[] | List of tables accessed (only when non-empty) |
| `query_log.databases_csv` | string | Legacy-compatible comma-separated list of databases accessed (only when non-empty) |
| `query_log.tables_csv` | string | Legacy-compatible comma-separated list of tables accessed (only when non-empty) |

`query_log.databases` and `query_log.tables` are emitted as OTLP string-array
attributes in scheduled/live mode so Datadog facets fan out by each individual
database or table. The `_csv` fields preserve the previous joined-string shape
for dashboards or monitors that already match exact table/database combinations.

Enrichment is non-fatal: if the query fails for any reason, a warning is logged (with exponential backoff) and spans are exported without enrichment. Spans without a `clickhouse.query_id` attribute are skipped entirely.

!!! warning "Enrichment scope — root query spans only"
    Only spans carrying the `clickhouse.query_id` attribute receive `query_log.*` attributes. In practice, this is the root query span for each ClickHouse query — child spans (execution stages, merges, etc.) within the same trace are **not** enriched. This is by design: `query_id` identifies the query, not the trace, so attaching query-level stats to internal sub-operations would be misleading. If you need `query_log.*` attributes on all spans in a trace, use your OTEL collector to propagate them from the root span.

!!! note "Distributed queries"
    Distributed queries generate separate `query_log` rows per shard, each with its own `query_id`. Each matching span is enriched independently; stats like `query_log.read_rows` reflect the per-shard execution, not the full distributed query total.

!!! note "Privacy"
    `query_log.user`, `query_log.client_address`, and `query_log.client_hostname` may contain identity or internal network information. `query_log.normalized_query` removes literal values but can still reveal table names, column names, and query shape. If your spans are exported to an external backend (Datadog, Honeycomb, etc.), consider filtering these attributes at your OTEL collector layer if PII, internal IPs, schema names, or query shapes should not leave your network.

To disable, set `monitor.enrich_from_query_log: false` in the config.

---

## Trace Context Propagation

ClickHouse supports [W3C Trace Context](https://www.w3.org/TR/trace-context/) propagation over its HTTP interface. When a client sends the `traceparent` header with queries, ClickHouse continues the upstream trace ID for all internal spans it generates.

Click-dog preserves trace IDs from `system.opentelemetry_span_log` as-is — so if your application propagates `traceparent`, the ClickHouse spans exported by click-dog will share the same trace ID as your application spans. **Trace linking works automatically** with no click-dog configuration needed.

### Setup

1. Enable `opentelemetry_span_log` in ClickHouse (already required for click-dog)
2. Configure your ClickHouse client to send the `traceparent` header with the current trace context
3. No click-dog config changes needed

### Client Examples

**Python (clickhouse-connect):**
```python
import clickhouse_connect
from opentelemetry import trace

tracer = trace.get_tracer(__name__)
with tracer.start_as_current_span("my-query"):
    # clickhouse-connect automatically propagates trace context
    # when opentelemetry-api is installed
    client.query("SELECT count() FROM events")
```

**Go (clickhouse-go):**
```go
// Pass a context with an active span — clickhouse-go propagates
// the trace context via the native protocol.
ctx, span := tracer.Start(ctx, "my-query")
defer span.End()
rows, err := conn.Query(ctx, "SELECT count() FROM events")
```

**HTTP (any language):**
```
GET /?query=SELECT+1 HTTP/1.1
Host: clickhouse:8123
traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
```

---

## Backfill Mode — Query Log

In backfill mode, click-dog reads from `system.query_log` and creates new single-span traces for each query.
Trace and span IDs are deterministic for each query-log row (`query_id`, `event_time`, and `query_kind`), so retries or reruns of the same backfill window produce the same span identity.

### Resource Attributes

| Attribute | Value |
|---|---|
| `service.name` | Configured `exporters.otel[].service_name` (default: `click-dog-monitor`) |

### Span Fields

| Field | Description |
|---|---|
| `trace_id` | Deterministically derived from the query-log row identity |
| `span_id` | Deterministically derived from the query-log row identity |
| `name` | `clickhouse.query` |
| `kind` | CLIENT |
| `start_time` | `event_time - query_duration` |
| `end_time` | `event_time` from query log |

### Span Attributes

#### Query Identity

| Attribute | Type | Description |
|---|---|---|
| `click_dog.source` | string | Always `query_log` in backfill mode (added by click-dog; distinguishes backfill spans from scheduled mode's `span_log`) |
| `db.system` | string | Always `clickhouse` |
| `db.statement` | string | SQL query text (truncated per `exporters.otel[].max_query_length`) |
| `db.query_id` | string | ClickHouse query ID |
| `db.query_kind` | string | Event type: `QueryFinish` or `ExceptionWhileProcessing` |
| `db.query_duration_ms` | int64 | Query duration in milliseconds |
| `db.normalized_query_hash` | string | Exact ClickHouse normalized query hash for grouping query shapes (only when supported by ClickHouse) |
| `db.normalized_query` | string | Truncated `normalizeQuery(query)` preview with literals removed (only when supported by ClickHouse) |
| `db.user` | string | ClickHouse user that executed the query |

#### Client Information

| Attribute | Type | Description |
|---|---|---|
| `client.address` | string | Client IP address |
| `client.name` | string | Client application name |
| `client.hostname` | string | Client hostname |

#### Query Statistics

| Attribute | Type | Description |
|---|---|---|
| `db.read_rows` | int64 | Number of rows read |
| `db.read_bytes` | int64 | Bytes read |
| `db.written_rows` | int64 | Number of rows written |
| `db.written_bytes` | int64 | Bytes written |
| `db.result_rows` | int64 | Number of result rows |
| `db.result_bytes` | int64 | Result size in bytes |
| `db.memory_usage` | int64 | Peak memory usage in bytes |

#### Scope Information

| Attribute | Type | Description |
|---|---|---|
| `db.databases` | string[] | List of databases accessed (if any) |
| `db.tables` | string[] | List of tables accessed (if any) |

#### Error Information

Present only when the query failed (`ExceptionWhileProcessing`):

| Attribute | Type | Description |
|---|---|---|
| `error` | bool | `true` if the query failed |
| `db.exception_code` | int | ClickHouse exception code |

---

## Query Text Truncation

SQL query text (`db.statement`) and normalized query previews (`db.normalized_query`, `query_log.normalized_query`) are bounded before export:

1. **Fetch-time** (`monitor.max_query_length`): Queries with SQL longer than this are skipped entirely during the ClickHouse query
2. **Live enrichment** (`monitor.max_query_length`): `query_log.normalized_query` is truncated to this length with `...` appended
3. **Export-time** (`exporters.otel[].max_query_length` / `exporters.splunk_hec[].max_query_length`): `db.statement`, `db.normalized_query`, and Splunk query fields are truncated to this length with `...` appended

All default to 100,000 characters after config loading. A value of `0` or an
omitted key is coerced to the 100,000-character default for
`monitor.max_query_length`, `exporters.otel[].max_query_length`, and
`exporters.splunk_hec[].max_query_length`, so `0` does not disable these limits
at the config layer.

Normalized query attributes are exact ClickHouse-native query-family primitives: `normalized_query_hash` is the grouping key, and `normalized_query` is a human-readable literal-stripped preview. They do not perform fuzzy or semantic rollups across related SQL shapes; use `log_comment` for explicit application intent.

## Query Family Rollups

Click-dog also has a higher-level query-family rollup path for dashboard and offline analysis code. This is not part of the hot live-export path and it does not cluster individual query executions. The reader first aggregates `system.query_log` into exact groups keyed by one `normalized_query_hash`, then performs deterministic in-memory rollups over those exact groups.

Product contract:

- **Exact group**: one `normalized_query_hash`, one bounded representative `normalizeQuery(query)` preview, and aggregate stats from matching `system.query_log` rows.
- **Rollup family**: one or more exact groups that pass conservative structural similarity checks. The family preserves its member `normalized_query_hash` values, a representative normalized query preview, merge explanations, and family-level stats.

The baseline rollup algorithm extracts statement kind, safely referenced tables, major clause structure (`WHERE`, `JOIN`, `GROUP BY`, `ORDER BY`, `LIMIT`), token sets, and token shingles from normalized SQL. It uses Jaccard-style similarity over those features with hard blockers for different statement kinds and disjoint safely extracted tables. There is no AI/LLM naming in this path. `log_comment` remains the strongest way to attach application or business intent.

Family stats are derived from exact-group aggregates. Counts are summed; memory and percentile-like stats are conservatively rolled up from member groups because raw executions are not clustered. Top users, clients, and tables are combined from the exact groups' top values.

Privacy: normalized query previews remove literal values, but they can still reveal table names, column names, and query shape. Rollup representative previews are bounded/truncated for display, but they should still be treated as schema-sensitive telemetry.

Dashboard/export limitation: the shipped Datadog query dashboard consumes span attributes, so it currently shows exact normalized query families (`query_log.normalized_query_hash` and `query_log.normalized_query`) rather than these higher-level rollup families. A future export or storage surface can expose rollup family IDs and labels cleanly without forcing them into per-span trace export.

---

## Semantic Conventions

Click-Dog follows [OpenTelemetry Semantic Conventions](https://opentelemetry.io/docs/specs/semconv/) where applicable:

- `db.system` — identifies the database system
- `db.statement` — the database statement being executed
- `db.user` — the database user
- `client.address` — client IP address
- `service.name` — the logical service name
