# Getting Started

This guide takes the shortest path from a fresh host to a running Click-Dog
service, then confirms the ClickHouse data plane and shows where to see the
first value. For Kubernetes, Docker, Ansible, air-gapped installs, and manual
verification paths, use the full [Install guide](install.md).

## 1. Install

The recommended installer verifies release artifacts against a cosign-signed
`checksums.txt` before installing and fails closed if anything is missing or
tampered with. Download it, optionally inspect it, then run the guided
quickstart:

```bash
curl -fsSL https://github.com/coltconsulting/click-dog/releases/latest/download/install.sh -o install.sh
less install.sh
sudo bash install.sh
```

The guided quickstart walks you through enabling ClickHouse span logging, creating
a dedicated read-only `click_dog_monitor` user, confirming your OTEL collector,
and installing the binary + optional systemd unit. Nothing is installed until you
confirm. The installer requires the
[cosign CLI](https://docs.sigstore.dev/cosign/installation/) on PATH for release
signature verification.

## 2. Verify

After install, validate the generated config and run the end-to-end readiness
check:

```bash
click-dog -validate -config /etc/click-dog/click-dog.yaml
click-dog check -config /etc/click-dog/click-dog.yaml
```

`-validate` parses the YAML offline. `click-dog check` connects to ClickHouse
and the exporter, verifies span/query-log grants, checks recent spans, confirms
`clickhouse.query_id` enrichment, and reports whether `normalized_query_hash` is
available for query-family dashboards.

Check the installed service and health endpoints:

```bash
click-dog deploy status
curl -s localhost:8686/status | jq
curl -s localhost:9090/metrics | head
```

The guided install enables the dedicated health listener on `:8686` and the
Prometheus scrape listener on `:9090` in the generated config.

## 3. See Value

For Datadog, create the packaged query-analysis and health dashboards:

```bash
DD_API_KEY=... DD_APP_KEY=... click-dog create-dashboards
```

For any backend, run a local read-only query analysis report:

```bash
click-dog analyze queries -config /etc/click-dog/click-dog.yaml
```

The service runs scheduled mode by default, polling
`system.opentelemetry_span_log` every `check_interval_s` seconds and exporting
qualifying spans to your configured backend. See [Operation Modes](modes.md) for
dry-run, backfill, and manifest generation.

## Next steps

- [Install](install.md) — single-node, Kubernetes, Docker, Ansible, air-gapped, and updating
- [Datadog](integrations/datadog.md) — packaged dashboards, self-monitoring, and monitors
- [Query Analysis](query-analysis.md) — local read-only analysis reports
- [Honeycomb](integrations/honeycomb.md) · [Generic OTLP](integrations/generic-otlp.md) — export setup and backend-specific interest notes
- [Configuration](configuration.md) — full reference for all settings
- [Operating Contract](operating.md) — delivery guarantees, compatibility, tuning
