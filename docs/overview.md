# Documentation

Click-Dog reads query spans from ClickHouse `system.opentelemetry_span_log` and
exports them to OTLP-compatible backends and Splunk HEC. Start with
[Getting Started](getting-started.md), then branch into your backend and your
operations from the sections below.

## Get started

<div class="grid cards" markdown>

-   __[Getting Started](getting-started.md)__

    From a fresh host to a running export.

-   __[Install](install.md)__

    Run Click-Dog as a systemd service, a container, or a single binary.

-   __[Configuration](configuration.md)__

    The full YAML reference — every key, default, and env-var override.

</div>

## Integrations

<div class="grid cards" markdown>

-   __[Datadog](integrations/datadog.md)__

    The packaged path: OTLP export, provisioned dashboards, and self-monitoring.

-   __[Honeycomb](integrations/honeycomb.md)__

    Export traces to Honeycomb through an OpenTelemetry Collector.

-   __[Generic OTLP](integrations/generic-otlp.md)__

    Send to any OTLP/gRPC backend — Tempo, Jaeger, Elastic APM, and more.

-   __[Splunk HEC](integrations/splunk-hec.md)__

    Fan out span-shaped query events to Splunk HTTP Event Collector.

</div>

## Query analysis

<div class="grid cards" markdown>

-   __[Query Analysis](query-analysis.md)__

    Explain query behavior, not just export spans.

-   __[Span Attributes](span-attributes.md)__

    Every attribute carried on the spans Click-Dog exports.

</div>

## Operate

<div class="grid cards" markdown>

-   __[Observability](observability.md)__

    Watch Click-Dog itself: structured logs, Prometheus metrics, webhooks.

-   __[Resilience](resilience.md)__

    Circuit breaker and adaptive backoff that protect ClickHouse and Click-Dog.

-   __[Troubleshooting](troubleshooting.md)__

    Diagnose common issues, starting with debug logging.

-   __[Operating Contract](operating.md)__

    What the public beta guarantees — and what it doesn't.

-   __[Kubernetes — ClickHouse Setup](kubernetes-clickhouse.md)__

    Deploy the collector side against a ClickHouse operator.

</div>

## Reference

<div class="grid cards" markdown>

-   __[Operation Modes](modes.md)__

    Scheduled, backfill, validation, dry-run, analysis, and manifest generation.

-   __[Filtering](filtering.md)__

    The layers that decide which spans get exported.

-   __[Architecture](architecture.md)__

    How the single Go binary sits beside your database.

-   __[Changelog](changelog.md)__

    Notable changes across releases.

-   __[Support](support.md)__

    Community and commercial support paths.

</div>
