# Installing Click-Dog

Install means getting Click-Dog running in your environment: one systemd service
on a host, one sidecar per ClickHouse node, a centralized Kubernetes Deployment,
Docker Compose, or a fleet rollout through Ansible.

The default host topology is **sidecar per ClickHouse node**. Each instance
queries its local `system.opentelemetry_span_log` and exports spans via gRPC to
your OTEL collector. Kubernetes can also run a centralized Deployment that reads
the whole cluster via `cluster()` queries; that path is covered below.

!!! tip "Before a fleet rollout, read the [Operating Contract](operating.md)"
    It covers the beta delivery guarantees and loss windows (restart,
    backend-outage, multi-sink-failure behavior), the tested-vs-expected
    [compatibility matrix](operating.md#compatibility-matrix), performance
    tuning, span-volume/cost estimation, and cluster topology / `use_cluster_queries`
    placement.

---

## Quick Start (Single Node)

Run the installer with no arguments for a guided setup:

```bash
curl -fsSL https://github.com/coltconsulting/click-dog/releases/latest/download/install.sh -o install.sh
less install.sh
sudo bash install.sh
```

If you are working from a repository checkout, the equivalent local command is
`./deploy/install.sh`.

The installer walks you through 3 steps, with confirmation at each:

1. **ClickHouse** — enables span logging, detects TLS, and sets up a dedicated read-only `click_dog_monitor` user from idempotent SQL (`CREATE USER IF NOT EXISTS` + two `GRANT SELECT`s; never replaces an existing user, and only resets a password on explicit confirmation), then verifies spans are readable **as that user**
2. **OTEL collector** — confirm where to send traces (with connectivity check)
3. **Install** — shows what will be created, then downloads, configures, and starts the service

Nothing is installed until step 3. You can exit at any point.

### What this turns on

The guided install gives you a working, observable baseline before you touch any
advanced deployment options:

| Capability | What you get |
|------------|--------------|
| Read-only source access | Dedicated `click_dog_monitor` user with only the required `SELECT` grants |
| Generated config | `/etc/click-dog/click-dog.yaml` with production profile defaults |
| Secret handling | `/etc/click-dog/.secret` referenced by `clickhouse.password_file` |
| Service lifecycle | Optional systemd unit plus `click-dog deploy status` |
| Safety defaults | Circuit breaker, adaptive backoff, connection caps, dedup cache, and operation-noise filtering |
| Health and metrics | `/healthz`, `/readyz`, `/status`, and Prometheus `/metrics` enabled |
| First-value commands | `click-dog check`, `click-dog analyze queries`, and Datadog dashboard provisioning |

### After install

Everything is editable. Nothing is locked in.

| What | How to change |
|------|---------------|
| Config | `vi /etc/click-dog/click-dog.yaml && systemctl restart click-dog` |
| Password | Edit `/etc/click-dog/.secret` + `ALTER USER click_dog_monitor ...` in ClickHouse |
| Service | `systemctl stop/start/restart click-dog` |
| Logs | `journalctl -u click-dog -f` |
| Liveness | `curl -s localhost:8686/healthz` |
| Readiness | `curl -s localhost:8686/readyz` |
| Status JSON | `curl -s localhost:8686/status \| jq` |
| Metrics | `curl -s localhost:9090/metrics` |
| Uninstall | `sudo bash install.sh uninstall` |

Uninstall uses the same signed installer script. If you no longer have
`install.sh` in the current directory, download it again with the Quick Start
`curl` command first.

---

## Prerequisites

### ClickHouse OpenTelemetry

ClickHouse must be configured to emit spans. The guided installer prints the SQL to check this, but if you need to enable it from scratch:

Add to `/etc/clickhouse-server/config.d/opentelemetry.xml`:

```xml
<clickhouse>
    <opentelemetry_span_log>
        <engine>
            ENGINE = MergeTree
            PARTITION BY toYYYYMM(finish_date)
            ORDER BY (finish_date, finish_time_us, trace_id)
        </engine>
        <database>system</database>
        <table>opentelemetry_span_log</table>
        <flush_interval_milliseconds>7500</flush_interval_milliseconds>
    </opentelemetry_span_log>
</clickhouse>
```

Then restart ClickHouse:

```bash
sudo systemctl restart clickhouse-server
```

Verify:

```sql
SELECT count() FROM system.opentelemetry_span_log;
```

Once click-dog is configured, `click-dog check` runs this and the rest of
the data-plane readiness checks (span/query log grants, recent spans, duration
thresholds, `clickhouse.query_id` propagation, enrichment join, and
`normalized_query_hash` support) in one pass — see
[Troubleshooting](troubleshooting.md#no-spans-being-exported).

### OTEL Collector

Click-Dog sends spans via gRPC to any OTEL-compatible endpoint on port 4317. See [Integrations](integrations/datadog.md) for platform-specific setup (Datadog Agent, OTEL Collector, Grafana Alloy).

### Release-signature verification (auto-download path)

When the installer downloads a release archive (no `-b /path/to/binary`),
it verifies the cosign keyless signature on `checksums.txt` against the
pinned click-dog release-workflow identity, then verifies the archive's
SHA256 against that signed `checksums.txt`. The install aborts before
extraction on any verification failure.

This requires the [cosign CLI](https://docs.sigstore.dev/cosign/installation/)
on the host running `install.sh`, plus `sha256sum` (Linux) or `shasum`
(macOS). Keyless verification also reaches Rekor (`rekor.sigstore.dev`)
and Fulcio (`fulcio.sigstore.dev`) — open egress for these or pre-verify
on a host that can, then deploy with `-b /path/to/click-dog` (see
[Air-gapped environments](#air-gapped-environments)).

**Private repository:** while `coltconsulting/click-dog` is private, the
auto-download path needs a GitHub token with read access — export
`GITHUB_TOKEN` (or `GH_TOKEN`) before running `install.sh`, or skip the
download entirely with `-b /path/to/click-dog`. Without a token the GitHub
API version probe and the release assets both return `404`. `install.sh`
downloads each asset through the authenticated release-asset API, so the
token works the same way `self-update` documents.

---

## Choosing an install method

| Method | Runs from | Targets | Scale | Off-node? |
|--------|-----------|---------|-------|-----------|
| `curl … \| bash` quickstart | the node | localhost (binary + systemd) | 1 | no |
| `install.sh install` | the node | localhost | 1 | no |
| `init --wizard` → `install.sh install -f` | laptop renders, node installs | localhost | 1 | config only |
| Ansible `playbook.yaml` | control node → SSH | N ClickHouse nodes (one sidecar each) | n | yes |
| Ansible `playbook-canary.yaml` | control node → SSH | staged subset → roll forward | n | yes |
| `deploy kubernetes` | anywhere (renders manifests) | k8s — one centralized Deployment reads the whole cluster via `cluster()` | 1→n | yes |
| `deploy docker` | anywhere (renders compose) | container host | 1 | yes |

Notes:

- **Installing is on-node** for the `install`/`quickstart` paths — they write
  to local `/usr/local/bin` and systemd and have no SSH. For more than one
  host, use Ansible. There is currently no single "install to one remote host"
  path; that's a known gap (use Ansible with a one-host inventory, or run the
  script on the box).
- **`deploy kubernetes`/`deploy docker` are off-node** — they only render
  manifests/compose, so run them from anywhere and apply elsewhere.
- `curl … | bash` cannot generate k8s/docker output (no on-disk templates when
  piped); use a checkout for those.

### Passwords per method

click-dog reads the ClickHouse password from `clickhouse.password_file` (a file
holding the raw secret; one trailing newline is trimmed). The inline
`password:` / `${CLICKHOUSE_PASSWORD}` form still works but is mutually
exclusive with `password_file` — setting both is a config error. A
`password_file` must hold a non-empty secret (an empty file, or one containing
only a newline, is rejected); for an intentionally-empty password use the
inline `password: ""` form instead. The same applies to
`splunk_hec[].token_file` and `ha.keeper.auth_password_file`.

| Method | Where the secret lives |
|--------|------------------------|
| `install.sh` (systemd) | `/etc/click-dog/.secret` (0600), referenced by `password_file`; not in the service environment |
| Ansible | `{{ click_dog_config_dir }}/.secret` (0600, `no_log`), via `--extra-vars`/Vault → `password_file` |
| `deploy kubernetes` | a k8s Secret injected as `CLICKHOUSE_PASSWORD` (env). Mounting the Secret as a file + `password_file` is planned, not yet wired in the manifests. |
| Manual run | write the password to a file and set `password_file`, or `export CLICKHOUSE_PASSWORD` for the inline form |

The Splunk HEC token (`token_file`) and Keeper auth password
(`auth_password_file`) follow the same file-based pattern. `click-dog check`
warns when an inline `${VAR}` (e.g. `CLICKHOUSE_PASSWORD`) is referenced but
unset, so a forgotten export shows up front instead of as an opaque auth error.

## Multi-Node Deploy

`install.sh` is the single-node entry point — it installs Click-Dog on the host it is invoked from. For fleet rollouts use the Ansible playbook shipped in `deploy/ansible/`, which uses the same sidecar lifecycle shape (config, systemd unit, restart/validation hooks) and handles inventory, parallelism, rolling restarts, and updates.

```bash
# 1. Copy the example inventory and edit it for your fleet.
cp deploy/ansible/inventory.ini.example deploy/ansible/inventory.ini
vi deploy/ansible/inventory.ini

# 2. Apply across the fleet.
ansible-playbook -i deploy/ansible/inventory.ini deploy/ansible/playbook.yaml \
  --extra-vars "clickhouse_password=YOUR_PASSWORD"

# 3. Run with bounded SSH concurrency (10 hosts in flight at once).
#    `-f` is a real ansible-playbook flag; for batched rollouts that
#    finish a wave before starting the next, add `serial: 10` to the
#    play in playbook.yaml (it's a play-level YAML keyword, not a
#    CLI flag).
ansible-playbook -i deploy/ansible/inventory.ini deploy/ansible/playbook.yaml \
  --extra-vars "clickhouse_password=YOUR_PASSWORD" \
  -f 10
```

A `playbook-canary.yaml` is also included for staged rollouts that pause after a small subset of nodes are upgraded.

> If you already have the `click-dog` binary on PATH, the same manifest
> generators (and an installed-state reporter) are also reachable directly
> as `click-dog deploy kubernetes` / `click-dog deploy docker` /
> `click-dog deploy status`. See
> [Operation Modes — `click-dog deploy`](modes.md#click-dog-deploy-manifest-generators)
> for the flag reference. The `install.sh` recipes below remain the
> recommended entry point because the script wraps cosign verification,
> systemd unit creation, and the guided quickstart.

### `install.sh` commands

The shell entry point covers single-node systemd, Kubernetes manifest generation, and Docker Compose scaffolding:

| Command | Description |
|---------|-------------|
| `install` | Install Click-Dog on the current host: binary + config + optional systemd unit |
| `update` | Swap the binary on the current host (config untouched) |
| `status` | Print the current host's service / health status |
| `uninstall` | Stop the service and remove all click-dog files on the current host |
| `kubernetes` | Render Kubernetes manifests under `click-dog-k8s/` |
| `docker` | Render a Docker Compose stack under `click-dog-docker/` |

| Flag | Where | Description |
|------|-------|-------------|
| `-c ADDRESS` | install/kubernetes/docker | OTEL collector address (required unless `-f`) |
| `-f PATH` | install | Use this pre-rendered `click-dog.yaml` instead of letting install.sh render one; pair with `click-dog init --wizard` from a laptop |
| `-u USER` | install | ClickHouse username (default: `click_dog_monitor`) — dedicated, namespaced name so re-running install never collides with or replaces an existing `monitoring` account |
| `-v VERSION` | install/update | Version to install/generate (default: latest release) |
| `--prerelease` | install/update | When `-v` is unset, resolve the newest pre-release (alpha/beta) instead of the latest GA release |
| `-b BINARY` | install/update | Path to a pre-verified click-dog binary (skips download + cosign verification) |
| `-k HOSTS` | install | Keeper hosts for HA, comma-separated |
| `-C CMD` | install | `clickhouse-client` command/path (e.g. `-C "clickhouse-client -u admin --password X"`) |
| `-x PROXY` | install/update | HTTPS proxy for binary download |
| `-o DIR` | kubernetes/docker | Output directory for rendered manifests |
| `--systemd` | install | Create and enable a systemd unit |

The ClickHouse password is supplied via the `CLICKHOUSE_PASSWORD` environment
variable (for non-interactive runs) or the interactive prompt when it is unset
and a TTY is available — never a command-line flag, which would leak the secret
into process listings and shell history.

#### Bring your own YAML (`install -f`)

For audit / version-control workflows where you want to render the
config locally, eyeball it, then deploy:

```bash
# laptop — render and review (commit to your config repo).
# -ch-password-file emits `password_file: /etc/click-dog/.secret` so the
# config reads the secret from a file, matching the systemd unit below.
click-dog init --wizard -ch-password-file /etc/click-dog/.secret -o click-dog.yaml

# target box — install.sh fetches + verifies the release binary itself.
# Pass the password at install time; install.sh writes it to
# /etc/click-dog/.secret (0600), where password_file points.
scp click-dog.yaml HOST:/tmp/
ssh HOST 'curl -fsSL https://github.com/coltconsulting/click-dog/releases/latest/download/install.sh -o /tmp/install.sh'
ssh HOST 'sudo CLICKHOUSE_PASSWORD=... bash /tmp/install.sh install -f /tmp/click-dog.yaml --systemd'
```

> **Don't pair the inline `${CLICKHOUSE_PASSWORD}` form with `install -f --systemd`.**
> The generated systemd unit has no `EnvironmentFile=`, so `CLICKHOUSE_PASSWORD`
> is never set at runtime and an inline `password: ${CLICKHOUSE_PASSWORD}` expands
> to empty — ClickHouse auth then fails. Render with
> `-ch-password-file /etc/click-dog/.secret` so the config uses `password_file`;
> install.sh writes the password it receives (`CLICKHOUSE_PASSWORD`) to that
> file as raw 0600 bytes.

`install -f` only swaps out the inline `click-dog init` render step;
writing `/etc/click-dog/.secret` from `CLICKHOUSE_PASSWORD`, the systemd
unit, and the validate-then-swap run exactly as in a normal `install`. It
does **not** provision a ClickHouse user — that is the guided quickstart's
job — so the YAML you supply owns `clickhouse.username`, and that user must
already exist with a password matching `/etc/click-dog/.secret`.
`click-dog init --wizard` writes `username: default`; pass
`-ch-user click_dog_monitor` if you want the dedicated monitoring
account the quickstart provisions. Air-gapped hosts can add
`-b /tmp/click-dog` with a pre-verified binary instead of letting
install.sh download one.

### Ansible playbook variables

| Variable | Description |
|----------|-------------|
| `clickhouse_password` | Required password for the ClickHouse monitoring user; pass with `--extra-vars` or Ansible Vault |
| `clickhouse_username` | ClickHouse username, default `click_dog_monitor` |
| `otel_collector_address` | OTEL collector address, for example `otel-collector:4317` |
| `keeper_hosts` | Optional Keeper hosts for HA leader election |
| `click_dog_version` | Release tag to install, or `latest` |
| `click_dog_download_url` | Internal archive URL for mirrored or air-gapped environments; the playbook downloads this directly, so verify it before publishing to the mirror |
| `click_dog_local_binary` | Local verified binary to copy instead of downloading an archive; use this for strict cosign-verified fleet rollouts |

### Air-gapped environments

Verify the release on an internet-connected host, then transfer the **verified** binary into the air-gapped network. Skipping verification on the internet-side host defeats the point of an air-gap — the binary that lands on the bastion is only as trustworthy as the host that produced it.

```bash
# 1. On a machine with internet AND sigstore egress (rekor.sigstore.dev,
#    fulcio.sigstore.dev): run the full cosign + sha256 verification flow
#    documented in "Verifying releases manually" below. Don't extract or
#    use the binary until both commands exit zero.

# 2. Once verified, extract the binary from the verified archive:
VERSION=26.04.9
tar -xzf "click-dog_${VERSION}_linux_amd64.tar.gz"

# 3. Transfer the verified binary into the air-gapped network and deploy:
#    - Single node: run `install.sh -b /tmp/click-dog` on each host.
#    - Fleet: drop the binary onto the Ansible control host and pass
#      `click_dog_local_binary=/tmp/click-dog` when running the playbook in
#      `deploy/ansible/`. The playbook then bypasses the auto-download.
scp click-dog bastion:/tmp/click-dog
ssh bastion 'sudo ./deploy/install.sh install -c collector:4317 -b /tmp/click-dog --systemd'
```

See [Verifying releases manually](#verifying-releases-manually) for the exact
verification commands. `click_dog_local_binary` tells Ansible to copy the
already-verified binary instead of downloading a release archive.

---

## Verifying releases manually

Each release ships a `checksums.txt` plus a cosign keyless signature pair —
`checksums.txt.sig` (signature) and `checksums.txt.pem` (Fulcio-issued
certificate tied to the GitHub Actions release workflow). `install.sh`
(auto-download path) and `self-update` verify these automatically; this
section is for operators who download binaries directly.

You'll need the [cosign CLI](https://docs.sigstore.dev/cosign/installation/).

```bash
VERSION=26.04.9
BASE="https://github.com/coltconsulting/click-dog/releases/download/v${VERSION}"

# Fetch the archive, checksums, signature, and certificate.
curl -fsSL -O "${BASE}/click-dog_${VERSION}_linux_amd64.tar.gz"
curl -fsSL -O "${BASE}/checksums.txt"
curl -fsSL -O "${BASE}/checksums.txt.sig"
curl -fsSL -O "${BASE}/checksums.txt.pem"

# 1. Verify the cosign keyless signature on checksums.txt.
#    The pinned identity ties the signature to the release.yml workflow
#    on a release tag — a stolen GITHUB_TOKEN alone cannot forge it.
cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature checksums.txt.sig \
  --certificate-identity-regexp '^https://github\.com/coltconsulting/click-dog/\.github/workflows/release\.yml@refs/tags/v.+$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  checksums.txt

# 2. Verify the archive against the now-trusted checksums.txt.
sha256sum -c checksums.txt --ignore-missing
```

Both commands must exit zero before extracting and running the binary.
A signature failure means the release is not the one published by the
click-dog release workflow — do not install it.

> **Egress requirement for `self-update`:** keyless verification calls
> Rekor (`rekor.sigstore.dev`) and Fulcio (`fulcio.sigstore.dev`). If
> your egress firewall allows GitHub but blocks sigstore, `self-update`
> will fail closed after a 60s timeout with "Rekor/Fulcio unreachable?"
> — by design, since the alternative is silently trusting an
> unverified binary. Either allow egress to the sigstore hosts above,
> or skip `self-update` and use the manual flow above on a host that
> can reach sigstore, then ship the verified binary in via
> `deploy/install.sh -b`.

Container images on `ghcr.io/coltconsulting/click-dog` are signed with
the same identity:

```bash
cosign verify ghcr.io/coltconsulting/click-dog:${VERSION} \
  --certificate-identity-regexp '^https://github\.com/coltconsulting/click-dog/\.github/workflows/release\.yml@refs/tags/v.+$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
```

### Proxy support

The script only makes outbound HTTPS requests when auto-downloading the binary. Use `-x` or standard environment variables:

```bash
sudo ./deploy/install.sh install -c collector:4317 -x http://proxy.corp:3128
# or
export HTTPS_PROXY=http://proxy.corp:3128
```

---

## Kubernetes

Generate templated manifests with kustomize:

```bash
export CLICKHOUSE_PASSWORD="your-monitoring-password"
./deploy/install.sh kubernetes \
  -c otel-collector:4317 \
  --ch-host clickhouse.clickhouse.svc.cluster.local \
  --cluster main
```

`install.sh kubernetes` runs `click-dog deploy kubernetes` from the published
image (so it works from a laptop — it needs **Docker**, or run the generator
directly on a Linux host). `--ch-host` is **required**: it's the ClickHouse
Service DNS click-dog connects to — a standalone click-dog pod can't reach
ClickHouse via `localhost`. `--cluster` enables `cluster()` reads across all
shards.

This creates a `click-dog-k8s/` directory with: Namespace, ServiceAccount, ConfigMap, Secret, and a single-replica **Deployment**:

```bash
kubectl apply -k click-dog-k8s/
```

The generated ConfigMap enables the dedicated health listener on `:8686`, and the generated Deployment probes `/healthz` for liveness and `/readyz` for readiness on the `health` port. See [Kubernetes — ClickHouse setup](kubernetes-clickhouse.md) for topology (centralized vs sidecar) and the ClickHouse-side prerequisites.

### Updating

```bash
./deploy/install.sh kubernetes update -v 26.03.2
kubectl apply -k click-dog-k8s/
```

!!! warning "Migrating a pre-Deployment install"
    Output directories generated before the DaemonSet→Deployment change hold a
    `daemonset.yaml` (not `deployment.yaml`); `update` refuses them because an
    image-tag bump can't migrate the topology. **Regenerate** the directory
    (`./deploy/install.sh kubernetes -c … --ch-host …`), then **delete the old
    DaemonSet** — `kubectl apply -k` won't prune it, so it would keep running
    and double-export alongside the new Deployment:
    ```bash
    kubectl delete daemonset click-dog -n click-dog
    ```

### StatefulSet sidecar

If ClickHouse runs as a StatefulSet, add click-dog as a container in the pod spec:

```yaml
- name: click-dog
  # Pin an exact release tag in production; replace this example with the
  # current release tag and avoid :latest so rollouts and rollbacks stay
  # reproducible.
  image: ghcr.io/coltconsulting/click-dog:26.04.6
  args: ["-config", "/etc/click-dog/click-dog.yaml"]
  env:
    - name: CLICKHOUSE_USERNAME
      valueFrom:
        secretKeyRef:
          name: click-dog-secret
          key: CLICKHOUSE_USERNAME
    - name: CLICKHOUSE_PASSWORD
      valueFrom:
        secretKeyRef:
          name: click-dog-secret
          key: CLICKHOUSE_PASSWORD
  resources:
    requests: { cpu: 50m, memory: 64Mi }
    limits:   { cpu: 200m, memory: 128Mi }
  volumeMounts:
    - name: click-dog-config
      mountPath: /etc/click-dog
      readOnly: true
```

### TLS via cert-manager

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
vi deploy/kubernetes/certificate.yaml   # set your email + collector FQDN
kubectl apply -f deploy/kubernetes/certificate.yaml
# Then uncomment the TLS block in configmap.yaml and re-apply
```

---

## Docker

Generate a docker-compose setup:

```bash
export CLICKHOUSE_PASSWORD="your-monitoring-password"
./deploy/install.sh docker -c otel-collector:4317
```

Creates a `click-dog-docker/` directory with `docker-compose.yml`, `click-dog.yaml`, and `.env`.

```bash
cd click-dog-docker && docker compose up -d
```

### Updating

```bash
./deploy/install.sh docker update -v 26.03.2
cd click-dog-docker && docker compose up -d
```

---

## Installed Files

After install, each node has:

| Component | Path |
|-----------|------|
| Binary | `/usr/local/bin/click-dog` |
| Config | `/etc/click-dog/click-dog.yaml` |
| Secrets | `/etc/click-dog/.secret` |
| Logs | stderr (`journalctl -u click-dog`) |
| systemd unit | `/etc/systemd/system/click-dog.service` |
| User | `click-dog` (system, no shell) |

---

## Updating

`install.sh update` operates on the current host. It downloads or accepts a binary, smoke-tests it, rejects downgrades, validates the existing config against the staged binary, writes `/usr/local/bin/click-dog.prev`, swaps the binary, and restarts systemd. It does **not** rewrite `/etc/click-dog/click-dog.yaml`.

```bash
# Single node: verified binary swap + config validation
sudo ./deploy/install.sh update

# Pin a specific version
sudo ./deploy/install.sh update -v 26.03.2

# Backward-compatible no-op; update-time config merging was removed
sudo ./deploy/install.sh update --no-merge

# Fleet: same playbook, with a pinned version and bounded SSH
# concurrency. For batched rollouts that finish a wave before
# starting the next, add `serial: 10` to the play in playbook.yaml.
ansible-playbook -i deploy/ansible/inventory.ini deploy/ansible/playbook.yaml \
  --extra-vars "clickhouse_password=YOUR_PASSWORD click_dog_version=26.03.2" \
  -f 10
```

New config keys are supplied by the binary's runtime defaults when omitted.
If you want newly recommended settings materialized in YAML, render a fresh
profile with `click-dog init`, compare it to your checked-in config, and apply
the changes deliberately.

### In-place `self-update`

The binary can also replace itself from the latest GitHub release (verifying
the cosign signature as described in [Verifying releases manually](#verifying-releases-manually)):

```bash
click-dog self-update -check        # report whether an update is available
click-dog self-update               # download, verify, and swap the binary
```

`self-update` tracks the **stable** channel only — it uses GitHub's
`/releases/latest`, which excludes prereleases. To opt into alpha/beta builds,
add `--prerelease`:

```bash
click-dog self-update -check -prerelease   # see the newest build incl. prereleases
click-dog self-update -prerelease          # update onto it
```

While the repository is private, `self-update` requires `GITHUB_TOKEN` to be set
(a fine-grained token with read access to releases is sufficient).

---

## Smart Defaults

The table below describes the posture the **installer** writes into a generated
`click-dog.yaml`. The compiled binary's own defaults are conservative — both
the metrics scrape listener and the health listener are **off** unless their
respective `enabled: true` flags are present in your config. `install.sh` sets
them on; hand-rolled configs that omit the blocks will not open either port.

| Feature | Installer default | Why |
|---------|-------------------|-----|
| Circuit breaker | Enabled (open after 3 failures) | Protects ClickHouse during collector outages |
| Adaptive backoff | Enabled (up to 5 min between polls) | Graceful degradation under failures |
| HA leader election | Auto-enabled when `-k` is set | Infrastructure for future coordination features |
| Rate limiting | 1000 spans/cycle per node | Prevents collector overload |
| Dedup cache | 10k entries | Prevents duplicate exports across poll cycles |
| TLS | Off (see below) | Easy to add, not required for internal networks |
| Connection pool | 2 max connections | Low per-node footprint on ClickHouse |
| Scrape listener (`metrics:`) | Enabled on `:9090` | `/metrics` for Prometheus / Agent / Alloy, plus legacy `/health` |
| Health listener (`health:`) | Enabled on `:8686` | `/healthz`, `/readyz`, `/status` for Kubernetes / orchestrators — separate from the scrape listener |

---

## Enabling TLS

Click-Dog's gRPC connection to the OTEL collector carries span data over the network. TLS encrypts query text, durations, and trace metadata in transit.

> The ClickHouse connection (`localhost:9000`) stays on loopback and typically doesn't need TLS.

```yaml
exporters:
  otel:
    - collector_address: collector.internal:4317
      secure: true
      ca_cert: /etc/click-dog/tls/ca.pem        # CA that signed collector's cert
      client_cert: /etc/click-dog/tls/tls.crt    # optional, for mTLS
      client_key: /etc/click-dog/tls/tls.key     # optional, for mTLS
```

If your collector uses a well-known CA (e.g., cloud provider), you only need `secure: true`.

| Key | Purpose |
|-----|---------|
| `exporters.otel[].secure` | Enable TLS on gRPC to collector |
| `exporters.otel[].ca_cert` | CA certificate to verify collector |
| `exporters.otel[].client_cert` | Client cert for mTLS (optional) |
| `exporters.otel[].client_key` | Client key for mTLS (optional) |
| `exporters.otel[].insecure_skip_verify` | Skip verification (testing only) |
| `clickhouse.secure` | Enable TLS to ClickHouse (rarely needed) |
| `clickhouse.ca_cert` | CA cert for ClickHouse TLS |

---

## Scaling Considerations

### OTEL Collector Capacity

All click-dog instances push to your OTEL collector. Size it for:

```
Max throughput = N nodes × 1000 spans/cycle × (60s / 30s interval)
              = N × 2000 spans/minute (worst case)
```

For 50 nodes: up to 100k spans/minute. Mitigations:
- Collector pool behind a load balancer
- Increase `check_interval_s` to reduce frequency
- Lower `max_spans_per_cycle` per node
- Use `batch_size` + `batch_delay_ms` to smooth bursts

### Resource Footprint

| Resource | Per Node | 50-Node Cluster |
|----------|----------|-----------------|
| CPU | <50m | ~2.5 vCPU |
| Memory | ~40-60 MiB | ~3 GiB |
| Connections | 2 to ClickHouse | 100 total |
| Network | Minimal | — |
| Disk | Logs only (stderr) | — |

### ClickHouse Impact

Each sidecar runs a lightweight SELECT against `system.opentelemetry_span_log` every 30 seconds, using at most 2 connections with a 30-second timeout. Circuit breaker stops querying if ClickHouse is struggling. Total cluster impact is negligible.

---

## Verifying the Deployment

```bash
# Per-host systemd status
sudo ./deploy/install.sh status

# Fleet-wide status via an Ansible ad-hoc command — the playbook
# does not currently expose a `status` tag, so run systemctl
# directly over the same inventory:
ansible clickhouse -i deploy/ansible/inventory.ini --become \
  -m systemd -a "name=click-dog"

# Kubernetes
kubectl get pods -n click-dog -o wide

# Logs (look for "Exported:" lines)
ssh ch-node-01 journalctl -u click-dog -n 20 --no-pager

# Liveness + readiness (dedicated health listener)
curl -s localhost:8686/healthz
curl -s localhost:8686/readyz | jq

# Prometheus metrics (scrape listener)
curl -s localhost:9090/metrics
```

---

## Troubleshooting

### Instance Not Exporting Spans

1. Check ClickHouse tracing is enabled:
   ```sql
   SELECT count() FROM system.opentelemetry_span_log WHERE finish_date = today()
   ```
2. Check click-dog can reach its local ClickHouse: logs will show connection status.
3. Check the OTEL collector is reachable: logs will show export errors if not.

### Circuit Breaker Open on Multiple Nodes

The shared downstream (OTEL collector) is likely overwhelmed or down. Check collector health first. All instances will back off independently.

### Leader Election Failing

1. Verify Keeper is reachable: `echo ruok | nc keeper-01 9181` should return `imok`.
2. Check `ha.keeper.hosts` in config matches your Keeper ensemble.
3. Click-dog falls back to standalone mode if Keeper is unreachable — it logs a warning but continues exporting.

### High Memory on a Node

The default `dedup_cache_size: 10000` uses ~160 KiB. Only increase if you see duplicate exports in your observability backend.

---

## License and contributing

Click-Dog is licensed under [Apache 2.0](https://github.com/coltconsulting/click-dog/blob/master/LICENSE). Forks and redistributions are welcome under the same terms.

Contributions require a [Developer Certificate of Origin](https://developercertificate.org/) (DCO) `Signed-off-by` trailer on every commit — `git commit -s` adds it for you. The DCO is checked in CI; see the root [CONTRIBUTING.md](https://github.com/coltconsulting/click-dog/blob/master/CONTRIBUTING.md) for the full contributor contract.

"Click-Dog" is an unregistered trademark of Colt Consulting Ltd; the Apache 2.0 grant does not cover the name (per section 6 of the license). Operational and packaging guidance is in [TRADEMARKS.md](https://github.com/coltconsulting/click-dog/blob/master/TRADEMARKS.md). Third-party attributions are in [NOTICE](https://github.com/coltconsulting/click-dog/blob/master/NOTICE).
