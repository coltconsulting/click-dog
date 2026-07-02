# Filtering

Click-Dog provides multiple filtering layers to control which spans get exported.

## Filter Evaluation Order

1. **Operation blacklist** (SQL-level) — `blacklist_operations` patterns are pushed down to ClickHouse as `NOT LIKE` clauses. Matching spans are never fetched. This is the primary volume control — use it to drop high-volume internal pipeline spans like `MergeTreeIndex`.
2. **User filter** — `whitelist_users` is pushed down to the `query_log` enrichment WHERE clause, `blacklist_users` is kept for the Go-layer span re-check, and both lists are pushed down for the slow-query / backfill path.
3. **IP whitelist** — if configured, reject spans from IPs not in the list
4. **Operation whitelist** — if configured, reject spans with non-matching operation names
5. **Query blacklist** — reject spans whose SQL text matches any blacklist pattern
6. **Query redaction** — rewrite matching SQL fragments before export; this does not affect whether a span is exported

Step 1 runs entirely at the SQL level (before data transfer). Step 2 is mixed: the enrichment leg is SQL-level, the per-span re-check is post-fetch. Steps 3–5 run in Go after spans are fetched. A span must pass all checks in steps 1–5 to be exported. Step 6 is a post-filter transform that runs before export. If a whitelist is not configured (empty list), that check is skipped.

The list above describes *enforcement order* (SQL first, then Go filtering, then redaction). Within the Go-level loop the per-span user re-check runs alongside the IP / operation / query checks; the relative order of those Go-level checks is an implementation detail and shouldn't be relied on for debugging filter behavior.

Note: `blacklist_operations` (SQL-level, step 1) and `whitelist_operations` (post-fetch, step 4) are independent. The blacklist always wins because matching spans are never fetched — they cannot reach the whitelist check.

## Operation Blacklist (SQL-level)

Drop high-volume internal ClickHouse spans before they are fetched. Patterns are matched as substrings against `operation_name` using SQL `NOT LIKE '%pattern%'`.

```yaml
filters:
  blacklist_operations:
    - "MergeTreeSource"
    - "MergeTreeMarksLoader"
    - "MergeTreeIndex"
    - "MergeTreeSequentialSource"
    - "VFSWrite"
    - "WriteBufferFromS3"
    - "ConcurrentJoin"
    - "QueryPipelineEx"
```

This is the default for new installs. It cuts span volume by ~90-99% without affecting query-level spans or their sub-queries.

## Operation Whitelist

Include only spans with matching operation names. Supports `*` as a wildcard character.

```yaml
filters:
  whitelist_operations:
    - "DB::InterpreterSelectQuery::execute()"
    - "DB::Interpreter*::execute()"
    - "HTTPHandler::*"
```

### Wildcard Behavior

The `*` character matches any sequence of characters. Internally, it's converted to `.*` in a regex anchored with `^...$`.

| Pattern | Matches | Doesn't Match |
|---------|---------|---------------|
| `DB::InterpreterSelectQuery::execute()` | Exact match only | Any other operation |
| `DB::Interpreter*::execute()` | `DB::InterpreterSelectQuery::execute()`, `DB::InterpreterInsertQuery::execute()` | `HTTPHandler::handleRequest()` |
| `*::execute()` | Any operation ending in `::execute()` | `DB::merge()` |
| `*` | Everything | — |

### When Not Configured

If `whitelist_operations` is empty or not set, all operation names are allowed through.

---

## IP Whitelist

Include only spans from specific client IP addresses.

```yaml
filters:
  whitelist_ips:
    - "10.0.1.50"
    - "10.0.1.51"
    - "192.168.1.100"
    - "10.2.0.0/16"
```

Entries without `/` match by exact IP string. Entries in CIDR notation match any
address in that range. The value comes from ClickHouse's
`IPv6NumToString(address)` in the query log or the `client.address` attribute in
the span log.

### When Not Configured

If `whitelist_ips` is empty or not set, all IP addresses are allowed through.

---

## User Filter

Restrict export by originating ClickHouse user. Two independent lists:

```yaml
filters:
  whitelist_users:           # If set, only these users' spans are exported
    - "app_frontend"
    - "app_analytics"
  blacklist_users:           # Never export spans from these users
    - "patient_records"
    - "ml_training"
```

The user is sourced from `system.query_log.user`. The whitelist is pushed down to the enrichment query as `AND user IN (...)`. The blacklist is **not** pushed down to the enrichment query — doing so would strip blacklisted users' rows from the enrichment map and leave the per-span resolver unable to identify the user, which the Go-level blacklist (permissive on unknown users by design) would then silently let through. Blacklist is enforced exclusively at the Go layer for the span path. For the slow-query / backfill path, both lists are pushed to SQL because that query returns rows with `user` populated.

| Filter | Enrichment SQL (`query_log` by `query_id`) | Slow-query SQL (backfill / flush) | Go-layer re-check |
|---|---|---|---|
| `whitelist_users` | `AND user IN ?` | `AND user IN ?` | yes |
| `blacklist_users` | (not pushed — see rationale above) | `AND user NOT IN ?` | yes |

> **Requires `monitor.enrich_from_query_log: true`** (the default). With enrichment disabled the per-span resolver has no `query_log` row to consult, every span resolves to an empty user, and the filter degenerates: a whitelist drops everything; a blacklist passes everything. Click-dog's config validation rejects this combination at startup.

Matches are case-sensitive (exact string equality), mirroring ClickHouse's username handling — `App_Frontend` will not match `app_frontend`. When both lists are configured, a user must appear in `whitelist_users` AND not appear in `blacklist_users` to pass.

`query_log.user` is the user that initiated the outermost query — not a sub-query author or a UDF runner. Multi-tenant deployments where one CH service account runs queries on behalf of many end-users can't be separated at this layer; surface the end-user identity as a `log_comment` attribute instead, or run distinct CH users per tenant.

If a future ClickHouse version starts populating a `clickhouse.user` attribute on `system.opentelemetry_span_log`, click-dog will use that attribute in preference to the `query_log.user` enrichment lookup. The two may legitimately differ (e.g. an executing user after `SET ROLE` versus the session user); operators relying on the user filter for compliance should verify which identity their CH version emits before upgrading.

### Use cases

- **Compliance / PII segregation.** Pair with minimum-privilege ClickHouse grants
  and the [span-attribute privacy notes](span-attributes.md#query_log-enrichment)
  to ensure data from a sensitive user — e.g. `patient_records` — is never
  forwarded to an external observability backend.
- **Noise reduction.** Exclude background users (replication, backup, maintenance bots) that generate span volume without analytical value.

### Whitelist semantics

`whitelist_users` is strict, mirroring `whitelist_ips`: any span whose user cannot be determined (typically internal child spans without a `clickhouse.query_id`) is dropped. If most of your traffic is single-step queries this is fine; if you rely on multi-span trace fan-out, prefer `blacklist_users` or pair with the ClickHouse-side user grants for hard segregation.

### When not configured

If both lists are empty, the user filter is a no-op and no enrichment SQL is added.

### Debug-log note

`log_level: debug` writes the matching/non-matching ClickHouse user name to the log when a span is filtered (e.g. `Filtering span from blacklisted user: "patient_records"`). For PII-segregation deployments where usernames themselves are sensitive, keep `log_level: info` or higher in production.

---

## Query Blacklist

Exclude spans whose SQL query text matches any of the provided regex patterns.

```yaml
filters:
  blacklist_queries:
    - "^SELECT \\* FROM system\\."    # System table queries
    - "SHOW TABLES"                    # SHOW commands
    - "(?i)healthcheck"                # Health checks (case-insensitive)
    - "INSERT INTO.*_staging"          # Staging table writes
```

### Regex Syntax

Patterns use Go's [regexp](https://pkg.go.dev/regexp/syntax) syntax (RE2). Common patterns:

| Pattern | Description |
|---------|-------------|
| `^SELECT` | Queries starting with SELECT |
| `(?i)pattern` | Case-insensitive matching |
| `system\\.` | Literal dot (escaped) |
| `table1\|table2` | Match either table |
| `.*` | Match anything |

### YAML Escaping

Remember that backslashes need to be doubled in YAML strings:

```yaml
# Correct - double backslash
blacklist_queries:
  - "^SELECT \\* FROM system\\."

# Wrong - single backslash (YAML will consume it)
blacklist_queries:
  - "^SELECT \* FROM system\."
```

---

## Query Redaction

Rewrite sensitive fragments in SQL text before export without dropping the span.
Redaction rules use Go's RE2 regexp syntax, run after query blacklist filtering,
and are applied sequentially. If `replacement` is omitted, matches are replaced
with `[REDACTED]`.

```yaml
filters:
  redact_queries:
    - pattern: |
        (?i)identified\s+by\s+'[^']*'
      replacement: "IDENTIFIED BY '[REDACTED]'"
    - pattern: "(?i)token\\s*=\\s*'[^']*'"
```

Redaction changes exported query attributes such as `db.statement`; it does not
change ClickHouse data and it does not affect filter decisions. If a query
matches `blacklist_queries`, it is dropped entirely and no redacted copy is
exported.

---

## Duration Filtering

Duration thresholds are applied at the SQL level in ClickHouse, not in application code. This minimizes data transfer.

```yaml
monitor:
  # Trace-level: which traces to look at
  min_trace_duration_ms: 1000    # Traces with at least one span >= 1s
  max_trace_duration_ms: 300000  # Skip traces with spans > 5 minutes

  # Span-level: which spans to export from matching traces
  min_span_duration_ms: 500      # Only export spans >= 500ms
  max_span_duration_ms: 60000    # Skip spans > 1 minute
```

### Two-Tier Approach

**Step 1 — Trace selection**: Find trace IDs where at least one span falls within `[min_trace_duration_ms, max_trace_duration_ms]`.

**Step 2 — Span export**: From those traces, export individual spans within `[min_span_duration_ms, max_span_duration_ms]`.

This lets you find traces that contain slow operations (step 1) while controlling which spans from those traces you actually export (step 2).

### Examples

Export all spans from traces containing a slow span:
```yaml
min_trace_duration_ms: 5000    # Traces with 5s+ spans
min_span_duration_ms: 0        # But export ALL spans from those traces
```

Export only the slow spans themselves:
```yaml
min_trace_duration_ms: 5000    # Traces with 5s+ spans
min_span_duration_ms: 5000     # Only export the 5s+ spans
```

Skip outlier traces (likely system operations):
```yaml
min_trace_duration_ms: 1000
max_trace_duration_ms: 600000  # Skip traces > 10 minutes
```

---

## Query Length Filtering

Skip queries with excessively long SQL text. There are two separate stages:

```yaml
monitor:
  max_query_length: 100000     # Skip queries with SQL > 100k chars (at fetch time)

exporters:
  otel:
    - collector_address: localhost:4317
      max_query_length: 100000 # Truncate SQL > 100k chars (at export time)
  splunk_hec:
    - endpoint: https://splunk.internal:8088
      token: ${SPLUNK_HEC_TOKEN}
      max_query_length: 100000 # Truncate SQL > 100k chars (at export time)
```

| Setting | When Applied | Behavior |
|---------|-------------|----------|
| `monitor.max_query_length` | During ClickHouse query | Queries with SQL text longer than this are **excluded entirely** |
| `exporters.otel[].max_query_length` | During OTEL export | SQL text is **truncated** to this length with `...` appended |
| `exporters.splunk_hec[].max_query_length` | During Splunk HEC export | SQL text is **truncated** to this length with `...` appended |

---

## Combining Filters

All filters can be used together. Here's a production example:

```yaml
monitor:
  min_trace_duration_ms: 1000
  max_trace_duration_ms: 600000
  min_span_duration_ms: 500
  max_query_length: 50000

filters:
  # Only care about query execution
  whitelist_operations:
    - "DB::Interpreter*::execute()"

  # Only from application servers
  whitelist_ips:
    - "10.0.1.50"
    - "10.0.1.51"
    - "10.0.1.52"

  # Exclude noise
  blacklist_queries:
    - "^SELECT \\* FROM system\\."
    - "(?i)healthcheck"
    - "SHOW TABLES"
    - "INSERT INTO.*_tmp_"
```

## Debugging Filters

Set `log_level: debug` to see which spans are being filtered and why:

```
[DEBUG] Filtering span from IP not in whitelist: 172.16.0.5
[DEBUG] Filtering operation not in whitelist: TCPHandler::runImpl()
[DEBUG] Filtering query matching blacklist pattern: ^SELECT \* FROM system\.
[DEBUG] Filtering span from user not in whitelist: "patient_records"
[DEBUG] Filtering span from blacklisted user: "ml_training"
```
