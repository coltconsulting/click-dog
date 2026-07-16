# Generic OTLP Integration

Click-Dog exports traces via OTLP/gRPC, making it compatible with any backend that accepts OpenTelemetry traces. This includes Grafana Tempo, Jaeger, Zipkin (via OTEL Collector), Elastic APM, and others.

!!! note "Packaged backend experiences"
    Generic OTLP export works today. Backend-specific dashboards, saved views,
    and alert recipes for Tempo, Jaeger, Elastic, and other receivers are being
    prioritized from user demand. If one of these would help your rollout, tell
    us which backend and which views matter via [Support](../support.md).

## Direct OTLP Backend

If your backend supports OTLP gRPC natively (e.g., Grafana Tempo, Jaeger with OTLP receiver):

```yaml
exporters:
  otel:
    - collector_address: tempo.internal:4317
      service_name: click-dog-monitor
```

---

## Via OpenTelemetry Collector

For backends that don't support OTLP directly, use an OTEL Collector as a proxy.

### Grafana Tempo

```yaml
# otel-collector-config.yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317

exporters:
  otlp:
    endpoint: tempo.internal:4317
    tls:
      insecure: true

service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [otlp]
```

### Jaeger

```yaml
# otel-collector-config.yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317

exporters:
  otlp:
    endpoint: jaeger.internal:4317
    tls:
      insecure: true

service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [otlp]
```

### Elastic APM

```yaml
# otel-collector-config.yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317

exporters:
  otlp/elastic:
    endpoint: apm-server.internal:8200
    headers:
      Authorization: "Bearer ${ELASTIC_APM_SECRET_TOKEN}"

service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [otlp/elastic]
```

### Multiple Backends

Send traces to multiple destinations simultaneously:

```yaml
# otel-collector-config.yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317

exporters:
  otlp/tempo:
    endpoint: tempo.internal:4317
    tls:
      insecure: true

  otlp/jaeger:
    endpoint: jaeger.internal:4317
    tls:
      insecure: true

service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [otlp/tempo, otlp/jaeger]
```

---

## TLS Configuration

### Plain TLS

```yaml
exporters:
  otel:
    - collector_address: otel-collector.internal:4317
      service_name: click-dog-monitor
      secure: true
      ca_cert: /path/to/ca.pem
```

### Mutual TLS (mTLS)

```yaml
exporters:
  otel:
    - collector_address: otel-collector.internal:4317
      service_name: click-dog-monitor
      secure: true
      ca_cert: /path/to/ca.pem
      client_cert: /path/to/client.pem
      client_key: /path/to/client-key.pem
```

Both `client_cert` and `client_key` must be provided together for mTLS.

---

## Exported Data Format

Click-Dog exports standard OTLP trace data. Every span includes:

### Resource Attributes
- `service.name`: Configured via `exporters.otel[].service_name`

### Instrumentation Scope
- Scope name: `clickhouse`

### Span Fields
- `trace_id`: Original ClickHouse trace ID (16 bytes)
- `span_id`: Original span ID (8 bytes)
- `parent_span_id`: Parent span ID (if any)
- `name`: Operation name from ClickHouse
- `kind`: Span kind (INTERNAL, SERVER, CLIENT, PRODUCER, CONSUMER)
- `start_time`: Microsecond precision from ClickHouse
- `end_time`: Microsecond precision from ClickHouse
- `status`: OK for live span-log spans; backfill query-log spans carry ERROR when the query failed (`exception_code != 0`)

### Span Attributes

See [Span Attributes Reference](../span-attributes.md) for the complete list.

---

## Click-Dog Configuration

```yaml
exporters:
  otel:
    - collector_address: your-endpoint:4317
      service_name: click-dog-monitor
      max_query_length: 100000
```

The endpoint, service name, and certificate-path string fields support
environment variable expansion:
```yaml
exporters:
  otel:
    - collector_address: ${OTEL_COLLECTOR_ADDRESS}
      service_name: ${OTEL_SERVICE_NAME}
```
