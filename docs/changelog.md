# Changelog

All notable changes to Click-Dog will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/), and this project uses [calendar versioning](https://calver.org/) (`vYY.MM.idx`).

## [v26.07.3] - 2026-07-16

An accuracy release. The public site, README, and install path were audited
against shipping behavior; the corrections below include two code fixes and one
change to `install.sh`.

### Fixed
- **`service_name` now expands environment variables.** In an `exporters.otel` block, `service_name: ${OTEL_SERVICE}` was passed through as the literal string while every other field in the block (`collector_address`, `ca_cert`, `client_cert`, `client_key`) expanded. Config that templated the service name per environment exported under the unexpanded value.
- **The README's architecture link resolves.** It pointed at an internal-only contributor document that is never published, so the link was broken for every reader of the public repository. It now points at `docs/architecture.md`.
- **Resource-hog findings read correctly when exactly one metric trips** — `(1 metric above threshold)` rather than `(1 metrics above threshold)`.

### Changed
- **`install.sh` no longer requires `GITHUB_TOKEN`.** The token was mandatory while the repository was private, since the release and asset endpoints 404 unauthenticated. Now that releases are public it is optional, and useful only to raise the GitHub API rate limit on a shared host. The 401/403 and 404 diagnostics no longer attribute the failure to a missing token.
- **Duplicate-delivery documentation states what click-dog guarantees rather than what backends do.** Restart, retry, and leader-partition windows can resend spans carrying stable `(trace_id, span_id)` identities. The docs and code comments no longer assert that OTEL collectors or backends collapse those repeats — OTLP does not require it, and the behavior is backend-specific.
- **The site describes the product as public beta** under Apache 2.0, replacing the "truly open source — no restrictions, ever" copy on the homepage and closing CTA.

### Added
- **`CONTRIBUTING.md` documents how an external pull request lands.** The public repository is the contribution target; accepted PRs are imported into the development tree with authorship preserved and ship in the next release.

### Removed
- **The orphan `/docs/` index page.** Nothing linked to it once `llms.txt` was repointed at `/overview/`.

## [v26.07.2] - 2026-07-04

First public release. The application source is unchanged from the internal
v26.07.1 tag, whose release failed partway through (see below), but the builds
are versioned as v26.07.2 and the release pipeline is fixed.

### Fixed
- **Public release provenance is single-source.** Release tags run the full gate in both repositories, but artifacts and container images are built and signed only in the public `coltconsulting/click-dog` repository. OCI source labels now point to that public source tree.
- **Public mirror publishing no longer touches the internal working tree.** The fence script materializes the tagged archive only in the dedicated public checkout and retains its secret-scan backstop.

### Added
- **Secret-scan configuration** scopes the gitleaks exception to the query analyzers' metric-key literals in `internal/analysis/`, keeping the release gate strict everywhere else.

## [v26.07.1] - 2026-07-02

The internal release published private binary and checksum assets, then
GoReleaser failed during the container build after deriving the project name
from the wrong git remote. The tag and release were not mirrored to the public
repository, so these application changes first shipped publicly in v26.07.2.

### Added
- **Public source, documentation site, and verified release path** under Apache 2.0, with one aggregate commit per GA release.
- **Read-only query analysis and trace drilldown** through `click-dog analyze queries` and `click-dog analyze trace`, including deterministic JSON schemas for tooling.
- **OTLP self-metrics push** alongside the independent Prometheus scrape endpoint, plus updated Datadog health dashboards.
- **Self-metrics state is visible at startup and in `-validate`**, including an explicit disabled-state message so a blank health dashboard is diagnosable.
- **Topology self-audit and richer health reporting** for duplicate whole-cluster reader detection, `/status`, and leader-served `/clusterz` aggregation.

### Changed
- **Filtering and redaction are surfaced in generated profiles and dashboards**, including the query-ID enrichment prerequisite and high-volume internal-operation exclusions.
- **`clickhouse.port: 0` is rejected.** An omitted port still defaults to `9000`; an explicit value must be in the range 1-65535.
- **The public documentation is task-oriented**, with dedicated onboarding, support, integration, operating-contract, and machine-readable entry points.

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
- **Live-mode dedup keys off the composite `(trace_id, span_id)` pair** (issue #96). Previously the LRU dedup cache keyed off `span_id` alone, but OTLP span IDs are only unique within a trace — when two unrelated traces produced spans with the same 64-bit span ID (rare but possible), the second arrival was silently dropped instead of exported. The cache is now keyed by a `model.SpanKey { TraceID, SpanID }` struct, and exporter results carry accepted `SpanKey` values so the processor marks only successfully exported composite keys as seen. Partial-failure semantics are unchanged: failed exports still leave every unexported span available for retry. Repeat delivery preserves the same composite identity; downstream duplicate handling is backend-specific.

### Changed
- **Breaking: exporter results now include per-sink status** (issues #126, #130) — `SpanExporter.ExportSpans` and `ExportQuery` return `model.ExportResult` plus `error`, reporting total sent/accepted counts and one `ExportSinkStatus` per backend. Custom exporter implementations must update to the new interface. `MultiExporter` still keeps the strict all-sinks-required retry policy, while metrics and warning logs now expose which sink accepted or failed each export attempt via the `click_dog_export_*_total{sink}` counters.
- **MultiExporter span fan-out policy is explicit** (issue #127) — `NewMultiExporter` now accepts `WithMultiExporterPolicy(...)`. The default `PolicyAllRequired` preserves the strict all-sinks-required behavior; `PolicyAnySuccess` returns spans accepted by any successful sink while still surfacing failed sink status through `ExportResult`. `ExportQuery` remains all-required for backfill correctness. No YAML setting is added in this change; production config keeps the default strict policy.
- **MultiExporter is now strict by default** — when multiple sinks are configured, *any* sink failure (partial or total) returns an error from `ExportSpans` / `ExportQuery`. The previous behavior of treating "at least one sink succeeded" as success silently hid dead backends from the circuit breaker, adaptive backoff, error metrics, and `/readyz`. With the new contract every cycle that fails a sink is marked an error (`click_dog_cycle_results_total{result="error"}`) and feeds the breaker; sinks that previously succeeded receive the spans again on retry with stable IDs, and duplicates may remain visible downstream. See [Configuration — Multiple Export Backends](configuration.md#multiple-export-backends).
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
- **Normalized query family attributes** (issue #84) — when ClickHouse supports it, live spans carrying `clickhouse.query_id` gain `query_log.normalized_query_hash`; backfill query spans gain `db.normalized_query_hash`. Both paths include a literal-stripped normalized-query preview. Internal child spans without a query ID pass through without query-log enrichment. The Datadog query dashboard groups by these. See [Span Attributes — query_log Enrichment](span-attributes.md#query_log-enrichment) and [Query Family Rollups](span-attributes.md#query-family-rollups).
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
