# Configuration Reference

Click-Dog uses YAML configuration with environment variable support. When
`-config` is omitted, it first looks for
`/etc/click-dog/click-dog.yaml`, then falls back to `./click-dog.yaml` in the
current directory. Override with `-config`:

```bash
./click-dog -config /path/to/config.yaml
```

Unknown keys are rejected at load time, so a typo like `check_intervla_s` fails fast instead of being silently ignored. Use `-validate` to check that your config parses and passes validation without starting the service.

## Environment Variable Expansion

Sensitive fields support `${VAR}` and `$VAR` syntax. Unset variables resolve to empty strings.

```yaml
clickhouse:
  host: ${CLICKHOUSE_HOST}
  username: ${CLICKHOUSE_USERNAME}
  password: ${CLICKHOUSE_PASSWORD}

exporters:
  otel:
    - collector_address: ${OTEL_COLLECTOR_ADDRESS}
      ca_cert: ${OTEL_CA_CERT}
      client_cert: ${OTEL_CLIENT_CERT}
      client_key: ${OTEL_CLIENT_KEY}
```

The following fields support environment variable expansion:
- `clickhouse.host`, `clickhouse.username`, `clickhouse.password`, `clickhouse.ca_cert`
- `exporters.otel[].collector_address`, `exporters.otel[].ca_cert`, `exporters.otel[].client_cert`, `exporters.otel[].client_key`
- `exporters.splunk_hec[].endpoint`, `exporters.splunk_hec[].token`
- `ha.keeper.auth_user`, `ha.keeper.auth_password`
- `metrics.otlp.collector_address`, `metrics.otlp.host`, `metrics.otlp.service_name`
- `webhook.url`

The `*_file` secret-path fields (`clickhouse.password_file`, `exporters.splunk_hec[].token_file`, `ha.keeper.auth_password_file`) are also expanded, so the path can reference an environment variable — e.g. `password_file: ${SECRET_PATH}`.

---

## ClickHouse Connection

```yaml
clickhouse:
  host: localhost              # ClickHouse hostname
  port: 9000                   # Native protocol port (default: 9000)
  database: default            # Database name
  username: default            # Username (supports env vars)
  password: ${CLICKHOUSE_PASSWORD}  # Password (supports env vars)

  # TLS
  secure: false                # Enable TLS (default: false)
  insecure_skip_verify: false  # Skip cert verification (TESTING ONLY)
  ca_cert: ""                  # Path to CA cert for self-signed certs

  # Cluster
  cluster: ""                  # Cluster name for distributed queries
  use_cluster_queries: false   # Use cluster() function (default: false)

  # Connection pool
  max_open_conns: 2            # Max open connections (default: 2)
  max_idle_conns: 1            # Max idle connections (default: 1)
  query_timeout_s: 30          # Query timeout in seconds (default: 30)

  # Memory
  max_memory_usage: 104857600  # Per-query memory limit in bytes (default: 100MB)
```

### Connection Details

| Field | Default | Description |
|-------|---------|-------------|
| `host` | — | ClickHouse server hostname |
| `port` | `9000` | Native protocol port |
| `database` | — | Database name |
| `username` | — | Username |
| `password` | — | Password (supports `${ENV}` expansion). Mutually exclusive with `password_file` |
| `password_file` | — | Path to a file holding the password (read at load; one trailing newline — CRLF or LF — trimmed). Path supports `${VAR}` expansion. Keeps the secret out of the process environment. Mutually exclusive with `password`; an empty file is rejected — for an intentionally-empty password use the inline `password: ""` form |

### TLS

| Field | Default | Description |
|-------|---------|-------------|
| `secure` | `false` | Enable TLS encryption |
| `insecure_skip_verify` | `false` | Skip certificate verification. **Insecure** — use only for testing |
| `ca_cert` | — | Path to CA certificate file for TLS verification with self-signed or internal CA certs |

### Cluster Mode

There is one concept — **a reader and the scope it covers** — giving two
topologies. **`single` is just `sidecar` with n=1** (identical config; only the
instance count differs).

**Sidecar (default, recommended):** Deploy one Click-Dog instance per ClickHouse
node. Each instance reads only its **local** `system.opentelemetry_span_log`;
the partitions are disjoint by host, so the union gives full cluster coverage
with zero coordination. This is the topology that distributes the expensive
cluster-wide read. Each instance has minimal permissions and generates minimal
load. No Keeper, no leader election. Deploy the same config once per node.

**Cluster (`use_cluster_queries: true`):** A Click-Dog instance reads the
**whole cluster** by wrapping the span query as
`cluster('cluster_name', system.opentelemetry_span_log)` (one replica per shard).
Requires the ClickHouse user to have cross-node query permissions and network
access to all nodes, and a non-empty `cluster:` name. Cluster mode is **always
leader-gated** (see below): run **1** instance (the leader of an election of
one) or **2–3** for automatic failover. Adding instances doesn't divvy up the
read — they **stand by** behind one active exporter.

| Field | Default | Description |
|-------|---------|-------------|
| `cluster` | — | Cluster name. Required when `use_cluster_queries` is `true` |
| `use_cluster_queries` | `false` | When `true`, uses `cluster('name', table)` to read one replica per shard across the cluster (**cluster** topology). When `false`, reads only this node's local tables (**sidecar** topology). Optional normalized query attributes are enabled in cluster query mode only after `clusterAllReplicas('name', system.columns)` verifies every replica exposes `normalized_query_hash`; mixed or unknown clusters keep the safe fallback |

#### Cluster mode is always leader-gated

In cluster mode the fetch/export cycle runs **only on the election leader**;
other instances stand by (recorded as skipped cycles, so a standby's
last-success gauge legitimately stays at zero). The rule is **export iff no
election is active, or this instance is the leader** — so:

- A **single** cluster reader with **no Keeper** configured is valid and common:
  no election starts, so it always exports (no regression for existing
  `use_cluster_queries: true` users).
- With **Keeper** (2–3 instances), one is elected leader and exports; the rest
  defer. On leader loss a standby promotes within ~one Keeper session TTL (≤10s)
  and re-reads from `now - lookback`, so the only overlap at failover is a
  deduped re-send — never a permanent gap.
- Leader election is driven **solely by the presence of `ha.keeper.hosts`** —
  there is no separate enable flag. Configure Keeper hosts and instances
  coordinate (only the leader exports); omit them and each instance runs
  standalone. This is uniform across topologies: a cluster reader or a sidecar
  fleet both opt into coordination the same way.

The delivery contract is **best-effort, deduplicated downstream by
`(trace_id, span_id)`** → effectively-once at the backend. The guarantee is
"**no steady-state duplication**," not "never duplicates": under a healthy
Keeper exactly one leader exports. Every Keeper disruption after an instance has
joined **fails open** — a session loss, reconnect, partition, or watch error
drops candidate state so the instance keeps exporting (rather than stalling as a
gated standby), and the election goroutine retries until it rejoins. The
resulting duplicate window is therefore **bounded** and recovers to a single
exporter when Keeper returns. (Keeper unreachable *at startup*, before any join,
is the one exception: that instance runs standalone until restarted.) Click-Dog
does **not** fail closed — it prefers availability.

!!! note "Pick one topology per fleet"
    Don't run per-node sidecars with `use_cluster_queries: true`. Leader-gating
    now prevents steady-state duplication when those instances share an
    election, but sidecars that are **not** sharing an election (different
    `base_path`, or no Keeper) would each go cluster-wide and duplicate. If you
    want whole-cluster reads, run **cluster** mode with a shared Keeper; if you
    want one reader per node, leave `use_cluster_queries` off. See
    [Operating · Cluster topology](operating.md#cluster-topology-and-sidecar-placement).

    This is no longer **only** documented: when `use_cluster_queries: true`,
    click-dog runs an in-process **topology self-audit** that detects more than
    one instance doing whole-cluster reads and surfaces it via the
    `click_dog_topology_warning` metric, the `/status` `topology_warning` field,
    a throttled WARN log, and the Datadog health dashboard. It is
    observability-only — it never gates readiness or de-rotates a node — and is a
    backstop for the case leader-gating can't reach (instances not sharing an
    election). See [`monitor.topology_audit`](#topology-self-audit) below.

### Topology self-audit

When `clickhouse.use_cluster_queries: true`, click-dog periodically checks
whether more than one instance is running whole-cluster span reads — the sidecar
+ `use_cluster_queries` anti-pattern that causes N× duplicate exports. It is a
hard **no-op** unless this instance itself uses cluster queries (zero cost for
the default per-node sidecar topology), and is **observability-only**: it emits
the `click_dog_topology_warning{reason}` gauge, the `/status` `topology_warning`
field, and a throttled WARN, but never affects `/readyz` or `/healthz`.

| Field | Default | Description |
|-------|---------|-------------|
| `monitor.topology_audit.enabled` | `true` | Enable the self-audit (still a no-op unless `use_cluster_queries` is on) |
| `monitor.topology_audit.interval_s` | `300` | Audit tick interval in seconds |
| `monitor.topology_audit.debounce_count` | `2` | Consecutive positive ticks before a warning latches (absorbs rollout/restart transients) |
| `monitor.topology_audit.query_log_lookback_minutes` | `15` | Window of `system.query_log` scanned for distinct cluster readers |

First detection lands roughly `interval_s × debounce_count` after a misconfig
appears — **~10 minutes at the defaults**. Lower `interval_s` for faster
detection. The `reason` label is `sidecar_cluster_queries` when this instance's
`clickhouse.host` is loopback (co-located sidecars) and
`multi_instance_cluster_queries` otherwise (separate readers not sharing an
election).

> **Keep `interval_s` well above the poll cadence.** The auditor counts a host as
> a live cluster reader only if its most recent whole-cluster read falls inside a
> recency window it derives from `monitor.check_interval_s`, capped at
> `interval_s / 2`. If you raise `check_interval_s` past `interval_s / 2`, that cap
> drops the window *below* the poll cadence, so a genuinely-concurrent reader —
> which only reads once per `check_interval_s` — can fall outside it between ticks:
> the misconfig goes undetected, or the warning flaps. Keep
> `interval_s ≥ 6 × check_interval_s` to preserve the full window. click-dog emits a
> load-time `WARN` when this combination is detected.

### Connection Pool

| Field | Default | Description |
|-------|---------|-------------|
| `max_open_conns` | `2` | Maximum number of open connections |
| `max_idle_conns` | `1` | Maximum number of idle connections |
| `query_timeout_s` | `30` | Query timeout in seconds. Prevents runaway queries |
| `max_memory_usage` | `104857600` | Per-query memory limit in bytes (100 MB). Prevents a single monitoring query from consuming excessive memory |

---

## OTEL Exporter

```yaml
exporters:
  otel:
    - collector_address: localhost:4317  # OTEL collector gRPC address
      service_name: click-dog-monitor     # Service name in traces

      max_query_length: 100000     # Truncate queries longer than this (default: 100000)

      # TLS
      secure: false                # Enable TLS for gRPC (default: false)
      insecure_skip_verify: false  # Skip cert verification (TESTING ONLY)
      ca_cert: ""                  # Path to CA cert
      client_cert: ""              # Client cert for mTLS
      client_key: ""               # Client key for mTLS
```

| Field | Default | Description |
|-------|---------|-------------|
| `collector_address` | — | OTEL collector gRPC endpoint (e.g., `localhost:4317`) |
| `service_name` | `click-dog-monitor` | Service name that appears in traces |
| `max_query_length` | `100000` | Truncate SQL query text longer than this in exported spans. `0` or omitted is coerced to the `100000` default; the limit is not disabled at config load |
| `secure` | `false` | Enable TLS for the gRPC connection |
| `insecure_skip_verify` | `false` | Skip TLS cert verification. **Insecure** |
| `ca_cert` | — | CA certificate path for TLS verification |
| `client_cert` | — | Client certificate for mutual TLS (mTLS). Must be used with `client_key` |
| `client_key` | — | Client key for mTLS. Must be used with `client_cert` |

!!! note "Authentication"
    Click-Dog supports TLS and mTLS for the gRPC connection. It does not currently support bearer tokens, API keys, or custom gRPC metadata headers. If your OTEL collector requires header-based auth, place an authenticating proxy in front of it or use mTLS for client authentication.

### Multiple Export Backends

The `exporters` section accepts any number of OTLP/gRPC sinks under `exporters.otel[]`, and can be combined with Splunk HEC under `exporters.splunk_hec[]`:

```yaml
exporters:
  otel:
    - collector_address: primary-collector:4317
      service_name: click-dog-monitor
    - collector_address: backup-collector:4317
      service_name: clickdog-backup
  splunk_hec:
    - endpoint: https://splunk.internal:8088
      token: ${SPLUNK_HEC_TOKEN}
      index: clickhouse
      source: click-dog
      source_type: _json
      allow_insecure_http: false
      max_query_length: 100000
```

Each `exporters.splunk_hec[]` entry takes a Splunk HTTP Event Collector endpoint, a token (env-expandable), and optional `index`, `source`, `source_type`, `max_query_length`, and TLS fields. Use `https://` endpoints by default because the HEC token is sent in the `Authorization` header; `http://` endpoints are accepted only when `allow_insecure_http: true` is set explicitly for local, development, or test HEC receivers. The token can also be supplied via `token_file` (a path, itself `${VAR}`-expandable, read at load with one trailing newline — CRLF or LF — trimmed) to keep it out of the process environment; `token` and `token_file` are mutually exclusive.

When more than one exporter is configured, spans are sent to all backends sequentially under the default **`PolicyAllRequired`** contract: any backend failure — partial or total — is treated as a real export error. The cycle is marked an error (`click_dog_cycle_results_total{result="error"}`), feeds the circuit breaker and adaptive backoff, and shows up as the most recent error on `/status` and `/readyz`. The per-sink counters (`click_dog_export_{attempts,accepted,errors}_total{sink}`) plus warning log summaries surface exactly which backend failed, so a dead sink can't silently drop out of the fan-out.

A span is marked as "seen" in the dedup cache only if **every** backend accepts it. If any backend fails, no span from that batch is marked seen, and the next cycle re-delivers the full batch to *every* sink. Backends that already accepted those spans will see the same `(trace_id, span_id)` pair again — accepted by design as an at-least-once delivery trade-off. OTEL collectors and trace backends can deduplicate by that identity; Splunk HEC receives the same stable IDs, but duplicate collapse depends on downstream Splunk searches or index-time posture. The same contract applies to backfill mode: a partial multi-sink failure counts the query as `Failed` in the run summary, the process exits non-zero, and re-running the window re-delivers to every sink.

An alternate `PolicyAnySuccess` policy exists in code — spans are marked seen as soon as one sink accepts them, while failed sinks remain visible through `ExportResult` and the per-sink metrics. It is not exposed via YAML today; production configs use the strict default policy. The per-call deadline applied to every backend in the fan-out is `monitor.export_timeout_s` (see [Monitor](#monitor) below).

---

## High Availability

Leader election via ClickHouse Keeper for multi-node deployments. **Configuring
`keeper.hosts` is what turns election on** — there is no separate enable flag.
Omit the `ha` block entirely and each instance runs standalone.

```yaml
ha:
  keeper:
    hosts:
      - keeper-01:9181
      - keeper-02:9181
      - keeper-03:9181
    secure: false                 # Enable TLS to Keeper (default: false)
    session_timeout_s: 10        # Session timeout (default: 10, range: 5-30)
    base_path: /click-dog/election  # Znode path prefix
    # auth_user: ""              # Optional digest auth
    # auth_password: ""          # Optional digest auth
```

| Field | Default | Description |
|-------|---------|-------------|
| `keeper.hosts` | — | ClickHouse Keeper (ZooKeeper-compatible) endpoints. **Presence enables leader election**; empty/omitted means standalone |
| `keeper.secure` | `false` | Enable TLS for the Keeper connection. Use with Keeper digest auth on untrusted networks so credentials are not sent in plaintext |
| `keeper.session_timeout_s` | `10` | Session timeout in seconds. Must be 5-30 |
| `keeper.base_path` | `/click-dog/election` | Znode path prefix. Must be exactly `/click-dog` or under `/click-dog/`; paths overlapping ClickHouse's own Keeper paths (`/clickhouse...`) are rejected |
| `keeper.auth_user` | — | Optional digest authentication username (supports `${ENV}` expansion) |
| `keeper.auth_password` | — | Optional digest authentication password (supports `${ENV}` expansion). Mutually exclusive with `keeper.auth_password_file` |
| `keeper.auth_password_file` | — | Path to a file holding the digest auth password (read at load; one trailing newline — CRLF or LF — trimmed). Path supports `${VAR}` expansion. Mutually exclusive with `keeper.auth_password` |

Each click-dog instance creates an ephemeral sequential znode. The lowest sequence number becomes leader. When the leader dies, its session expires, the znode is deleted, and the next candidate auto-promotes (~10 seconds failover).

When `keeper.auth_user` is omitted, election znodes use Keeper world ACLs; click-dog emits a startup warning so that choice is visible. Configure digest auth for shared Keeper ensembles, and set `keeper.secure: true` when Keeper supports TLS.

!!! note "Leader gating depends on topology"
    In **cluster** mode (`use_cluster_queries: true`), leadership gates the data path: only the leader fetches and exports; standbys skip each cycle (recorded as a skipped cycle, no export) so the cluster's spans aren't duplicated. In **sidecar** mode each instance reads its own node's disjoint local scope and always exports regardless of leader status — there leadership only drives coordination duties (flush, `/clusterz`). Either way, configuring `ha.keeper.hosts` is what activates election.

If Keeper is unreachable at startup, click-dog logs a warning and falls back to standalone mode — it continues exporting normally without leader election.

---

## Monitor

```yaml
monitor:
  enabled: true                # Enable scheduled monitoring (default: true)

  # Duration thresholds (milliseconds)
  min_trace_duration_ms: 1000  # Find traces with spans >= this (required)
  min_span_duration_ms: 0      # Only export spans >= this (0 = all)
  max_trace_duration_ms: 0     # Skip traces with spans > this (0 = no limit)
  max_span_duration_ms: 0      # Skip spans > this (0 = no limit)

  max_query_length: 100000     # Skip queries with SQL > this chars (see also exporters.otel[].max_query_length which truncates instead)

  # Polling
  check_interval_s: 30         # Polling interval in seconds
  lookback_s: 40               # How far back to look (default: check_interval_s + lookback_buffer_s)
  lookback_buffer_s: 10        # Extra seconds added when defaulting lookback_s (default: 10)

  # Rate limiting
  max_spans_per_cycle: 1000    # Max spans fetched and exported per polling cycle (default: 1000)

  # Batch processing
  batch_size: 0                # Batch size (0 = no batching)
  batch_delay_ms: 0            # Delay between batches (0 = no delay)
  export_timeout_s: 30         # Per-call exporter deadline in seconds (0 disables)

  # Deduplication
  dedup_cache_size: 10000      # LRU cache entries (default: 10000)
```

### Duration Filtering

Click-Dog uses a two-tier duration filtering approach:

1. **Trace-level** (`min_trace_duration_ms` / `max_trace_duration_ms`): Find traces that contain at least one span within this duration range
2. **Span-level** (`min_span_duration_ms` / `max_span_duration_ms`): From those traces, only export individual spans within this duration range

All duration filtering happens in SQL for maximum efficiency.

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `true` | Enable scheduled monitoring. Set `false` for backfill-only usage |
| `min_trace_duration_ms` | — | **Required.** Find traces containing spans >= this duration (ms) |
| `min_span_duration_ms` | `0` | Only export spans >= this (ms). `0` = export all spans from matching traces |
| `max_trace_duration_ms` | `0` | Skip traces with spans > this (ms). `0` = no upper limit |
| `max_span_duration_ms` | `0` | Skip individual spans > this (ms). `0` = no upper limit |
| `max_query_length` | `100000` | Skip queries with SQL text longer than this (chars) and truncate live `query_log.normalized_query` previews to this length. `0` or omitted is coerced to the `100000` default; the limit is not disabled at config load. Different from `exporters.otel[].max_query_length` which truncates at export time |

### Polling

| Field | Default | Description |
|-------|---------|-------------|
| `check_interval_s` | `30` | How often to poll ClickHouse (seconds). Must be greater than 0 when `enabled` is true |
| `lookback_s` | `check_interval_s + lookback_buffer_s` | How far back to look for spans. Defaults to interval + the buffer below, to avoid gaps between polls. Overlapping spans are caught by the dedup cache and skipped before export — they do not count against `max_spans_per_cycle`. Set this explicitly to override the computed default |
| `lookback_buffer_s` | `10` | Seconds added to `check_interval_s` when computing the default `lookback_s`. Absorbs clock skew and slow poll cycles; increase under high load if you see missed spans. Ignored when `lookback_s` is set explicitly |

### Rate Limiting & Batching

| Field | Default | Description |
|-------|---------|-------------|
| `max_spans_per_cycle` | `1000` | Maximum number of **spans** fetched and exported per polling cycle. Applied as a SQL `LIMIT` on the spans query (and as an upper bound on the trace ID query, since each trace contributes at least one span). When the cap is hit, a trace can be exported in pieces across cycles: the lookback overlap re-selects the same trace next cycle and the dedup cache skips already-exported spans, so the remainder is exported as long as the trace stays within `lookback_s`. Spans that are still in flight when their trace ages out of the lookback window are dropped. A `debug`-level log line reports the total span count per cycle. The absolute ceiling is `100000` spans per cycle to bound memory; the spans query clamps to it, and config validation rejects larger values at load |
| `batch_size` | `0` | Process spans in batches of this size. `0` = process all at once |
| `batch_delay_ms` | `0` | Milliseconds to wait between batches. `0` = no delay |
| `export_timeout_s` | `30` | Per-call deadline for exporter calls. Applies to live span batches, backfill query exports, and degraded-mode canary span exports so a stuck collector fails into the normal retry, backoff, and circuit-breaker path. Set to `0` to disable the client-side export deadline |
| `dedup_cache_size` | `10000` | Size of the LRU cache for span deduplication, keyed by the composite OTLP span identity `(trace_id, span_id)`. OTLP span IDs are only unique within a trace, so dedup must include the trace ID; a span-id-only cache would silently drop sibling spans in different traces that collide on the 64-bit ID. Each entry's `SpanKey` is 24 B (16 B trace_id + 8 B span_id), but the LRU container adds doubly-linked-list pointers and a map bucket, so the practical cost is ~80–100 B per entry on 64-bit. 10,000 entries ≈ 1 MiB. Memory scales linearly. Increase for high-throughput scenarios. The cache is in-memory only; after a restart, spans still inside `lookback_s` are re-exported once and deduped downstream by collectors |
| `extract_log_comment` | `true` | Extract `log_comment` from URI attributes and promote JSON keys as span attributes prefixed with `log_comment.`. See [Span Attributes](span-attributes.md#log_comment-extraction) |
| `enrich_from_query_log` | `true` | Enrich spans with metadata from `system.query_log` (user, client, tables, query stats) by joining on `query_id` (from the `clickhouse.query_id` span attribute). **Note:** includes `query_log.user`, `query_log.client_address`, and `query_log.client_hostname` which may contain PII/internal network info — see [privacy note](span-attributes.md#query_log-enrichment). See [Span Attributes](span-attributes.md#query_log-enrichment) |

---

## Circuit Breaker

Protects ClickHouse by stopping queries after repeated failures.

```yaml
monitor:
  circuit_breaker:
    enabled: false             # Enable circuit breaker (default: false)
    failure_threshold: 3       # Open after N consecutive failures
    success_threshold: 1       # Close after N successes in half-open state
    reset_timeout_s: 60        # Try again after N seconds when open
```

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `false` | Enable the circuit breaker. Note: the [install script](install.md) enables this by default in generated configs |
| `failure_threshold` | `3` | Number of consecutive failures before opening the circuit |
| `success_threshold` | `1` | Number of successful requests needed in half-open state to close the circuit |
| `reset_timeout_s` | `60` | Seconds to wait before transitioning from open to half-open |

See [Resilience](resilience.md) for details on circuit breaker behavior.

---

## Canary

Keeps a lightweight probe running while the circuit breaker is open. Instead of
going fully dark during an incident, click-dog runs a single cheap canary query
against ClickHouse and exports one synthetic span with the result, so operators
retain visibility into whether long queries still exist. A successful canary
also records a circuit-breaker success, helping the breaker recover.

```yaml
monitor:
  canary:
    enabled: false               # Run canary probes while the breaker is open (default: false)
    threshold_duration_ms: 60000 # Query-duration threshold the canary probes for (default: 60000)
```

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `false` | Run the canary query and export its synthetic span during degraded (breaker-open) mode |
| `threshold_duration_ms` | `60000` | Duration threshold (ms) the canary query probes for. `0` or omitted falls back to the default; negative values are rejected at config load |

Canary spans carry `click_dog.canary="true"` and `click_dog.degraded="true"` so
they can be filtered in your backend. See
[Troubleshooting — canary queries](troubleshooting.md#canary-queries-in-degraded-mode).

---

## Adaptive Backoff

Increases polling interval when errors occur, reducing load on a struggling ClickHouse.

```yaml
monitor:
  backoff:
    enabled: false             # Enable adaptive backoff (default: false)
    max_interval_s: 300        # Maximum polling interval (default: 300 = 5 min)
    backoff_factor: 2.0        # Multiply interval on failure (default: 2.0)
```

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `false` | Enable adaptive backoff. Note: the [install script](install.md) enables this by default in generated configs |
| `max_interval_s` | `300` | Maximum polling interval in seconds (5 minutes) |
| `backoff_factor` | `2.0` | Multiply the current interval by this factor on each failure. Must be greater than `1` — values in `(0, 1]` are rejected at config load |

See [Resilience](resilience.md) for details on backoff behavior.

---

## Filters

```yaml
filters:
  whitelist_operations: []     # Operation names to INCLUDE (supports * wildcard)
  blacklist_operations:        # Operation name substrings to EXCLUDE at SQL level
    - "MergeTreeIndex"
    - "VFSWrite"
  whitelist_ips: []            # IP addresses to INCLUDE
  whitelist_users: []          # ClickHouse users to INCLUDE (strict — drops spans with unknown user)
  blacklist_users: []          # ClickHouse users to EXCLUDE (defense-in-depth alongside CH user grants)
  blacklist_queries:           # Regex patterns to EXCLUDE
    - "^SELECT \\* FROM system\\."
    - "SHOW TABLES"
    - "(?i)healthcheck"
  redact_queries:              # Regex replacements applied before export
    - pattern: "(?i)identified\\s+by\\s+'[^']*'"
      replacement: "IDENTIFIED BY '[REDACTED]'"
```

| Field | Default | Description |
|-------|---------|-------------|
| `whitelist_operations` | `[]` | If set, only export spans with matching operation names. Supports `*` wildcard (e.g., `DB::Interpreter*::execute()`) |
| `blacklist_operations` | `[]` | Substring patterns matched against `operation_name`. Pushed down to ClickHouse as `NOT LIKE '%pattern%'` so matching spans are never fetched. Use this to drop high-volume internal pipeline spans (`MergeTreeIndex`, `VFSWrite`, etc.) without affecting query-level spans |
| `whitelist_ips` | `[]` | If set, only export spans from these client IP addresses. Entries may be exact IPs (`10.0.1.50`) or CIDR ranges (`10.0.0.0/8`) |
| `whitelist_users` | `[]` | If set, only export spans whose `query_log.user` is on the list. Pushed down to enrichment SQL as `AND user IN (...)` and re-checked per span. Strict — spans with an unresolvable user are dropped |
| `blacklist_users` | `[]` | Never export spans whose `query_log.user` is on the list. Enforced at the Go layer for the span path (pushing it to enrichment SQL would strip the user and let blacklisted spans bypass the check); pushed down to SQL on the slow-query / backfill path |
| `blacklist_queries` | `[]` | Regex patterns matched against SQL query text. Matching spans are excluded |
| `redact_queries[].pattern` | — | Regex pattern matched against SQL query text after blacklist filtering and before export. Required when a redaction rule is present |
| `redact_queries[].replacement` | `[REDACTED]` | Replacement text used for matches. Empty or omitted uses the default replacement |

Note: `blacklist_operations` runs at the SQL level *before* data is fetched. `whitelist_operations` runs *after* fetch in Go. The blacklist always takes precedence because matching spans never reach the whitelist check.

See [Filtering](filtering.md) for detailed examples and evaluation order.

---

## Logging

```yaml
log_level: info                # debug, info, warn, error (default: info)
log_file: ""                   # Log file path (default: stderr)
log_format: text               # text or json (default: text)
log_rotation:
  max_size_mb: 100             # Rotate when the active file passes this size (default: 100; must be >= 1)
  max_files: 3                 # Keep this many rotated files (default: 3; must be >= 1)
```

| Field | Default | Description |
|-------|---------|-------------|
| `log_level` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |
| `log_file` | — | Optional file path. If empty, logs go to stderr (container-friendly) |
| `log_format` | `text` | Output format: `text` (human-readable) or `json` (structured). JSON emits `{"ts":"...","level":"...","msg":"..."}` |
| `log_rotation.max_size_mb` | `100` | Rotation threshold per file. **Omit** the key to take the default; an explicit `0` is rejected (would disable rotation and produce unbounded growth). |
| `log_rotation.max_files` | `3` | Number of rotated files to retain. Must be `>= 1`. |

### Log file permissions

When `log_file` is set, click-dog creates the file with mode **`0640`**
(owner: read/write, group: read, world: none). Operational metadata —
client IPs, operation names, error traces — is sensitive enough to keep
off world-readable storage but the service group still needs read access
for log shipping.

If a separate log-shipping agent runs alongside click-dog (Filebeat,
Fluent Bit node agent, journald forwarder, Vector, Promtail, etc.) under
a **different UID**, that agent must be in the click-dog service user's
**group** for log shipping to work. Otherwise the agent will silently
fail to read rotated files. Typical setup:

```bash
# Add the log shipper's user to click-dog's group
usermod -aG click-dog filebeat
systemctl restart filebeat
```

Earlier releases created the log file with mode `0644` (world-readable).
Upgrading deployments that relied on an unprivileged log shipper reading
the file via the world bit will need to adjust group membership as
above.

---

## Metrics

```yaml
metrics:
  enabled: false                          # Enable scrape + admin endpoints
  listen_address: ":9090"                 # Scrape listener — /metrics + legacy /health (default: :9090)
  admin_listen_address: "127.0.0.1:9091"  # Admin listener — POST /flush (default: 127.0.0.1:9091)
  otlp:
    enabled: false                        # Optional push path for self-metrics
    inherit_otel_connection: true          # Reuse exporters.otel[0] when enabled
    interval_seconds: 10
```

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `false` | Start both the scrape and admin HTTP listeners. Gates both — setting `false` also disables HTTP `POST /flush` (`SIGUSR1` still works) |
| `listen_address` | `:9090` | Scrape listener (`/metrics`, legacy `/health`) — safe to expose to your scraper |
| `admin_listen_address` | `127.0.0.1:9091` | Admin listener (`POST /flush`) — loopback by default; non-loopback binds emit a startup `WARN` |
| `otlp.enabled` | `false` | Push click-dog self-metrics over OTLP. Additive to the `/metrics` scrape endpoint; it may run with `metrics.enabled: false` |
| `otlp.inherit_otel_connection` | `true` when OTLP metrics are enabled and the key is omitted | Reuse `exporters.otel[0]` connection settings for the self-metrics exporter |
| `otlp.collector_address` | — | Standalone OTLP metrics collector address, required when `inherit_otel_connection: false` |
| `otlp.secure` | `false` | Enable TLS for a standalone OTLP metrics connection. Use inheritance when CA or mTLS settings are needed |
| `otlp.host` | OS hostname | Host identity used on pushed self-metrics |
| `otlp.service_name` | `exporters.otel[0].service_name`, then `click-dog-monitor` | Service name used on pushed self-metrics |
| `otlp.interval_seconds` | `10` | Push interval. Must be greater than 0 when `otlp.enabled` is true |
| `otlp.rename` | `{}` | Optional map from canonical self-metric keys to emitted OTLP metric names. Keys are validated at load time |

> The `metrics:` and `health:` blocks are **independent**. `metrics.enabled` does
> not control the Kubernetes-shaped `/healthz` / `/readyz` / `/status` probes —
> those live in the separate `health:` block below. Disabling metrics will not
> turn off the health listener, and vice versa.

See [Observability](observability.md#metrics-endpoint) for the full
endpoint table, the rationale for splitting scrape from admin, and
deployment recipes (systemd / Docker / Kubernetes).

---

## Health Endpoints

```yaml
health:
  enabled: false              # Off by default; install.sh enables it on :8686
  listen_address: ":8686"     # /healthz, /readyz, /status (default when enabled)
  cluster:
    enabled: false            # Leader-only /clusterz aggregate endpoint
    self: ""                  # Routable host:port for this instance
    peer_timeout_ms: 3000
    peers: []
```

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `false` | Start the health listener. Independent of `metrics.enabled` |
| `listen_address` | `:8686` when `health.enabled: true` | `/healthz` (liveness), `/readyz` (readiness — pings ClickHouse), `/status` (JSON report). If this normalizes to the same socket as `metrics.listen_address`, the handlers are mounted on the metrics listener so only one port is opened |
| `health.cluster.enabled` | `false` | Mount leader-only `/clusterz` aggregate health when `health.enabled` is also true |
| `health.cluster.self` | — | Routable `host:port` for this instance's `/readyz`; required when cluster health is enabled |
| `health.cluster.peer_timeout_ms` | `3000` | Per-peer `/readyz` fanout deadline |
| `health.cluster.peers` | `[]` | Static list of peer `host:port` addresses to include in `/clusterz` |

`/healthz` always returns `200`; `/readyz` returns `503` while ClickHouse is
unreachable or the circuit breaker is open. `/status` is a reporting endpoint
that always returns `200` — assess health from its JSON body, not its status
code.

See [Observability — Health Endpoints](observability.md#health-endpoints)
for the full endpoint table, the `/status` JSON shape, Kubernetes probe
recipes, and cluster-aware health roll-up.

---

## Webhook Notifications

```yaml
webhook:
  enabled: false
  url: ${SLACK_WEBHOOK_URL}    # Webhook URL (supports env vars)
  timeout_s: 10                # HTTP timeout in seconds (default: 10)
  events:                      # Event filter (default: all events)
    - circuit_breaker_opened
    - circuit_breaker_closed
    - startup
    - shutdown
```

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `false` | Enable webhook notifications |
| `url` | — | HTTP POST endpoint. Supports `${VAR}` expansion. Must be `http://` or `https://` with a host; redirects are not followed |
| `timeout_s` | `10` | HTTP request timeout in seconds |
| `events` | `[]` (all) | List of event names to fire on. Empty = all events |

Valid events: `circuit_breaker_opened`, `circuit_breaker_closed`, `backfill_complete`, `backfill_failed`, `error_spike`, `startup`, `shutdown`.

See [Observability](observability.md) for payload format, behavior details, and compatible services.

---

## Complete Example

See the `config.yaml.example` file in the repository root for a fully commented configuration file with all options.
