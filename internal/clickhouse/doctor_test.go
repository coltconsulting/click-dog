package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// findCheck returns the check with the given name, or a zero-value check with
// a sentinel status when absent so tests can assert "not present".
func findCheck(checks []ReadinessCheck, name string) (ReadinessCheck, bool) {
	for _, ck := range checks {
		if ck.Name == name {
			return ck, true
		}
	}
	return ReadinessCheck{}, false
}

func statusOf(t *testing.T, checks []ReadinessCheck, name string) ReadinessStatus {
	t.Helper()
	ck, ok := findCheck(checks, name)
	if !ok {
		t.Fatalf("expected a %q check, got: %v", name, checkNames(checks))
	}
	return ck.Status
}

func checkNames(checks []ReadinessCheck) []string {
	out := make([]string, len(checks))
	for i, ck := range checks {
		out[i] = ck.Name
	}
	return out
}

func TestEvaluateReadiness_Healthy(t *testing.T) {
	checks := evaluateReadiness(readinessObs{
		lookback:           24 * time.Hour,
		minTraceDurationMs: 1000,
		enrichmentNeeded:   true,
		spanTotal:          500,
		spanQualifying:     40,
		spanMaxUs:          5_000_000, // 5000ms
		qidSampled:         500,
		qidWith:            480,
		queryLogCount:      9000,
		joinSampleIDs:      20,
		joinMatched:        18,
		normSupported:      true,
	})

	for _, name := range []string{
		"span_log readable", "span_log recent data", "duration vs min_trace_duration_ms",
		"clickhouse.query_id on spans", "query_log readable", "query_log enrichment join",
		"normalized_query_hash support",
	} {
		if s := statusOf(t, checks, name); s != ReadinessPass {
			t.Errorf("%s = %s, want PASS", name, s)
		}
	}
	if hasFail(checks) {
		t.Errorf("healthy report should have no FAIL lines: %v", checks)
	}
}

func TestEvaluateReadiness_MissingSpanLog(t *testing.T) {
	checks := evaluateReadiness(readinessObs{
		lookback:         24 * time.Hour,
		enrichmentNeeded: true,
		spanLogErr:       errors.New("UNKNOWN_TABLE"),
		queryLogCount:    10,
		normSupported:    true,
	})

	if s := statusOf(t, checks, "span_log readable"); s != ReadinessFail {
		t.Errorf("span_log readable = %s, want FAIL", s)
	}
	ck, _ := findCheck(checks, "span_log readable")
	if ck.Remedy == "" {
		t.Error("FAIL check must include a remedy")
	}
	// Span-dependent checks should not appear when the span log is unreadable.
	if _, ok := findCheck(checks, "clickhouse.query_id on spans"); ok {
		t.Error("query_id check should be omitted when span log is unreadable")
	}
	if _, ok := findCheck(checks, "query_log enrichment join"); ok {
		t.Error("enrichment join should be omitted when span log is unreadable")
	}
}

func TestEvaluateReadiness_MissingQueryLog(t *testing.T) {
	base := readinessObs{
		lookback:       24 * time.Hour,
		spanTotal:      100,
		spanQualifying: 10,
		qidSampled:     100,
		qidWith:        90,
		queryLogErr:    errors.New("ACCESS_DENIED"),
		normSupported:  true,
	}

	t.Run("needed fails", func(t *testing.T) {
		obs := base
		obs.enrichmentNeeded = true
		checks := evaluateReadiness(obs)
		if s := statusOf(t, checks, "query_log readable"); s != ReadinessFail {
			t.Errorf("query_log readable = %s, want FAIL when enrichment needed", s)
		}
	})

	t.Run("not needed warns", func(t *testing.T) {
		obs := base
		obs.enrichmentNeeded = false
		checks := evaluateReadiness(obs)
		if s := statusOf(t, checks, "query_log readable"); s != ReadinessWarn {
			t.Errorf("query_log readable = %s, want WARN when enrichment not needed", s)
		}
		if hasFail(checks) {
			t.Errorf("query_log gap should not fail when not needed: %v", checks)
		}
	})
}

func TestEvaluateReadiness_NoRecentData(t *testing.T) {
	checks := evaluateReadiness(readinessObs{
		lookback:           24 * time.Hour,
		minTraceDurationMs: 1000,
		enrichmentNeeded:   true,
		spanTotal:          0,
		queryLogCount:      0,
		joinSampleIDs:      0,
		normSupported:      true,
	})

	if s := statusOf(t, checks, "span_log readable"); s != ReadinessPass {
		t.Errorf("span_log readable = %s, want PASS (selectable but empty)", s)
	}
	if s := statusOf(t, checks, "span_log recent data"); s != ReadinessWarn {
		t.Errorf("span_log recent data = %s, want WARN", s)
	}
	// With no rows, the duration distribution can't be assessed.
	if _, ok := findCheck(checks, "duration vs min_trace_duration_ms"); ok {
		t.Error("duration check should be omitted when there are no recent spans")
	}
	if hasFail(checks) {
		t.Errorf("empty data should warn, not fail: %v", checks)
	}
}

func TestEvaluateReadiness_DurationThresholdTooHigh(t *testing.T) {
	checks := evaluateReadiness(readinessObs{
		lookback:           24 * time.Hour,
		minTraceDurationMs: 10000,
		spanTotal:          300,
		spanQualifying:     0,
		spanMaxUs:          2_000_000, // 2000ms < 10000ms threshold
		normSupported:      true,
	})

	ck, _ := findCheck(checks, "duration vs min_trace_duration_ms")
	if ck.Status != ReadinessWarn {
		t.Fatalf("duration check = %s, want WARN", ck.Status)
	}
	if !strings.Contains(ck.Detail, "max observed 2000ms") {
		t.Errorf("duration detail should report observed max: %q", ck.Detail)
	}
	if ck.Remedy == "" {
		t.Error("duration WARN must include a remedy")
	}
}

func TestEvaluateReadiness_EnrichmentUnavailable_NoQueryID(t *testing.T) {
	checks := evaluateReadiness(readinessObs{
		lookback:         24 * time.Hour,
		enrichmentNeeded: true,
		spanTotal:        200,
		spanQualifying:   20,
		qidSampled:       200,
		qidWith:          0, // no spans carry clickhouse.query_id
		queryLogCount:    5000,
		joinSampleIDs:    0, // nothing to join
		normSupported:    true,
	})

	if s := statusOf(t, checks, "clickhouse.query_id on spans"); s != ReadinessWarn {
		t.Errorf("query_id check = %s, want WARN when no spans carry query_id", s)
	}
	if s := statusOf(t, checks, "query_log enrichment join"); s != ReadinessSkip {
		t.Errorf("enrichment join = %s, want SKIP when there are no query IDs to test", s)
	}
}

func TestEvaluateReadiness_EnrichmentJoinEmpty(t *testing.T) {
	checks := evaluateReadiness(readinessObs{
		lookback:         24 * time.Hour,
		enrichmentNeeded: true, // join is only evaluated when enrichment is configured
		spanTotal:        200,
		qidSampled:       200,
		qidWith:          150,
		queryLogCount:    5000,
		joinSampleIDs:    20,
		joinMatched:      0, // query IDs exist but none found in query_log
		normSupported:    true,
	})

	ck, _ := findCheck(checks, "query_log enrichment join")
	if ck.Status != ReadinessWarn {
		t.Fatalf("enrichment join = %s, want WARN when no IDs match", ck.Status)
	}
	if ck.Remedy == "" {
		t.Error("enrichment join WARN must include a remedy")
	}
}

// When enrichment/user-filters aren't configured, the query_id-presence and
// enrichment-join probes are skipped (no attribute-Map scan, no query_log
// lookup) and reported as SKIP rather than run — query_id on spans is
// irrelevant to a config that never enriches.
func TestEvaluateReadiness_EnrichmentNotConfigured_SkipsQidAndJoin(t *testing.T) {
	checks := evaluateReadiness(readinessObs{
		lookback:         24 * time.Hour,
		enrichmentNeeded: false,
		spanTotal:        200,
		spanQualifying:   20,
		queryLogCount:    5000,
		normSupported:    true,
	})

	if s := statusOf(t, checks, "clickhouse.query_id on spans"); s != ReadinessSkip {
		t.Errorf("query_id check = %s, want SKIP when enrichment not configured", s)
	}
	if s := statusOf(t, checks, "query_log enrichment join"); s != ReadinessSkip {
		t.Errorf("enrichment join = %s, want SKIP when enrichment not configured", s)
	}
	if hasFail(checks) {
		t.Errorf("skipping enrichment probes must not fail: %v", checks)
	}
}

// gatherReadiness must not even issue the qid/join queries when enrichment
// isn't configured — guards the cost saving, not just the reporting.
func TestGatherReadiness_NoEnrichment_SkipsQidAndJoinQueries(t *testing.T) {
	var seen []string
	rowFn := func(query string) driver.Row {
		seen = append(seen, query)
		return healthyDoctorRow(query)
	}
	reader := newDoctorReader(rowFn, func(query string) (driver.Rows, error) {
		seen = append(seen, query)
		return newFakeRows([]any{"qid-1"}), nil
	})

	reader.RunReadinessChecks(context.Background(), ReadinessOptions{
		Lookback:         24 * time.Hour,
		EnrichmentNeeded: false,
	})

	joined := strings.Join(seen, "\n")
	if strings.Contains(joined, "with_qid") {
		t.Errorf("query_id-presence probe must not run when enrichment is not configured:\n%s", joined)
	}
	if strings.Contains(joined, "AS matched") || strings.Contains(joined, "DISTINCT attribute['clickhouse.query_id']") {
		t.Errorf("enrichment-join sample/lookup must not run when enrichment is not configured:\n%s", joined)
	}
}

func TestEvaluateReadiness_NormalizedHashUnavailable(t *testing.T) {
	checks := evaluateReadiness(readinessObs{
		lookback:      24 * time.Hour,
		spanTotal:     10,
		queryLogCount: 10,
		normSupported: false,
	})
	if s := statusOf(t, checks, "normalized_query_hash support"); s != ReadinessWarn {
		t.Errorf("normalized_query_hash = %s, want WARN", s)
	}
	if hasFail(checks) {
		t.Errorf("missing normalized_query_hash should warn, not fail: %v", checks)
	}
}

func TestEvaluateReadiness_NormalizedHashProbeErrorHasRemedy(t *testing.T) {
	checks := evaluateReadiness(readinessObs{
		lookback:      24 * time.Hour,
		spanTotal:     10,
		queryLogCount: 10,
		normErr:       errors.New("ACCESS_DENIED"),
	})
	ck, _ := findCheck(checks, "normalized_query_hash support")
	if ck.Status != ReadinessWarn {
		t.Fatalf("normalized_query_hash = %s, want WARN on probe error", ck.Status)
	}
	if !strings.Contains(ck.Detail, "ACCESS_DENIED") {
		t.Errorf("probe error detail should include original error: %q", ck.Detail)
	}
	if !strings.Contains(ck.Remedy, "system.columns") {
		t.Errorf("probe error remedy should mention system.columns grant: %q", ck.Remedy)
	}
}

func TestEvaluateReadiness_ClusterMode(t *testing.T) {
	checks := evaluateReadiness(readinessObs{
		lookback:             24 * time.Hour,
		useClusterQueries:    true,
		clusterName:          "prod",
		normSupported:        true,
		normClusterReplicas:  3,
		normClusterSupported: 3,
		spanTotal:            10,
		queryLogCount:        10,
	})
	if s := statusOf(t, checks, "normalized_query_hash support"); s != ReadinessPass {
		t.Errorf("normalized_query_hash = %s, want PASS when all cluster replicas support it", s)
	}
	ck, ok := findCheck(checks, "cluster query mode")
	if !ok {
		t.Fatal("cluster query mode check should be present when cluster queries are enabled")
	}
	if ck.Status != ReadinessPass || !strings.Contains(ck.Detail, "prod") {
		t.Errorf("cluster mode check = %+v, want PASS mentioning cluster name", ck)
	}
}

func TestEvaluateReadiness_ClusterModeMixedNormalizedHash(t *testing.T) {
	checks := evaluateReadiness(readinessObs{
		lookback:             24 * time.Hour,
		useClusterQueries:    true,
		clusterName:          "prod",
		normClusterReplicas:  3,
		normClusterSupported: 2,
		spanTotal:            10,
		queryLogCount:        10,
	})
	ck, _ := findCheck(checks, "normalized_query_hash support")
	if ck.Status != ReadinessWarn {
		t.Fatalf("normalized_query_hash = %s, want WARN for mixed cluster support", ck.Status)
	}
	if !strings.Contains(ck.Detail, "2 of 3") {
		t.Errorf("cluster mixed-support detail should include supported/replica counts: %q", ck.Detail)
	}
	if ck.Remedy == "" {
		t.Error("cluster mixed-support WARN must include a remedy")
	}
}

func TestEvaluateReadiness_ClusterModeZeroReplicas(t *testing.T) {
	checks := evaluateReadiness(readinessObs{
		lookback:             24 * time.Hour,
		useClusterQueries:    true,
		clusterName:          "prod",
		normClusterReplicas:  0,
		normClusterSupported: 0,
		spanTotal:            10,
		queryLogCount:        10,
	})
	ck, _ := findCheck(checks, "normalized_query_hash support")
	if ck.Status != ReadinessWarn {
		t.Fatalf("normalized_query_hash = %s, want WARN when zero replicas found", ck.Status)
	}
	if !strings.Contains(ck.Detail, "could not verify any cluster replicas") {
		t.Errorf("zero-replica detail should avoid 0-of-0 wording: %q", ck.Detail)
	}
	if ck.Remedy == "" {
		t.Error("zero-replica WARN must include a remedy")
	}
}

// TestDoctorEnrichJoinSQL_MatchesRuntimeTerminalFilter guards the enrichment-join
// regression: the probe must mirror runtime enrichment (queryLogEnrichSelectSQL),
// which only joins terminal rows. Counting QueryStart rows — or any non-terminal
// row — would report the join as viable when runtime enrichment could never
// match it, recreating the false-PASS this probe exists to catch.
func TestDoctorEnrichJoinSQL_MatchesRuntimeTerminalFilter(t *testing.T) {
	if !strings.Contains(doctorEnrichJoinSQL, "type IN ('QueryFinish', 'ExceptionWhileProcessing')") {
		t.Errorf("enrichment-join probe must filter to terminal rows (matching queryLogEnrichSelectSQL); SQL:\n%s", doctorEnrichJoinSQL)
	}
	// Same terminal filter the runtime enrichment select uses, so the probe and
	// the real join can never disagree on which rows count.
	if !strings.Contains(queryLogEnrichSelectSQL, "type IN ('QueryFinish', 'ExceptionWhileProcessing')") {
		t.Errorf("runtime enrichment select changed its terminal filter; keep the join probe in sync:\n%s", queryLogEnrichSelectSQL)
	}
	if !strings.Contains(doctorEnrichJoinSQL, "countDistinct(query_id)") {
		t.Errorf("enrichment-join probe must count distinct query_ids so 'N of M' stays accurate; SQL:\n%s", doctorEnrichJoinSQL)
	}
}

func TestReadinessStatusString(t *testing.T) {
	cases := map[ReadinessStatus]string{
		ReadinessPass: "PASS", ReadinessWarn: "WARN",
		ReadinessFail: "FAIL", ReadinessSkip: "SKIP",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("status %d = %q, want %q", s, got, want)
		}
	}
}

func hasFail(checks []ReadinessCheck) bool {
	for _, ck := range checks {
		if ck.Status == ReadinessFail {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// End-to-end gather tests with a dispatching fake connection. These exercise
// the real SQL dispatch + row scanning, not just the pure classifier.
// ---------------------------------------------------------------------------

type doctorRow struct {
	vals []any
	err  error
}

func (r doctorRow) Err() error { return r.err }
func (r doctorRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.vals) {
		return fmt.Errorf("doctorRow.Scan got %d destinations, row has %d values", len(dest), len(r.vals))
	}
	for i := range dest {
		if err := assignScanValue(dest[i], r.vals[i]); err != nil {
			return fmt.Errorf("column %d: %w", i, err)
		}
	}
	return nil
}
func (r doctorRow) ScanStruct(any) error { return nil }

// doctorConn dispatches QueryRow/Query by matching the SQL text, so each deep
// probe can be given its own result or error.
type doctorConn struct {
	*fakeConn
	rowFn  func(query string) driver.Row
	rowsFn func(query string) (driver.Rows, error)
}

func (c *doctorConn) QueryRow(_ context.Context, query string, _ ...any) driver.Row {
	return c.rowFn(query)
}
func (c *doctorConn) Query(_ context.Context, query string, _ ...any) (driver.Rows, error) {
	if c.rowsFn != nil {
		return c.rowsFn(query)
	}
	return newFakeRows(), nil
}

// healthyDoctorRow returns the default healthy response for each probe query.
func healthyDoctorRow(query string) driver.Row {
	switch {
	case strings.Contains(query, "max_us"):
		return doctorRow{vals: []any{uint64(500), uint64(40), uint64(5_000_000)}}
	case strings.Contains(query, "with_qid"):
		return doctorRow{vals: []any{uint64(500), uint64(480)}}
	case strings.Contains(query, "AS matched"):
		return doctorRow{vals: []any{uint64(18)}}
	case strings.Contains(query, "clusterAllReplicas") && strings.Contains(query, "system.columns"):
		return doctorRow{vals: []any{uint64(3), uint64(3)}}
	case strings.Contains(query, "system.columns"):
		return doctorRow{vals: []any{uint64(1)}}
	case strings.Contains(query, "AS total"): // query_log count (span stats matched max_us above)
		return doctorRow{vals: []any{uint64(9000)}}
	default:
		return doctorRow{err: fmt.Errorf("unexpected QueryRow: %s", query)}
	}
}

func newDoctorReader(rowFn func(string) driver.Row, rowsFn func(string) (driver.Rows, error)) *ClickHouseReader {
	return &ClickHouseReader{
		conn:         &doctorConn{fakeConn: &fakeConn{}, rowFn: rowFn, rowsFn: rowsFn},
		queryTimeout: time.Second,
		spanLogRef:   "system.opentelemetry_span_log",
		queryLogRef:  "system.query_log",
	}
}

func TestRunReadinessChecks_HealthyEndToEnd(t *testing.T) {
	reader := newDoctorReader(healthyDoctorRow, func(string) (driver.Rows, error) {
		return newFakeRows([]any{"qid-1"}, []any{"qid-2"}), nil
	})

	checks := reader.RunReadinessChecks(context.Background(), ReadinessOptions{
		Lookback:           24 * time.Hour,
		MinTraceDurationMs: 1000,
		EnrichmentNeeded:   true,
	})

	if hasFail(checks) {
		t.Fatalf("healthy end-to-end run should not fail: %v", checks)
	}
	if s := statusOf(t, checks, "query_log enrichment join"); s != ReadinessPass {
		t.Errorf("enrichment join = %s, want PASS", s)
	}
}

func TestRunReadinessChecks_MissingSpanLogEndToEnd(t *testing.T) {
	rowFn := func(query string) driver.Row {
		if strings.Contains(query, "max_us") {
			return doctorRow{err: errors.New("UNKNOWN_TABLE: system.opentelemetry_span_log")}
		}
		return healthyDoctorRow(query)
	}
	reader := newDoctorReader(rowFn, nil)

	checks := reader.RunReadinessChecks(context.Background(), ReadinessOptions{
		Lookback:         24 * time.Hour,
		EnrichmentNeeded: true,
	})

	if s := statusOf(t, checks, "span_log readable"); s != ReadinessFail {
		t.Errorf("span_log readable = %s, want FAIL", s)
	}
	if !hasFail(checks) {
		t.Error("missing span log should produce a FAIL")
	}
}

func TestRunReadinessChecks_ClusterModeUsesClusterRefs(t *testing.T) {
	var seen []string
	rowFn := func(query string) driver.Row {
		seen = append(seen, query)
		return healthyDoctorRow(query)
	}
	reader := newDoctorReader(rowFn, func(query string) (driver.Rows, error) {
		seen = append(seen, query)
		return newFakeRows([]any{"qid-1"}), nil
	})
	reader.useClusterQueries = true
	reader.cluster = "prod"
	reader.spanLogRef = "cluster('prod', system.opentelemetry_span_log)"
	reader.queryLogRef = "cluster('prod', system.query_log)"

	checks := reader.RunReadinessChecks(context.Background(), ReadinessOptions{
		Lookback:         24 * time.Hour,
		EnrichmentNeeded: true,
	})

	joined := strings.Join(seen, "\n")
	if !strings.Contains(joined, "cluster('prod', system.opentelemetry_span_log)") {
		t.Errorf("span queries should use the cluster ref:\n%s", joined)
	}
	if !strings.Contains(joined, "cluster('prod', system.query_log)") {
		t.Errorf("query_log queries should use the cluster ref:\n%s", joined)
	}
	if s := statusOf(t, checks, "normalized_query_hash support"); s != ReadinessPass {
		t.Errorf("normalized check = %s, want PASS when all cluster replicas support it", s)
	}
	if !strings.Contains(joined, "clusterAllReplicas('prod', system.columns)") {
		t.Errorf("cluster mode should probe system.columns through clusterAllReplicas:\n%s", joined)
	}
}

func TestRunReadinessChecks_ClusterModeMixedNormalizedEndToEnd(t *testing.T) {
	rowFn := func(query string) driver.Row {
		if strings.Contains(query, "clusterAllReplicas") && strings.Contains(query, "system.columns") {
			return doctorRow{vals: []any{uint64(3), uint64(2)}}
		}
		return healthyDoctorRow(query)
	}
	reader := newDoctorReader(rowFn, func(string) (driver.Rows, error) {
		return newFakeRows([]any{"qid-1"}), nil
	})
	reader.useClusterQueries = true
	reader.cluster = "prod"
	reader.spanLogRef = "cluster('prod', system.opentelemetry_span_log)"
	reader.queryLogRef = "cluster('prod', system.query_log)"

	checks := reader.RunReadinessChecks(context.Background(), ReadinessOptions{
		Lookback:         24 * time.Hour,
		EnrichmentNeeded: true,
	})

	ck, _ := findCheck(checks, "normalized_query_hash support")
	if ck.Status != ReadinessWarn {
		t.Fatalf("normalized check = %s, want WARN for mixed cluster support", ck.Status)
	}
	if !strings.Contains(ck.Detail, "2 of 3") {
		t.Errorf("normalized check detail should include replica counts: %q", ck.Detail)
	}
}
