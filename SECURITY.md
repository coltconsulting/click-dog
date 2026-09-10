# Security Policy

## Supported Versions

Click-Dog is in public beta. Only the latest released tag receives security fixes. Pin to a specific `vYY.MM.idx` tag in production and upgrade promptly when advisories are published.

## Reporting a Vulnerability

Please report suspected security vulnerabilities privately. Do not open a public GitHub issue for a security report.

Preferred channels:

1. **GitHub Security Advisory**: <https://github.com/coltconsulting/click-dog/security/advisories/new>. This is the fastest way to coordinate disclosure.

Include in your report:

- A description of the issue and the impact you expect.
- Steps to reproduce, ideally with a minimal config or command sequence.
- The version (`click-dog -version`) and deployment surface (binary, Docker, Kubernetes, Ansible).
- Whether you intend to publish your own write-up, and if so on what timeline.

## What to Expect

- Acknowledgement of your report within 5 business days.
- A status update within 14 days of acknowledgement, including whether the report is accepted, needs more information, or is out of scope.
- A coordinated disclosure timeline if the report is accepted, typically targeting a fix within 90 days for high-severity issues. Earlier release for issues with known active exploitation.

## Scope

In scope:

- The `click-dog` binary and its handling of ClickHouse connections, OTEL/Splunk export, configuration parsing, and HTTP endpoints (`/metrics`, `/health`, `/healthz`, `/readyz`, `/status`, `/flush`).
- Release artifact integrity: GoReleaser archives, checksums, cosign signatures, the install script, and the self-update path.
- Bundled deployment assets in `deploy/` (install.sh, Ansible playbook, Kubernetes manifests).

Out of scope:

- Vulnerabilities in ClickHouse itself, the OTEL Collector, Datadog Agent, or other backend services Click-Dog connects to.
- Misconfiguration in the user's environment (e.g. writing ClickHouse credentials to a world-readable file).
- Reports based solely on automated scanners without a demonstrated impact.

## Hardening Guidance

Operational hardening recommendations are documented in
[Observability](https://click-dog.com/observability/),
[Configuration](https://click-dog.com/configuration/), and the install script's
generated configs (least-privilege ClickHouse role, `readonly=2` enforced on
the connection, no write access).
