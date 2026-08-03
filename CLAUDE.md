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

- Go 1.25+ required for compiling (`go` directive); CI and releases pin Go 1.26.5 so the govulncheck gate runs on the reviewed toolchain. Go 1.26.4 cleared GO-2026-5037/5039, and Go 1.26.5 additionally clears GO-2026-5856 in `crypto/tls`
- Integration tests use Docker Compose with a 3-node ClickHouse cluster
- Integration tests are tagged: `// +build integration`
- CI runs test, lint, and linux/amd64 build on eligible non-draft PRs and non-PR events; PR tests omit `-race`, while master/tag/manual/scheduled runs use it. `govulncheck` and integration tests skip PRs; the linux/arm64 cross-build runs on `v*` tags only. PRs whose changes all match `docs/**`, `mkdocs.yml`, or `**/*.md` skip full CI; `docs.yml` separately validates its `docs/**`/`mkdocs.yml` paths
- `govulncheck ./...` is enforced both in CI and `make preflight`; install with `go install golang.org/x/vuln/cmd/govulncheck@v1.3.0` (CI pin)

## Project Structure

Top of tree (`package main`):

```
main.go                      Entry point, CLI flag/subcommand routing, scheduled+backfill wiring
dryrun.go                    DryRunExporter — discards exports and prints a summary
health_adapter.go            Wires the processor's runtime state into internal/health probes
cmd_init.go                  `click-dog init` — generate starter config
cmd_init_wizard.go           Guided `click-dog init --wizard` prompts + shared renderer
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
export_gate.go               Cluster-mode leader gate for scheduled export
```

`click-dog init` and `click-dog init --wizard` share a single YAML renderer
(`renderWizardYAML` in `cmd_init_wizard.go`). `install.sh` never emits YAML
itself; its quickstart / install / docker paths shell out to
`click-dog init` for the actual rendering. The `docs/examples/click-dog-*.yaml`
files are the reference output for each profile and are pinned against the
renderer by `TestWizard_ParityWithProfileTemplates`.

Internal packages (`internal/`):

```
analysis/     Query-analysis and trace-drilldown report contracts + deterministic analyzers
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

## Repository Split & Publishing

- Development happens on `coltconsulting/click-dog-internal`: all branches and
  internal PRs belong there, whichever repo a session was started from.
- `coltconsulting/click-dog` is the public release target. Its `master`
  advances ONLY via `make update-public PUSH=1` release commits — never push
  branches, merge, or open internal PRs there.
- External contributor PRs arrive on the public repo and are reviewed there,
  then imported into the development tree with `scripts/import-public-pr.sh`
  (`git am`, authorship preserved) and ship in the next release. Full flow:
  `docs/development/contributing-flow.md`.

## Git Workflow

- Main branch: `master`
- Feature branches merged via PRs
- Release tags: `vYY.MM.idx` — calendar versioning (GoReleaser)
- CI workflows: ci.yml (reusable test/lint/build/govulncheck gate with event-gated integration/arm64), release.yml (tag wrapper; internal is gate-only, public alone runs GoReleaser + cosign), degradation.yml, docs.yml (PR build-only gate: `mkdocs build --strict`; publishes nothing), site.yml (site deploys), claude.yml, claude-code-review.yml
- Docs site (click-dog.com): published by `site.yml` to Cloudflare Pages. Master merges touching site paths auto-deploy staging; `site-vN` tags (and GA `v*` tags, as a docs refresh) deploy prod with docs pinned to the latest GA tag. Details: `docs/development/docs-publishing.md`

## Key Architectural Decisions

- **readonly=2**: All ClickHouse connections enforce read-only mode
- **In-memory dedup**: LRU cache (not persisted); restarts and retries can resend stable span identities, and downstream duplicate handling is backend-specific
- **SQL-level filtering**: Duration filters applied in ClickHouse query to minimize data transfer
- **Live span selection**: First find qualifying trace_ids, then fetch spans for those traces
- **Circuit breaker + adaptive backoff**: Layered protection — breaker blocks cycles, backoff increases intervals
