# Resilience

Click-Dog includes built-in resilience features to protect both ClickHouse and itself during failure scenarios. These are especially important in production where transient failures, network issues, or ClickHouse overload can occur.

## Health Checks

Before each polling cycle, click-dog performs a non-blocking health check on the ClickHouse connection:

```
[WARN] ClickHouse connection health check failed: ...
```

Health checks use a 5-second timeout and prevent stale connections from triggering the circuit breaker. If the health check fails, the cycle is skipped and an error is recorded for backoff purposes.

---

## Circuit Breaker

The circuit breaker pattern prevents click-dog from overwhelming a struggling ClickHouse instance with repeated failed queries.

### Configuration

```yaml
monitor:
  circuit_breaker:
    enabled: true
    failure_threshold: 3     # Open after 3 consecutive failures
    success_threshold: 1     # Close after 1 success in half-open
    reset_timeout_s: 60      # Try again after 60 seconds
```

### States

```
              failure >= threshold
  CLOSED  ──────────────────────────>  OPEN
    ^                                    │
    │                                    │ reset_timeout elapsed
    │                                    v
    │        success >= threshold     HALF-OPEN
    └────────────────────────────────────┘
                                         │
              failure in half-open       │
              ─────────────────────> OPEN ┘
```

**Closed** (normal operation): All requests pass through. Consecutive failures are tracked.

**Open** (blocking): All requests are blocked. After `reset_timeout_s` seconds, transitions to half-open.

**Half-Open** (testing): Allows requests through. If the first `success_threshold` requests succeed, the circuit closes. Any failure immediately re-opens the circuit.

### Log Output

```
[WARN] Circuit breaker opened after 3 consecutive failures
[INFO] Circuit breaker transitioning to half-open state
[INFO] Circuit breaker closed after successful recovery
[WARN] Circuit breaker re-opened after failure in half-open state
```

### When to Enable

Enable the circuit breaker when:
- Running against a shared ClickHouse cluster where your queries could impact other workloads
- ClickHouse is prone to transient failures (network issues, resource contention)
- You want to fail fast rather than queue up timeout errors

---

## Adaptive Backoff

Adaptive backoff increases the polling interval when errors occur, giving ClickHouse time to recover. The interval resets to normal after sustained success.

### Configuration

```yaml
monitor:
  backoff:
    enabled: true
    max_interval_s: 300      # Cap at 5 minutes
    backoff_factor: 2.0      # Double interval on each failure
```

### Behavior

With a base `check_interval_s` of 30 and default backoff settings:

| Consecutive Failures | Polling Interval |
|---------------------|-----------------|
| 0 | 30s (base) |
| 1 | 60s |
| 2 | 120s |
| 3 | 240s |
| 4+ | 300s (max) |

On success, if the interval has been backed off, it immediately resets to the base interval.

### Log Output

```
[WARN] Backoff increased to 1m0s after 1 consecutive failures
[WARN] Backoff increased to 2m0s after 2 consecutive failures
[INFO] Backoff reset to base interval: 30s
```

### When to Enable

Enable adaptive backoff when:
- ClickHouse may experience periods of overload
- You want to reduce monitoring load during incidents
- Your polling interval is aggressive (< 30 seconds)

---

## Using Both Together

Circuit breaker and adaptive backoff complement each other:

- **Circuit breaker** provides fast failure detection and hard stops
- **Adaptive backoff** provides gradual recovery and load reduction

```yaml
monitor:
  check_interval_s: 15
  circuit_breaker:
    enabled: true
    failure_threshold: 3
    reset_timeout_s: 60
  backoff:
    enabled: true
    max_interval_s: 300
    backoff_factor: 2.0
```

**Failure scenario timeline:**

1. Failure 1 — backoff increases interval to 30s
2. Failure 2 — backoff increases interval to 60s
3. Failure 3 — circuit breaker opens, all requests blocked for 60s. Backoff continues to 120s
4. After 60s — circuit transitions to half-open, allows one request
5. If success — circuit closes, backoff resets to 15s base interval
6. If failure — circuit re-opens for another 60s

---

## Connection Pooling

Connection pool settings also contribute to resilience:

```yaml
clickhouse:
  max_open_conns: 2      # Limit concurrent connections
  max_idle_conns: 1      # Limit idle connections
  query_timeout_s: 30    # Prevent runaway queries
```

- **`max_open_conns`**: Prevents overwhelming ClickHouse with too many concurrent connections. Default of 2 is conservative and suitable for most setups.
- **`max_idle_conns`**: Keeps a small pool of connections ready. Set lower than `max_open_conns`.
- **`query_timeout_s`**: Kills queries that take too long, preventing connection exhaustion from slow queries.

---

## Rate Limiting

Additional rate-limiting settings protect both ClickHouse and your OTEL collector:

```yaml
monitor:
  max_spans_per_cycle: 1000    # Cap spans per polling cycle
  batch_size: 100              # Process in smaller chunks
  batch_delay_ms: 200          # Add delay between chunks
  export_timeout_s: 30         # Fail stuck exporter calls into retry/backoff (0 disables)
```

If an export call times out, Click-Dog treats that batch as failed and retries it in full on the next cycle through the lookback overlap. Any spans the collector accepted before the timeout may appear again; downstream collectors should deduplicate by trace/span identity.

See [Filtering](filtering.md) for duration-based and content-based filtering options.

---

## Heartbeat Logging

Click-Dog emits periodic heartbeat summaries instead of logging every cycle. This reduces log noise while confirming the daemon is alive. The interval is fixed at 5 minutes and is not configurable.

```
[INFO] Heartbeat: cycles=120, exported=4500, filtered=300, duplicates=7, errors=2, cb=closed, interval=10s, uptime=25m0s (last 5m0s)
```

The heartbeat reports cumulative counters since the last summary plus current gauge values, so log-based monitors (Datadog Logs, CloudWatch Logs Insights, etc.) can extract metrics without scraping `/metrics`:

- **cycles** — polling cycles completed (window)
- **exported** — total spans exported (window)
- **filtered** — total spans filtered out (window)
- **duplicates** — spans skipped by the in-memory dedup cache (window)
- **errors** — cycles that encountered errors (window)
- **cb** — circuit breaker state (`closed` / `half_open` / `open`)
- **interval** — live polling interval (same value as the `click_dog_backoff_interval_seconds` gauge); equals `check_interval` when the breaker is closed and the adaptive poller has not backed off
- **uptime** — process uptime since start

Counters reset after each heartbeat log; gauges reflect the live value at emission time. If you don't see heartbeat lines, the process may have stopped.

---

## Recommended Production Settings

```yaml
monitor:
  check_interval_s: 30
  max_spans_per_cycle: 500
  batch_size: 50
  batch_delay_ms: 100
  dedup_cache_size: 20000

  circuit_breaker:
    enabled: true
    failure_threshold: 5
    success_threshold: 2
    reset_timeout_s: 120

  backoff:
    enabled: true
    max_interval_s: 300
    backoff_factor: 2.0

clickhouse:
  max_open_conns: 3
  max_idle_conns: 1
  query_timeout_s: 45
```
