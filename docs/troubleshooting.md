# Troubleshooting

## Enable Debug Logging

The first step for any issue is to enable debug-level logging:

```yaml
log_level: debug
```

This reveals detailed information about each polling cycle:

```
[DEBUG] Found 23 spans
[DEBUG] Processing batch 1-23 of 23 spans
[DEBUG] Filtering span from IP not in whitelist: 172.16.0.5
[DEBUG] Filtering operation not in whitelist: TCPHandler::runImpl()
[INFO] Exported: 15, Filtered: 3, Duplicates: 5
```

---

## Common Issues

### No Spans Being Exported

**Symptom:** `Exported: 0` every cycle.

**Fastest path: run the readiness check.** `click-dog check` automates
the manual SQL below — it reports whether `system.opentelemetry_span_log` and
`system.query_log` are readable and granted, whether recent spans exist, whether
your `min_trace_duration_ms` threshold excludes everything, whether spans carry
`clickhouse.query_id` (required for enrichment and user filters), whether the
query-log enrichment join returns rows, and whether `normalized_query_hash` is
available. Each warn/fail line prints the exact SQL or config prerequisite to
fix. Use `--lookback` to widen the window (default `24h`). The manual
checks below remain useful for ad-hoc spelunking.

**Check 1: ClickHouse has OpenTelemetry data**

```sql
SELECT count() FROM system.opentelemetry_span_log
WHERE finish_date >= today();
```

If this returns 0, OpenTelemetry logging isn't enabled in ClickHouse. See [Install — Prerequisites](install.md#clickhouse-opentelemetry).

**Check 2: Duration threshold is too high**

```sql
SELECT
    count(),
    min((finish_time_us - start_time_us) / 1000) as min_duration_ms,
    max((finish_time_us - start_time_us) / 1000) as max_duration_ms,
    avg((finish_time_us - start_time_us) / 1000) as avg_duration_ms
FROM system.opentelemetry_span_log
WHERE finish_date >= today();
```

If your `min_trace_duration_ms` is higher than `max_duration_ms`, no spans will match. Lower the threshold.

**Check 3: Filters are too restrictive**

Set `log_level: debug` and look for filtering messages. Try temporarily removing all filters:

```yaml
filters:
  whitelist_operations: []
  whitelist_ips: []
  blacklist_queries: []
```

**Check 4: All spans are deduplicated**

If you see `Duplicates: N` with a high count, the same spans are being found each cycle. This is normal if there are no new slow queries. The dedup cache is working correctly.

---

### Connection Refused to ClickHouse

**Symptom:** `Failed to connect to ClickHouse` or `connection refused`.

- Verify ClickHouse is running and accepting connections on the configured port
- Check that the port is the **native protocol port** (default 9000), not the HTTP port (8123)
- If using TLS (`secure: true`), make sure ClickHouse is configured for TLS on that port (typically 9440)
- Check firewall rules between click-dog and ClickHouse

---

### Connection Refused to OTEL Collector

**Symptom:** `Failed to create gRPC connection` or `connection refused` on the OTEL side.

- Verify the OTEL collector is running and listening on the configured port (default 4317)
- For Datadog Agent: ensure `otlp_config.receiver.protocols.grpc.endpoint` is configured
- Check that you're using the gRPC port (4317), not the HTTP port (4318)
- If using TLS, verify certificates are correct

---

### Spans Exported But Not Appearing in Backend

**Symptom:** Click-dog reports `Exported: N` but traces don't appear in Datadog/Honeycomb/etc.

- Check the OTEL collector logs for errors
- Verify the `service_name` matches what you're searching for in your backend
- Some backends have ingestion delays — wait a few minutes
- For Datadog: check the Agent logs for OTLP ingestion errors
- For Honeycomb: verify the API key and dataset name

---

### High Memory Usage

**Symptom:** Click-dog memory grows over time.

- Reduce `dedup_cache_size` (default 10,000). The LRU cache stores `(trace_id, span_id)` keys in memory
- Reduce `max_spans_per_cycle` to process fewer spans per cycle
- Ensure `query_timeout_s` is set to prevent long-running queries from accumulating

---

### Circuit Breaker Keeps Opening

**Symptom:** `Circuit breaker opened after N consecutive failures` repeating.

- Check ClickHouse health directly: `clickhouse-client -q "SELECT 1"`
- Look at the error messages before the circuit breaker opens
- Increase `failure_threshold` if transient errors are expected
- Increase `reset_timeout_s` to give ClickHouse more recovery time

---

### Backfill Producing No Results

**Symptom:** `Found 0 slow queries in range`.

**Check 1: Data exists in the time range**

```sql
SELECT count(), min(event_time), max(event_time)
FROM system.query_log
WHERE event_time >= '2024-01-15 00:00:00'
  AND event_time <= '2024-01-15 23:59:59';
```

**Check 2: Queries meet the duration threshold**

```sql
SELECT count(), max(query_duration_ms)
FROM system.query_log
WHERE event_time >= '2024-01-15 00:00:00'
  AND event_time <= '2024-01-15 23:59:59'
  AND type IN ('QueryFinish', 'ExceptionWhileProcessing');
```

**Check 3: Time format is correct**

Times must be RFC3339 format with timezone:
```bash
# Correct
-backfill-start "2024-01-15T00:00:00Z"

# Wrong
-backfill-start "2024-01-15 00:00:00"
```

---

### TLS Certificate Errors

**Symptom:** `x509: certificate signed by unknown authority` or similar TLS errors.

- Provide the CA certificate: `ca_cert: /path/to/ca.pem`
- For testing only: `insecure_skip_verify: true` (never in production)
- For mTLS: ensure both `client_cert` and `client_key` are provided

---

### Backoff Increasing Unexpectedly

**Symptom:** `Backoff increased to Xs after N consecutive failures` in logs.

- This means ClickHouse health checks or queries are failing repeatedly
- Check ClickHouse connectivity: `clickhouse-client -q "SELECT 1"`
- Review the error messages _before_ the backoff warning — they show the root cause
- If ClickHouse is healthy, check network stability between click-dog and ClickHouse
- Backoff resets to the base interval automatically on the first successful poll after recovery

---

### SQL Redaction Not Working

**Symptom:** Sensitive data still appearing in exported `db.statement` attributes.

- Verify your regex pattern matches the target text. Test with a regex tool first
- YAML escaping is tricky with regex. Prefer block scalar syntax to avoid double-escaping:
  ```yaml
  filters:
    redact_queries:
      - pattern: |
          (?i)identified\s+by\s+'[^']*'
        replacement: "IDENTIFIED BY '[REDACTED]'"
  ```
- Leading/trailing whitespace in patterns is trimmed automatically
- Redaction runs _after_ blacklist filtering — if the query is blacklisted, it's dropped entirely (not redacted)
- Set `log_level: debug` and look for the query text in filter decisions

---

### Canary Queries in Degraded Mode

**Symptom:** Seeing `click-dog.canary` spans in your backend, or wanting to enable canary.

Canary queries are **opt-in**. When enabled, they run a lightweight `COUNT` query during circuit breaker open or high backoff states:

```yaml
monitor:
  canary:
    enabled: true
    threshold_duration_ms: 60000  # count spans > 60s
```

- Canary spans have attributes `click_dog.canary=true` and `click_dog.degraded=true`
- If canary queries also fail, the circuit breaker records additional failures
- Canary only counts spans finishing today — spans from previous calendar days are excluded
- To filter canary spans in your backend, query for `click_dog.canary != "true"` (the value is the string `"true"`, not a boolean)

---

### High Availability Issues

**Symptom:** Leader election not working or multiple instances exporting duplicates.

**Leader election not starting:**

- Verify Keeper is accessible on port 9181: `echo ruok | nc keeper-host 9181`
- Check that `ha.keeper.hosts` lists the correct Keeper nodes
- Verify `ha.keeper.base_path` starts with `/click-dog/` or is exactly `/click-dog` (required by path confinement)
- Session timeout must be between 5s and 30s

**Multiple instances exporting the same spans:**

- With no `ha.keeper.hosts` configured, each instance operates independently — duplicates are expected
- With Keeper hosts configured (leader election active), ensure all instances can reach the same Keeper cluster
- If Keeper is unreachable at startup, the instance runs in standalone mode. If connectivity is lost while running, the election goroutine attempts to reconnect and rejoin automatically
- OTEL collectors and backends handle duplicate `(trace_id, span_id)` pairs — this is by design

---

## Log Levels

| Level | What's Logged |
|-------|--------------|
| `error` | Errors only. Minimal output |
| `warn` | Warnings (insecure configs, circuit breaker state changes, backoff increases) |
| `info` | Startup config, connection status, export summaries per cycle (default) |
| `debug` | Individual span processing, batch details, filter decisions |

---

## Investigating queries with `analyze`

`click-dog analyze` is a **local, read-only** triage surface for ClickHouse
query behavior. It opens only the ClickHouse reader — it never constructs
exporters, opens exporter connections, starts the metrics/health servers,
runs leader election, or writes anything back to ClickHouse — so it is safe to
run against production. Reports are operational artifacts: they emit bounded
normalized query previews, never raw query text, so treat JSON output like
logs/traces.

Both verbs share `-config`, `-lookback` (default `1h`), `-timeout`
(default `2m`), `-format` (`table` or `json`), and `-output` (write to a file
instead of stdout; the file is created `0600`).

### `analyze queries` — what looks suspicious in this window

Runs a deterministic registry of analyzers (`coverage`, `resource_hog`,
`attribution_gap`, `skew`) over the lookback window and reports findings:
query-family resource outliers, missing `log_comment` ownership attribution,
user/client/host skew, and coverage prerequisites.

```bash
# Last hour as a table
click-dog analyze queries

# Last 6h as stable JSON, redacting user/client/host values
click-dog analyze queries -lookback 6h -format json -redact-dimensions -output report.json
```

The command exits `0` even when findings are critical; it exits `2` on bad CLI
usage and `1` on a config, connection, or required-query failure. Partial data
(unsupported rollups, no span sample, query-log enrichment failures) degrades
to a coverage warning rather than failing the run.

### `analyze trace` — drill from a query to its trace

The inverse path for incident response: *"I have this running/recent query;
show me the trace and nearby context."* It finds a query, fetches the native
trace spans that carry it via the `clickhouse.query_id` span attribute, then
fans out to query-log stats, the matching query-family rollup, and the analyzer
findings for that family.

```bash
# Guided flow (requires an interactive terminal)
click-dog analyze trace -wizard

# Search recent finished/failed queries in the window
click-dog analyze trace -source recent -match "events" -lookback 1h

# Search running queries; wait up to 30s for the trace to materialize
click-dog analyze trace -source current -match "events" -wait 30s

# Drill an explicit identity directly (query/trace IDs skip candidate search)
click-dog analyze trace -query-id <id>
click-dog analyze trace -trace-id <id>

# Look up by normalized query hash (exact recent query-log lookup; auto-drills only when exactly one matches)
click-dog analyze trace -normalized-query-hash <hash> -fanout trace,stats,similar,findings
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-source` | `recent` | `recent` (`system.query_log`), `current` (`system.processes`), or `other` (explicit identity). |
| `-match` | empty | Case-insensitive substring over normalized previews and candidate metadata. |
| `-query-id` / `-trace-id` | empty | Explicit identity; a direct drill that skips candidate search. |
| `-normalized-query-hash` | empty | Exact (`=`) recent query-log lookup over candidate search — may match zero, one, or several candidates, and auto-drills only when exactly one matches. |
| `-candidate-limit` | `20` | Max candidates listed or returned. |
| `-span-limit` | `1000` | Max spans fetched for the selected trace IDs. |
| `-wait` | `0s` | Bounded poll for a running query's trace to materialize (capped by `-timeout`). |
| `-fanout` | `trace,stats` | Comma-separated `trace`, `stats`, `similar`, `findings`. |
| `-redact-dimensions` | `false` | Redact user/client/host values in the report. |

Notes:

- The configured operation blacklist still applies to the fetched trace spans,
  even on a direct `-trace-id` drill.
- A `-source current` candidate may not have finished: its query-log row,
  normalized hash, and trace spans may not exist yet. These become warnings, not
  failures.
- `-wizard` is TTY-only; a non-interactive `-wizard` exits `2` rather than
  blocking. In non-interactive mode the command never waits for input — a
  single matching candidate is drilled, otherwise the bounded candidate list is
  printed.
- A produced report exits `0` even with warnings (missing trace, unsupported
  rollups, no matching findings). It exits `1` on config/connection/required-query
  failures and `2` on bad CLI usage.

---

## Getting Help

If you're still stuck:

1. Set `log_level: debug` and capture output
2. Check the [GitHub issues](https://github.com/coltconsulting/click-dog/issues) for similar problems
3. Open a new issue with your configuration (redact secrets) and debug log output
