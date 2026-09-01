<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://click-dog.com/assets/click-dog-logo-light.svg">
    <img alt="Click-Dog" src="https://click-dog.com/assets/click-dog-logo.svg" width="140">
  </picture>
</p>

# Click-Dog

> [!NOTE]
> **Public beta.** Click-Dog is newly open source, so the public API and config may still change. It has run in production for months.

Operational query observability without making the source system harder to run.

Click-Dog turns database telemetry into traces, dashboards, and query-analysis
signals for OTEL-compatible backends such as Datadog, Honeycomb, Grafana Tempo,
and more. Today it reads ClickHouse `system.opentelemetry_span_log` for live
telemetry and `system.query_log` for historical backfill; additional source
adapters are planned.

It limits source load with zero DDL, read-only connections, bounded export
volume, SQL-level filtering, circuit breaker protection, and adaptive backoff.

## What ships

| Capability | What it provides |
|---|---|
| **Native tracing and smoke tests** | Export ClickHouse parent-child spans, then validate source readiness, exporter acceptance, and native trace topology before go-live. |
| **Query families and regression analysis** | Group queries by normalized hash, capture an explicit known-good baseline, and detect conservative latency regressions and failure spikes. |
| **Finding policy and notifications** | Gate automation with `-fail-on warning` or `-fail-on critical`; explicitly send eligible new or critical findings to webhooks and Datadog Events with `-notify`. |
| **Query-text privacy modes** | Select `raw`, `redacted`, `normalized_only`, or `none` at the final export boundary. |
| **Dashboards, health, and resilience** | Provision Datadog dashboards, monitor each sink independently, and protect ClickHouse with bounded polling, a circuit breaker, and adaptive backoff. |

## Install

### Recommended: verified installer

The installer verifies release artifacts with a cosign-signed `checksums.txt`
before installing and fails closed if anything is missing or tampered with.
Inspect the script first if you want to read what runs.

```bash
curl -fsSL https://github.com/coltconsulting/click-dog/releases/latest/download/install.sh -o install.sh
less install.sh           # inspect (optional)
sudo bash install.sh      # guided quickstart
# or unattended:
sudo env CLICKHOUSE_PASSWORD=... bash install.sh install -c <collector-host>:4317 --systemd
```

Requires the [cosign CLI](https://docs.sigstore.dev/cosign/installation/) on
PATH. See the [Install Guide](https://click-dog.com/install/) for Ansible, Kubernetes, and
Docker deployment modes.

### Manual download (verify yourself)

If you cannot run the installer, follow the verified manual recipe at
[Verifying releases manually](https://click-dog.com/install/#verifying-releases-manually).
It walks through fetching the archive plus the cosign-signed `checksums.txt`,
verifying the signature against the pinned release-workflow identity, and
verifying the archive's SHA-256 against the signed manifest before extraction.

Do not skip the verification steps. Without the signature and checksum checks,
installation trusts whatever controls the TLS connection and release
distribution path.

### Build from source

```bash
git clone https://github.com/coltconsulting/click-dog.git
cd click-dog
make build
```

## Quick Start

Copy one of the parity-tested starter configs from `examples/` (also included
in every binary archive), or create `click-dog.yaml`:

```yaml
clickhouse:
  host: localhost
  port: 9000
  username: default
  password: ${CLICKHOUSE_PASSWORD}

exporters:
  otel:
    - collector_address: localhost:4317
      service_name: click-dog-monitor

filters:
  query_text_mode: normalized_only

monitor:
  enabled: true
  min_trace_duration_ms: 1000
  check_interval_s: 30

log_level: info
```

Run it:

```bash
export CLICKHOUSE_PASSWORD="..."
click-dog -config click-dog.yaml
```

Validate config without running:

```bash
click-dog -validate -config click-dog.yaml
```

> **Datadog dashboards:** the config above exports traces, which is enough for the
> **Application Query Analysis** and **Exported User Activity** dashboards
> (`click-dog create-dashboards --dashboard query` / `--dashboard activity`).
> The **Health** dashboard reads click-dog's own metrics.
> For the OTLP path, enable `metrics.otlp.enabled: true`. The
> installer-generated / `click-dog init --profile production` configs enable the
> Prometheus `/metrics` endpoint for the legacy Datadog Agent OpenMetrics scrape
> fallback. See
> [Datadog Self-Monitoring](https://click-dog.com/integrations/datadog/self-monitoring/).

## Modes

| Mode | Usage |
|------|-------|
| **Scheduled** (default) | Continuous polling with circuit breaker + adaptive backoff |
| **Backfill** | One-shot historical export over an RFC 3339 range: `-backfill-start YYYY-MM-DDT00:00:00Z -backfill-end YYYY-MM-DDT00:00:00Z` |
| **Validate** | Check config and exit: `-validate` |
| **Dry-run** | Read real data, discard exports, print a one-shot summary and exit: `--dry-run` |
| **Test** | Exercise exporter acceptance or native ClickHouse tracing without starting the scheduled service: `test export` / `test tracing` |
| **Analyze** | Local read-only query reports: `analyze queries` and `analyze trace` |

`click-dog deploy <kubernetes\|docker\|status>` is a separate subcommand
family that emits deployment manifests or reports installed state.
see [Operation Modes](https://click-dog.com/modes/#click-dog-deploy-manifest-generators).

## Documentation

- [Install Guide](https://click-dog.com/install/): single-node `install.sh`, Kubernetes / Docker template rendering, Ansible playbook for fleet rollouts
- [Configuration Reference](https://click-dog.com/configuration/): narrative reference; [config.yaml.example](config.yaml.example) is the fully commented YAML companion
- [Operation Modes](https://click-dog.com/modes/): scheduled, backfill, validate, dry-run, test, analyze, and deploy
- [Query Analysis](https://click-dog.com/query-analysis/): query families, known-good baselines, regression findings, policy gates, and notifications
- [Security and Privacy](https://click-dog.com/security/): query-text modes, secret handling, and privacy boundaries
- [Observability](https://click-dog.com/observability/): `/metrics`, `/healthz`, `/readyz`, `/status`, webhooks, per-sink export counters
- [Operating Contract](https://click-dog.com/operating/): beta delivery guarantees and loss windows, compatibility matrix, performance tuning, cost estimation, cluster/sidecar topology, config at scale
- [Span Attributes](https://click-dog.com/span-attributes/): live spans, backfill query attributes, normalized query family
- [Datadog](https://click-dog.com/integrations/datadog/) / [Honeycomb](https://click-dog.com/integrations/honeycomb/) / [Generic OTLP](https://click-dog.com/integrations/generic-otlp/): backend integration guides
- [Architecture](https://click-dog.com/architecture/): source, processing pipeline, sinks, and deployment topology

## Building

Every target is prefixed by what it does: `test-` runs tests, `check-` runs
tests plus linting and analysis, `build-` produces artifacts, `release-` ships
something, and `print-` only tells you something. `make help` lists them all.

| Target | Does |
|---|---|
| `make help` | List available targets |
| `make doctor` | Diagnose branch, remote, toolchain, and caches |
| `make fmt` | Format every Go file with gofmt |
| `make build` | Build the binary for this platform, version injected |
| `make run` | Run locally against `config.yaml` |
| `make clean` | Remove built binaries and site output |
| `make test` | Run unit tests |
| `make test-race` | Run unit tests with the race detector |
| `make check-fast` | Formatting + unit tests |
| `make check` | The full PR gate — run this before pushing (needs `make dev-setup` once) |
| `make check-lint` | golangci-lint on its own, for iterating on failures |

Less common: `make build-all` cross-compiles linux amd64 and arm64,
`make test-integration` runs the full suite against a ClickHouse cluster
(requires Docker), and `make check-deep` adds race, vulnerability, and
integration coverage on top of `make check`.

## Security

To report a vulnerability, see [SECURITY.md](SECURITY.md). Please do not open a public GitHub issue for security reports.

## Contributing

Contributions are welcome under the [Apache 2.0 License](LICENSE) and require a [DCO](https://developercertificate.org/) `Signed-off-by` trailer on every commit (`git commit -s`). See [CONTRIBUTING.md](CONTRIBUTING.md).

## License & Trademarks

Source code is licensed under [Apache 2.0](LICENSE).

"Click-Dog" is an unregistered trademark of Colt Consulting Ltd. Apache 2.0 does not grant rights to use the trademark; see [TRADEMARKS.md](TRADEMARKS.md) for the policy. Third-party attributions are in [NOTICE](NOTICE).
