// Package selfaudit implements an in-process, observability-only backstop that
// detects the sidecar + use_cluster_queries anti-pattern: a per-node sidecar
// fleet each wrapping its span read in cluster(...) → N× duplicate exports and
// cluster-wide query load.
//
// It never gates readiness, never crashes, and never de-rotates. It only
// surfaces the condition via the click_dog_topology_warning gauge, the /status
// topology_warning field, a throttled WARN log, and a Datadog tile. The Auditor
// only runs when this instance is itself doing cluster reads
// (clickhouse.use_cluster_queries: true) — a hard no-op otherwise (the caller
// in main.go skips construction).
//
// See docs/development/specs/topology-self-audit.md for the full design.
package selfaudit

import (
	"context"
	"math/rand/v2"
	"net"
	"time"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/metrics"
)

// ShouldAudit reports whether the topology self-audit should run for this
// config. It is a hard no-op unless this instance is itself doing cluster reads
// (clickhouse.use_cluster_queries) — the only config that can be the
// anti-pattern — AND the audit is enabled. main.go gates construction on this.
func ShouldAudit(cfg *config.Config) bool {
	return cfg.ClickHouse.UseClusterQueries && cfg.Monitor.TopologyAudit.Enabled
}

// persistWarnInterval bounds how often the WARN log repeats while a detection
// persists: once on transition into detected, then at most once per hour.
const persistWarnInterval = time.Hour

// jitterFraction is the ±fraction applied to each audit tick (and the basis for
// the random initial offset) so a fleet rolled out together does not fan out its
// clusterAllReplicas(system.query_log) scans in lockstep — a correlated
// cluster-wide query burst. It is also what keeps the active window strictly
// shorter than the minimum gap between consecutive ticks (interval ×
// (1-jitterFraction)); see activeWindowFor. See issue #223.
const jitterFraction = 0.1

// clusterReaderProber is the narrow slice of *clickhouse.ClickHouseReader the
// auditor needs: the count trigger plus the query_log sanity probe behind the
// blindness warning. The narrow interface keeps the auditor fakeable without a
// live ClickHouse.
type clusterReaderProber interface {
	// CountDistinctClusterReaders counts distinct hosts whose most recent
	// whole-cluster span read falls inside activeWindow (scanned over the
	// coarse lookback).
	CountDistinctClusterReaders(ctx context.Context, lookback, activeWindow time.Duration) (int, error)
	// HasRecentQueryLogRows reports whether the local query_log has any rows in
	// the lookback window — disambiguates a zero-reader scan from a blind one.
	HasRecentQueryLogRows(ctx context.Context, lookback time.Duration) (bool, error)
}

// warningSink receives the per-reason gauge writes. Satisfied by
// *metrics.Metrics; an interface so tests can capture writes directly.
type warningSink interface {
	SetTopologyWarning(reason string, on bool)
}

// Options configures an Auditor. main.go builds these from config; tests build
// them with fakes.
type Options struct {
	// Host is clickhouse.host — the loopback discriminator that decides the
	// reason label (loopback ⇒ sidecar; non-loopback ⇒ multi-instance).
	Host string
	// Interval is the audit tick period (monitor.topology_audit.interval_s).
	Interval time.Duration
	// Debounce is the number of consecutive positive ticks required before a
	// reason flips to detected (monitor.topology_audit.debounce_count).
	Debounce int
	// Lookback is the coarse query_log scan window
	// (monitor.topology_audit.query_log_lookback_minutes) — how far back the scan
	// reads, NOT the recency trigger (that is the derived active window below).
	Lookback time.Duration
	// CheckInterval is the scheduled span-poll cadence (monitor.check_interval_s).
	// The active window — the recency bound that decides whether a host still
	// counts as a live reader — is derived from it (see activeWindowFor).
	CheckInterval time.Duration
	// Reader runs the query_log trigger — the sole count trigger. Required.
	Reader clusterReaderProber
	// Sink receives the gauge writes. Required.
	Sink warningSink
	// now is an injectable clock for the WARN throttle (defaults to time.Now).
	now func() time.Time
}

// Auditor runs the periodic topology self-audit.
type Auditor struct {
	host         string
	interval     time.Duration
	debounce     int
	lookback     time.Duration
	activeWindow time.Duration

	reader clusterReaderProber
	sink   warningSink

	// counters and detected are per-reason debounce state, keyed by the gauge
	// reason label so each reason converges independently.
	counters map[string]int
	detected map[string]bool

	// now is the injectable clock for the WARN throttle.
	now func() time.Time

	warn *throttle

	// blindnessChecked latches after one successful query_log sanity probe so
	// the blindness caveat is logged (or ruled out) at most once per process.
	blindnessChecked bool
}

// New constructs an Auditor. Reader and Sink must be non-nil.
func New(opts Options) *Auditor {
	now := opts.now
	if now == nil {
		now = time.Now
	}
	return &Auditor{
		host:         opts.Host,
		interval:     opts.Interval,
		debounce:     opts.Debounce,
		lookback:     opts.Lookback,
		activeWindow: activeWindowFor(opts.CheckInterval, opts.Interval),
		reader:       opts.Reader,
		sink:         opts.Sink,
		counters:     make(map[string]int, len(metrics.KnownTopologyReasons)),
		detected:     make(map[string]bool, len(metrics.KnownTopologyReasons)),
		now:          now,
		warn:         newThrottle(persistWarnInterval, now),
	}
}

// activeWindowFor derives the recency window — how fresh a host's most recent
// whole-cluster read must be to still count as a live reader — from the span-poll
// cadence and the audit interval. It must sit in (checkInterval, interval):
//
//   - ABOVE the poll cadence so a genuinely-concurrent reader, which reads every
//     checkInterval, is always inside the window and is still detected. 3× gives
//     slack for jitter and the odd skipped poll.
//   - WELL BELOW the audit interval so a host that read once and stopped ages out
//     before two consecutive audit ticks (≥ interval × (1-jitterFraction) apart)
//     could both count it — which is what would let a transient reader satisfy the
//     debounce and false-positive. Capping at interval/2 guarantees that with room
//     to spare. When the cap and the 3×-cadence floor conflict (a poll slower than
//     half the audit interval), the cap wins: prefer a possible missed detection
//     over a false positive on a healthy cluster.
//
// Non-positive inputs fall back to the 1-minute floor.
func activeWindowFor(checkInterval, interval time.Duration) time.Duration {
	w := 3 * checkInterval
	if w < time.Minute {
		w = time.Minute
	}
	if maxWindow := interval / 2; maxWindow > 0 && w > maxWindow {
		w = maxWindow
	}
	return w
}

// Run drives the audit ticker until ctx is canceled. Mirrors the leader
// election goroutine: launched with `go auditor.Run(ctx)` and stopped via a
// deferred context cancel.
//
// The first tick is offset by a random fraction of the interval and every
// subsequent tick is jittered by ±jitterFraction, so co-started instances don't
// probe system.query_log at the same instants (see jitterFraction).
func (a *Auditor) Run(ctx context.Context) {
	timer := time.NewTimer(a.initialDelay())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			a.evaluate(ctx)
			timer.Reset(a.jittered(a.interval))
		}
	}
}

// initialDelay returns a random offset in [0, interval) so co-started instances
// decorrelate their first probe. Falls back to the raw interval for a
// non-positive interval (config validation forbids that in production).
func (a *Auditor) initialDelay() time.Duration {
	if a.interval <= 0 {
		return a.interval
	}
	return time.Duration(rand.Int64N(int64(a.interval)))
}

// jittered applies ±jitterFraction to d (a no-op for a non-positive d).
func (a *Auditor) jittered(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	frac := (rand.Float64()*2 - 1) * jitterFraction
	return d + time.Duration(float64(d)*frac)
}

// evaluate runs one audit tick: probe the trigger, advance/clear the per-reason
// debounce, then write the full known-reason gauge set.
func (a *Auditor) evaluate(ctx context.Context) {
	reason := a.reasonForHost()
	indicated, probeWorked := a.probe(ctx, reason)

	// Fail-open: if the probe errored, the tick is a full no-op — it neither
	// advances a reason toward detection nor clears one, and the gauge is left
	// unchanged. An errored observation is not a clean tick, so a query_log
	// outage can neither raise a false warning nor clear a real one.
	if !probeWorked {
		clicklog.Debug("Topology audit: probe errored this tick — no-op (gauge unchanged)")
		return
	}

	// Per-reason debounce/convergence over the full known set: the active reason
	// advances when indicated; every other known reason converges toward clear.
	// The probe's active-window recency filter (CountDistinctClusterReaders) is
	// what keeps startup-era and post-failover departed readers out of the count,
	// so no separate warm-up gate is needed here — see issue #223.
	for _, kr := range metrics.KnownTopologyReasons {
		a.step(kr, indicated[kr])
	}

	// Write the full known-reason set every tick (active → 1, others → 0) so a
	// stale reason can never hold the Datadog tile red after the active reason
	// changes, and so an externally-perturbed gauge re-converges.
	for _, kr := range metrics.KnownTopologyReasons {
		a.sink.SetTopologyWarning(kr, a.detected[kr])
	}
}

// probe runs the count trigger and returns which reasons are indicated this tick
// plus whether the probe succeeded. A probe that errors contributes nothing — it
// is logged at DEBUG and skipped (fail-open), so probeWorked stays false and
// evaluate() leaves the gauge untouched. The trigger requires >1 (duplication
// needs ≥2 readers); a lone instance never trips.
//
// The query_log distinct-host scan is the sole count trigger. The Keeper
// sibling-count trigger from the original design was dropped: post-leader-gating
// it counts election *sharers* (the recommended 2–3 instance HA topology, where
// only the leader exports) rather than the backstop target of cluster instances
// *not* sharing an election — see docs/development/specs/topology-self-audit.md.
func (a *Auditor) probe(ctx context.Context, reason string) (indicated map[string]bool, probeWorked bool) {
	indicated = make(map[string]bool, len(metrics.KnownTopologyReasons))

	n, err := a.reader.CountDistinctClusterReaders(ctx, a.lookback, a.activeWindow)
	if err != nil {
		clicklog.Debug("Topology audit: query_log scan failed (fail-open, skipped this tick): %v", err)
		return indicated, false
	}
	if n == 0 {
		a.checkQueryLogBlindness(ctx)
	}
	if n > 1 {
		indicated[reason] = true
	}
	return indicated, true
}

// checkQueryLogBlindness warns once per process when the scan's data source is
// empty: a successful zero-reader probe is ambiguous between "no duplicate
// readers" and "query_log disabled or outside retention", and only the former
// is safe to stay silent about (the spec's logged-once caveat — #238). Runs
// only on a zero-count tick, so a healthy fleet — where the leader's own
// cluster reads keep the count ≥1 — never pays for the extra local query. A
// failed sanity probe — including ctx canceled between the count and this
// call — does not set the latch and retries on the next zero-count tick; a
// *missing* system.query_log table surfaces as count-probe errors (fail-open,
// DEBUG), never here.
func (a *Auditor) checkQueryLogBlindness(ctx context.Context) {
	if a.blindnessChecked {
		return
	}
	hasRows, err := a.reader.HasRecentQueryLogRows(ctx, a.lookback)
	if err != nil {
		clicklog.Debug("Topology audit: query_log sanity probe failed (retried next zero-count tick): %v", err)
		return
	}
	a.blindnessChecked = true
	if !hasRows {
		clicklog.Warn("Topology audit: system.query_log has no rows in the last %v — query logging appears disabled or outside retention, so duplicate cluster-wide reads cannot be detected. This is observability-only — readiness is unaffected.", a.lookback)
	}
}

// step advances or clears one reason's debounce. Called only on ticks where at
// least one probe worked, so a "not indicated" here is a genuine clean
// observation and clears immediately.
func (a *Auditor) step(reason string, indicated bool) {
	if indicated {
		if a.counters[reason] < a.debounce {
			a.counters[reason]++
		}
		switch {
		case a.counters[reason] >= a.debounce && !a.detected[reason]:
			a.detected[reason] = true
			a.warnDetected(reason)
		case a.detected[reason]:
			a.warnPersist(reason)
		}
		return
	}
	// Clean tick: reset the counter and, if we were detected, clear and log once.
	a.counters[reason] = 0
	if a.detected[reason] {
		a.detected[reason] = false
		a.warn.reset(reason)
		clicklog.Info("Topology audit: %s cleared — topology looks correct again", reason)
	}
}

// reasonForHost decides the gauge reason label from this instance's
// clickhouse.host: a loopback host means co-located sidecars each going
// cluster-wide; a non-loopback host means separate centralized readers not
// sharing an election.
func (a *Auditor) reasonForHost() string {
	if isLoopbackHost(a.host) {
		return metrics.TopologyReasonSidecar
	}
	return metrics.TopologyReasonMultiInstance
}

// isLoopbackHost reports whether host is the loopback interface — a literal
// "localhost" or an IP that parses as loopback (127.0.0.0/8, ::1).
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func (a *Auditor) warnDetected(reason string) {
	// The transition WARN always fires. allow()'s return is intentionally
	// discarded: we call it only to seed/reset the throttle window so the next
	// warnPersist is at least persistWarnInterval away. (After a clear, step()
	// resets the throttle, so this first allow() always succeeds anyway.)
	a.warn.allow(reason)
	clicklog.Warn("Topology audit DETECTED (%s): more than one instance is running whole-cluster reads (use_cluster_queries) → duplicate exports. %s "+
		"This is observability-only — readiness is unaffected.", reason, remediation(reason))
}

func (a *Auditor) warnPersist(reason string) {
	if a.warn.allow(reason) {
		clicklog.Warn("Topology audit still detected (%s): duplicate cluster-wide reads persist. %s", reason, remediation(reason))
	}
}

// remediation returns a one-line fix hint for the reason.
func remediation(reason string) string {
	switch reason {
	case metrics.TopologyReasonSidecar:
		return "For per-node reads, set clickhouse.use_cluster_queries: false on the sidecars; for whole-cluster reads, run a single cluster-mode instance (or 2–3 sharing a Keeper election)."
	default:
		return "Run a single cluster-mode reader (or 2–3 sharing one Keeper election); separate readers not sharing an election each go cluster-wide. A sustained Keeper outage (instances fail open and export by design) or a long-running backfill from another host also produces this state and clears on its own once resolved."
	}
}
