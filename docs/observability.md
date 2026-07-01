# Observability

Click-Dog includes three opt-in features for monitoring its own health: structured logging, a Prometheus metrics endpoint, and webhook notifications. All three are disabled by default and add zero external dependencies.

---

## Structured JSON Logging

By default, click-dog logs in human-readable text format:

```
2024-01-15 10:30:45 [INFO] Exported 42 spans from 3 traces
```

Set `log_format: json` to emit machine-parseable JSON instead:

```json
{"ts":"2024-01-15T10:30:45Z","level":"info","msg":"Exported 42 spans from 3 traces"}
```

### Configuration

```yaml
log_level: info       # debug, info, warn, error
log_file: ""          # File path, or empty for stderr
log_format: json      # text (default) or json
```

JSON format is useful when shipping logs to aggregators like Loki, Datadog Logs, or Elasticsearch. The `ts` field uses RFC 3339 / UTC.

---

## Metrics Endpoint

When `metrics.enabled: true`, click-dog opens **two** HTTP listeners:

- a **scrape listener** for `/metrics` and `/health` — open to anywhere a
  Prometheus / Grafana Agent / Datadog Agent / OTel Collector scraper can
  reach, by default `:9090` (all interfaces);
- an **admin listener** for `POST /flush` — bound to `127.0.0.1:9091` by
  default, so the export-trigger endpoint is unreachable off-host.

This split follows the Vector / Grafana Alloy / Datadog Agent pattern: keep
scrape-shaped endpoints separable from admin-shaped actions so an operator
exposing `/metrics` doesn't simultaneously expose `/flush`. `/flush`'s
effect is bounded (channel cap 1, no data flow) and is equivalent in reach
to `SIGUSR1`, which is the existing first-class flush trigger; a localhost
bind is the proportional control.

### Configuration

```yaml
metrics:
  enabled: true
  listen_address: ":9090"               # scrape listener — /metrics + /health
  admin_listen_address: "127.0.0.1:9091" # admin listener — POST /flush
```

`enabled` gates **both** listeners. Setting it to `false` disables the
HTTP `POST /flush` endpoint as well; `SIGUSR1` remains the always-on
cross-host equivalent and does not depend on this flag.

If `admin_listen_address` resolves to a non-loopback bind (e.g. `:9091` or
`0.0.0.0:9091`), click-dog logs a startup `WARN` in the same style as the
TLS-skip-verify warning. That's intentional — exposing the admin listener
off-host is supported but not the default; operators who do it deliberately
should see the choice acknowledged in their logs.

### Endpoints

| Listener | Endpoint | Content-Type | Purpose |
|----------|----------|--------------|---------|
| scrape | `GET /metrics` | `text/plain` | Prometheus text exposition format |
| scrape | `GET /health` | `application/json` | Legacy liveness — returns `{"status":"ok"}`. Predates the dedicated [Health Endpoints](#health-endpoints) block. For Kubernetes probes use `/healthz` on the health listener, **not** `/health` on the scrape listener. |
| admin  | `POST /flush` | `application/json` | Triggers an immediate export cycle (`200 {"status":"flushing"}`, or `409 {"status":"already_flushing"}` if a flush is already queued). `GET` returns `405`. |

> Note: `/healthz`, `/readyz`, and `/status` are **not** served by the scrape
> listener. Hitting `:9090/healthz` returns `404`. They live on the separate
> [health listener](#health-endpoints) (default `:8686`), which has its own
> `health.enabled: true` toggle.

`POST /flush` deliberately has no authentication. The localhost bind is the
boundary — anyone who can hit `127.0.0.1:9091` already has shell on the
host and can `kill -USR1 $(pidof click-dog)` for the same effect. If you
move the admin listener off-host, treat it like SSH access to that host.

### Triggering /flush from another host

The default loopback bind is the recommended posture; common ways to reach
it from elsewhere without changing the bind:

- **Same host, ad-hoc:** `curl -XPOST http://127.0.0.1:9091/flush`
- **systemd:** `kill -USR1 $MAINPID` from a one-off unit, or
  `systemctl kill --signal=SIGUSR1 click-dog.service`. SIGUSR1 is the
  signal-equivalent of POST /flush — same channel, same backpressure,
  no listener exposure required.
- **Docker:** publish the admin port to the loopback only —
  `-p 127.0.0.1:9091:9091` — and `curl` from the host. Do **not**
  `-p 9091:9091` (binds 0.0.0.0) unless you understand the trade-off.
- **Kubernetes (sidecar):** in a sidecar pod, the admin listener is on
  `localhost` because containers in a Pod share a network namespace. A
  sidecar that needs to flush can simply `curl 127.0.0.1:9091/flush`.
  Don't add a `Service` or `containerPort` for `9091`.
- **Kubernetes (DaemonSet):** if you genuinely need to flush remotely
  (rare — prefer SIGUSR1 via `kubectl exec`), set
  `metrics.admin_listen_address: ":9091"` and add a `NetworkPolicy`
  restricting `9091/tcp` to a specific operator pod or namespace. The
  startup WARN is your reminder that this is a non-default posture.

### Available Metrics

**Counters** (monotonically increasing):

| Metric | Description |
|--------|-------------|
| `click_dog_spans_exported_total` | Total spans exported to backends |
| `click_dog_spans_filtered_total` | Total spans filtered out by rules |
| `click_dog_spans_duplicates_total` | Total duplicate spans skipped by dedup cache |
| `click_dog_export_attempts_total{sink}` | Export items submitted by sink, including retries |
| `click_dog_export_accepted_total{sink}` | Export items accepted by sink |
| `click_dog_export_errors_total{sink}` | Export attempts that returned an error by sink |
| `click_dog_cycle_results_total{result}` | Cycle outcomes by result label (`success`, `error`, `skipped`) |

> **Cycle counting note:** `click_dog_cycle_results_total` tags every tick of the main loop — including circuit-blocked and canary-only ticks — with exactly one of `success`, `error`, or `skipped` (canary sentinel errors resolve to non-error). So `sum(rate(click_dog_cycle_results_total))` is the overall cycle rate and `rate(click_dog_cycle_results_total{result="error"}) / sum(rate(click_dog_cycle_results_total))` is a reliable error ratio in all states. `skipped` covers cycles that did no ClickHouse work (e.g. circuit breaker open without canary). The `result` label is a fixed three-value enum to keep cardinality bounded.
>
> **Per-sink export counters:** `click_dog_export_*_total` metrics use the exporter sink label (`otel`, `splunk_hec`, `dry_run`, or the configured `MultiExporter` sink name). In live mode dedup still follows the top-level accepted keys, so a partial multi-sink failure is retried even when one sink's accepted counter increased.

**Gauges** (current state):

| Metric | Description |
|--------|-------------|
| `click_dog_circuit_breaker_state` | Circuit breaker: 0=closed, 1=half_open, 2=open |
| `click_dog_backoff_interval_seconds` | Current polling interval in seconds |
| `click_dog_last_success_timestamp_seconds` | Unix timestamp of last successful cycle. The bare series is retained for compatibility with existing scrapes and custom alerts; the same metric is also emitted with `role="active"` or `role="standby"` so HA dashboards can exclude leader-gated standbys from fleet stale-export calculations |
| `click_dog_leader` | `1` when this instance is the active exporter/leader, `0` when it is a leader-gated standby |
| `click_dog_uptime_seconds` | Seconds since process start |
| `click_dog_last_cycle_duration_seconds` | Duration of the most recent processing cycle in seconds |
| `click_dog_last_cycle_exported_spans` | Spans exported in the most recent cycle |
| `click_dog_last_cycle_filtered_spans` | Spans filtered in the most recent cycle |
| `click_dog_last_cycle_duplicate_spans` | Duplicate spans skipped in the most recent cycle |
| `click_dog_span_log_last_poll_timestamp_seconds` | Unix timestamp of the last span-log fetch that completed without error (zero rows still counts) |
| `click_dog_span_log_newest_row_age_seconds` | Age in seconds of the newest span-log row observed in the most recent non-empty fetch; computed at scrape time so it keeps growing across empty cycles (the staleness signal). `0` is ambiguous — it can mean "no observation yet" OR "the newest row arrived this scrape interval"; alert on `> threshold` (a value Prometheus' `time() - ...` can never make negative) rather than `== 0` to avoid the ambiguity |
| `click_dog_span_log_rows_last_cycle` | Raw row count returned by the last span-log fetch, before filter/dedup |
| `click_dog_query_log_enrichment_match_ratio` | Last cycle's matched / requested query_id ratio; persists across failed attempts (`0` before first success) |
| `click_dog_spans_with_query_id_ratio` | Last cycle's ratio of fetched spans carrying `clickhouse.query_id`; empty cycles preserve the previous value |
| `click_dog_normalized_query_supported` | `1` if `system.query_log.normalized_query_hash` is available and in use, `0` otherwise. In cluster query mode, `1` means every replica passed the startup compatibility probe. Set once at startup |

> **ClickHouse data-plane health (#183):** the six gauges and three counters
> (`click_dog_query_log_enrichment_{attempts,successes,failures}_total`) named
> above are independent of "is click-dog exporting" — they answer "is
> ClickHouse producing the data click-dog needs?" Absence of the entire
> metric family means click-dog is down; presence with a high
> `newest_row_age_seconds`, a non-zero failures rate, or
> `spans_with_query_id_ratio < 1.0` means click-dog is fine but the data
> source is degraded. All values are derived from data each cycle already
> fetches — no extra ClickHouse queries are issued.

**Counters (data-plane health, #183):**

| Metric | Description |
|--------|-------------|
| `click_dog_query_log_enrichment_attempts_total` | Cycles that attempted query_log enrichment (had at least one `clickhouse.query_id`). Non-enrichment configs never tick. |
| `click_dog_query_log_enrichment_successes_total` | Enrichment attempts that returned without error |
| `click_dog_query_log_enrichment_failures_total` | Enrichment attempts that returned an error; pair with the failures rate panel on the Health dashboard |

> **Last-cycle gauges** are emitted unconditionally — they are zero before the first cycle so the metric set stays stable across scrapes. They reflect the most recent cycle only, not a cumulative total; pair them with `click_dog_cycle_results_total` for rate-style questions.

### Example Output

```
# HELP click_dog_spans_exported_total Total spans exported to backends.
# TYPE click_dog_spans_exported_total counter
click_dog_spans_exported_total 14523
# HELP click_dog_cycle_results_total Total processing cycles by outcome (success, error, skipped).
# TYPE click_dog_cycle_results_total counter
click_dog_cycle_results_total{result="success"} 1287
click_dog_cycle_results_total{result="error"} 2
click_dog_cycle_results_total{result="skipped"} 14
# HELP click_dog_export_errors_total Total export attempts that returned an error by sink.
# TYPE click_dog_export_errors_total counter
click_dog_export_errors_total{sink="otel"} 0
# HELP click_dog_circuit_breaker_state Circuit breaker state (0=closed, 1=half_open, 2=open).
# TYPE click_dog_circuit_breaker_state gauge
click_dog_circuit_breaker_state 0
# HELP click_dog_uptime_seconds Seconds since click-dog started.
# TYPE click_dog_uptime_seconds gauge
click_dog_uptime_seconds 3600
```

### Kubernetes Probes

For Kubernetes probes prefer the dedicated health endpoints (`/healthz`,
`/readyz`, `/status`) described in the [Health Endpoints](#health-endpoints)
section below. `/health` on the metrics server is kept as a simple always-OK
liveness signal for setups that can't opt into the separate health server.

---

## Health Endpoints

Click-Dog exposes three dedicated HTTP endpoints for orchestrators: `/healthz`
(liveness), `/readyz` (readiness), and `/status` (detailed JSON). Disabled by
default; opt in with the `health:` block.

### Configuration

```yaml
health:
  enabled: true
  listen_address: ":8686"   # default
```

When `health.listen_address` resolves to the same socket as
`metrics.listen_address`, the health handlers mount on the metrics listener
so only one port is opened. The comparison is normalized — `":8686"`,
`"0.0.0.0:8686"`, and `"[::]:8686"` are all treated as equivalent wildcard
binds. If you write the two addresses differently but they normalize to
the same form, click-dog will log a warning on startup so you notice the
inconsistency. Explicit host binds (e.g. `127.0.0.1:8686`) must match
literally — they won't be normalized against wildcards. If the normalized
forms differ, a dedicated health listener is started on the configured
address.

### Endpoints

| Endpoint | Status codes | Purpose |
|----------|--------------|---------|
| `GET /healthz` | `200` always | Liveness — the process is running |
| `GET /readyz` | `200` / `503` | Readiness — `200` when ClickHouse is reachable AND the circuit breaker is closed; `503` otherwise |
| `GET /status` | `200` always | JSON: version, uptime, CB state, backoff interval, last cycle counts/duration |

`/status` always returns `200` — it's a reporting endpoint, not a probe.
Assess health from the JSON body fields (`clickhouse_healthy`,
`circuit_breaker`, `last_cycle.error`), not the HTTP status code. Alerts
that fire on "/status non-200" will never trigger; alert on the body
fields instead.

**Exposure note:** `last_cycle.error` is the raw error string returned
by the ClickHouse driver, which may contain query fragments, table
names, or column names depending on what ClickHouse included in its
error response. This is consistent with `/metrics` (which exposes
similar operational internals), but on multi-tenant infrastructure you
may want to restrict network access to the health port rather than
exposing it outside the cluster.

`/status` response shape:

```json
{
  "version": "v26.04.6",
  "uptime_s": 3600,
  "clickhouse_healthy": true,
  "circuit_breaker": "closed",
  "backoff_interval_s": 30,
  "last_cycle": {
    "exported": 15,
    "filtered": 3,
    "duplicates": 2,
    "duration_ms": 450,
    "at": "2026-04-19T12:34:56Z"
  }
}
```

### Kubernetes Probes

```yaml
livenessProbe:
  httpGet:
    path: /healthz
    port: 8686
  initialDelaySeconds: 10
  periodSeconds: 30
readinessProbe:
  httpGet:
    path: /readyz
    port: 8686
  initialDelaySeconds: 5
  periodSeconds: 10
```

Note on scaling: `/readyz` issues a live ClickHouse ping (2s deadline)
on every probe call. With a 10-second `periodSeconds` that's 6
pings/minute per pod — negligible for a single instance, but on a 50-node
DaemonSet that's 300 pings/minute against ClickHouse. When ClickHouse is
unhealthy all those probes block for the full 2s deadline simultaneously.
For clusters larger than ~20 nodes consider raising `periodSeconds` to
`30` (matches the livenessProbe) to cap per-minute ping volume.

### Readiness vs. status on startup

`/readyz` and `/status.clickhouse_healthy` use different signals and will
briefly disagree while click-dog is warming up. `/readyz` returns `200`
as soon as ClickHouse is reachable and the circuit breaker is closed —
which is true from process start. `/status.clickhouse_healthy`, on the
other hand, stays `false` until at least one processing cycle has run
successfully (the cycle-completed gate guards against a false-green
before any actual ClickHouse query has executed). Expect a short window
where Kubernetes considers the pod `Ready` but the status dashboard
shows `clickhouse_healthy: false`. Both behaviors are intentional.

The concrete consequence: on pod startup Kubernetes will add the pod to
Service endpoints (readiness satisfied) **before** click-dog has
confirmed an actual span-fetch cycle works. click-dog exports spans
outbound and serves no user traffic, so this is low risk — but if you
are watching a dashboard during a rollout, expect `clickhouse_healthy`
to flip to `true` a few seconds after the pod goes `Ready`.

In HA cluster mode, a follower that is intentionally idle behind the leader gate
reports `standby: true`; its last cycle is marked `skipped` with
`skip_reason: "leader_standby"`. That state is considered healthy for
`/status.clickhouse_healthy` when the circuit breaker is closed, because the
instance is doing exactly what it should. Breaker-open skips use
`skip_reason: "circuit_open"` and remain unhealthy.

```json
{
  "clickhouse_healthy": true,
  "standby": true,
  "circuit_breaker": "closed",
  "last_cycle": {
    "skipped": true,
    "skip_reason": "leader_standby"
  }
}
```

### Readiness during circuit breaker recovery

`/readyz` returns `503` for **any** circuit breaker state other than
`closed` — this includes the `half_open` state that the breaker
transitions through while probing whether ClickHouse has recovered. If
your click-dog pods are behind a Kubernetes Service (e.g. for Prometheus
to scrape `/metrics`), a half-open breaker that takes more than
`failureThreshold × periodSeconds` to close will cause K8s to remove the
pod from the Service endpoints mid-recovery — which can hide the very
metrics you need to diagnose the outage.

Two mitigations:

1. **Raise `failureThreshold`** on the readinessProbe so short
   half-open windows don't evict the pod. The default (3 with a 10s
   period) gives ~30s; setting it to 6 gives ~60s. See the comment in
   `deploy/kubernetes/deployment.yaml`.
2. **Scrape metrics directly, not via a Service** — Prometheus pod discovery
   bypasses readiness entirely, so half-open transitions don't affect scrape
   continuity.

click-dog is primarily a sidecar that pushes spans to a collector; it
serves no user traffic, so a 503 during recovery is purely a metadata
signal for the orchestrator. The strict behavior was chosen so that
readiness stays a reliable "CB is closed and a cycle succeeded" signal;
if you prefer a more lenient contract, track [#18][phase2] for a
follow-up `health.readyz_half_open_ready` config knob.

[phase2]: https://github.com/coltconsulting/click-dog/issues/18

### Cluster health

The HA leader can serve a single aggregate `/clusterz` endpoint that fans
out `/readyz` to every configured peer. Operators get one URL for
monitoring dashboards, on-call paging, or load-balancer health targets
instead of scraping every node individually.

This is phase 1 of [#19][cluster19] — peers are configured statically.
Keeper-based peer discovery is a deferred follow-up.

```yaml
health:
  enabled: true
  listen_address: ":8686"
  cluster:
    enabled: true
    self: "node-0:8686"     # routable host:port for THIS instance — required
    peer_timeout_ms: 3000   # per-peer fanout deadline (default 3000)
    peers:
      - "node-0:8686"
      - "node-1:8686"
      - "node-2:8686"
```

Each peer entry is the routable `host:port` for that node's `/readyz`.
Write the same `peers:` list on every instance, but set `self` to the
node's own routable address — different on each instance. The leader
matches `self` against `peers` to skip its own HTTP self-loop and to
label its in-process readiness in the response. Only the current leader
serves `/clusterz`; followers return 404.

`listen_address` is intentionally separate: it's a bind expression
(typically `:8686`, meaning "all interfaces") and isn't routable, so it
can't double as `self`.

**`self` not in `peers` is allowed but warns.** A single-node deployment
that runs `peers: []` is fine — `/clusterz` aggregates just the self
entry. In a multi-node deployment, if `self` is non-empty but absent
from a non-empty `peers` list, click-dog logs a startup warning and
adds self as an *extra* node in the response (`total = len(peers) + 1`).
That's almost always an operator slip — write the same `peers` list on
every instance and have each instance set `self` to its own routable
entry from that list.

**Inter-node fanout is plaintext (phase 1).** The leader fetches each
peer over `http://`, with no authentication. That mirrors the existing
`/readyz` posture (no auth today). Combined with the dial-error
exposure note below, the recommendation is the same: keep the health
port restricted to the cluster's internal network. A configurable
scheme (and optional auth) is on the phase-2 list alongside Keeper-
based discovery.

Behavior:

- `/clusterz` is mounted only when `cluster.enabled: true` AND
  `health.enabled: true`. The combination is enforced at config load.
- When HA is disabled, the single instance treats itself as leader and
  `/clusterz` aggregates whatever peers are listed (typically just self).
- Followers respond `404 {"status":"not_leader"}`. A `Location:` header
  pointing at the leader is reserved for phase 2 once the Keeper payload
  carries health-endpoint addresses.
- Each peer is fetched in parallel with an independent `peer_timeout_ms`
  deadline. Slow or dead peers are reported `unreachable` without
  blocking the rest of the fanout.
- The leader's own readiness is computed in-process; no HTTP self-loop.
  When the leader's `health.listen_address` matches one of the `peers:`
  entries, that slot is filled by the in-process result.
- Responses are cached for 5 seconds. Bursty pollers (active-checks at
  short intervals) collapse onto one fanout per window.

`/clusterz` returns `200 OK` when every node is healthy; `503` when any
node is degraded or unreachable.

```json
{
  "status": "degraded",
  "leader": "node-0:8686",
  "nodes": {
    "node-0:8686": {
      "status": "ok",
      "checks": { "clickhouse": "ok", "circuit_breaker": "closed" }
    },
    "node-1:8686": {
      "status": "ok",
      "checks": { "clickhouse": "ok", "circuit_breaker": "closed" }
    },
    "node-2:8686": {
      "status": "unreachable",
      "error": "dial tcp 10.0.3.12:8686: connect: connection refused"
    }
  },
  "summary": {
    "total": 3,
    "healthy": 2,
    "degraded": 0,
    "unreachable": 1
  }
}
```

`status` per node is one of:

- `ok` — peer responded `200` to `/readyz`.
- `degraded` — peer responded but not `200`, or with a body that didn't
  parse as a `/readyz` response. `checks` is echoed when present.
- `unreachable` — peer never responded (timeout, connection refused,
  read error). `error` carries the network error string; `checks` is
  omitted because there's nothing to echo.

**Exposure note:** `nodes.<peer>.error` is the raw Go dial error, which
typically includes the peer's resolved IP and port (e.g.
`dial tcp 10.0.3.12:8686: connect: connection refused`). For an
internal-only health endpoint this is fine — operators want to see the
exact address that failed — but if you expose `/clusterz` outside the
cluster (e.g. through an ingress) you're publishing the internal
topology of any unreachable peer. Restrict network access to the health
port the same way you would for `/status`.

[cluster19]: https://github.com/coltconsulting/click-dog/issues/19

### Why Prometheus Format, Not OpenTelemetry Metrics?

Click-Dog is an OpenTelemetry *trace exporter* — so why does it expose its own health metrics in Prometheus format rather than pushing them as OTEL metrics?

**Failure domain separation.** If the OTEL collector goes down, click-dog can't report "I can't reach the OTEL collector" via the OTEL collector. A pull-based `/metrics` endpoint stays available independently, so your monitoring of click-dog doesn't depend on the same pipeline click-dog feeds into.

**It's already OTEL-compatible.** The OTEL Collector has a built-in [Prometheus receiver](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/receiver/prometheusreceiver) that scrapes `/metrics` endpoints and converts them into OTEL metrics. If you want click-dog's health data in your OTEL pipeline, point the collector's Prometheus receiver at click-dog's metrics port. Zero code changes needed.

**Zero dependencies.** The metrics endpoint emits Prometheus text exposition format with `fmt.Fprintf` — no `prometheus/client_golang`, no OTEL metrics SDK. This keeps the binary small and the attack surface minimal.

**Universal compatibility.** Every monitoring stack scrapes Prometheus natively: Datadog Agent, Grafana Agent, OTEL Collector, Victoria Metrics, Thanos, and plain Prometheus. By speaking this format, click-dog is compatible with everything out of the box.

In short: Prometheus text format is the *lingua franca* of operational metrics. The OTEL Collector translates it into OTEL metrics for you. Separate failure domains, zero dependencies, universal compatibility.

---

## Webhook Notifications

Click-Dog can send HTTP POST notifications to a webhook URL on critical events. The payload is compatible with Slack incoming webhooks and works with any endpoint that accepts `{"text": "..."}` JSON.

### Configuration

```yaml
webhook:
  enabled: true
  url: ${SLACK_WEBHOOK_URL}    # env var recommended for secrets
  timeout_s: 10                # HTTP timeout (default: 10s)
  events:                      # which events to fire (default: all)
    - circuit_breaker_opened
    - circuit_breaker_closed
    - startup
    - shutdown
```

### Events

| Event | Fires when |
|-------|-----------|
| `circuit_breaker_opened` | Circuit breaker tripped — ClickHouse queries paused |
| `circuit_breaker_closed` | Circuit breaker recovered — normal operation resumed |
| `backfill_complete` | A backfill run finished with no export failures |
| `backfill_failed` | A backfill run had partial or total export failures (counts in message) |
| `error_spike` | Error threshold exceeded |
| `startup` | Click-Dog process started |
| `shutdown` | Click-Dog process shutting down |

If the `events` list is empty or omitted, all events fire.

### Payload Format

```json
{"text": "[click-dog] Circuit breaker opened after 3 consecutive failures"}
```

This format works directly with:

- **Slack** incoming webhooks
- **Discord** webhooks (via Slack-compatible endpoint)
- **PagerDuty** (via Events API v2 integration)
- Any custom endpoint that accepts JSON POST

### Behavior

- **Non-blocking**: notifications fire in a background goroutine and never delay the main processing loop
- **Nil-safe**: if webhook is not configured, `Notify()` calls are no-ops with zero overhead
- **Fire-and-forget**: HTTP errors are logged but do not affect click-dog operation
- **Filterable**: use the `events` list to receive only the notifications you care about
- **No redirect following**: click-dog treats HTTP 3xx responses as delivery failures instead of following them. Configure the final trusted `http://` or `https://` webhook URL directly.

---

## Combining All Three

A typical production setup enables all three features:

```yaml
log_level: info
log_format: json

metrics:
  enabled: true
  listen_address: ":9090"

webhook:
  enabled: true
  url: ${SLACK_WEBHOOK_URL}
  events:
    - circuit_breaker_opened
    - circuit_breaker_closed
    - startup
    - shutdown
```

This gives you:

1. **JSON logs** for your log aggregator (Loki, Datadog, Splunk)
2. **Prometheus metrics** for dashboards and alerts (scraped by your OTEL Collector, Prometheus, or Datadog Agent)
3. **Slack notifications** for human-actionable events (circuit breaker state changes, startups)

All three are independent — enable any combination, or none.
