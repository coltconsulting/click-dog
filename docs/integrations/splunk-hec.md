# Splunk HEC Integration

Click-Dog can fan out span-shaped query events to Splunk HTTP Event Collector
(HEC) alongside OTLP exporters. Use this when Splunk is your operational search
surface or when you want ClickHouse query telemetry in an existing Splunk index.

!!! note "Packaged Splunk experience"
    HEC export works today. Packaged Splunk dashboards, saved searches, and alert
    recipes are being scoped. If you are using Click-Dog with Splunk, tell us
    which searches or dashboards would make this production-ready via
    [Support](../support.md).

## Configure Click-Dog

```yaml
exporters:
  splunk_hec:
    - endpoint: https://splunk.internal:8088
      token_file: /etc/click-dog/splunk-hec.token
      index: clickhouse
      source: click-dog
      source_type: _json
```

Use `https://` for HEC endpoints by default because the token is sent in the
`Authorization` header. Local development receivers can use `http://` only when
`allow_insecure_http: true` is set explicitly.

`token_file` keeps the HEC token out of the process environment and config file.
The inline `token: ${SPLUNK_HEC_TOKEN}` form is also supported, but `token` and
`token_file` are mutually exclusive.

## Verify

Run the connectivity and data-plane checks:

```bash
click-dog check -config /etc/click-dog/click-dog.yaml
```

To verify HEC credentials, routing, and Splunk ingestion end to end without
querying ClickHouse, send a synthetic span to every configured sink:

```bash
click-dog test-span -config /etc/click-dog/click-dog.yaml
```

The event carries `click_dog.test=true`, so it is easy to find or exclude.

Then search the configured Splunk index for events with:

```text
source="click-dog" sourcetype="_json"
```

The exported payload contains ClickHouse query text, durations, host metadata,
and the same redaction/truncation controls documented in
[Configuration](../configuration.md#multiple-export-backends).

## Multiple Backends

Splunk HEC can be combined with OTLP exporters:

```yaml
exporters:
  otel:
    - collector_address: datadog-agent.internal:4317
      service_name: click-dog-monitor
  splunk_hec:
    - endpoint: https://splunk.internal:8088
      token_file: /etc/click-dog/splunk-hec.token
      index: clickhouse
```

With multiple sinks configured, Click-Dog uses the strict all-required delivery
contract: a sink failure marks the cycle as an error and the next cycle retries
the full batch. See [Operating Contract](../operating.md#multi-sink-failure-semantics).
