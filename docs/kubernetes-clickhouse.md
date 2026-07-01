# Kubernetes — ClickHouse setup (operator)

Click-Dog ships Kubernetes manifests for the **collector side** (the Deployment
that runs click-dog itself, under [`deploy/kubernetes/`](https://github.com/coltconsulting/click-dog/tree/master/deploy/kubernetes)).
But click-dog only *reads* spans — it cannot enable them. Before click-dog has
anything to export, the **ClickHouse side** has to be set up:

1. `system.opentelemetry_span_log` enabled on **every** ClickHouse pod.
2. A **read-only monitoring user** with `SELECT` on exactly two system tables.
3. `system.query_log` available (it is on by default) for live query enrichment.

On host / systemd installs [`deploy/install.sh`](install.md) does all of this
for you. In Kubernetes the ClickHouse server is usually managed by an operator,
so this page shows the same prerequisites expressed as a
[ClickHouseInstallation](#1-enable-span-logging-on-every-pod) for the
[Altinity clickhouse-operator](https://github.com/Altinity/clickhouse-operator).

!!! warning "click-dog will run but export nothing until these are in place"
    A click-dog deployment against a ClickHouse cluster with no span-log config
    starts cleanly, passes its own health checks, and exports zero spans — the
    source table simply does not exist. Set up the ClickHouse side first.

The two example manifests referenced below live next to the collector
manifests:

| File | Purpose |
|------|---------|
| [`clickhouse-operator-spanlog.yaml`](https://github.com/coltconsulting/click-dog/blob/master/deploy/kubernetes/clickhouse-operator-spanlog.yaml) | `ClickHouseInstallation` enabling span_log + the read-only user/grants |
| [`clickhouse-setup-job.yaml`](https://github.com/coltconsulting/click-dog/blob/master/deploy/kubernetes/clickhouse-setup-job.yaml) | Alternative: a `Job` that creates the user/grants via `clickhouse-client` SQL |

---

## Prerequisites

- The [Altinity clickhouse-operator](https://github.com/Altinity/clickhouse-operator)
  installed (it provides the `ClickHouseInstallation` / `chi` CRD):

  ```bash
  kubectl apply -f https://raw.githubusercontent.com/Altinity/clickhouse-operator/master/deploy/operator/clickhouse-operator-install-bundle.yaml
  ```

- A ClickHouse cluster you can edit the server config for. (For ClickHouse
  Cloud and other managed services where you cannot, see
  [Managed ClickHouse](#managed-clickhouse-clickhouse-cloud).)

---

## 1. Enable span logging on every pod

The operator renders entries under `spec.configuration.files` into
`/etc/clickhouse-server/config.d/` on **every** pod, so a per-node click-dog
sidecar reading its local `system.opentelemetry_span_log` covers the whole
cluster.

These settings are **identical to what `install.sh` writes** on host installs —
keep them byte-for-byte the same; click-dog's queries assume this exact
engine / partition / order layout:

```yaml
apiVersion: "clickhouse.altinity.com/v1"
kind: "ClickHouseInstallation"
metadata:
  name: clickhouse
  namespace: clickhouse
spec:
  configuration:
    files:
      config.d/opentelemetry.xml: |
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

| Setting | Value | Matches |
|---------|-------|---------|
| Engine | `MergeTree` | `install.sh` |
| Partition | `toYYYYMM(finish_date)` | `install.sh` |
| Order | `(finish_date, finish_time_us, trace_id)` | `install.sh` |
| Database / table | `system` / `opentelemetry_span_log` | `install.sh` |
| Flush interval | `7500` ms | `install.sh` |

!!! note "Enabling span_log requires a pod restart"
    `opentelemetry_span_log` is loaded at server start. After the operator
    applies the config it performs a rolling update; ClickHouse only begins
    writing the table once each pod has restarted.

Span generation also needs tracing to actually fire. ClickHouse emits spans for
queries that carry a parent trace context, or you can force sampling with the
`opentelemetry_start_trace_probability` setting on the relevant profile (set it
to `1` to trace every query while you validate the pipeline, then lower it).

---

## 2. Create the read-only monitoring user

Two equivalent options. Both grant `SELECT` on **only** the two tables
click-dog reads — no database-wide or system-wide privileges — and put the user
on the built-in `readonly` profile (`readonly=2`: cannot write, cannot change
persistent settings). This matches the host install exactly.

### Option A — declare the user in the operator (recommended)

Add to the same `ClickHouseInstallation`:

```yaml
spec:
  configuration:
    users:
      click_dog_monitor/networks/ip:
        - "::/0"                       # tighten to your pod CIDR in production
      click_dog_monitor/password_sha256_hex: REPLACE_WITH_SHA256_HEX_OF_PASSWORD
      click_dog_monitor/profile: readonly
      click_dog_monitor/quota: default
      click_dog_monitor/grants/query:
        - "GRANT SELECT ON system.opentelemetry_span_log TO click_dog_monitor"
        - "GRANT SELECT ON system.query_log TO click_dog_monitor"
```

Generate the hash and keep the plaintext in your click-dog Secret:

```bash
printf '%s' 'YOUR_PASSWORD' | sha256sum | awk '{print $1}'
# uncomment CLICKHOUSE_PASSWORD in deploy/kubernetes/secret.yaml
# and set it to YOUR_PASSWORD before applying
```

The full file is
[`deploy/kubernetes/clickhouse-operator-spanlog.yaml`](https://github.com/coltconsulting/click-dog/blob/master/deploy/kubernetes/clickhouse-operator-spanlog.yaml).
Apply it:

```bash
kubectl apply -f deploy/kubernetes/clickhouse-operator-spanlog.yaml
```

### Option B — run the grant SQL as a Job

If you manage ClickHouse users imperatively (or run a vanilla StatefulSet
ClickHouse rather than the operator), use
[`deploy/kubernetes/clickhouse-setup-job.yaml`](https://github.com/coltconsulting/click-dog/blob/master/deploy/kubernetes/clickhouse-setup-job.yaml).
It runs the same SQL `install.sh` issues (`CREATE USER IF NOT EXISTS`, never
`OR REPLACE`, so re-running never clobbers an existing user):

```sql
CREATE USER IF NOT EXISTS click_dog_monitor IDENTIFIED WITH sha256_hash BY '<hash>';
GRANT SELECT ON system.opentelemetry_span_log TO click_dog_monitor;
GRANT SELECT ON system.query_log TO click_dog_monitor;
```

Set the admin DSN and uncomment/fill the required password values in the Job's
`click-dog-ch-admin` Secret, then:

```bash
kubectl apply -f deploy/kubernetes/clickhouse-setup-job.yaml
kubectl wait --for=condition=complete job/click-dog-ch-setup -n click-dog --timeout=120s
# delete the admin Secret once the user exists
kubectl delete secret click-dog-ch-admin -n click-dog
```

The admin credentials are needed only to *create* the user; the monitoring user
itself can never create users or write data.

---

## 3. Deploy click-dog: sidecar vs centralized

Two supported topologies. **Pick one** — they must not be combined (see the
danger note below). The deciding factor is network reach: click-dog reads
`system.opentelemetry_span_log` over a ClickHouse TCP connection, so it must be
able to reach a ClickHouse endpoint.

### Centralized cluster queries (recommended on Kubernetes)

A **single** click-dog Deployment (or a few for redundancy, with HA leader
election) reaches ClickHouse through its **Service** and queries across all
shards using ClickHouse's `cluster()` function. This works out of the box on
Kubernetes — no pod surgery, no `localhost` assumptions:

- point `clickhouse.host` at the ClickHouse **Service** DNS (e.g.
  `clickhouse.clickhouse.svc.cluster.local`), **not** `localhost`
- set `clickhouse.use_cluster_queries: true` **and** `clickhouse.cluster` to
  your cluster name (the sample CHI above uses `main`)
- run click-dog as a **Deployment** — the shipped `deploy/kubernetes/`
  manifests do exactly this (a single-replica Deployment)

```yaml
clickhouse:
  host: clickhouse.clickhouse.svc.cluster.local
  port: 9000
  use_cluster_queries: true
  cluster: main          # REQUIRED — without it, cluster_queries is a no-op
```

`use_cluster_queries: true` only wraps reads in
`cluster('<name>', system.opentelemetry_span_log)` when `cluster` is **also**
set. Leave `cluster` empty and click-dog silently queries plain
`system.opentelemetry_span_log` through the Service — reading only whichever
single pod the Service routes to, not all shards. The monitoring user needs
query reachability across the cluster. See
[Configuration · Cluster Mode](configuration.md#cluster-mode).

### Same-pod sidecar (node-local, for very large clusters)

One click-dog **container co-located inside each ClickHouse pod**, reading that
pod's **local** `system.opentelemetry_span_log` over `localhost` with
`use_cluster_queries: false`. It scales linearly and needs no cross-node
permissions — but click-dog only reaches ClickHouse on `localhost` when it
**shares the ClickHouse pod's network namespace**, so it must be injected as an
extra container into the ClickHouse pod spec (operator: a `podTemplate` under
`spec.templates.podTemplates` on the `ClickHouseInstallation`; StatefulSet: the
pod's `containers`), with a click-dog ConfigMap whose `clickhouse.host` is
`localhost`.

!!! note "`localhost` is only valid in a same-pod sidecar"
    The shipped [`deploy/kubernetes/`](https://github.com/coltconsulting/click-dog/tree/master/deploy/kubernetes)
    manifests implement the **centralized** topology above: a single-replica
    `Deployment` whose ConfigMap points `clickhouse.host` at the ClickHouse
    Service (edit it to your Service DNS). `clickhouse.host: localhost` works
    **only** in the same-pod sidecar, where click-dog shares the ClickHouse
    pod's network namespace. A standalone Deployment/DaemonSet runs in its own
    pod, so `localhost` resolves to the click-dog container itself and never
    reaches ClickHouse — `click-dog deploy kubernetes` rejects a `localhost`
    `-ch-host` for this reason.

!!! danger "Never run node-local sidecars with `use_cluster_queries: true`"
    Cluster-query mode wraps the spans query in
    `cluster('name', system.opentelemetry_span_log)`, which fetches across all
    shards from one instance. Combined with one click-dog per pod, *every*
    sidecar fetches *every* shard — massive duplication and cluster-wide load.
    In cluster-query mode run a single centralized Deployment, not one per pod.
    See [Operating · Cluster topology](operating.md#cluster-topology-and-sidecar-placement).

---

## 4. Where click-dog exports to

click-dog exports spans over OTLP/gRPC. In Kubernetes it typically points at an
in-cluster collector:

- **OTEL Collector** — set `exporters.otel[].collector_address` to the
  collector Service, e.g. `otel-collector.monitoring:4317` (the shipped
  ConfigMap default). The collector then forwards to Datadog / Honeycomb / etc.
- **Datadog Agent** — if you run the Datadog Agent with OTLP ingest enabled,
  point `collector_address` at the Agent's OTLP gRPC port (commonly `:4317`).

click-dog's own Prometheus metrics (`:9090/metrics`) and health endpoints
(`:8686/healthz`, `/readyz`) are exposed by the click-dog pod for scraping and
probes — these are click-dog self-monitoring, separate from the span export
path. See [Integrations · Datadog](integrations/datadog.md) for OTLP wiring and
the Agent OpenMetrics scrape config.

---

## 5. Verify

After the rolling restart and user creation, confirm the pipeline end to end.

**click-dog's own check** — validates config, ClickHouse connectivity, and
exporter reachability:

```bash
# centralized Deployment: deploy/click-dog · sidecar: the ClickHouse pod
kubectl exec -n click-dog deploy/click-dog -- click-dog check -config /etc/click-dog/click-dog.yaml
```

**Span-log table exists and is filling** — run against ClickHouse as the
monitoring user:

```sql
EXISTS TABLE system.opentelemetry_span_log;
SELECT count() FROM system.opentelemetry_span_log WHERE finish_date >= today();
```

```bash
kubectl exec -n clickhouse <clickhouse-pod> -- clickhouse-client \
  --user click_dog_monitor --password "$MONITORING_PASSWORD" \
  --multiquery --query "EXISTS TABLE system.opentelemetry_span_log; SELECT count() FROM system.opentelemetry_span_log WHERE finish_date >= today()"
```

A non-zero count (after running a few traced queries) confirms ClickHouse is
writing spans and the monitoring user can read them. If the count stays at zero,
make sure queries are actually being traced (parent trace context or
`opentelemetry_start_trace_probability`), and that pods restarted after the
config landed.

---

## Managed ClickHouse (ClickHouse Cloud)

On ClickHouse Cloud and most managed services you **cannot** ship a
`config.d/` file, so the span-log `files` block above does not apply — you
cannot enable `opentelemetry_span_log` the same way. Options:

- Use only the settings the platform lets you set. Some managed offerings expose
  `opentelemetry_start_trace_probability` and a small set of profile settings,
  but **do not** let you define the `opentelemetry_span_log` *destination table*
  via server config — without that table click-dog has nothing to read.
- If the managed service does not expose `system.opentelemetry_span_log` at all,
  click-dog's live span-log path is not available there. Consider the
  query-log backfill path instead (`system.query_log` is generally available),
  noting it is a separate, historical data source — see
  [Operation Modes](modes.md) and
  [Integrations · Datadog query dashboard contract](integrations/datadog.md#query-dashboard-data-source-contract).
- The read-only monitoring user / grants still apply where the service supports
  SQL-level `CREATE USER` / `GRANT` (Option B above), scoped to whatever of the
  two tables the platform exposes.

Check your provider's docs for which server settings and system tables are
editable/available before assuming the span-log path will work.
