# Security Policy

## Supported Versions

Only the latest release receives security fixes. Pin production to a specific `vYY.MM.idx` tag and upgrade promptly when an advisory is published. Any release can change configuration keys, CLI flags, HTTP endpoints, or metric names; see the [compatibility policy](https://click-dog.com/operating/).

## Reporting a Vulnerability

Please report suspected security vulnerabilities privately. Do not open a public GitHub issue for a security report.

Report through either channel:

1. **GitHub Security Advisory** (preferred): <https://github.com/coltconsulting/click-dog/security/advisories/new>.
2. **Email**: <info@coltconsulting.nz>.

Include in the report:

- A description of the issue and its expected impact.
- Steps to reproduce, ideally with a minimal config or command sequence.
- The version (`click-dog --version`) and deployment surface (binary, Docker, Kubernetes, Ansible).
- Whether the reporter intends to publish a write-up, and if so on what timeline.

## What to Expect

- Acknowledgement of the report within 5 business days.
- A status update within 14 days of acknowledgement, including whether the report is accepted, needs more information, or is out of scope.
- A coordinated disclosure timeline if the report is accepted, typically targeting a fix within 90 days for high-severity issues. Earlier release for issues with known active exploitation.

## Scope

In scope:

- The `click-dog` binary and its handling of ClickHouse connections, OTEL/Splunk export, configuration parsing, and HTTP endpoints (`/metrics`, `/healthz`, `/readyz`, `/status`, `/clusterz`, `/flush`).
- Release artifact integrity: GoReleaser archives, container images, checksums, cosign signatures, the install script, and the self-update path.
- Bundled deployment assets in `deploy/` (install.sh, Ansible playbook, Kubernetes manifests).

Out of scope:

- Vulnerabilities in ClickHouse itself, the OTEL Collector, Datadog Agent, or other backend services Click-Dog connects to.
- Misconfiguration in the user's environment (e.g. writing ClickHouse credentials to a world-readable file).
- Reports based solely on automated scanners without a demonstrated impact.

## Hardening Guidance

Click-Dog enforces `readonly=2` on every ClickHouse connection. Hardening
guidance is in [Security and Privacy](https://click-dog.com/security/),
[Configuration](https://click-dog.com/configuration/), and
[Observability](https://click-dog.com/observability/).
