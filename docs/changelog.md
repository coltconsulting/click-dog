# Changelog

All notable changes to Click-Dog will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/), and this project uses [calendar versioning](https://calver.org/) (`vYY.MM.idx`).

## [v26.06.6-beta] - 2026-06-12

### Changed
- **Breaking (beta): the default OTEL service name is now `click-dog-monitor`** (was `clickdog-monitor`) — brand consistency with everything else click-dog ships. Changed at every source of the value: the config default, `click-dog init` and the wizard, `install.sh -service`, the deploy templates (Ansible / Kubernetes / Docker), the example configs, the Datadog query dashboard's `$service` template-variable default, and the docs. A deployment that relied on the *default* exports under the new name after upgrading — update dashboards/monitors filtering `service:clickdog-monitor`, or pin `service_name: clickdog-monitor` in config to keep the old name. Re-run `click-dog create-dashboards --on-exists overwrite` to pick up the new dashboard default.
- **`create-dashboards` output now states what was found vs what was done** — `[query] not found in Datadog — creating`, `up to date — the live dashboard already matches this build`, `live dashboard differs from this build (live: X → build: Y)` — replacing the ambiguous `definition source: embedded (X)` line. Headless (non-interactive) runs print the same findings.
- **Install lifecycle defaults aligned across install paths** (issue #233) — the production `click-dog init` profile now enables the dedicated health listener, so default systemd installs serve `/healthz`/`/readyz` and `click-dog deploy status` works out of the box. The Ansible playbook picks up the production filter/metrics/health defaults, `Restart=on-failure` with start limits, and post-install `/healthz` (plus canary `/readyz`) verification. The guided update path for an existing install stages the new binary, rejects downgrades, validates the existing config before the swap, and keeps a `.prev` rollback binary.

### Added
- **HA leader/standby state is first-class in metrics** (issue #239) — new `click_dog_leader` gauge (`1` on the active leader, `0` on standbys) and a `role="active|standby"`-labeled variant of `click_dog_last_success_timestamp_seconds`. The bare legacy series is retained for existing scrapes; new HA-aware alerts should key on `role:active`. The shipped Datadog health dashboard and docs now exclude standbys from stale-export alerting.

### Fixed
- **Healthy HA standbys no longer read as unhealthy** (issue #239) — leader-gated standby cycles are recorded under an explicit skip reason, so `/status` reports a healthy idle standby while breaker-open skips remain unhealthy.
- **Legacy `ha.enabled` key now fails with a pointed error** (issue #237) — configs still carrying the removed key get *"ha.enabled has been removed: leader election is enabled by setting ha.keeper.hosts — delete the ha.enabled key"* instead of a generic unknown-field error.
- **Disabled or short-retention `query_log` no longer silently blinds the topology audit** (issue #238) — on zero-reader ticks the detector probes local `system.query_log` for recent rows and WARNs once if the window is empty (log-only; gauge and debounce behavior unchanged, and a healthy fleet never pays for the probe).
- **Topology warning remediation acknowledges legitimate duplication** (issue #241) — the `multi_instance_cluster_queries` remediation text, the dashboard's topology note, and the Datadog guide now say a red tile can also reflect bounded by-design duplication (Keeper-outage fail-open, long-running backfill from another host) and self-clears once resolved.

### Security
- **Self-update fails closed on oversized archive members** (issue #246) — a binary entry larger than the extraction limit is now rejected before extraction instead of being silently truncated by the limited reader.

## [v26.06.5-beta] - 2026-06-08

_Cumulative notes for the alpha/beta line through v26.06.5-beta._

### Fixed
- **Adaptive backoff now recovers after a sustained outage.** `RecordSuccess` only reset the poll interval to base when the *previous* success was within `reset_after_s`, but `RecordFailure` never advanced that timestamp — so after any outage long enough to push the interval past `reset_after_s` (default 60s), the poller stayed pinned at `max_interval` (default 300s) until restart, silently degrading span freshness ~10×. A single successful poll now snaps the interval straight back to base.
- **`query_log` backfill exports now get deterministic span IDs.** The OTLP `ExportQuery` path derived a fresh random `(trace_id, span_id)` on every call, so a partial-failure retry or a re-run backfill window produced duplicate spans the collector couldn't dedup. IDs are now derived deterministically from the ClickHouse `query_id` (random fallback only for the rare row with no `query_id`).
- **Live-mode dedup keys off the composite `(trace_id, span_id)` pair** (issue #96). Previously the LRU dedup cache keyed off `span_id` alone, but OTLP span IDs are only unique within a trace — when two unrelated traces produced spans with the same 64-bit span ID (rare but possible), the second arrival was silently dropped instead of exported. The cache is now keyed by a `model.SpanKey { TraceID, SpanID }` struct, and exporter results carry accepted `SpanKey` values so the processor marks only successfully exported composite keys as seen. Partial-failure semantics are unchanged: failed exports still leave every unexported span available for retry. Downstream collectors continue to dedup by `(trace_id, span_id)`.

### Changed
- **Breaking: exporter results now include per-sink status** (issues #126, #130) — `SpanExporter.ExportSpans` and `ExportQuery` return `model.ExportResult` plus `error`, reporting total sent/accepted counts and one `ExportSinkStatus` per backend. Custom exporter implementations must update to the new interface. `MultiExporter` still keeps the strict all-sinks-required retry policy, while metrics and warning logs now expose which sink accepted or failed each export attempt via the `click_dog_export_*_total{sink}` counters.
- **MultiExporter span fan-out policy is explicit** (issue #127) — `NewMultiExporter` now accepts `WithMultiExporterPolicy(...)`. The default `PolicyAllRequired` preserves the strict all-sinks-required behavior; `PolicyAnySuccess` returns spans accepted by any successful sink while still surfacing failed sink status through `ExportResult`. `ExportQuery` remains all-required for backfill correctness. No YAML setting is added in this change; production config keeps the default strict policy.
- **MultiExporter is now strict by default** — when multiple sinks are configured, *any* sink failure (partial or total) returns an error from `ExportSpans` / `ExportQuery`. The previous behavior of treating "at least one sink succeeded" as success silently hid dead backends from the circuit breaker, adaptive backoff, error metrics, and `/readyz`. With the new contract every cycle that fails a sink is marked an error (`click_dog_cycle_results_total{result="error"}`) and feeds the breaker; sinks that previously succeeded will receive the spans again on retry, and downstream collectors dedup by `(trace_id, span_id)`. See [Multi-Sink Architecture](https://github.com/coltconsulting/click-dog/blob/master/docs/development/architecture.md#multiexporter-behavior).
- **Processor pipeline extracted to `internal/processor`** (issue #133). The scheduled-mode loop, canary, heartbeat, enrichment, and backfill driver now live in a dedicated package. No behavioral change; this only affects custom forks that imported the old top-level `processor.go`.
- **OTLP span batches stream into size-bounded chunks** (issue #121). `OTELExporter.ExportSpans` now estimates the proto-encoded size of each span incrementally and flushes a chunk before the next span would push it over gRPC's 4 MiB ceiling. Recursive splitting handles pathological single-span overflow. Operators no longer need to size `batch_size` defensively around span text length; the exporter never sends a request larger than the gRPC limit.
- **Datadog Application Query Analysis dashboard is now explicitly live-span-only** — widget queries require `resource_name:query @click_dog.source:span_log` and the dashboard/docs call out the separate historical backfill contract (`clickhouse.query`, `click_dog.source=query_log`, `db.*`). Operators importing the updated dashboard need click-dog `v26.03.1` or newer, or any build that emits `click_dog.source`; older binaries will not match the new source filter until upgraded.

### Removed
- **Breaking: the legacy top-level `otel:` config key.** Use the `exporters.otel[]` list form (the documented form since `v25.04.0`). Configs still using the top-level `otel:` block now fail to load with an unknown-field error instead of being silently auto-promoted. Migrate `otel: { collector_address: … }` to `exporters: { otel: [ { collector_address: … } ] }`.
- **Breaking: the `click_dog_cycles_total` and `click_dog_errors_total` metrics.** Both are derivable from `click_dog_cycle_results_total{result}` (sum across results = total cycles; `{result="error"}` = error cycles). The shipped Datadog health dashboard and its OpenMetrics rename list have been repointed; external dashboards or alerts on the old names must migrate.
- **The `monitor.backoff.reset_after_s` setting.** Backoff now resets on the first successful poll (see Fixed above), so the knob no longer had any effect; it is rejected as an unknown config field. Remove it from existing configs.

### Added
- **Cluster query mode now supports normalized query fields when all replicas are compatible** (issue #184) — at startup click-dog probes `clusterAllReplicas('<cluster>', system.columns)` and enables `normalized_query_hash` / `normalized_query` only when every replica reports the column. Mixed-version, unreachable, or unconfigured-cluster setups keep the safe fallback. The verdict is logged and surfaced as the `normalized_query_hash support` line in `click-dog check` (with the `N of N replicas` count).
- **Unresolved `${VAR}` config references now warn at load.** A typo'd or unset braced reference such as `${CLICKHOUSE_PASSWORD}` previously expanded silently to an empty string; click-dog now emits a warning at startup and under `-validate` naming the unset variable.
- **Per-call exporter timeout** (`monitor.export_timeout_s`, default `30`s) — every backend call (live span batch, backfill query, degraded-mode canary) runs under this deadline. A stuck collector now fails into the normal retry / backoff / circuit-breaker path instead of pinning a cycle indefinitely. Multi-sink fan-out shares the same budget rather than multiplying it per sink. Set to `0` to disable the client-side deadline.
- **Per-sink export observability counters** — `click_dog_export_attempts_total{sink}`, `click_dog_export_accepted_total{sink}`, and `click_dog_export_errors_total{sink}` expose which backend accepted or failed each export attempt. With a single configured exporter the label is the bare kind (`otel` or `splunk_hec`); with the multi-sink fan-out wrapper each sink is labeled `otel[<i>]:<collector_address>` / `splunk_hec[<i>]:<endpoint>` so dashboards can distinguish the individual backends. See [Observability — Available Metrics](observability.md#available-metrics).
- **Normalized query family attributes** (issue #84) — when ClickHouse supports it, every exported span gains `query_log.normalized_query_hash` (live) or `db.normalized_query_hash` (backfill) and a literal-stripped `normalized_query` preview. The Datadog query dashboard groups by these. See [Span Attributes — query_log Enrichment](span-attributes.md#query_log-enrichment) and [Query Family Rollups](span-attributes.md#query-family-rollups).
- **Datadog health-cockpit dashboard** — the shipped `dashboards/datadog-clickdog-health.json` now leads with current action state (last success age, breaker, error rate, throughput, backoff) and follows with trends and lifetime counters. `click-dog create-dashboards` provisions it alongside the query dashboard.
- **`/clusterz` aggregate health endpoint** (phase 1 of issue #19) — the HA leader can fan out `/readyz` to a statically configured peer list and serve a single dashboard / on-call URL. Followers return `404 {"status":"not_leader"}`. See [Observability — Cluster Health](observability.md#cluster-health).

### Added (earlier in this development line)
- **Multi-sink exporter architecture** — `SpanExporter` interface allows multiple export backends simultaneously.
- **MultiExporter** — fan-out wrapper that sends spans to all configured backends.
- **Multiple OTEL backends** — configure multiple `exporters.otel[]` entries (and/or `exporters.splunk_hec[]`) for redundancy or multi-destination export.
- **Heartbeat logging** — periodic summary logs replace per-cycle noise for quieter operation.
- **Degradation simulation tests** — Docker-based tests that validate circuit breaker and backoff against real cluster failures.
- **Validate mode** — `-validate` flag to check config without connecting.
- Official documentation site with user/developer split.
- Deployment guide (install script, Ansible, Kubernetes, systemd, containers).
- Integration guides for Datadog, Honeycomb, and generic OTLP.
- Architecture and testing guides for contributors.

### Changed (earlier in this development line)
- Renamed `max_queries_per_run` to `max_spans_per_cycle` for clarity. Config loading now rejects unknown keys, so the old name surfaces as a load-time error — rename it to `max_spans_per_cycle`.
- **`max_spans_per_cycle` now caps spans, not traces** (issue #91). Previously the value bounded the trace ID query and step 2 multiplied it by 100, so up to `max_spans_per_cycle * 100` spans could be fetched and exported per cycle (clamped at the internal `MaxSpanQueryLimit` of 100,000). The configured value is now the actual SQL `LIMIT` on the spans query, matching the documented behavior. **Operator impact:** under the same config, total exported span volume per cycle drops from up to 100x the cap to at most the cap. If you previously sized the value down to compensate, you may now need to raise it; partial traces continue across cycles via the lookback overlap + dedup cache.
- Degradation tests excluded from regular CI, available as separate GitHub Actions workflow.
- Default `log_level` is `info` (not `error` as previously documented).

### Security
- **Log file mode tightened to `0640`** — previously `0644` (world-readable). A separate log-shipping agent running under a different UID must now share the click-dog service user's group; see [Logging](configuration.md#log-file-permissions) for setup notes.
- **`log_rotation.max_size_mb: 0` and `max_files: 0` are rejected** — explicit `0` would silently disable rotation and produce unbounded log growth. Omit the keys to take the defaults (100 MB / 3 files).
- **Wider credential redaction in log output** — sanitizer now scrubs space-separated forms (e.g. `password 'x'`, `AuthToken xyz`) emitted by some driver / wrapper errors. Defense-in-depth; no known leak today.

### Project
- **Apache 2.0 + DCO contribution model** — Click-Dog is licensed under [Apache 2.0](https://github.com/coltconsulting/click-dog/blob/master/LICENSE) and contributions require a [DCO](https://developercertificate.org/) `Signed-off-by` trailer on every commit (`git commit -s`). The root [CONTRIBUTING.md](https://github.com/coltconsulting/click-dog/blob/master/CONTRIBUTING.md) is the authoritative source; [TRADEMARKS.md](https://github.com/coltconsulting/click-dog/blob/master/TRADEMARKS.md) and [NOTICE](https://github.com/coltconsulting/click-dog/blob/master/NOTICE) cover the trademark policy and third-party attributions respectively.
- **`gofmt` drift is enforced in CI and `make preflight`** — `make fmt-check` runs the same sweep both gates use; format your tree before pushing or CI will bounce the PR.
- **Hybrid installer** — the install path is now a thin bootstrap shell that downloads and cosign-verifies the signed release archive, then hands off to per-mode templates under `deploy/templates/` for Kubernetes and Docker output. Multi-host SSH plumbing is no longer in the shell entry point; use the Ansible playbook in `deploy/ansible/` for multi-node rollouts. See [Install](install.md).

---

## [0.1.0] - Initial Release

### Added
- OpenTelemetry span export from `system.opentelemetry_span_log`
- Query log export from `system.query_log` (backfill mode)
- Scheduled mode with configurable polling interval
- Backfill mode for historical time ranges
- Two-tier duration filtering (trace-level and span-level)
- SQL-level filtering for efficiency
- LRU deduplication cache (configurable size, default 10,000)
- Batch processing with configurable size and delay
- Circuit breaker pattern for ClickHouse protection
- Adaptive backoff with exponential retry
- Connection health checks
- Operation whitelist with wildcard support
- IP whitelist
- Query blacklist with regex patterns
- Query length filtering (fetch-time skip + export-time truncation)
- Rate limiting (`max_spans_per_cycle`)
- Connection pooling with configurable limits
- Query timeouts
- ClickHouse cluster mode support (`cluster()` function)
- TLS support for both ClickHouse and OTEL connections
- Mutual TLS (mTLS) for OTEL exporter
- Environment variable expansion in config (`${VAR}` and `$VAR`)
- Configurable logging (debug/info/warn/error) to stderr or file
- YAML-based configuration
- CI/CD with GitHub Actions
- Automated releases with GoReleaser
- Comprehensive test suite with mock OTEL collector
