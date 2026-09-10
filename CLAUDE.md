# CLAUDE.md — click-dog

## What is click-dog?

A standalone Go binary that reads OpenTelemetry spans from ClickHouse `system.opentelemetry_span_log` and exports them via gRPC to OTEL-compatible backends (Datadog, Honeycomb, etc.). Solves the gap where ClickHouse generates spans but has no native export tool.

## Build & Test

```bash
make doctor           # Diagnose branch/remote, tools, caches, and capabilities
make check-fast       # Formatting + concise unit tests
make check            # Normal local PR gate
make check-deep       # Expensive release-level gate (Docker + full toolchain)
make build            # Build for current platform
make test             # Unit tests (shell/public-boundary suites run in `make check`)
make test-race        # Unit tests with race detector
make check-lint             # golangci-lint
make check-shell      # shellcheck over every tracked *.sh
make check-vuln        # govulncheck ./... (release gate)
make fmt              # go fmt
make test-integration      # Full integration suite (requires Docker)
make check-release    # Pre-tag gate: clean master + full local verification
```

- Go 1.25+ required for compiling (`go` directive); CI and releases pin Go 1.26.6 so the govulncheck gate runs on the reviewed toolchain. Go 1.26.5 cleared GO-2026-5856 in `crypto/tls`, and Go 1.26.6 additionally clears GO-2026-6218/6091/6090/6089/5972/5026 across `net/url`, `html/template`, `crypto/tls`, `net/http`, and `encoding/asn1`
- Integration tests use Docker Compose with a 3-node ClickHouse cluster
- `make dev-setup` installs pinned repo-managed tools under `.agent-cache/`;
  versions are sourced from `scripts/tool-versions.env`
- Some unit tests bind loopback HTTP/gRPC listeners and some need a writable Go
  cache. In a restricted environment run `make check`, then state plainly which
  parts could not run and why — do not report a partial run as a passing gate.
  `make doctor` reports both constraints
- Integration tests are tagged: `// +build integration`
- CI runs test, lint, and linux/amd64 build on eligible non-draft PRs and non-PR events; PR tests omit `-race`, while master/tag/manual/scheduled runs use it. Integration tests also run on non-draft PRs that touch ClickHouse, processor, leader, health, exporter, analysis, query-family, filtering, config, resilience, integration-fixture, command, dependency, integration-test, or CI-workflow paths; `govulncheck` still skips PRs, and the linux/arm64 cross-build runs on `v*` tags only. PRs whose changes all match `docs/**`, `mkdocs.yml`, or `**/*.md` skip full CI; `docs.yml` separately validates its `docs/**`/`mkdocs.yml` paths
- `govulncheck ./...` is enforced both in CI and `make check-release`; install with `go install golang.org/x/vuln/cmd/govulncheck@v1.3.0` (CI pin)

## Agent Workflow

This file is the normative repository instruction source for humans and coding
agents. `AGENTS.md`, `.github/copilot-instructions.md`, and developer-facing
agent documentation may point here, but must not add independent requirements.

Before changing files:

1. Read the task source and identify its acceptance criteria, scope, and
   explicit do-not-touch boundaries.
2. Run `git status --short --branch` and inspect the `origin` URL. Preserve all
   unrelated changes; never discard, stash, or rewrite work you did not create.
3. Run `make doctor` when the environment or available verification tools are
   unknown. Use `docs/development/change-map.md` to find companion surfaces.

Task and specification rules:

- Beads are used only for work explicitly linked to a bead. Invoke the tracker
  through `scripts/bd-local.sh` so it cannot discover a parent/global database.
  Claim/update/close that bead as work progresses; do not create beads for
  ordinary interactive requests unless asked. Tracker changes do not authorize
  git or external mutations.
- Baseline OpenSpec files describe shipped behavior. User-visible behavior
  changes should use an `openspec/changes/<name>/` delta unless the task says
  otherwise, then sync and archive that delta after the implementation lands.
- Keep AI-related product suggestions downstream of deterministic Query
  Analysis JSON. Do not introduce MCP, LLM calls, automatic query rewrites,
  automatic `EXPLAIN`, or hot-path analysis unless the task explicitly asks.

Remote-action authority is determined by how the agent was invoked:

- **Interactive local task:** editing and verification are allowed. Commit,
  push, PR, issue, release, deployment, and publishing actions require an
  explicit user request.
- **GitHub implementation task:** an authorized `@claude implement` invocation
  may create an agent-prefixed branch, commit, push, and open a draft internal
  PR. It must never push to `master`, merge, tag, release, or publish.
- **Review or ordinary GitHub mention:** read and comment only; do not edit,
  commit, or push.
- **Release/public operation:** requires explicit operator authorization for
  the exact action. The public repository is never a target for internal
  branches or PRs.

Before any commit or push, verify that `origin` is
`coltconsulting/click-dog-internal`, HEAD is a named feature branch (not
detached and not `master`), and the diff contains no unrelated changes. Never
clear stashes or prune branches as session cleanup.

Work is complete when the requested behavior and documentation are present,
the change-appropriate checks have passed, the diff is scoped, and any linked
tracker state is current. Report checks that could not run and why. A commit,
push, or PR is part of completion only when the invocation authorized it.

## Make targets

Three files, so the public tree carries only what works there:

```
Makefile           configuration, the confirm guard, and help
Makefile.shared    every target that works in any clone
Makefile.internal  publishing and docs-site targets; export-ignored, so the
                   `-include` in Makefile silently no-ops in the public tree
```

Targets are prefixed by what they do — `test-` runs tests, `check-` runs tests
plus linting and analysis, `build-` produces artifacts, `release-` ships
something, `print-` only tells you something. The gates form a strict
escalation: `check-fast` → `check` → `check-deep` → `check-release`. A target
in `Makefile.internal` may add itself to a shared gate by declaring a
prerequisite (`check-deep: check-public`), which merges with the shared rule.
Full table: `docs/development/contributing.md`.

## Project Structure

Top of tree (`package main`):

```
main.go                      Entry point, CLI flag/subcommand routing, scheduled+backfill wiring
dryrun.go                    DryRunExporter — discards exports and prints a summary
health_adapter.go            Wires the processor's runtime state into internal/health probes
cmd_init.go                  `click-dog init` — generate starter config
cmd_init_wizard.go           Guided `click-dog init --wizard` prompts + shared renderer
cmd_check.go                 `click-dog check` — validate config + connectivity
cmd_validate.go              `click-dog validate` — offline config validation (also backs the deprecated -validate flag)
cmd_backfill.go              `click-dog backfill` — arg surface only; rewrites onto main.go's mode flags
cmd_test_command.go          `click-dog test` — smoke-test command family
cmd_test_export.go           `click-dog test export` — synthetic span sender
cmd_test_tracing.go          `click-dog test tracing` — native trace propagation smoke test
cmd_test_span.go             Deprecated `click-dog test-span` compatibility alias
cmd_flush.go                 `click-dog flush` — signal a running instance to export
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
`click-dog init` for the actual rendering. The public
`examples/click-dog-*.yaml` release files are the reference output for each
profile; the private `docs/examples/` copies feed the site. Both are pinned
against the renderer by `TestWizard_ParityWithProfileTemplates`.

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
examples/                 Public release starter configs (parity-tested)
docs/                     Product documentation (MkDocs page sources)
docs/examples/            Private site copies of the starter configs (parity-tested)
docs/development/issues/  GitHub issue body files
docs/development/specs/   Internal specs for in-flight work
www/                      The site itself: landing templates (mkdocs custom_dir)
                          + landing-owned assets. Internal-only (export-ignored)
target/                   MkDocs build output (gitignored)
```

`docs/` and `www/` are both site sources, split by how they reach production:
prod rebuilds `docs/` from the release recorded in the deploying `site-vN` tag, while `www/` always ships from
the commit being deployed. Details: `docs/development/docs-publishing.md`.

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
- **Backfill** (`click-dog backfill --start/--end`): One-time historical export between timestamps, then exit
- **Validate** (`click-dog validate`): Load config, print parsed settings, exit
- The pre-subcommand spellings `-validate` and `-backfill-start`/`-backfill-end` still work as deprecated aliases

## Repository Split & Publishing

- Development happens on `coltconsulting/click-dog-internal`: all branches and
  internal PRs belong there, whichever repo a session was started from.
- `coltconsulting/click-dog` is the public release target. Its `master`
  advances ONLY via `make release-public` release commits — never push
  branches, merge, or open internal PRs there.
- Maintainers and coding agents never create branches or PRs in the public
  repository. Its active no-bypass ruleset rejects every non-default branch,
  including attempts made with repository-admin credentials. A task started
  from a public checkout must move to an internal feature branch before any
  commit, push, or PR action.
- Public PRs are reserved for external contributors working from forks. They
  are reviewed there, imported into the development tree with
  `scripts/import-public-pr.sh` (`git am`, authorship preserved), and ship in
  the next release. Full flow: `docs/development/contributing-flow.md`.

## Git Workflow

- Main branch: `master`
- Feature branches merged via PRs
- Release channels, decided by tag shape (GoReleaser, CalVer):
  `vYY.MM.idx-alpha.N` internal-only private prerelease (the default, `make
  release-alpha`); `vYY.MM.idx-beta.N` public GitHub prerelease, reachable
  via `install.sh --prerelease`, and it does not advance public `master`;
  `vYY.MM.idx` GA, public production. alpha never leaves the internal repo;
  beta and ga are gates internally and build after being mirrored to public
- CI workflows: ci.yml (reusable test/lint/build/govulncheck gate with event-gated integration/arm64 and exact-public-tree release snapshot), release.yml (exact channel classifier: internal alpha builds a private prerelease, internal beta/GA are gate-only, public beta builds a prerelease and public GA builds production artifacts), degradation.yml (internal-only; export-ignored), dco.yml (contributor sign-off gate; guarded to the public repo, where DCO applies), docs.yml (PR build-only gate: `mkdocs build --strict`; publishes nothing), site.yml (site deploys), claude.yml, claude-code-review.yml
- Docs site (click-dog.com): published by `site.yml` to Cloudflare Pages, built to `target/`. Master merges touching site paths auto-deploy staging, which builds `master` as-is. Only a pushed `site-vN` tag deploys prod — there is no prod dispatch, so every prod deploy leaves a version behind. The job swaps `docs/` + `mkdocs.yml` for the copy from the release tag recorded in that site tag's annotation — the newest GA by default, or a beta named with `make release-site DOCS=<tag>` — and ships `www/` from the deployed commit. A GA tag does not touch the site; `make print-site-status` reports when prod has fallen behind. Details: `docs/development/docs-publishing.md`

## Key Architectural Decisions

- **readonly=2**: All ClickHouse connections enforce read-only mode
- **In-memory dedup**: LRU cache (not persisted); restarts and retries can resend stable span identities, and downstream duplicate handling is backend-specific
- **SQL-level filtering**: Duration filters applied in ClickHouse query to minimize data transfer
- **Live span selection**: First find qualifying trace_ids, then fetch spans for those traces
- **Circuit breaker + adaptive backoff**: Layered protection — breaker blocks cycles, backoff increases intervals
