# Honeycomb Integration

Click-Dog exports traces to Honeycomb via an OpenTelemetry Collector. Honeycomb natively supports OTLP, making integration straightforward.

!!! note "Packaged Honeycomb experience"
    OTLP export works today. Packaged Boards, query templates, and alert
    recipes are being scoped. If you are using Click-Dog with Honeycomb, tell us
    what would make this production-ready for your team via
    [Support](../support.md).

## Setup

### 1. Configure the OpenTelemetry Collector

Create an OTEL Collector config (`otel-collector-config.yaml`):

```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317

exporters:
  otlp:
    endpoint: api.honeycomb.io:443
    headers:
      x-honeycomb-team: ${HONEYCOMB_API_KEY}
      x-honeycomb-dataset: clickhouse-traces

service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [otlp]
```

Run the collector:

```bash
# Docker
docker run -d \
  -e HONEYCOMB_API_KEY=your-api-key \
  -p 4317:4317 \
  -v $(pwd)/otel-collector-config.yaml:/etc/otelcol/config.yaml \
  otel/opentelemetry-collector-contrib:latest

# Or binary
otelcol-contrib --config otel-collector-config.yaml
```

### 2. Configure Click-Dog

```yaml
exporters:
  otel:
    - collector_address: localhost:4317
      service_name: click-dog-monitor
```

If the OTEL Collector is on a separate host:
```yaml
exporters:
  otel:
    - collector_address: otel-collector.internal:4317
      service_name: click-dog-monitor
```

### 3. Verify

In Honeycomb, navigate to the `clickhouse-traces` dataset (or whichever dataset you configured) and look for traces with:
- `service.name` = `click-dog-monitor`
- Attributes like `db.statement`, `duration_ms`, `hostname`

---

## Honeycomb-Specific Tips

### Dataset Naming

The dataset in Honeycomb is determined by the `x-honeycomb-dataset` header in your OTEL Collector config. For Honeycomb Environments (the newer model), traces are routed based on the `service.name` resource attribute instead.

### Useful Queries in Honeycomb

**Slow queries heatmap:**
- Visualize: `HEATMAP(duration_ms)`
- Group by: `hostname`

**Top slow queries:**
- Visualize: `MAX(duration_ms)`
- Group by: `db.statement`

**Error rate:**
- Visualize: `COUNT`
- Where: `error = true`

The `error` attribute is present on backfill spans from `system.query_log`.
Scheduled/live spans from `system.opentelemetry_span_log` normally report status
OK, so live-mode error analysis should use ClickHouse exception attributes from
query-log enrichment when available.

### Sampling

If you're generating too many traces, you can add a tail sampler in the OTEL Collector:

```yaml
processors:
  tail_sampling:
    decision_wait: 10s
    policies:
      - name: slow-queries
        type: latency
        latency:
          threshold_ms: 5000

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [tail_sampling]
      exporters: [otlp]
```

---

## With TLS

If your OTEL Collector requires TLS:

```yaml
exporters:
  otel:
    - collector_address: otel-collector.internal:4317
      service_name: click-dog-monitor
      secure: true
      ca_cert: /path/to/ca.pem
```

---

## Example: Full Setup

```yaml
clickhouse:
  host: ${CLICKHOUSE_HOST}
  port: 9000
  database: default
  username: ${CLICKHOUSE_USERNAME}
  password: ${CLICKHOUSE_PASSWORD}

exporters:
  otel:
    - collector_address: otel-collector.internal:4317
      service_name: click-dog-monitor

monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30

filters:
  blacklist_queries:
    - "^SELECT \\* FROM system\\."
    - "(?i)healthcheck"

log_level: info
```
