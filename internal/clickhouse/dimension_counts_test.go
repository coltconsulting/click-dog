package clickhouse

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/queryfamily"
)

func dimensionCountReader() *ClickHouseReader {
	return &ClickHouseReader{
		queryTimeout:       time.Second,
		queryLogRef:        "system.query_log",
		queryLogNormalized: true,
	}
}

func dimensionCountOpts() QueryFamilyDimensionCountOptions {
	return QueryFamilyDimensionCountOptions{
		StartTime:         time.Unix(1000, 0).UTC(),
		EndTime:           time.Unix(2000, 0).UTC(),
		MinExecutionCount: 3,
		ExactGroupLimit:   200,
		TopK:              5,
	}
}

func TestQueryFamilyDimensionsAllowlist(t *testing.T) {
	want := []QueryFamilyDimension{QueryFamilyDimensionUser, QueryFamilyDimensionClient, QueryFamilyDimensionHost}
	if !reflect.DeepEqual(queryFamilyDimensions, want) {
		t.Fatalf("queryFamilyDimensions = %v, want %v", queryFamilyDimensions, want)
	}
	// tables must never appear: it is an array column needing arrayJoin.
	for _, dim := range queryFamilyDimensions {
		if string(dim) == "tables" || string(dim) == "databases" {
			t.Errorf("array column %q must not be a skew dimension", dim)
		}
	}
}

func TestQueryFamilyDimensionCountsBuilder_FixedDimensionsOnly(t *testing.T) {
	r := dimensionCountReader()
	b := r.queryFamilyDimensionCountsBuilder(dimensionCountOpts())

	// One parallel ARRAY JOIN unpivots the fixed allowlist columns instead of
	// one query per dimension.
	for _, want := range []string{
		"ARRAY JOIN",
		"['user', 'client_name', 'client_hostname'] AS dimension_name",
		"[toString(user), toString(client_name), toString(client_hostname)] AS dimension_value",
		"GROUP BY normalized_query_hash, dimension_name, dimension_value",
	} {
		if !strings.Contains(b.query, want) {
			t.Errorf("dimension SQL missing %q:\n%s", want, b.query)
		}
	}

	// Array columns must never be unpivoted as skew dimensions.
	if strings.Contains(b.query, "tables") || strings.Contains(b.query, "databases") {
		t.Errorf("array column appeared in dimension SQL:\n%s", b.query)
	}
}

func TestQueryFamilyDimensionCountsBuilder_SingleScan(t *testing.T) {
	r := dimensionCountReader()
	b := r.queryFamilyDimensionCountsBuilder(dimensionCountOpts())

	// query_log is referenced exactly twice: the main scan and the shared
	// top-family subquery — not once per dimension.
	if got := strings.Count(b.query, "system.query_log"); got != 2 {
		t.Errorf("system.query_log reference count = %d, want 2 (main scan + subquery):\n%s", got, b.query)
	}
	if got := strings.Count(b.query, "ARRAY JOIN"); got != 1 {
		t.Errorf("ARRAY JOIN count = %d, want 1:\n%s", got, b.query)
	}
}

func TestQueryFamilyDimensionCountsBuilder_WhereClauses(t *testing.T) {
	r := dimensionCountReader()
	b := r.queryFamilyDimensionCountsBuilder(dimensionCountOpts())

	for _, want := range []string{
		"event_time >= ?",
		"event_time <= ?",
		"type IN ('QueryFinish', 'ExceptionWhileProcessing')",
		"normalized_query_hash != 0",
		"dimension_value != ''",
		"HAVING count() >= ?",
	} {
		if !strings.Contains(b.query, want) {
			t.Errorf("dimension SQL missing %q:\n%s", want, b.query)
		}
	}
}

func TestQueryFamilyDimensionCountsBuilder_TopKIsLiteralNotParam(t *testing.T) {
	r := dimensionCountReader()
	opts := dimensionCountOpts()
	opts.TopK = 7
	b := r.queryFamilyDimensionCountsBuilder(opts)

	if !strings.Contains(b.query, "LIMIT 7 BY normalized_query_hash, dimension_name") {
		t.Errorf("TopK should be formatted as an integer literal in LIMIT n BY:\n%s", b.query)
	}
	if strings.Contains(b.query, "LIMIT ? BY") {
		t.Errorf("TopK must not be a bound parameter in LIMIT n BY:\n%s", b.query)
	}
	// The window bounds, min executions, and group limit stay bound params:
	// start, end (outer) + start, end (subquery) + min + group limit = 6.
	if len(b.params) != 6 {
		t.Fatalf("params = %d, want 6 bound runtime values: %#v", len(b.params), b.params)
	}
	if b.params[4] != uint64(3) {
		t.Errorf("min executions param = %#v, want uint64(3)", b.params[4])
	}
	if b.params[5] != 200 {
		t.Errorf("group limit param = %#v, want 200", b.params[5])
	}
}

func TestQueryFamilyDimensionCountsBuilder_UsesQueryLogRef(t *testing.T) {
	r := &ClickHouseReader{
		queryTimeout:       time.Second,
		queryLogRef:        "cluster('prod', system.query_log)",
		queryLogNormalized: true,
	}
	b := r.queryFamilyDimensionCountsBuilder(dimensionCountOpts())

	if !strings.Contains(b.query, "cluster('prod', system.query_log)") {
		t.Errorf("dimension SQL should use the resolved cluster-aware queryLogRef:\n%s", b.query)
	}
	if strings.Contains(b.query, "{TABLE_QUERY_LOG}") {
		t.Errorf("dimension SQL left an unresolved table placeholder:\n%s", b.query)
	}
}

func TestQueryFamilyDimensionCountsBuilder_AppliesUserFilters(t *testing.T) {
	r := dimensionCountReader()
	r.whitelistUsers = []string{"api", "worker"}
	b := r.queryFamilyDimensionCountsBuilder(dimensionCountOpts())

	// The whitelist clause appears in both the outer query and the subquery.
	if got := strings.Count(b.query, "AND user IN ?"); got != 2 {
		t.Errorf("AND user IN ? count = %d, want 2 (outer + subquery):\n%s", got, b.query)
	}
}

func TestFetchQueryFamilyDimensionCounts_ScansAndSortsDeterministically(t *testing.T) {
	// One scan returns interleaved (hash, dimension_name, value, count) rows in
	// arbitrary order; the fetch must map dimensions and re-sort into the
	// stable hash / allowlist-order / count-desc / value-asc shape.
	conn := &fakeConn{rows: newFakeRows(
		[]any{uint64(200), "user", "worker", uint64(50)},
		[]any{uint64(100), "client_name", "ch-go", uint64(70)},
		[]any{uint64(100), "user", "api", uint64(10)},
		[]any{uint64(100), "user", "etl", uint64(90)},
		[]any{uint64(100), "client_hostname", "h1", uint64(80)},
	)}
	r := &ClickHouseReader{
		conn:               conn,
		queryTimeout:       time.Second,
		queryLogRef:        "system.query_log",
		queryLogNormalized: true,
	}

	counts, err := r.FetchQueryFamilyDimensionCounts(context.Background(), QueryFamilyDimensionCountOptions{
		StartTime:       time.Unix(1000, 0).UTC(),
		EndTime:         time.Unix(2000, 0).UTC(),
		ExactGroupLimit: 200,
	})
	if err != nil {
		t.Fatalf("FetchQueryFamilyDimensionCounts failed: %v", err)
	}

	// All three dimensions are counted in a single query_log scan.
	if conn.queryCalls != 1 {
		t.Errorf("query calls = %d, want 1 (single scan for all dimensions)", conn.queryCalls)
	}

	want := []QueryFamilyDimensionCount{
		{NormalizedQueryHash: 100, Dimension: QueryFamilyDimensionUser, Value: "etl", ExecutionCount: 90},
		{NormalizedQueryHash: 100, Dimension: QueryFamilyDimensionUser, Value: "api", ExecutionCount: 10},
		{NormalizedQueryHash: 100, Dimension: QueryFamilyDimensionClient, Value: "ch-go", ExecutionCount: 70},
		{NormalizedQueryHash: 100, Dimension: QueryFamilyDimensionHost, Value: "h1", ExecutionCount: 80},
		{NormalizedQueryHash: 200, Dimension: QueryFamilyDimensionUser, Value: "worker", ExecutionCount: 50},
	}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("counts = %+v\nwant %+v", counts, want)
	}
}

func TestFetchQueryFamilyDimensionCounts_UnsupportedWithoutNormalizedHash(t *testing.T) {
	r := &ClickHouseReader{queryTimeout: time.Second}
	_, err := r.FetchQueryFamilyDimensionCounts(context.Background(), QueryFamilyDimensionCountOptions{
		StartTime: time.Unix(1000, 0).UTC(),
		EndTime:   time.Unix(2000, 0).UTC(),
	})
	if err != ErrQueryFamilyRollupsUnsupported {
		t.Fatalf("err = %v, want ErrQueryFamilyRollupsUnsupported", err)
	}
}

func TestNormalizeQueryFamilyDimensionCountOptions(t *testing.T) {
	start := time.Unix(1000, 0).UTC()
	end := time.Unix(2000, 0).UTC()

	got, err := normalizeQueryFamilyDimensionCountOptions(QueryFamilyDimensionCountOptions{StartTime: start, EndTime: end})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.MinExecutionCount != 1 {
		t.Errorf("MinExecutionCount = %d, want default 1", got.MinExecutionCount)
	}
	if got.ExactGroupLimit != defaultQueryFamilyExactGroupLimit {
		t.Errorf("ExactGroupLimit = %d, want default", got.ExactGroupLimit)
	}
	if got.TopK != queryfamily.TopKLimit {
		t.Errorf("TopK = %d, want default %d", got.TopK, queryfamily.TopKLimit)
	}

	if _, err := normalizeQueryFamilyDimensionCountOptions(QueryFamilyDimensionCountOptions{}); err == nil {
		t.Error("expected error for missing time bounds")
	}
	if _, err := normalizeQueryFamilyDimensionCountOptions(QueryFamilyDimensionCountOptions{StartTime: end, EndTime: start}); err == nil {
		t.Error("expected error for start after end")
	}
	if _, err := normalizeQueryFamilyDimensionCountOptions(QueryFamilyDimensionCountOptions{StartTime: start, EndTime: end, TopK: maxQueryFamilyDimensionTopK + 1}); err == nil {
		t.Error("expected error for TopK over the maximum")
	}

	got, err = normalizeQueryFamilyDimensionCountOptions(QueryFamilyDimensionCountOptions{StartTime: start, EndTime: end, ExactGroupLimit: 999999})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ExactGroupLimit != maxQueryFamilyExactGroupLimit {
		t.Errorf("ExactGroupLimit = %d, want clamped max %d", got.ExactGroupLimit, maxQueryFamilyExactGroupLimit)
	}
}
