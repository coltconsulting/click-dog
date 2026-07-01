package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/coltconsulting/click-dog/internal/queryfamily"
)

type fakeConn struct {
	row               driver.Row
	rows              driver.Rows
	lastQuery         string
	lastArgs          []any
	lastQueryRow      string
	queryCalls        int
	queryRowCallCount int
}

func (f *fakeConn) Contributors() []string { return nil }
func (f *fakeConn) ServerVersion() (*driver.ServerVersion, error) {
	return nil, nil
}
func (f *fakeConn) Select(context.Context, any, string, ...any) error { return nil }
func (f *fakeConn) Query(_ context.Context, query string, args ...any) (driver.Rows, error) {
	f.queryCalls++
	f.lastQuery = query
	f.lastArgs = append([]any(nil), args...)
	return f.rows, nil
}
func (f *fakeConn) QueryRow(_ context.Context, query string, _ ...any) driver.Row {
	f.queryRowCallCount++
	f.lastQueryRow = query
	return f.row
}
func (f *fakeConn) PrepareBatch(context.Context, string, ...driver.PrepareBatchOption) (driver.Batch, error) {
	return nil, nil
}
func (f *fakeConn) Exec(context.Context, string, ...any) error { return nil }
func (f *fakeConn) AsyncInsert(context.Context, string, bool, ...any) error {
	return nil
}
func (f *fakeConn) Ping(context.Context) error { return nil }
func (f *fakeConn) Stats() driver.Stats        { return driver.Stats{} }
func (f *fakeConn) Close() error               { return nil }

type fakeRow struct {
	count uint64
	vals  []any
	err   error
}

func (r fakeRow) Err() error { return r.err }
func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if r.vals != nil {
		if len(dest) != len(r.vals) {
			return fmt.Errorf("fakeRow.Scan got %d destinations, row has %d values", len(dest), len(r.vals))
		}
		for i := range dest {
			if err := assignScanValue(dest[i], r.vals[i]); err != nil {
				return fmt.Errorf("column %d: %w", i, err)
			}
		}
		return nil
	}
	if len(dest) != 1 {
		return fmt.Errorf("fakeRow.Scan got %d destinations, want 1", len(dest))
	}
	return assignScanValue(dest[0], r.count)
}
func (r fakeRow) ScanStruct(any) error { return nil }

type fakeRows struct {
	values [][]any
	idx    int
	err    error
}

func newFakeRows(values ...[]any) *fakeRows {
	return &fakeRows{values: values, idx: -1}
}

func (r *fakeRows) Next() bool {
	r.idx++
	return r.idx < len(r.values)
}
func (r *fakeRows) Scan(dest ...any) error {
	if r.idx < 0 || r.idx >= len(r.values) {
		return errors.New("fakeRows.Scan called without current row")
	}
	row := r.values[r.idx]
	if len(dest) != len(row) {
		return fmt.Errorf("fakeRows.Scan got %d destinations, row has %d values", len(dest), len(row))
	}
	for i := range dest {
		if err := assignScanValue(dest[i], row[i]); err != nil {
			return fmt.Errorf("column %d: %w", i, err)
		}
	}
	return nil
}
func (r *fakeRows) ScanStruct(any) error             { return nil }
func (r *fakeRows) ColumnTypes() []driver.ColumnType { return nil }
func (r *fakeRows) Totals(...any) error              { return nil }
func (r *fakeRows) Columns() []string                { return nil }
func (r *fakeRows) Close() error                     { return nil }
func (r *fakeRows) Err() error                       { return r.err }
func (r *fakeRows) HasData() bool                    { return len(r.values) > 0 && r.idx < len(r.values) }

func assignScanValue(dest any, value any) error {
	dv := reflect.ValueOf(dest)
	if dv.Kind() != reflect.Pointer || dv.IsNil() {
		return fmt.Errorf("destination %T is not a non-nil pointer", dest)
	}
	vv := reflect.ValueOf(value)
	if !vv.IsValid() {
		dv.Elem().Set(reflect.Zero(dv.Elem().Type()))
		return nil
	}
	if vv.Type().AssignableTo(dv.Elem().Type()) {
		dv.Elem().Set(vv)
		return nil
	}
	if vv.Type().ConvertibleTo(dv.Elem().Type()) {
		dv.Elem().Set(vv.Convert(dv.Elem().Type()))
		return nil
	}
	return fmt.Errorf("cannot assign %T to %T", value, dest)
}

func TestDetectQueryLogNormalizedSupport(t *testing.T) {
	ctx := context.Background()

	conn := &fakeConn{row: fakeRow{count: 1}}
	if !detectQueryLogNormalizedSupport(ctx, conn, "", false) {
		t.Fatal("expected normalized query support when system.columns count is positive")
	}
	if conn.queryRowCallCount != 1 {
		t.Errorf("probe calls = %d, want 1", conn.queryRowCallCount)
	}
	for _, want := range []string{"system.columns", "database = 'system'", "table = 'query_log'", "name = 'normalized_query_hash'"} {
		if !strings.Contains(conn.lastQueryRow, want) {
			t.Errorf("probe SQL missing %q:\n%s", want, conn.lastQueryRow)
		}
	}

	conn = &fakeConn{row: fakeRow{count: 0}}
	if detectQueryLogNormalizedSupport(ctx, conn, "", false) {
		t.Fatal("expected no normalized query support when system.columns count is zero")
	}

	conn = &fakeConn{row: fakeRow{err: errors.New("permission denied")}}
	if detectQueryLogNormalizedSupport(ctx, conn, "", false) {
		t.Fatal("expected probe errors to disable optional fields")
	}

	conn = &fakeConn{row: fakeRow{vals: []any{uint64(3), uint64(3)}}}
	if !detectQueryLogNormalizedSupport(ctx, conn, "prod", true) {
		t.Fatal("expected cluster query mode to enable optional fields when all replicas support them")
	}
	if conn.queryRowCallCount != 1 {
		t.Errorf("cluster probe calls = %d, want 1", conn.queryRowCallCount)
	}
	if !strings.Contains(conn.lastQueryRow, "clusterAllReplicas('prod', system.columns)") {
		t.Errorf("cluster probe SQL should use clusterAllReplicas:\n%s", conn.lastQueryRow)
	}

	conn = &fakeConn{row: fakeRow{vals: []any{uint64(3), uint64(2)}}}
	if detectQueryLogNormalizedSupport(ctx, conn, "prod", true) {
		t.Fatal("expected mixed cluster support to disable optional fields")
	}

	conn = &fakeConn{row: fakeRow{err: errors.New("UNKNOWN_TABLE")}}
	if detectQueryLogNormalizedSupport(ctx, conn, "prod", true) {
		t.Fatal("expected unknown cluster support to disable optional fields")
	}

	conn = &fakeConn{row: fakeRow{count: 1}}
	if detectQueryLogNormalizedSupport(ctx, conn, "", true) {
		t.Fatal("cluster query mode without a cluster name should disable optional fields")
	}
	if conn.queryRowCallCount != 0 {
		t.Errorf("empty-cluster mode should not probe system.columns, got %d calls", conn.queryRowCallCount)
	}
}

func TestCapabilityProbeContextZeroIsNotCanceled(t *testing.T) {
	ctx, cancel := capabilityProbeContext(0)
	defer cancel()

	if err := ctx.Err(); err != nil {
		t.Fatalf("zero timeout probe context is already canceled: %v", err)
	}
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("zero timeout probe context should not have a deadline")
	}
}

func TestCapabilityProbeContextPositiveTimeoutHasDeadline(t *testing.T) {
	ctx, cancel := capabilityProbeContext(5)
	defer cancel()

	if err := ctx.Err(); err != nil {
		t.Fatalf("positive timeout probe context is already canceled: %v", err)
	}
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("positive timeout probe context should have a deadline")
	}
}

func TestBuildQueryLogSQL_NormalizedColumns(t *testing.T) {
	for _, template := range []string{queryLogSelectSQL, queryLogEnrichSelectSQL} {
		got := buildQueryLogSQL(template, "system.query_log", true)
		for _, want := range []string{"normalized_query_hash", "normalizeQuery(query) AS normalized_query"} {
			if !strings.Contains(got, want) {
				t.Errorf("normalized SQL missing %q:\n%s", want, got)
			}
		}

		got = buildQueryLogSQL(template, "system.query_log", false)
		for _, notWant := range []string{"normalized_query_hash", "normalizeQuery(query) AS normalized_query", queryLogNormalizedColumns} {
			if strings.Contains(got, notWant) {
				t.Errorf("fallback SQL unexpectedly contains %q:\n%s", notWant, got)
			}
		}
	}
}

func TestQueryFamilyExactGroupsSQLTopKUsesRollupLimit(t *testing.T) {
	want := fmt.Sprintf("topK(%d)", queryfamily.TopKLimit)
	if got := strings.Count(queryFamilyExactGroupsSQL, want); got != 3 {
		t.Fatalf("query family SQL has %d occurrences of %q, want 3:\n%s", got, want, queryFamilyExactGroupsSQL)
	}
}

func TestExecuteQueryLogQueryScansNormalizedFields(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	conn := &fakeConn{rows: newFakeRows([]any{
		"qid-1", "QueryFinish", now, uint64(1500), "SELECT * FROM events WHERE id = 42",
		uint64(987654321), "SELECT * FROM events WHERE id = ?",
		"default", "clickhouse-go", "app-1", "10.0.0.1",
		[]string{"default"}, []string{"default.events"}, int32(0),
		uint64(100), uint64(200), uint64(0), uint64(0), uint64(10), uint64(20), uint64(300),
	})}
	reader := &ClickHouseReader{
		conn:               conn,
		queryTimeout:       time.Second,
		queryLogSelect:     buildQueryLogSQL(queryLogSelectSQL, "system.query_log", true),
		queryLogNormalized: true,
	}

	logs, err := reader.executeQueryLogQuery(context.Background(), reader.slowQueriesBuilder(100, 0, 0, time.Minute, 1))
	if err != nil {
		t.Fatalf("executeQueryLogQuery failed: %v", err)
	}
	if !strings.Contains(conn.lastQuery, "normalized_query_hash") || !strings.Contains(conn.lastQuery, "normalizeQuery(query) AS normalized_query") {
		t.Errorf("query did not select normalized columns:\n%s", conn.lastQuery)
	}
	if len(logs) != 1 {
		t.Fatalf("logs length = %d, want 1", len(logs))
	}
	if logs[0].NormalizedQueryHash != 987654321 {
		t.Errorf("NormalizedQueryHash = %d, want 987654321", logs[0].NormalizedQueryHash)
	}
	if logs[0].NormalizedQuery != "SELECT * FROM events WHERE id = ?" {
		t.Errorf("NormalizedQuery = %q", logs[0].NormalizedQuery)
	}
}

func TestExecuteQueryLogQueryFallbackScansCurrentColumns(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	conn := &fakeConn{rows: newFakeRows([]any{
		"qid-1", "QueryFinish", now, uint64(1500), "SELECT 1",
		"default", "clickhouse-go", "app-1", "10.0.0.1",
		[]string{"default"}, []string{"default.events"}, int32(0),
		uint64(100), uint64(200), uint64(0), uint64(0), uint64(10), uint64(20), uint64(300),
	})}
	reader := &ClickHouseReader{
		conn:           conn,
		queryTimeout:   time.Second,
		queryLogSelect: buildQueryLogSQL(queryLogSelectSQL, "system.query_log", false),
	}

	logs, err := reader.executeQueryLogQuery(context.Background(), reader.slowQueriesBuilder(100, 0, 0, time.Minute, 1))
	if err != nil {
		t.Fatalf("executeQueryLogQuery failed: %v", err)
	}
	if strings.Contains(conn.lastQuery, "normalized_query_hash") || strings.Contains(conn.lastQuery, "normalizeQuery(query)") {
		t.Errorf("fallback query selected optional normalized columns:\n%s", conn.lastQuery)
	}
	if len(logs) != 1 {
		t.Fatalf("logs length = %d, want 1", len(logs))
	}
	if logs[0].NormalizedQueryHash != 0 || logs[0].NormalizedQuery != "" {
		t.Errorf("fallback normalized fields = (%d, %q), want zero/empty", logs[0].NormalizedQueryHash, logs[0].NormalizedQuery)
	}
	if logs[0].User != "default" || logs[0].MemoryUsage != 300 {
		t.Errorf("fallback scan shifted existing fields: %+v", logs[0])
	}
}

func TestFetchQueryLogByQueryIDsScansNormalizedFields(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	conn := &fakeConn{rows: newFakeRows([]any{
		"qid-1", "QueryFinish", now, uint64(1500),
		uint64(987654321), "SELECT * FROM events WHERE id = ?",
		"default", "clickhouse-go", "app-1", "10.0.0.1",
		[]string{"default"}, []string{"default.events"}, int32(0),
		uint64(100), uint64(200), uint64(0), uint64(0), uint64(10), uint64(20), uint64(300),
	})}
	reader := &ClickHouseReader{
		conn:               conn,
		queryTimeout:       time.Second,
		enrichSelect:       buildQueryLogSQL(queryLogEnrichSelectSQL, "system.query_log", true),
		queryLogNormalized: true,
	}
	reader.setUserFilter(nil, nil)

	logs, err := reader.FetchQueryLogByQueryIDs(context.Background(), []string{"qid-1"}, 1)
	if err != nil {
		t.Fatalf("FetchQueryLogByQueryIDs failed: %v", err)
	}
	if !strings.Contains(conn.lastQuery, "normalized_query_hash") || !strings.Contains(conn.lastQuery, "normalizeQuery(query) AS normalized_query") {
		t.Errorf("enrichment query did not select normalized columns:\n%s", conn.lastQuery)
	}
	if got := logs["qid-1"].NormalizedQueryHash; got != 987654321 {
		t.Errorf("NormalizedQueryHash = %d, want 987654321", got)
	}
	if got := logs["qid-1"].NormalizedQuery; got != "SELECT * FROM events WHERE id = ?" {
		t.Errorf("NormalizedQuery = %q", got)
	}
}

func TestFetchQueryLogByQueryIDsFallbackScansCurrentColumns(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	conn := &fakeConn{rows: newFakeRows([]any{
		"qid-1", "QueryFinish", now, uint64(1500),
		"default", "clickhouse-go", "app-1", "10.0.0.1",
		[]string{"default"}, []string{"default.events"}, int32(0),
		uint64(100), uint64(200), uint64(0), uint64(0), uint64(10), uint64(20), uint64(300),
	})}
	reader := &ClickHouseReader{
		conn:         conn,
		queryTimeout: time.Second,
		enrichSelect: buildQueryLogSQL(queryLogEnrichSelectSQL, "system.query_log", false),
	}
	reader.setUserFilter(nil, nil)

	logs, err := reader.FetchQueryLogByQueryIDs(context.Background(), []string{"qid-1"}, 1)
	if err != nil {
		t.Fatalf("FetchQueryLogByQueryIDs failed: %v", err)
	}
	if strings.Contains(conn.lastQuery, "normalized_query_hash") || strings.Contains(conn.lastQuery, "normalizeQuery(query)") {
		t.Errorf("fallback enrichment query selected optional normalized columns:\n%s", conn.lastQuery)
	}
	ql := logs["qid-1"]
	if ql.NormalizedQueryHash != 0 || ql.NormalizedQuery != "" {
		t.Errorf("fallback normalized fields = (%d, %q), want zero/empty", ql.NormalizedQueryHash, ql.NormalizedQuery)
	}
	if ql.User != "default" || ql.MemoryUsage != 300 {
		t.Errorf("fallback scan shifted existing enrichment fields: %+v", ql)
	}
}

func TestNormalizeQueryFamilyRollupOptions(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)

	opts, err := normalizeQueryFamilyRollupOptions(QueryFamilyRollupOptions{}, now)
	if err != nil {
		t.Fatalf("normalizeQueryFamilyRollupOptions failed: %v", err)
	}
	if !opts.EndTime.Equal(now) {
		t.Errorf("EndTime = %s, want %s", opts.EndTime, now)
	}
	if !opts.StartTime.Equal(now.Add(-defaultQueryFamilyLookback)) {
		t.Errorf("StartTime = %s, want default lookback", opts.StartTime)
	}
	if opts.MinExecutionCount != 1 {
		t.Errorf("MinExecutionCount = %d, want 1", opts.MinExecutionCount)
	}
	if opts.ExactGroupLimit != defaultQueryFamilyExactGroupLimit {
		t.Errorf("ExactGroupLimit = %d, want default", opts.ExactGroupLimit)
	}
	if opts.SimilarityThreshold != 0.75 {
		t.Errorf("SimilarityThreshold = %.2f, want default", opts.SimilarityThreshold)
	}

	opts, err = normalizeQueryFamilyRollupOptions(QueryFamilyRollupOptions{ExactGroupLimit: 99999}, now)
	if err != nil {
		t.Fatalf("normalizeQueryFamilyRollupOptions clamp case failed: %v", err)
	}
	if opts.ExactGroupLimit != maxQueryFamilyExactGroupLimit {
		t.Errorf("ExactGroupLimit = %d, want clamped max %d", opts.ExactGroupLimit, maxQueryFamilyExactGroupLimit)
	}

	_, err = normalizeQueryFamilyRollupOptions(QueryFamilyRollupOptions{
		StartTime: now,
		EndTime:   now.Add(-time.Hour),
	}, now)
	if err == nil {
		t.Fatal("expected start-after-end validation error")
	}
}

func TestFetchQueryFamilyRollupsUnsupportedWithoutNormalizedHash(t *testing.T) {
	reader := &ClickHouseReader{queryTimeout: time.Second}

	_, err := reader.FetchQueryFamilyRollups(context.Background(), QueryFamilyRollupOptions{})
	if !errors.Is(err, ErrQueryFamilyRollupsUnsupported) {
		t.Fatalf("error = %v, want ErrQueryFamilyRollupsUnsupported", err)
	}
}

func TestFetchQueryFamilyRollupsAggregatesExactGroupsBeforeRollup(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	conn := &fakeConn{rows: newFakeRows(
		[]any{
			uint64(100), "SELECT id, name FROM app.users WHERE tenant_id = ? AND status = ? ORDER BY created_at DESC LIMIT ?",
			uint64(12), float64(150), float64(220), uint64(2048), float64(1200), float64(9000),
			[]string{"api", "worker"}, []string{"clickhouse-go"}, []string{"app.users"},
			now.Add(-3 * time.Hour), now,
		},
		[]any{
			uint64(200), "SELECT id, name FROM app.users WHERE tenant_id = ? ORDER BY created_at DESC LIMIT ?",
			uint64(11), float64(90), float64(140), uint64(512), float64(800), float64(6000),
			[]string{"api"}, []string{"clickhouse-go"}, []string{"app.users"},
			now.Add(-90 * time.Minute), now.Add(-30 * time.Minute),
		},
	)}
	reader := &ClickHouseReader{
		conn:               conn,
		queryTimeout:       time.Second,
		queryFamilySelect:  strings.ReplaceAll(queryFamilyExactGroupsSQL, tblQueryLog, "system.query_log"),
		queryLogNormalized: true,
		whitelistUsers:     []string{"api", "worker"},
	}

	rollups, err := reader.FetchQueryFamilyRollups(context.Background(), QueryFamilyRollupOptions{
		StartTime:         now.Add(-4 * time.Hour),
		EndTime:           now,
		MinExecutionCount: 2,
		ExactGroupLimit:   25,
	})
	if err != nil {
		t.Fatalf("FetchQueryFamilyRollups failed: %v", err)
	}
	for _, want := range []string{
		"normalized_query_hash",
		"min(normalizeQuery(query)) AS normalized_query",
		"count() AS execution_count",
		"quantileTDigest(0.95)(query_duration_ms) AS p95_duration_ms",
		"GROUP BY normalized_query_hash",
		"HAVING count() >= ?",
		"ORDER BY execution_count DESC LIMIT ?",
		"AND user IN ?",
	} {
		if !strings.Contains(conn.lastQuery, want) {
			t.Errorf("family SQL missing %q:\n%s", want, conn.lastQuery)
		}
	}
	if strings.Contains(conn.lastQuery, "query_id") {
		t.Errorf("family aggregation should not select individual query_id values:\n%s", conn.lastQuery)
	}
	if len(conn.lastArgs) != 5 {
		t.Fatalf("lastArgs length = %d, want 5: %#v", len(conn.lastArgs), conn.lastArgs)
	}
	if !reflect.DeepEqual(conn.lastArgs[2], reader.whitelistUsers) {
		t.Errorf("whitelist arg = %#v, want %#v", conn.lastArgs[2], reader.whitelistUsers)
	}
	if conn.lastArgs[3] != uint64(2) || conn.lastArgs[4] != int(25) {
		t.Errorf("min/limit args = %#v, want min=2 limit=25", conn.lastArgs[3:])
	}

	if len(rollups) != 1 {
		t.Fatalf("rollups length = %d, want 1: %+v", len(rollups), rollups)
	}
	if got, want := rollups[0].MemberHashesSorted, []uint64{100, 200}; !reflect.DeepEqual(got, want) {
		t.Fatalf("MemberHashesSorted = %v, want %v", got, want)
	}
	if rollups[0].Stats.ExecutionCount != 23 {
		t.Errorf("ExecutionCount = %d, want 23", rollups[0].Stats.ExecutionCount)
	}
	if rollups[0].RepresentativeQuery == "" {
		t.Fatal("RepresentativeQuery should be populated")
	}
	if len(rollups[0].MergeReasons) == 0 {
		t.Fatal("expected explainable merge metadata")
	}
}

func TestFetchQueryFamilyRollupsKeepsDifferentTablesSeparate(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	conn := &fakeConn{rows: newFakeRows(
		[]any{
			uint64(100), "SELECT id FROM app.users WHERE tenant_id = ? LIMIT ?",
			uint64(12), float64(150), float64(220), uint64(2048), float64(1200), float64(9000),
			[]string{"api"}, []string{"clickhouse-go"}, []string{"app.users"},
			now.Add(-3 * time.Hour), now,
		},
		[]any{
			uint64(200), "SELECT id FROM app.orders WHERE tenant_id = ? LIMIT ?",
			uint64(11), float64(90), float64(140), uint64(512), float64(800), float64(6000),
			[]string{"api"}, []string{"clickhouse-go"}, []string{"app.orders"},
			now.Add(-90 * time.Minute), now.Add(-30 * time.Minute),
		},
	)}
	reader := &ClickHouseReader{
		conn:               conn,
		queryTimeout:       time.Second,
		queryFamilySelect:  strings.ReplaceAll(queryFamilyExactGroupsSQL, tblQueryLog, "system.query_log"),
		queryLogNormalized: true,
	}

	rollups, err := reader.FetchQueryFamilyRollups(context.Background(), QueryFamilyRollupOptions{
		StartTime: now.Add(-4 * time.Hour),
		EndTime:   now,
	})
	if err != nil {
		t.Fatalf("FetchQueryFamilyRollups failed: %v", err)
	}
	if len(rollups) != 2 {
		t.Fatalf("rollups length = %d, want separate families for different tables: %+v", len(rollups), rollups)
	}
	for _, rollup := range rollups {
		if len(rollup.MemberHashesSorted) != 1 {
			t.Fatalf("rollup %s member hashes = %v, want singleton", rollup.FamilyID, rollup.MemberHashesSorted)
		}
		if len(rollup.MergeReasons) != 0 {
			t.Fatalf("rollup %s merge reasons = %+v, want none for singleton", rollup.FamilyID, rollup.MergeReasons)
		}
	}
}

func TestFetchQueryFamilyRollupsRowsErr(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	conn := &fakeConn{rows: &fakeRows{err: errors.New("iteration failed")}}
	reader := &ClickHouseReader{
		conn:               conn,
		queryTimeout:       time.Second,
		queryFamilySelect:  strings.ReplaceAll(queryFamilyExactGroupsSQL, tblQueryLog, "system.query_log"),
		queryLogNormalized: true,
	}

	_, err := reader.FetchQueryFamilyRollups(context.Background(), QueryFamilyRollupOptions{
		StartTime: now.Add(-time.Hour),
		EndTime:   now,
	})
	if err == nil {
		t.Fatal("expected rows.Err failure")
	}
	if !strings.Contains(err.Error(), "query family exact group row iteration error") || !strings.Contains(err.Error(), "iteration failed") {
		t.Fatalf("error = %v, want wrapped rows.Err", err)
	}
}
