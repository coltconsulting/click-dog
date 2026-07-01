# CLAUDE.md — click-dog

## What is click-dog?

A standalone Go binary that reads OpenTelemetry spans from ClickHouse `system.opentelemetry_span_log` and exports them via gRPC to OTEL-compatible backends (Datadog, Honeycomb, etc.). Solves the gap where ClickHouse generates spans but has no native export tool.

## Build & Test

```bash
make build            # Build for current platform
make test             # Unit tests
make test-race        # Unit tests with race detector
make lint             # golangci-lint
make vulncheck        # govulncheck ./... (release gate)
make fmt              # go fmt
make integration      # Full integration suite (requires Docker)
make preflight        # Pre-release checks (includes govulncheck)
```

- Go 1.25+ required for compiling (`go` directive); CI and releases use Go 1.26 so the govulncheck gate can clear stdlib advisories that are unfixed in earlier patch releases (e.g. GO-2026-5037/5039, fixed in go1.26.4)
- Integration tests use Docker Compose with a 3-node ClickHouse cluster
- Integration tests are tagged: `// +build integration`
- CI runs: lint, build (linux/amd64), govulncheck, test (PR: no -race; master/tag: with -race) — integration-test and build-arm64 are gated to non-PR runs (master push, tag push via release.yml, or workflow_dispatch) to keep PR billing down on a private repo
- `govulncheck ./...` is enforced both in CI and `make preflight`; install with `go install golang.org/x/vuln/cmd/govulncheck@v1.3.0` (CI pin)

## Project Structure

Top of tree (`package main`):

```
main.go                      Entry point, CLI flag/subcommand routing, scheduled+backfill wiring
dryrun.go                    DryRunExporter — discards exports and prints a summary
health_adapter.go            Wires the processor's runtime state into internal/health probes
cmd_init.go                  `click-dog init` — generate starter config
cmd_check.go                 `click-dog check` — validate config + connectivity
cmd_test_span.go             `click-dog test-span` — synthetic span sender
cmd_flush.go                 `click-dog flush` — one-shot lookback export
cmd_selfupdate.go            `click-dog self-update` — in-place binary update
cmd_create_dashboards.go     `click-dog create-dashboards` — Datadog dashboard provisioner
cmd_deploy.go                `click-dog deploy` — dispatcher for the deploy subcommand family
cmd_deploy_generate.go       Shared manifest renderer for `deploy kubernetes` and `deploy docker`
cmd_deploy_status.go         `click-dog deploy status` — query installed-deployment state
cmd_analyze.go               `click-dog analyze` — dispatcher + `analyze queries` (deterministic local query analysis)
cmd_analyze_trace.go         `click-dog analyze trace` — query-family trace drilldown + fan-out
cmd_analyze_trace_wizard.go  Guided `-wizard` flow backing `analyze trace`
```

`click-dog init` and `click-dog init --wizard` share a single YAML renderer
(`renderWizardYAML` in `cmd_init_wizard.go`). `install.sh` never emits YAML
itself; its quickstart / install / docker paths shell out to
`click-dog init` for the actual rendering. The `docs/examples/click-dog-*.yaml`
files are the reference output for each profile and are pinned against the
renderer by `TestWizard_ParityWithProfileTemplates`.

Internal packages (`internal/`):

```
analysis/     Query-analysis report contract + deterministic analyzers (pure; powers `analyze queries`)
clickhouse/   ClickHouse span/query reader (enforces readonly=2)
clicklog/     Custom leveled logging + rotating file writer
config/       YAML config loading, validation, defaults, env expansion, deprecation compat
deploy/       Templates used by `deploy kubernetes` and `deploy docker`
export/       OTEL gRPC + Splunk HEC exporters; MultiExporter fan-out
filter/       Query/IP/operation filtering + SQL redaction
health/       /healthz, /readyz, /status probe server (separate listener from metrics)
leader/       Leader election via ClickHouse Keeper (HA mode)
metrics/      Prometheus metrics + scrape HTTP server (also serves legacy /health)
model/        Shared types: spans, queries, exporter interfaces
processor/    Span fetch/filter/export pipeline + circuit-breaker/canary orchestration
queryfamily/  Normalized-query rollup family computation
resilience/   Circuit breaker + adaptive polling backoff
selfaudit/    In-process topology self-audit (sidecar+cluster anti-pattern); warning-only, never gates readiness
updater/      Self-update: GitHub release fetch, version compare, binary swap
webhook/      Outbound webhook notifier for lifecycle events
```

Other top-level dirs:

```
testing/                  Docker Compose + configs for integration tests
deploy/                   Ansible playbooks + Kubernetes manifests + install.sh
dashboards/               Datadog dashboard JSON templates
docs/                     MkDocs documentation site
docs/examples/            Canonical per-profile click-dog.yaml outputs (parity-tested)
docs/development/issues/  GitHub issue body files
docs/development/specs/   Internal specs for in-flight work
```

## Code Conventions

- **Go style**: Standard `go fmt`, `go vet`, golangci-lint defaults
- **Receivers**: Short pointer receivers — `(c *Config)`, `(cb *CircuitBreaker)`
- **Test naming**: `Test<Component>_<Scenario>` with table-driven tests
- **Error handling**: Explicit `if err != nil` returns; `LogFatal` for unrecoverable
- **Logging**: Custom `LogDebug/Info/Warn/Error/Fatal` functions (not stdlib log)
- **Imports**: stdlib first, then third-party, alphabetical within groups
- **Config keys**: snake_case in YAML, CamelCase in Go structs
- **Prose spelling**: US English everywhere — docs, specs, code comments, and
  UI/marketing copy (`behavior`, `license`, `defense`, `gray`, `normalize`,
  `labeled`, `artifact`; never
  `behaviour`/`licence`/`defence`/`grey`/`normalise`/`labelled`/`artefact`)
- **Domain language**: Prefer the concept names and boundaries in
  `docs/development/domain-language.md`; avoid promoting implementation
  patterns like the two-step fetch into user-facing nouns.

## Execution Modes

- **Scheduled** (default): Continuous polling loop with circuit breaker + adaptive backoff
- **Backfill**: One-time historical export between timestamps, then exit
- **Validate**: Load config, print parsed settings, exit

## Git Workflow

- Main branch: `master`
- Feature branches merged via PRs
- Release tags: `vYY.MM.idx` — calendar versioning (GoReleaser)
- CI workflows: ci.yml (reusable — test + lint + build + govulncheck + conditional integration/arm64), release.yml (tag-triggered wrapper that calls ci.yml then runs GoReleaser + cosign), degradation.yml, docs.yml, claude.yml, claude-code-review.yml
- Docs site (click-dog.com): `docs.yml` publishes `docs/` → `gh-pages` only on `v*` tags / `workflow_dispatch` (a plain master merge just validates). Manual `mkdocs gh-deploy` fallback + the pull-before-deploy gotcha: `docs/development/docs-publishing.md`

## Key Architectural Decisions

- **readonly=2**: All ClickHouse connections enforce read-only mode
- **In-memory dedup**: LRU cache (not persisted); OTEL collectors handle duplicate span_ids
- **SQL-level filtering**: Duration filters applied in ClickHouse query to minimize data transfer
- **Live span selection**: First find qualifying trace_ids, then fetch spans for those traces
- **Circuit breaker + adaptive backoff**: Layered protection — breaker blocks cycles, backoff increases intervals
