package processor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
)

// The three tests below cover the cycle states named in #183:
//   1. Healthy cycle  — span_log returns rows, enrichment succeeds.
//   2. Empty cycle    — span_log returns zero rows; staleness gauges
//                       keep growing across consecutive empty cycles.
//   3. Enrichment-failure cycle — span_log returns rows, but the
//                       query_log lookup returns an error.
//
// They exercise the helpers recordSpanLogObservation and the metrics
// package's RecordQueryLogEnrichmentCycle directly. We don't drive the
// full fetchAndProcessSpans pipeline because it takes a concrete
// *clickhouse.ClickHouseReader; the helper extraction is intentional —
// the same metric calls run in production, just wrapped in I/O.

// healthyCycleSpans returns a small representative span slice for a
// healthy cycle: every span has a clickhouse.query_id, the timestamps
// are fresh (within the lookback), and the newest row is the one with
// the most-recent FinishTimeUs.
//
// FinishTimeUs (microseconds since epoch) is what recordSpanLogObservation
// actually reads — NOT FinishDate, which is the ClickHouse date partition
// truncated to midnight. We still set FinishDate to keep the test fixtures
// production-shaped; the regression test below pins the field choice.
func healthyCycleSpans(now time.Time) []model.OpenTelemetrySpan {
	return []model.OpenTelemetrySpan{
		{
			SpanID:       1,
			FinishTimeUs: uint64(now.Add(-30 * time.Second).UnixMicro()),
			FinishDate:   now.Add(-30 * time.Second),
			Attributes:   map[string]string{"clickhouse.query_id": "q-1"},
		},
		{
			SpanID:       2,
			FinishTimeUs: uint64(now.Add(-15 * time.Second).UnixMicro()), // newest of the three
			FinishDate:   now.Add(-15 * time.Second),
			Attributes:   map[string]string{"clickhouse.query_id": "q-2"},
		},
		{
			SpanID:       3,
			FinishTimeUs: uint64(now.Add(-45 * time.Second).UnixMicro()),
			FinishDate:   now.Add(-45 * time.Second),
			Attributes:   map[string]string{"clickhouse.query_id": "q-3"},
		},
	}
}

// TestProcessor_HealthyCycleEmitsExpectedMetrics — happy path. All
// freshness gauges advance, ratio gauges land at 1.0, the enrichment
// attempt+success counters tick.
func TestProcessor_HealthyCycleEmitsExpectedMetrics(t *testing.T) {
	m := metrics.NewMetrics()
	now := time.Now()
	spans := healthyCycleSpans(now)

	// Run the span-log observation path the way fetchAndProcessSpans
	// does, then the enrichment path with a 100% match (3/3 query_ids
	// resolved, no error).
	recordSpanLogObservation(m, spans)
	m.RecordQueryLogEnrichmentCycle(3, 3, nil)

	body := scrapeMetrics(t, m)
	assertContains(t, body, "click_dog_span_log_rows_last_cycle 3\n")
	assertContains(t, body, "click_dog_spans_with_query_id_ratio 1.0000\n")
	assertContains(t, body, "click_dog_query_log_enrichment_attempts_total 1\n")
	assertContains(t, body, "click_dog_query_log_enrichment_successes_total 1\n")
	assertContains(t, body, "click_dog_query_log_enrichment_failures_total 0\n")
	assertContains(t, body, "click_dog_query_log_enrichment_match_ratio 1.0000\n")

	// last_poll_timestamp must be non-zero (proves it advanced) and not
	// the zero default. Render uses a fresh Now() so we just confirm the
	// stored value advanced past 0.
	if strings.Contains(body, "click_dog_span_log_last_poll_timestamp_seconds 0\n") {
		t.Errorf("last_poll_timestamp must advance past 0 after a successful poll, body:\n%s", body)
	}

	// newest_row_age should be roughly 15 seconds (the newest span is
	// 15s in the past). Allow a small wall-clock tolerance for the
	// scrape itself.
	found := false
	for _, want := range []string{
		"click_dog_span_log_newest_row_age_seconds 14\n",
		"click_dog_span_log_newest_row_age_seconds 15\n",
		"click_dog_span_log_newest_row_age_seconds 16\n",
		"click_dog_span_log_newest_row_age_seconds 17\n",
	} {
		if strings.Contains(body, want) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("newest_row_age_seconds outside 14-17s window, body:\n%s", body)
	}
}

// TestProcessor_EmptyCycleKeepsStalenessGrowing — span_log returns no
// rows. The interesting assertion is that newest_row_age does NOT reset
// across the second cycle: that gauge IS the staleness signal, and
// resetting it would hide the very problem the metric exists to
// surface.
func TestProcessor_EmptyCycleKeepsStalenessGrowing(t *testing.T) {
	m := metrics.NewMetrics()

	// Cycle 1: a single span observed two minutes ago. Sets the
	// newest-row timestamp gauge from a fresh observation. We seed
	// FinishTimeUs (the field recordSpanLogObservation actually reads)
	// — FinishDate is left at the zero value to prove the helper is
	// not falling back to it.
	twoMinAgo := time.Now().Add(-2 * time.Minute)
	recordSpanLogObservation(m, []model.OpenTelemetrySpan{{
		SpanID:       1,
		FinishTimeUs: uint64(twoMinAgo.UnixMicro()),
		Attributes:   map[string]string{"clickhouse.query_id": "q-1"},
	}})

	// Cycle 2: empty. rows_last_cycle goes to 0, last_poll_timestamp
	// advances (so the operator knows click-dog is still alive), and
	// newest_row_age MUST continue ticking from twoMinAgo, not reset.
	recordSpanLogObservation(m, nil)

	body := scrapeMetrics(t, m)
	assertContains(t, body, "click_dog_span_log_rows_last_cycle 0\n")

	// last_poll_timestamp must still be advancing — the empty cycle
	// counts as a successful poll.
	if strings.Contains(body, "click_dog_span_log_last_poll_timestamp_seconds 0\n") {
		t.Errorf("last_poll_timestamp must advance on empty cycle, body:\n%s", body)
	}

	// newest_row_age should still be roughly 120s (the seed observation
	// from cycle 1). Allow a few-second tolerance.
	found := false
	for _, want := range []string{
		"click_dog_span_log_newest_row_age_seconds 119\n",
		"click_dog_span_log_newest_row_age_seconds 120\n",
		"click_dog_span_log_newest_row_age_seconds 121\n",
		"click_dog_span_log_newest_row_age_seconds 122\n",
		"click_dog_span_log_newest_row_age_seconds 123\n",
	} {
		if strings.Contains(body, want) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("newest_row_age must be ~120s after empty cycle (preserve cycle 1 observation), body:\n%s", body)
	}

	// spans_with_query_id_ratio: cycle 1 set it to 1.0 (1 span had a
	// query_id), cycle 2 had total=0 so the guard kept the previous
	// value. The gauge should still read 1.0 — empty cycles must NOT
	// zero it.
	assertContains(t, body, "click_dog_spans_with_query_id_ratio 1.0000\n")

	// Enrichment was never attempted (no query_ids reached the
	// FetchQueryLogByQueryIDs caller). All three counters stay at 0.
	assertContains(t, body, "click_dog_query_log_enrichment_attempts_total 0\n")
	assertContains(t, body, "click_dog_query_log_enrichment_successes_total 0\n")
	assertContains(t, body, "click_dog_query_log_enrichment_failures_total 0\n")
}

// TestProcessor_EnrichmentFailureCycleTrackedSeparately — span_log
// returns rows fine, but the query_log enrichment call returns an
// error. Span-log gauges must still advance (the span fetch did
// succeed), AND the enrichment failures counter must tick separately —
// these are two independent signals and the dashboard distinguishes
// them.
func TestProcessor_EnrichmentFailureCycleTrackedSeparately(t *testing.T) {
	m := metrics.NewMetrics()
	now := time.Now()
	spans := healthyCycleSpans(now)

	recordSpanLogObservation(m, spans)
	m.RecordQueryLogEnrichmentCycle(0, 3, errors.New("query_log scan failed"))

	body := scrapeMetrics(t, m)
	// Span-log fetch was healthy — these gauges all advance.
	assertContains(t, body, "click_dog_span_log_rows_last_cycle 3\n")
	assertContains(t, body, "click_dog_spans_with_query_id_ratio 1.0000\n")

	// Enrichment failed — attempts ticks, failures ticks, successes
	// stays flat, and match_ratio is NOT updated (a failed call tells
	// us nothing about how well a future join would match, so the
	// previous value sticks; here that's the 0 default).
	assertContains(t, body, "click_dog_query_log_enrichment_attempts_total 1\n")
	assertContains(t, body, "click_dog_query_log_enrichment_successes_total 0\n")
	assertContains(t, body, "click_dog_query_log_enrichment_failures_total 1\n")
	assertContains(t, body, "click_dog_query_log_enrichment_match_ratio 0.0000\n")
}

// TestProcessor_NewestRowAgeDerivedFromFinishTimeUs is a regression
// pin for the #200 review finding. ClickHouse's
// system.opentelemetry_span_log.finish_date is a Date partition column
// truncated to the start of the day, while finish_time_us carries the
// actual microsecond timestamp. recordSpanLogObservation MUST use
// FinishTimeUs — using FinishDate would make every healthy span look
// 0–24 h old depending on the time of day (and the dashboard tile's
// 600 s red threshold would trip during normal operation).
//
// The test seeds FinishDate to midnight of today and FinishTimeUs to
// ~10 s ago, then asserts the rendered age is roughly 10 s and is
// nowhere near "hours since midnight" no matter what wall-clock time
// the test runs at.
func TestProcessor_NewestRowAgeDerivedFromFinishTimeUs(t *testing.T) {
	m := metrics.NewMetrics()
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	recordSpanLogObservation(m, []model.OpenTelemetrySpan{{
		SpanID:       1,
		FinishTimeUs: uint64(now.Add(-10 * time.Second).UnixMicro()),
		FinishDate:   midnight, // production shape: Date column, midnight
		Attributes:   map[string]string{"clickhouse.query_id": "q-1"},
	}})

	body := scrapeMetrics(t, m)

	// Age must be ~10 s. Allow [9, 13] s for scrape jitter / CI load.
	found := false
	for _, want := range []string{
		"click_dog_span_log_newest_row_age_seconds 9\n",
		"click_dog_span_log_newest_row_age_seconds 10\n",
		"click_dog_span_log_newest_row_age_seconds 11\n",
		"click_dog_span_log_newest_row_age_seconds 12\n",
		"click_dog_span_log_newest_row_age_seconds 13\n",
	} {
		if strings.Contains(body, want) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("newest_row_age must be ~10s (derived from FinishTimeUs); "+
			"if this looks like hours-since-midnight, the helper has regressed "+
			"to reading FinishDate. Body:\n%s", body)
	}
}

// assertContains is a thin wrapper around strings.Contains that prints
// the full body on failure — the rendered output is long enough that
// the default t.Errorf with "want X" alone leaves you guessing at what
// got emitted.
func assertContains(t *testing.T, body, line string) {
	t.Helper()
	if !strings.Contains(body, line) {
		t.Errorf("metrics body missing %q\n----\n%s\n----", line, body)
	}
}
