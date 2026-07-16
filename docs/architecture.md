# Architecture

Click-Dog is a single static Go binary that sits **beside** your database — never
in the request path. It reads telemetry over a read-only connection, filters and
batches at the source, then fans out to one or more OTLP / Splunk HEC backends.

## One bounded pipeline, source to sink

```
   Source                     Click-Dog                       Sinks (fan-out)
┌──────────────┐          ┌──────────────────┐          ┌────────────────────┐
│ ClickHouse   │          │ poll → filter →  │          │ Datadog (OTLP/gRPC)│
│ system.      │  read-   │ dedupe → batch   │  OTLP /  │ Honeycomb / Tempo  │
│ opentelemetry│  only ─▶ │                  │  HEC  ─▶ │ Jaeger / Elastic   │
│ _span_log    │          │ circuit breaker  │          │ Splunk HEC         │
│ system.      │          │ + adaptive       │          │ any OTLP backend   │
│ query_log    │          │ backoff + leader │          │                    │
└──────────────┘          └──────────────────┘          └────────────────────┘
        read-only · no DDL          one process per read scope
```

### Source

The first supported source is ClickHouse. Live telemetry comes from
`system.opentelemetry_span_log`; historical [backfill](modes.md) reads
`system.query_log`. All connections enforce **`readonly=2`** and require no DDL —
Click-Dog never writes to or alters the source system. Additional source adapters
are planned and reuse the same low-impact collection model.

### Click-Dog (the collector)

Each cycle polls the source, applies SQL-level duration filters at the database so
unqualifying data never leaves ClickHouse, deduplicates against an in-memory LRU
cache, batches, and exports. The pipeline is protected by a **circuit breaker**
(blocks cycles after repeated failures) layered with **adaptive backoff**
(stretches the poll interval under sustained failure), so a struggling source or
collector can't be hammered. In cluster-query HA deployments,
**Keeper-backed leader election** gates the shared whole-cluster read so only
one instance exports it at a time. Sidecars always export their disjoint
node-local scopes; Keeper leadership there is coordination-only.

### Sinks

A `MultiExporter` fans every batch out to one or more backends over OTLP/gRPC or
Splunk HEC, with per-sink health tracking so you know exactly which backend is
healthy and which is dropping spans. See the integration guides for
[Datadog](integrations/datadog.md), [Honeycomb](integrations/honeycomb.md), and
[Generic OTLP](integrations/generic-otlp.md) (Tempo, Jaeger, Elastic, and more).

## Why it stays out of the way

| Property | How |
|---|---|
| Read-only, non-invasive | `readonly=2` connections, zero DDL, bounded poll volume |
| Low source impact | SQL-level filtering, batching, capped spans per cycle, small connection pool |
| Self-protecting | Circuit breaker + adaptive backoff (see [Resilience](resilience.md)) |
| Safe to run HA | Cluster reads are leader-gated; sidecars keep independent local read scopes |
| Observable | `/healthz`, `/readyz`, `/status`, Prometheus `/metrics` (see [Observability](observability.md)) |

## Topology

Click-Dog can run as a **sidecar per ClickHouse node** (each instance reads its
local node) or as a **centralized deployment** that reads the whole cluster via
`cluster()` queries. See [Kubernetes — ClickHouse Setup](kubernetes-clickhouse.md)
for the topology trade-offs and the [Operating Contract](operating.md) for
cluster/sidecar placement guidance.
