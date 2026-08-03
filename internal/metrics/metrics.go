package metrics

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/model"
)

// Metrics collects operational counters and gauges for click-dog.
// Exposed as Prometheus text format on the /metrics HTTP endpoint.
// No external dependency — we emit the text exposition format directly.
type Metrics struct {
	mu sync.RWMutex

	// Counters (monotonically increasing)
	spansExported int64
	spansFiltered int64
	spansDupes    int64

	exportSinks map[string]exportSinkCounters

	// Per-outcome cycle counters, exposed as cycle_results_total{result}.
	// Mutually exclusive: every cycle increments exactly one of these, so their
	// sum is the total cycle count. "skipped" covers cycles that touched no
	// ClickHouse work (breaker open without canary, canary sentinel resolved to
	// non-error). Kept as a fixed three-value enum to keep the result label
	// low-cardinality.
	cyclesSuccess int64
	cyclesError   int64
	cyclesSkipped int64

	// Gauges (current state)
	circuitBreakerState string  // "closed", "open", "half_open"
	backoffIntervalSecs float64 // current polling interval in seconds
	lastSuccessUnix     int64   // unix timestamp of last successful cycle
	leader              int     // 1 = active exporter/leader, 0 = standby
	upSince             time.Time

	// haveLastCycle suppresses the /status last_cycle JSON field before the
	// first cycle (Prometheus gauges instead emit 0 unconditionally for a
	// stable metric set).
	lastCycle     CycleSnapshot
	haveLastCycle bool

	// ClickHouse data-plane health signals (issue #183). All derived from
	// data already flowing through the live cycle — no extra ClickHouse
	// queries are issued. These let an operator tell "click-dog is down"
	// (metric absent) apart from "ClickHouse is not emitting useful
	// span/query data" (metric present but stale or zero).

	// spanLogLastPollUnix is the wall-clock time of the most recent
	// span-log fetch that completed without error, regardless of how many
	// rows came back. A zero-row poll still counts — that's how we
	// surface "ClickHouse is reachable but emitting nothing". Stays at 0
	// until the first successful poll so dashboards can detect "never
	// polled" via the standard 0-timestamp idiom.
	spanLogLastPollUnix int64

	// spanLogNewestRowTimeUnix is the unix-seconds timestamp of the
	// newest span seen in the most recent non-empty span-log fetch,
	// derived from finish_time_us (microsecond precision) — NOT from
	// finish_date, which is the ClickHouse Date partition truncated to
	// midnight and would inflate every span's apparent age by up to 24h
	// (see processor.recordSpanLogObservation). Empty cycles do NOT
	// reset this, which is the whole point: age_seconds keeps growing
	// while no new spans arrive, exposing staleness as an unbounded
	// gauge.
	spanLogNewestRowTimeUnix int64

	// spanLogRowsLastCycle is the row count from the most recent span-log
	// fetch, BEFORE filter/dedup. Pair with newest_row_age to distinguish
	// "no rows because nothing matched the duration filter" from "no rows
	// because ClickHouse stopped emitting".
	spanLogRowsLastCycle int64

	// queryLogEnrichAttempts / Successes / Failures: cycle-level counters
	// (not per-query) gated by ShouldEnrichFromQueryLog AND len(queryIDs)>0,
	// so non-enrichment configs cost nothing. A "failure" is what already
	// bumps enrichFailCount in processor/live.go — see that file.
	queryLogEnrichAttempts  int64
	queryLogEnrichSuccesses int64
	queryLogEnrichFailures  int64

	// queryLogEnrichMatchRatio and spansWithQueryIDRatio are last-cycle
	// gauges in [0.0, 1.0]. We emit them as 0 before the first observation
	// (HELP text documents what 0 means); empty/skipped cycles preserve
	// the previous value rather than zero it out so a single empty poll
	// doesn't make the dashboard look catastrophic.
	queryLogEnrichMatchRatio float64
	spansWithQueryIDRatio    float64

	// normalizedQuerySupported reflects detectQueryLogNormalizedSupport's
	// startup probe (see internal/clickhouse/reader.go). Set once via
	// SetNormalizedQuerySupported during processor init and never changes
	// at runtime — re-probing would cost a per-cycle SELECT that buys us
	// nothing.
	normalizedQuerySupported int

	// queryOperationSupported mirrors the query_kind capability probe that
	// controls query_log.operation and query_log.access_type enrichment. Like
	// normalizedQuerySupported, it is set once during startup.
	queryOperationSupported int

	// topologyWarnings holds the click_dog_topology_warning{reason} gauge
	// value (0/1) per reason for the topology self-audit (the sidecar +
	// use_cluster_queries anti-pattern detector). Both known reasons are
	// registered at 0 in NewMetrics so the series is stable — never absent,
	// even when the auditor is skipped (use_cluster_queries off). A
	// deterministic green 0 is distinguishable from no-data (a scrape
	// failure), the exact ambiguity the dashboard note warns about.
	topologyWarnings map[string]bool
}

// Topology self-audit reason label values for click_dog_topology_warning.
// Exported so internal/selfaudit writes the same canonical set the gauge is
// initialized with. KnownTopologyReasons is the stable-0 series (alphabetical
// so the Prometheus output is deterministic).
const (
	TopologyReasonMultiInstance = "multi_instance_cluster_queries"
	TopologyReasonSidecar       = "sidecar_cluster_queries"
)

// KnownTopologyReasons is the full set of reason labels the gauge always emits.
var KnownTopologyReasons = []string{TopologyReasonMultiInstance, TopologyReasonSidecar}

type exportSinkCounters struct {
	sent     int64
	accepted int64
	errors   int64
}

// NormalizeExportSinkName returns the label used for a sink in metrics/logs.
func NormalizeExportSinkName(name string) string {
	if name == "" {
		return "unknown"
	}
	return name
}

// cycleResult is the low-cardinality outcome label used by
// click_dog_cycle_results_total. The constants exist to keep the
// wire-format label values out of fmt.Fprintf magic strings.
type cycleResult string

const (
	cycleResultSuccess cycleResult = "success"
	cycleResultError   cycleResult = "error"
	cycleResultSkipped cycleResult = "skipped"
)

const (
	SkipReasonCircuitOpen   = model.CycleSkipReasonCircuitOpen
	SkipReasonLeaderStandby = model.CycleSkipReasonLeaderStandby
)

// CycleSnapshot captures the results of the most recent processing cycle.
// Exposed via Metrics.LastCycle() for the /status health endpoint.
//
// Skipped distinguishes "cycle skipped without touching ClickHouse" (e.g.
// circuit breaker open, no canary configured) from "cycle ran successfully
// and exported 0 spans". Both record Err="" and counts=0; without Skipped
// a /status consumer can't tell them apart, and that ambiguity would let
// clickhouse_healthy go green based on a cycle that never made a query.
//
// IMPORTANT: This struct has a mirror in internal/health and an adapter
// in the top-level health_adapter.go. Adding a field here requires
// updating both to avoid losing the field at the package boundary.
type CycleSnapshot struct {
	Exported   int
	Filtered   int
	Duplicates int
	DurationMs int64
	Err        string // empty when the cycle succeeded OR was skipped
	Skipped    bool   // true when the cycle recorded no ClickHouse interaction
	SkipReason string // populated for meaningful skipped-cycle subtypes
	At         time.Time
}

// NewMetrics creates a new Metrics instance.
func NewMetrics() *Metrics {
	// Register both known topology reasons at 0 so the gauge renders a stable
	// series from startup — the stable-0 must not depend on a SetTopologyWarning
	// call, which never comes when the auditor is skipped.
	topo := make(map[string]bool, len(KnownTopologyReasons))
	for _, r := range KnownTopologyReasons {
		topo[r] = false
	}
	return &Metrics{
		circuitBreakerState: "closed",
		exportSinks:         make(map[string]exportSinkCounters),
		leader:              1,
		upSince:             time.Now(),
		topologyWarnings:    topo,
	}
}

// RecordCycle records one processing cycle's results.
// durationMs may be 0 for cycles that did no work; use RecordSkippedCycle
// for cycles that didn't touch ClickHouse at all so /status can distinguish
// an empty-but-real cycle from a skip.
func (m *Metrics) RecordCycle(exported, filtered, dupes int, durationMs int64, err error) {
	m.recordCycle(exported, filtered, dupes, durationMs, err, false, "")
}

// RecordSkippedCycle records a cycle that was skipped without issuing any
// ClickHouse queries (e.g. circuit breaker open with no canary). Counts and
// error are zero/empty, and the snapshot is flagged Skipped=true so /status
// can render it as a skip rather than a successful zero-work cycle.
//
// A skip increments cycle_results_total{result="skipped"} by design: it keeps
// the summed cycle rate stable during breaker-open periods, so operators
// correlating scrape rate with throughput don't see false dips.
// lastSuccessUnix is NOT advanced — a skip is not evidence of health.
func (m *Metrics) RecordSkippedCycle(durationMs int64) {
	m.RecordSkippedCycleWithReason(durationMs, "")
}

// RecordSkippedCycleWithReason records a skipped cycle and preserves the reason
// for consumers that need to distinguish a healthy standby from a breaker-open
// no-op.
func (m *Metrics) RecordSkippedCycleWithReason(durationMs int64, reason string) {
	m.recordCycle(0, 0, 0, durationMs, nil, true, reason)
}

// RecordExportResult records per-sink export counters from an ExportResult.
// It intentionally does not update the cycle counters; callers still report
// live-mode dedup progress through RecordCycle using result.Accepted.
func (m *Metrics) RecordExportResult(result model.ExportResult) {
	if len(result.Sinks) == 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.exportSinks == nil {
		m.exportSinks = make(map[string]exportSinkCounters)
	}
	for _, sink := range result.Sinks {
		name := NormalizeExportSinkName(sink.Name)
		c := m.exportSinks[name]
		c.sent += int64(sink.Sent)
		c.accepted += int64(sink.Accepted)
		if sink.Error != nil {
			c.errors++
		}
		m.exportSinks[name] = c
	}
}

func (m *Metrics) recordCycle(exported, filtered, dupes int, durationMs int64, err error, skipped bool, skipReason string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.spansExported += int64(exported)
	m.spansFiltered += int64(filtered)
	m.spansDupes += int64(dupes)

	now := time.Now()
	switch {
	case err != nil:
		m.cyclesError++
	case skipped:
		m.cyclesSkipped++
	default:
		// Only treat a real cycle as a success timestamp update. A skipped
		// cycle touched nothing so it shouldn't advance last_success.
		m.lastSuccessUnix = now.Unix()
		m.cyclesSuccess++
	}

	errStr := ""
	if err != nil {
		errStr = err.Error()
	}
	m.lastCycle = CycleSnapshot{
		Exported:   exported,
		Filtered:   filtered,
		Duplicates: dupes,
		DurationMs: durationMs,
		Err:        errStr,
		Skipped:    skipped,
		SkipReason: skipReason,
		At:         now,
	}
	m.haveLastCycle = true
}

// Snapshot is a consistent view of the gauge/last-cycle state under a
// single RLock. /status reads every field from one Snapshot call rather
// than racing against a cycle that updates several fields in sequence.
//
// IMPORTANT: Mirrored in internal/health.Snapshot; adding a field here
// requires updating both plus the adapter in health_adapter.go.
type Snapshot struct {
	LastCycle           CycleSnapshot
	HaveLastCycle       bool
	CircuitBreakerState string
	BackoffIntervalSecs float64
	Leader              int
	UpSince             time.Time
	// TopologyWarning is the active topology-audit reason (empty when clean).
	// Surfaced at /status as topology_warning; never affects /readyz.
	TopologyWarning string
}

// Snapshot returns an atomic snapshot of the fields read by /status and
// /readyz. Takes the RLock once so the returned values are mutually
// consistent even if a cycle updates the underlying state concurrently.
func (m *Metrics) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return Snapshot{
		LastCycle:           m.lastCycle,
		HaveLastCycle:       m.haveLastCycle,
		CircuitBreakerState: m.circuitBreakerState,
		BackoffIntervalSecs: m.backoffIntervalSecs,
		Leader:              m.leader,
		UpSince:             m.upSince,
		TopologyWarning:     m.activeTopologyReasonLocked(),
	}
}

// SetCircuitBreakerState updates the circuit breaker gauge. The value is
// normalized to the canonical form ("closed" / "open" / "half_open") so
// anything reading back via CircuitBreakerState() gets a consistent string.
// resilience.CircuitState.String returns "half-open" with a hyphen, but
// the external API (JSON, Prometheus label values) uses "half_open" with
// an underscore; canonicalising here keeps that invariant at the write
// side rather than every read site.
func (m *Metrics) SetCircuitBreakerState(state string) {
	if state == "half-open" {
		state = "half_open"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.circuitBreakerState = state
}

// SetTopologyWarning sets the click_dog_topology_warning gauge for one reason.
// The auditor writes the full known-reason set every tick (the active reason
// on=true, the others on=false) so a stale reason can never hold the Datadog
// tile red after the active reason changes. Only the known reasons render: an
// unknown reason is accepted but invisible — render() and the /status resolution
// iterate KnownTopologyReasons — so adding a reason means extending that set
// (and the dashboard note).
func (m *Metrics) SetTopologyWarning(reason string, on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.topologyWarnings[reason] = on
}

// SetLeader updates whether this instance is currently the active exporter
// (1) or a leader-gated standby (0). Non-cluster deployments default to active.
func (m *Metrics) SetLeader(active bool) {
	v := 0
	if active {
		v = 1
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.leader = v
}

// activeTopologyReasonLocked returns the reason currently warning (gauge=1), or
// "" when the topology is clean. Caller must hold at least the RLock. Reasons
// are mutually exclusive per instance (host decides exactly one), but if more
// than one were ever on, the alphabetically-first known reason wins for a
// deterministic /status value.
func (m *Metrics) activeTopologyReasonLocked() string {
	for _, r := range KnownTopologyReasons {
		if m.topologyWarnings[r] {
			return r
		}
	}
	return ""
}

// SetBackoffInterval updates the current polling interval gauge.
func (m *Metrics) SetBackoffInterval(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.backoffIntervalSecs = d.Seconds()
}

// RecordSpanLogPoll captures the outcome of a successful span-log fetch
// (#183). It is the single entry point that updates every span-log
// freshness gauge so the caller can't accidentally desync them.
//
//   - rowCount is the raw number of rows the ClickHouse query returned,
//     BEFORE click-dog's in-process filter/dedup. We want the operator
//     to see "the table is empty" cleanly, even if all rows would have
//     been filtered out.
//   - newestRow is the wall-clock timestamp of the freshest row in
//     the response (derived from finish_time_us, not the date-truncated
//     finish_date — see processor.recordSpanLogObservation). Pass
//     time.Time{} (zero value) for an empty result; we then leave the
//     newest-row-time gauge alone so age_seconds keeps growing. That
//     preservation is the staleness signal; do not "reset to now" on
//     empty fetches.
//
// Only call this when the fetch itself succeeded. A failed fetch is an
// error cycle (RecordCycle with err) and tells the operator nothing
// about span_log freshness — the previous gauges remain authoritative.
func (m *Metrics) RecordSpanLogPoll(rowCount int, newestRow time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spanLogLastPollUnix = time.Now().Unix()
	m.spanLogRowsLastCycle = int64(rowCount)
	if !newestRow.IsZero() {
		m.spanLogNewestRowTimeUnix = newestRow.Unix()
	}
}

// RecordQueryLogEnrichmentCycle accounts for one cycle's worth of
// query_log enrichment work. Always increments the attempt counter; on
// success also increments successes and refreshes the per-cycle match
// ratio, on failure increments failures (the per-span enrichFailCount
// in processor/live.go still drives log-spam suppression; this metric
// is the dashboard-facing duplicate of that signal).
//
// total is the number of distinct query_ids handed to
// FetchQueryLogByQueryIDs; matched is len(returned queryLogMap). When
// total is 0 the function is a no-op — there was no work to attempt,
// and bumping attempts here would inflate the failure rate during
// healthy-but-quiet windows.
func (m *Metrics) RecordQueryLogEnrichmentCycle(matched, total int, err error) {
	if total <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queryLogEnrichAttempts++
	if err != nil {
		m.queryLogEnrichFailures++
		// Match ratio is intentionally NOT updated on failure: a failed
		// fetch tells us nothing about how well future joins would match.
		return
	}
	m.queryLogEnrichSuccesses++
	m.queryLogEnrichMatchRatio = float64(matched) / float64(total)
}

// RecordSpansWithQueryIDRatio sets the per-cycle gauge tracking what
// fraction of fetched spans carry clickhouse.query_id. Counted on the
// raw span set (pre-filter, pre-dedup) so the gauge reflects what
// ClickHouse is producing, not what click-dog chooses to export.
//
// withQID is the number of spans whose Attributes contain a non-empty
// "clickhouse.query_id"; total is the full fetched count. total<=0 is
// a no-op so a single empty cycle doesn't zero out the gauge — that
// would falsely alarm dashboards that watch this ratio.
func (m *Metrics) RecordSpansWithQueryIDRatio(withQID, total int) {
	if total <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spansWithQueryIDRatio = float64(withQID) / float64(total)
}

// SetNormalizedQuerySupported records whether
// system.query_log.normalized_query_hash is available, as detected by
// detectQueryLogNormalizedSupport at reader init. Exposed as a 0/1
// gauge so a dashboard can correlate "normalized query attributes
// missing" with the explicit capability state rather than guessing.
//
// Called once at startup; not re-probed per cycle (the support flag
// only changes across ClickHouse upgrades or cluster-mode toggles,
// neither of which happen mid-process).
func (m *Metrics) SetNormalizedQuerySupported(supported bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if supported {
		m.normalizedQuerySupported = 1
	} else {
		m.normalizedQuerySupported = 0
	}
}

// SetQueryOperationSupported records whether system.query_log.query_kind is
// available for query operation and access-type enrichment. It is a stable
// process-wide 0/1 gauge set once from the reader's startup capability probe.
func (m *Metrics) SetQueryOperationSupported(supported bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if supported {
		m.queryOperationSupported = 1
	} else {
		m.queryOperationSupported = 0
	}
}

// Handler returns an http.HandlerFunc that writes Prometheus text exposition format.
func (m *Metrics) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write(m.render())
	}
}

// render serializes the current metric values into Prometheus text exposition
// format. The read lock is held only for the in-memory buffer build, never
// across the network write to the scraper — otherwise a slow or stalled scrape
// would hold the lock for up to the server's WriteTimeout and block every cycle
// that records metrics.
func (m *Metrics) render() []byte {
	var b bytes.Buffer

	m.mu.RLock()
	defer m.mu.RUnlock()

	// Counters
	name := writePromMetricHeader(&b, MetricSpansExported)
	_, _ = fmt.Fprintf(&b, "%s %d\n", name, m.spansExported)

	name = writePromMetricHeader(&b, MetricSpansFiltered)
	_, _ = fmt.Fprintf(&b, "%s %d\n", name, m.spansFiltered)

	name = writePromMetricHeader(&b, MetricSpansDuplicates)
	_, _ = fmt.Fprintf(&b, "%s %d\n", name, m.spansDupes)

	name = writePromMetricHeader(&b, MetricExportAttempts)
	for _, sink := range sortedExportSinkNames(m.exportSinks) {
		_, _ = fmt.Fprintf(&b, "%s{sink=\"%s\"} %d\n", name, promLabelValue(sink), m.exportSinks[sink].sent)
	}

	name = writePromMetricHeader(&b, MetricExportAccepted)
	for _, sink := range sortedExportSinkNames(m.exportSinks) {
		_, _ = fmt.Fprintf(&b, "%s{sink=\"%s\"} %d\n", name, promLabelValue(sink), m.exportSinks[sink].accepted)
	}

	name = writePromMetricHeader(&b, MetricExportErrors)
	for _, sink := range sortedExportSinkNames(m.exportSinks) {
		_, _ = fmt.Fprintf(&b, "%s{sink=\"%s\"} %d\n", name, promLabelValue(sink), m.exportSinks[sink].errors)
	}

	// Per-outcome cycle counter. The result label is a fixed three-value
	// enum so cardinality stays bounded regardless of cycle volume.
	name = writePromMetricHeader(&b, MetricCycleResults)
	_, _ = fmt.Fprintf(&b, "%s{result=\"%s\"} %d\n", name, cycleResultSuccess, m.cyclesSuccess)
	_, _ = fmt.Fprintf(&b, "%s{result=\"%s\"} %d\n", name, cycleResultError, m.cyclesError)
	_, _ = fmt.Fprintf(&b, "%s{result=\"%s\"} %d\n", name, cycleResultSkipped, m.cyclesSkipped)

	// Gauges
	name = writePromMetricHeader(&b, MetricCircuitBreakerState)
	_, _ = fmt.Fprintf(&b, "%s %d\n", name, circuitBreakerStateValue(m.circuitBreakerState))

	name = writePromMetricHeader(&b, MetricLeader)
	_, _ = fmt.Fprintf(&b, "%s %d\n", name, m.leader)

	// Topology self-audit. Both known reasons always emit (stable-0 series),
	// so the safe topology renders a deterministic 0 rather than no-data.
	name = writePromMetricHeader(&b, MetricTopologyWarning)
	for _, reason := range KnownTopologyReasons {
		v := 0
		if m.topologyWarnings[reason] {
			v = 1
		}
		_, _ = fmt.Fprintf(&b, "%s{reason=\"%s\"} %d\n", name, promLabelValue(reason), v)
	}

	name = writePromMetricHeader(&b, MetricBackoffIntervalSeconds)
	_, _ = fmt.Fprintf(&b, "%s %.1f\n", name, m.backoffIntervalSecs)

	name = writePromMetricHeader(&b, MetricLastSuccessTimestampSeconds)
	_, _ = fmt.Fprintf(&b, "%s %d\n", name, m.lastSuccessUnix)
	role := "active"
	if m.leader == 0 {
		role = "standby"
	}
	_, _ = fmt.Fprintf(&b, "%s{role=\"%s\"} %d\n", name, role, m.lastSuccessUnix)

	name = writePromMetricHeader(&b, MetricUptimeSeconds)
	_, _ = fmt.Fprintf(&b, "%s %.0f\n", name, time.Since(m.upSince).Seconds())

	// Last-cycle gauges. Always emitted (zero before the first cycle) so
	// the metric set is stable across scrapes — operators don't have to
	// special-case "metric appears after first cycle" in dashboards.
	// Counts here are the most recent cycle's values, not cumulative;
	// pair with cycle_results_total for rate-style questions.
	lastDurSecs := float64(m.lastCycle.DurationMs) / 1000.0
	name = writePromMetricHeader(&b, MetricLastCycleDurationSeconds)
	_, _ = fmt.Fprintf(&b, "%s %.3f\n", name, lastDurSecs)

	name = writePromMetricHeader(&b, MetricLastCycleExportedSpans)
	_, _ = fmt.Fprintf(&b, "%s %d\n", name, m.lastCycle.Exported)

	name = writePromMetricHeader(&b, MetricLastCycleFilteredSpans)
	_, _ = fmt.Fprintf(&b, "%s %d\n", name, m.lastCycle.Filtered)

	name = writePromMetricHeader(&b, MetricLastCycleDuplicateSpans)
	_, _ = fmt.Fprintf(&b, "%s %d\n", name, m.lastCycle.Duplicates)

	// ----------------------------------------------------------------------
	// ClickHouse data-plane health (#183). These signals exist so an
	// operator can distinguish "click-dog is down" (entire metric family
	// absent from /metrics) from "ClickHouse is not emitting useful data"
	// (metric present, but newest-row age is climbing or query_id ratio
	// is zero). All values are derived from data the cycle already fetched.
	// ----------------------------------------------------------------------

	name = writePromMetricHeader(&b, MetricSpanLogLastPollTimestamp)
	_, _ = fmt.Fprintf(&b, "%s %d\n", name, m.spanLogLastPollUnix)

	// Age is computed at scrape time so the gauge keeps growing during
	// empty cycles (which is the whole staleness signal). Before the
	// first observation we emit 0 — HELP text calls that out so an alert
	// rule can treat 0 distinctly from "newest row literally just now".
	var newestRowAge float64
	if m.spanLogNewestRowTimeUnix > 0 {
		newestRowAge = float64(time.Now().Unix() - m.spanLogNewestRowTimeUnix)
		if newestRowAge < 0 {
			// Defensive: clock skew between click-dog and ClickHouse
			// could push the diff slightly negative. Clamp to 0 rather
			// than expose a nonsensical age that confuses dashboards.
			newestRowAge = 0
		}
	}
	// HELP text mirrors the disambiguation in docs/observability.md so
	// operators reading the raw /metrics output see the same caveat as
	// the dashboard docs — `0` is genuinely ambiguous here, so alerts
	// should always use `> threshold` rather than `== 0` or `< N`.
	name = writePromMetricHeader(&b, MetricSpanLogNewestRowAgeSeconds)
	_, _ = fmt.Fprintf(&b, "%s %.0f\n", name, newestRowAge)

	name = writePromMetricHeader(&b, MetricSpanLogRowsLastCycle)
	_, _ = fmt.Fprintf(&b, "%s %d\n", name, m.spanLogRowsLastCycle)

	name = writePromMetricHeader(&b, MetricQueryLogEnrichmentAttempts)
	_, _ = fmt.Fprintf(&b, "%s %d\n", name, m.queryLogEnrichAttempts)

	name = writePromMetricHeader(&b, MetricQueryLogEnrichmentSuccesses)
	_, _ = fmt.Fprintf(&b, "%s %d\n", name, m.queryLogEnrichSuccesses)

	name = writePromMetricHeader(&b, MetricQueryLogEnrichmentFailures)
	_, _ = fmt.Fprintf(&b, "%s %d\n", name, m.queryLogEnrichFailures)

	// Ratio gauges always emit; 0 before first observation is documented
	// in HELP text. We could omit the line until first observation, but a
	// stable metric set is friendlier for dashboards and recording rules.
	name = writePromMetricHeader(&b, MetricQueryLogEnrichmentMatchRatio)
	_, _ = fmt.Fprintf(&b, "%s %.4f\n", name, m.queryLogEnrichMatchRatio)

	name = writePromMetricHeader(&b, MetricSpansWithQueryIDRatio)
	_, _ = fmt.Fprintf(&b, "%s %.4f\n", name, m.spansWithQueryIDRatio)

	name = writePromMetricHeader(&b, MetricNormalizedQuerySupported)
	_, _ = fmt.Fprintf(&b, "%s %d\n", name, m.normalizedQuerySupported)

	name = writePromMetricHeader(&b, MetricQueryOperationSupported)
	_, _ = fmt.Fprintf(&b, "%s %d\n", name, m.queryOperationSupported)

	return b.Bytes()
}

func writePromMetricHeader(b *bytes.Buffer, key string) string {
	d := mustDescriptor(key)
	name := PrometheusName(d)
	_, _ = fmt.Fprintf(b, "# HELP %s %s\n", name, d.Help)
	_, _ = fmt.Fprintf(b, "# TYPE %s %s\n", name, d.Kind)
	return name
}

func sortedExportSinkNames(sinks map[string]exportSinkCounters) []string {
	names := make([]string, 0, len(sinks))
	for name := range sinks {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func promLabelValue(s string) string {
	return strings.NewReplacer(
		`\`, `\\`,
		"\n", `\n`,
		`"`, `\"`,
	).Replace(s)
}

// MuxExtension registers additional handlers on the metrics HTTP mux.
// Used to mount health endpoints on the same listener when configured to
// share a port, avoiding a second goroutine + socket.
type MuxExtension func(*http.ServeMux)

// StartMetricsServer starts the scrape HTTP server for /metrics and /health.
// Additional extensions may register handlers on the same mux (e.g. /healthz,
// /readyz, /status from the health package).
//
// POST /flush deliberately does NOT mount here — it lives on the admin
// listener (StartAdminServer) so scrape-shaped endpoints stay separable from
// admin-shaped actions. This matches the Vector / Grafana Alloy / Datadog
// Agent pattern and lets operators bind /metrics on a public interface
// without exposing /flush.
//
// The bind is performed synchronously via net.Listen, mirroring health.Start:
// a port collision surfaces as a startup error rather than a silent goroutine
// exit. main calls clicklog.Fatal on bind failure so the pod crashes visibly
// instead of running with no /metrics listener.
func StartMetricsServer(addr string, metrics *Metrics, extensions ...MuxExtension) (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", metrics.Handler())
	// /health is the legacy always-200 liveness endpoint kept for
	// backward compatibility. For K8s probes prefer /healthz (liveness)
	// and /readyz (readiness) from the health package — they're mounted
	// on this same mux when listener sharing is enabled and encode
	// actual readiness signals rather than just "process is up".
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"status":"ok"}`)
	})
	for _, ext := range extensions {
		ext(mux)
	}

	return startHTTPServer("Metrics", addr, mux)
}

// StartAdminServer starts the admin HTTP server that hosts POST /flush.
//
// The admin listener defaults to 127.0.0.1:9091 (config default in
// MetricsConfig.AdminListenAddress) so /flush is unreachable off-host
// without an explicit operator opt-in. /flush's effect is bounded
// (channel cap 1, circuit-breaker gated, no data flow) — equivalent in
// reach to SIGUSR1 — so a localhost bind is the proportional control.
//
// If addr resolves to a non-loopback IP the call still succeeds, but a
// startup WARN is emitted in the same style as the InsecureSkipVerify
// warning. Operators who deliberately expose the admin listener (e.g. for
// a sidecar on a different host network namespace) see one line in the
// logs flagging the choice.
func StartAdminServer(addr string, flushChan chan<- struct{}) (*http.Server, error) {
	if flushChan == nil {
		return nil, fmt.Errorf("admin: flushChan is required")
	}

	if !isLoopbackAddress(addr) {
		clicklog.Warn("Admin listener bound to non-loopback address %s — POST /flush is reachable off-host. Use 127.0.0.1:9091 (default) unless you have a specific reason.", addr)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/flush", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		select {
		case flushChan <- struct{}{}:
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"status":"flushing"}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = fmt.Fprintf(w, `{"status":"already_flushing"}`)
		}
	})

	return startHTTPServer("Admin", addr, mux)
}

// startHTTPServer is the shared bind + serve helper for the scrape and admin
// listeners. Identical timeout posture and identical bind-fails-loudly
// semantics; only the log label and mux differ.
func startHTTPServer(label, addr string, mux *http.ServeMux) (*http.Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%s: bind %s: %w", strings.ToLower(label), addr, err)
	}

	srv := &http.Server{
		// Surface the actual bound address so callers (and tests using
		// ":0") can read the OS-assigned port from srv.Addr.
		Addr:              ln.Addr().String(),
		Handler:           mux,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		// Bound keep-alive idle time so stale scraper/probe sockets get
		// reclaimed instead of accumulating across rollouts. Same rationale
		// and value as health.Start.
		IdleTimeout: 60 * time.Second,
	}

	go func() {
		clicklog.Info("%s server listening on %s", label, ln.Addr())
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			clicklog.Error("%s server error: %v", label, err)
		}
	}()

	return srv, nil
}

// isLoopbackAddress reports whether addr resolves to a loopback bind. Used
// to decide whether to emit the off-host admin-listener warning. An empty
// or unparseable host is treated as "not loopback" so we err on the side of
// warning when the input shape is unfamiliar — i.e. an unnecessary warn
// (false positive) is preferable to a missed warn (false negative) on a
// non-loopback bind that should have been flagged.
func isLoopbackAddress(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		// e.g. ":9091" — wildcard bind, all interfaces.
		return false
	}
	if host == "localhost" {
		// String-match (not DNS) so startup never blocks on resolver. A
		// non-standard /etc/hosts that points "localhost" off-loopback
		// would silently dodge the warn — accepted trade-off; docs steer
		// operators toward literal 127.0.0.1 / ::1.
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Hostname (not a literal IP). Avoid DNS in startup-path code; treat
		// as non-loopback so the operator gets warned to use a literal.
		return false
	}
	return ip.IsLoopback()
}
