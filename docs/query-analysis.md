# Query Analysis

Click-Dog exports spans, but the goal is to **explain query behavior** — not just
ship traces. It groups statements into the handful of shapes that matter, enriches
slow-query traces with query-log context, and ships ready-made dashboards.

## What you get

| Capability | What it does |
|---|---|
| **Normalized query families** | Group thousands of statements into the handful of shapes that actually matter, keyed by ClickHouse's `normalized_query_hash`. |
| **Slow-query traces** | Every exported span enriched with `system.query_log` context and `log_comment` metadata, so a slow trace carries the query that caused it. |
| **Ready-made dashboards** | Datadog **Application Query Analysis** dashboard out of the box. |
| **Per-sink export health** | Know exactly which backend is healthy and which is dropping spans. |

## Local analysis report (`analyze queries`)

`click-dog analyze queries` builds a bounded, **read-only** analysis report
directly from `system.query_log` and `system.opentelemetry_span_log`: query-family
resource outliers, `log_comment` attribution gaps, user / client / host skew, and
coverage prerequisites. It opens a single read-only ClickHouse connection and
nothing is written to ClickHouse. The command still loads config through the
normal loader, so exporter fields are expanded and `*_file` secret paths are read
at config-load time, but no exporter clients are constructed and no exporter
connections are opened.

```bash
# Human-readable table for the last hour (default window)
./click-dog analyze queries -config click-dog.yaml

# Widen the window and emit JSON for tooling
./click-dog analyze queries -config click-dog.yaml -lookback 24h -format json -output report.json

# Redact user/client/host dimension values in the report
./click-dog analyze queries -config click-dog.yaml --redact-dimensions
```

| Flag | Default | Purpose |
|---|---|---|
| `-lookback` | `1h` | Analysis window ending at command start (e.g. `30m`, `24h`) |
| `-timeout` | `2m` | Command-level timeout for the run |
| `-format` | `table` | `table` or `json` |
| `-output` | stdout | Write the report to a path instead of stdout |
| `--redact-dimensions` | off | Redact user/client/host dimension values |
| `-min-executions` | `3` | Minimum executions for exact query-family groups |
| `-family-limit` | `200` | Exact normalized groups fetched before rollup |
| `-span-sample-limit` | `0` | Max spans sampled for attribution and coverage (`0` = auto) |
| `-query-preview-length` | `500` | Max normalized-query preview length in the report |
| `-config` | auto | Path to the configuration file. When omitted, tries `/etc/click-dog/click-dog.yaml` then `./click-dog.yaml` |

!!! note "Reports are operational artifacts"
    JSON reports contain normalized SQL and dimension values that can reveal
    schema and ownership shape — treat them like logs and traces.

## Trace drilldown (`analyze trace`)

`click-dog analyze trace` is the incident-response inverse of
`analyze queries`: start from a running query, recent query-log row, query ID,
trace ID, or normalized query hash, then fetch the native ClickHouse trace spans
that carry the query and fan out to query-log stats, the matching query-family
rollup, and any findings for that family.

```bash
# Guided flow
./click-dog analyze trace -config click-dog.yaml -wizard

# Search recent finished/failed queries
./click-dog analyze trace -config click-dog.yaml -source recent -match events

# Drill explicit identities
./click-dog analyze trace -config click-dog.yaml -query-id abc123
./click-dog analyze trace -config click-dog.yaml -trace-id <trace-id>
./click-dog analyze trace -config click-dog.yaml -normalized-query-hash 123456789
```

Like `analyze queries`, the trace drilldown is local and read-only: no exporters,
metrics or health servers, leader election, circuit breaker, or adaptive polling
are started. Missing trace spans, unsupported rollups, or no matching findings
produce warnings or empty sections rather than a command failure.

## AI-ready JSON

The JSON report is the supported boundary for AI-assisted workflows. It is
deterministic, schema-versioned, and built from the same bounded read-only
analysis path as the table output. Prefer giving assistants this report over
granting direct ClickHouse access.

```bash
./click-dog analyze queries \
  -config click-dog.yaml \
  -lookback 24h \
  -format json \
  --redact-dimensions \
  -output report.json
```

Use `--redact-dimensions` before sharing reports with external services. The
report can still reveal normalized SQL and schema shape, so review it with the
same care you would apply to logs or traces.

Machine-readable references:

- [`analysis.report.v1` schema](schemas/analysis-report-v1.schema.json)
- [`analysis.finding.v1` schema](schemas/analysis-finding-v1.schema.json)
- [Redacted report example](examples/analysis-report-redacted.json)

Click-Dog does not run LLM calls in the export path, and the launch surface does
not include MCP. AI narration or recommendation providers should consume these
JSON artifacts downstream.

## Datadog dashboards

Provision the Application Query Analysis dashboard from the Datadog API:

```bash
click-dog create-dashboards --dashboard query
```

The query dashboard reads the exported traces. A separate **Health** dashboard
reads Click-Dog's own metrics — see
[Datadog § Self-Monitoring](integrations/datadog.md#click-dog-self-monitoring)
for the OTLP self-metrics setup and legacy OpenMetrics fallback.

## How families are derived

ClickHouse assigns each statement a `normalized_query_hash`; Click-Dog rolls those
hashes up into query *families* and attributes sampled spans back to them. The
normalized query family and its attributes are documented in the
[Span Attributes reference](span-attributes.md). Filtering controls which spans get
exported in the first place — see [Filtering](filtering.md).

## Related

- [Span Attributes](span-attributes.md) — the normalized query family and every exported attribute
- [Architecture](architecture.md) — where analysis sits in the pipeline
- [Operation Modes](modes.md) — backfill historical `system.query_log` for analysis
- [Integrations](integrations/datadog.md) — wiring traces and metrics into your backend
