# Operation Modes

Click-Dog supports two long-running modes — **scheduled** (continuous
monitoring) and **backfill** (one-shot historical export) — plus two
short-lived inspection modes (**validate** and **dry-run**) and a
separate `click-dog deploy` subcommand family for emitting Kubernetes /
Docker manifests. Scheduled and backfill can run simultaneously as two
separate processes.

## Scheduled Mode

Continuously polls ClickHouse's `system.opentelemetry_span_log` table at a configurable interval and exports matching spans to your OTEL collector.

### How It Works

1. Queries `system.opentelemetry_span_log` for trace IDs containing spans within the configured duration range
2. Fetches all spans for those traces (optionally filtered by span-level duration)
3. Deduplicates against an LRU cache to avoid re-exporting
4. Applies operation, IP, and query filters
5. Exports to the OTEL collector via gRPC
6. Waits `check_interval_s` seconds, then repeats

### Usage

```bash
# With default config (click-dog.yaml)
./click-dog

# With custom config
./click-dog -config /path/to/config.yaml
```

### Configuration

```yaml
monitor:
  enabled: true
  min_trace_duration_ms: 1000  # Find traces with spans >= 1 second
  min_span_duration_ms: 0      # Export all spans from those traces
  check_interval_s: 30         # Poll every 30 seconds
  lookback_s: 40               # Look back 40 seconds each poll
```

### Key Behaviors

- **Runs on startup** — the first poll happens immediately, then repeats at `check_interval_s`
- **Deduplication** — an LRU cache (default 10,000 entries) keyed by the composite OTLP span identity `(trace_id, span_id)` prevents re-exporting the same spans across polling cycles. Span IDs are only unique within a trace, so the trace ID is part of the key
- **Lookback overlap** — set `lookback_s` slightly larger than `check_interval_s` (the default adds 10 seconds) to avoid gaps between polls
- **Graceful shutdown** — responds to SIGINT and SIGTERM. If a polling cycle is in progress, it completes before shutdown. The gRPC connection to the OTEL collector is closed with a 5-second timeout. The in-memory dedup cache is not persisted — after restart, spans still within the lookback window will be re-exported once (OTEL collectors dedup by `(trace_id, span_id)`)
- **Resilience** — supports circuit breaker and adaptive backoff to protect ClickHouse during failures

### Data Source

Scheduled mode reads from `system.opentelemetry_span_log`, which contains OpenTelemetry spans generated internally by ClickHouse. This includes:
- Query execution spans
- Internal ClickHouse operations (merges, mutations, etc.)
- Distributed query coordination spans

---

## Backfill Mode

One-time export of historical data from ClickHouse's `system.query_log` table for a specific time range.

### How It Works

1. Queries `system.query_log` for queries in the specified time range matching the duration threshold
2. Filters by `QueryFinish` and `ExceptionWhileProcessing` event types
3. Applies configured filters
4. Exports each query as a trace to the OTEL collector
5. Exits when complete

### Usage

```bash
# Backfill a specific time range
./click-dog \
  -config config.yaml \
  -backfill-start "2024-01-15T00:00:00Z" \
  -backfill-end "2024-01-15T23:59:59Z"
```

Time format must be RFC3339 (e.g., `2024-01-15T14:30:00Z`).

### Configuration

```yaml
monitor:
  enabled: false               # Disable scheduled mode (optional)
  min_trace_duration_ms: 1000  # Only export queries >= 1 second
  max_spans_per_cycle: 5000    # Limit total queries per run
  batch_size: 100              # Process in batches of 100
  batch_delay_ms: 50           # 50ms between batches
```

### Key Behaviors

- **Exits when done** — unlike scheduled mode, backfill runs once and exits
- **No deduplication cache** — since it's a one-time run, there's no need for the LRU cache
- **Rate controllable** — use `max_spans_per_cycle`, `batch_size`, and `batch_delay_ms` to control the export rate
- **Different data source** — reads from `system.query_log` (not `system.opentelemetry_span_log`), which provides richer query metadata
- **Filters applied** — IP whitelist and query blacklist filters work as expected. However, operation whitelist does not apply to backfill mode — `query_log` entries have no operation name, so if `whitelist_operations` is set, all backfill queries would be filtered out. Leave `whitelist_operations` empty when using backfill
- **Exit code reflects export outcome** — backfill exits 0 only when every (non-filtered) query exported successfully. Any export failure exits non-zero. The final log line and the `backfill_failed` webhook (if enabled) include `queries=N exported=N filtered=N failed=N` so an operator can tell whether the failure was partial (some data landed) or total (none did) and decide how to recover

### Data Source

Backfill mode reads from `system.query_log`, which contains finished query events. This provides attributes not available in the span log, such as:
- Read/write row and byte counts
- Memory usage
- Databases and tables accessed
- Exception codes

---

## Running Both Modes Together

You can run scheduled monitoring and backfill simultaneously as **two separate processes**. This is useful for:
- Recovering from an outage while continuing real-time monitoring
- Backfilling a historical period for investigation

```bash
# Terminal 1: Continuous monitoring
./click-dog -config config.yaml

# Terminal 2: Backfill a specific incident window
./click-dog -config config.yaml \
  -backfill-start "2024-01-15T10:00:00Z" \
  -backfill-end "2024-01-15T11:00:00Z"
```

### Chunked Backfill

For large time ranges, split the backfill into smaller chunks to control load:

```bash
# Process hour by hour
./click-dog -config config.yaml \
  -backfill-start "2024-01-15T00:00:00Z" \
  -backfill-end "2024-01-15T01:00:00Z"

./click-dog -config config.yaml \
  -backfill-start "2024-01-15T01:00:00Z" \
  -backfill-end "2024-01-15T02:00:00Z"

# ... and so on
```

## Validate Mode

Validates the configuration file and prints parsed settings without connecting to anything. Useful for checking config before deployment.

### Usage

```bash
./click-dog -config config.yaml -validate
```

### Output

Prints parsed settings including:
- ClickHouse connection details
- Configured exporters (count and types)
- Monitor settings (durations, intervals, batching)
- HA status
- Filter summary

Exits with code 0 if valid, non-zero if there are errors.

---

## Dry-run Mode

Runs **one** cycle of the pipeline against your real ClickHouse — same
queries, same filters, same dedup — then prints a cumulative summary
(traces, spans, queries, top operations) and exits. Routes exports
through `DryRunExporter`, so nothing is sent to OTEL or Splunk HEC.

Use it to:
- preview what a new filter or duration threshold will export before
  pointing the binary at a real collector;
- diagnose "why isn't this span being exported?" without sending traffic
  to your observability bill;
- smoke-test a config on a production cluster without writing data.

### Usage

```bash
# Dry-run the current lookback window (one cycle, then exit)
./click-dog --dry-run -config click-dog.yaml

# Dry-run a historical window (backfill, then exit)
./click-dog --dry-run -config click-dog.yaml \
  -backfill-start "2024-01-15T00:00:00Z" \
  -backfill-end   "2024-01-15T23:59:59Z"
```

### Key Behaviors

- **Single cycle, then exits** — scheduled `--dry-run` runs exactly the
  startup-check cycle (one `pipeline.Process` call), prints the summary,
  and returns. It does **not** enter the ticker loop. Backfill
  `--dry-run` runs the configured window, prints the summary, and exits
- **Real reads, no writes** — ClickHouse is queried normally. The OTEL /
  Splunk HEC connections are never opened
- **Summary shape** — `Traces`, `Spans`, `Queries`, and a "Top
  operations" table (top 10 by count) are printed to stdout via
  `DryRunExporter.PrintSummary`
- **Sink label** — Prometheus metrics emitted in the cycle carry
  `sink="dry_run"` so dashboards can distinguish dry-run traffic from
  live exports
- **No side effects** — webhook notifications still fire on lifecycle
  events (startup, shutdown), but nothing is exported

> **Want a continuous dry-run loop?** That isn't supported today —
> scheduled `--dry-run` exits after one cycle. If you need rolling
> summaries, re-invoke periodically (cron, `watch`, etc.) or open a
> follow-up issue.

---

## `click-dog deploy` (manifest generators)

`click-dog deploy` is a separate subcommand family that **emits** rather
than runs — it renders deployment manifests for orchestrators and
reports on installed state. It does not start the monitoring loop.

| Subcommand | Purpose |
|------------|---------|
| `click-dog deploy kubernetes -c COLLECTOR -ch-host CH_SVC` | Render a Deployment + ConfigMap + Secret to `./click-dog-k8s/` |
| `click-dog deploy docker -c HOST` | Render a Docker Compose stack to `./click-dog-docker/` |
| `click-dog deploy status` | Report installed version, systemd active/enabled state, and the local `/healthz` response. Add `--json` for machine output |

The Kubernetes and Docker generators take the same `-c` (collector
address), `-u` / `-p` (ClickHouse credentials), `-o` (output directory),
and `-v` (version) flags. Pass `--update` to refresh the image tag in an
existing output directory without overwriting other fields.

`click-dog deploy status` exits `0` only when systemd reports active
**and** `/healthz` returns `200`; otherwise `1`. Plain-text output is
printed in both cases.

See [Install](install.md) for the equivalent `install.sh kubernetes` /
`install.sh docker` recipes — they invoke the same generators.

---

## Comparison

| | Scheduled | Backfill | Validate | Dry-run | `deploy` |
|---|---|---|---|---|---|
| **Data source** | `system.opentelemetry_span_log` | `system.query_log` | None | Same as scheduled / backfill | None |
| **Duration** | Runs continuously | Runs once, then exits | Exits immediately | Runs once, then exits | Exits immediately |
| **Time range** | Rolling window (`lookback_s`) | Fixed range (`-backfill-start` / `-backfill-end`) | N/A | Same as the mode it shadows | N/A |
| **Deduplication** | LRU cache prevents re-exports | Not needed (one-time run) | N/A | Active during the single cycle | N/A |
| **Resilience** | Circuit breaker + adaptive backoff | None (single run) | N/A | Single-cycle — no loop to back off | N/A |
| **Exports anything?** | Yes | Yes | No | **No** — discards via `DryRunExporter` | No |
| **Use case** | Real-time monitoring | Historical analysis, outage recovery | Config verification | Preview / smoke-test without sending | Generate manifests, check installed state |
