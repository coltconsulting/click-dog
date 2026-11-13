package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/model"
)

// captureConn records each Query call and returns a per-call canned result so
// the trace-drilldown reads can be exercised without a live ClickHouse.
type captureConn struct {
	*fakeConn
	queries [][]any
	sqls    []string
	rowsFn  func(query string) (driver.Rows, error)
}

func (c *captureConn) Query(_ context.Context, query string, args ...any) (driver.Rows, error) {
	c.sqls = append(c.sqls, query)
	c.queries = append(c.queries, append([]any(nil), args...))
	if c.rowsFn != nil {
		return c.rowsFn(query)
	}
	return newFakeRows(), nil
}

func newTraceDrilldownReader(rowsFn func(query string) (driver.Rows, error)) (*ClickHouseReader, *captureConn) {
	conn := &captureConn{fakeConn: &fakeConn{}, rowsFn: rowsFn}
	r := &ClickHouseReader{
		conn:             conn,
		queryTimeout:     time.Second,
		spanLogRef:       "system.opentelemetry_span_log",
		queryLogRef:      "system.query_log",
		traceIDByQueryID: strings.ReplaceAll(traceIDByQueryIDSelectSQL, tblSpanLog, "system.opentelemetry_span_log"),
		spanSelect:       strings.ReplaceAll(spanSelectSQL, tblSpanLog, "system.opentelemetry_span_log"),
	}
	return r, conn
}

func TestFetchTraceIDsByQueryID_BuildsExpectedQuery(t *testing.T) {
	traceA := uuid.New()
	traceB := uuid.New()
	since := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	startA := uint64(since.Add(10 * time.Minute).UnixMicro())
	startB := uint64(since.Add(-2 * time.Hour).UnixMicro())
	reader, conn := newTraceDrilldownReader(func(string) (driver.Rows, error) {
		return newFakeRows([]any{traceA, startA}, []any{traceB, startB}), nil
	})

	ids, queryStart, err := reader.FetchTraceIDsByQueryID(context.Background(), "q-123", 1, since, 500)
	if err != nil {
		t.Fatalf("FetchTraceIDsByQueryID: %v", err)
	}
	if len(ids) != 2 || ids[0] != traceA.String() || ids[1] != traceB.String() {
		t.Fatalf("trace IDs = %v, want [%s %s]", ids, traceA, traceB)
	}
	// The earliest matched start, even one before the window: a long query
	// that finished inside it.
	if want := time.UnixMicro(int64(startB)); !queryStart.Equal(want) {
		t.Errorf("query start = %s, want the earliest matched start %s", queryStart, want)
	}

	// The SQL must match the spec's query-id -> trace-id query exactly.
	sql := conn.sqls[0]
	for _, want := range []string{
		"SELECT trace_id, min(start_time_us) AS min_start_time",
		"FROM system.opentelemetry_span_log",
		"finish_date >= today() - INTERVAL ? DAY",
		"finish_time_us >= ?",
		"attribute['clickhouse.query_id'] = ?",
		"GROUP BY trace_id",
		"LIMIT ?",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("query-id->trace-id SQL missing %q:\n%s", want, sql)
		}
	}
	// Args order: lookbackDays, finish floor, queryID, limit.
	args := conn.queries[0]
	if len(args) != 4 {
		t.Fatalf("args = %v, want [days floor queryID limit]", args)
	}
	if args[0] != 1 {
		t.Errorf("lookbackDays arg = %v, want 1", args[0])
	}
	if want := uint64(since.Add(-traceStartMargin).UnixMicro()); args[1] != want {
		t.Errorf("finish floor arg = %v, want %d (one margin before the window start)", args[1], want)
	}
	if args[2] != "q-123" {
		t.Errorf("queryID arg = %v, want q-123", args[2])
	}
	if args[3] != 500 {
		t.Errorf("limit arg = %v, want 500", args[3])
	}
}

// TestFetchTraceIDsByQueryID_ZeroSinceReadsTheDayRange keeps a caller without
// a window from building an underflowed floor: it reads the whole day range,
// as before the floor existed.
func TestFetchTraceIDsByQueryID_ZeroSinceReadsTheDayRange(t *testing.T) {
	reader, conn := newTraceDrilldownReader(func(string) (driver.Rows, error) {
		return newFakeRows(), nil
	})
	ids, queryStart, err := reader.FetchTraceIDsByQueryID(context.Background(), "q-1", 1, time.Time{}, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 0 || !queryStart.IsZero() {
		t.Errorf("no match = %v, %s; want no IDs and the zero time", ids, queryStart)
	}
	if conn.queries[0][1] != uint64(0) {
		t.Errorf("finish floor = %v, want 0 for a zero since", conn.queries[0][1])
	}
}

func TestFetchTraceIDsByQueryID_EmptyQueryIDSkipsQuery(t *testing.T) {
	reader, conn := newTraceDrilldownReader(nil)
	ids, _, err := reader.FetchTraceIDsByQueryID(context.Background(), "", 1, time.Time{}, 500)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("ids = %v, want empty for an empty query ID", ids)
	}
	if len(conn.sqls) != 0 {
		t.Errorf("empty query ID should not issue a query, got %d", len(conn.sqls))
	}
}

func TestFetchTraceIDsByQueryID_LookbackDaysFlooredAtOne(t *testing.T) {
	reader, conn := newTraceDrilldownReader(func(string) (driver.Rows, error) {
		return newFakeRows(), nil
	})
	if _, _, err := reader.FetchTraceIDsByQueryID(context.Background(), "q-1", 0, time.Time{}, 100); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conn.queries[0][0] != 1 {
		t.Errorf("lookbackDays floored to %v, want 1", conn.queries[0][0])
	}
}

func TestFetchTraceIDsByQueryID_QueryErrorPropagates(t *testing.T) {
	reader, _ := newTraceDrilldownReader(func(string) (driver.Rows, error) {
		return nil, errors.New("ACCESS_DENIED")
	})
	if _, _, err := reader.FetchTraceIDsByQueryID(context.Background(), "q-1", 1, time.Time{}, 100); err == nil {
		t.Fatal("expected the query error to propagate")
	}
}

func TestFetchSpansForTraceIDs_FetchesAllSpansNoDurationSelection(t *testing.T) {
	traceID := uuid.New()
	spanRow := []any{
		"host-1",            // hostname
		traceID,             // trace_id
		uint64(10),          // span_id
		uint64(0),           // parent_span_id
		"Query",             // operation_name
		"server",            // kind
		uint64(1_000_000),   // start_time_us
		uint64(3_000_000),   // finish_time_us
		time.Now().UTC(),    // finish_date
		map[string]string{}, // attribute
	}
	reader, conn := newTraceDrilldownReader(func(string) (driver.Rows, error) {
		return newFakeRows(spanRow), nil
	})

	spans, err := reader.FetchSpansForTraceIDs(context.Background(), []string{traceID.String()}, 1, time.Time{}, 1000, []string{"MergeTreeIndex"})
	if err != nil {
		t.Fatalf("FetchSpansForTraceIDs: %v", err)
	}
	if len(spans) != 1 || spans[0].SpanID != 10 {
		t.Fatalf("spans = %+v, want one span with id 10", spans)
	}

	sql := conn.sqls[0]
	// Must reuse the trace-id scan shape (WHERE trace_id IN ?), apply the
	// blacklist, and must NOT run duration-based trace selection.
	for _, want := range []string{"WHERE trace_id IN ?", "operation_name NOT LIKE ?", "LIMIT ?"} {
		if !strings.Contains(sql, want) {
			t.Errorf("span SQL missing %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "GROUP BY trace_id") || strings.Contains(sql, "max_finish_time") {
		t.Errorf("span fetch must not re-run duration-based trace selection:\n%s", sql)
	}
	if strings.Contains(sql, "(finish_time_us - start_time_us) >=") {
		t.Errorf("span fetch must not apply duration filters:\n%s", sql)
	}
}

// traceSpanRow is one span-log row in spanSelectSQL's column order.
func traceSpanRow(traceID uuid.UUID, spanID uint64, operation string, start, finish time.Time) []any {
	return []any{
		"host-1", traceID, spanID, uint64(0), operation, "server",
		uint64(start.UnixMicro()), uint64(finish.UnixMicro()), finish.UTC(), map[string]string{},
	}
}

// TestFetchSpansForTraceIDs_ReadsFromSince pins the span fetch's finish_time_us
// floor: one margin before since, after the day-floor and before the blacklist
// and limit arguments. A span that started inside the margin needs no second
// read: everything under it finished after it started, inside the floor.
func TestFetchSpansForTraceIDs_ReadsFromSince(t *testing.T) {
	traceID := uuid.New()
	since := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	reader, conn := newTraceDrilldownReader(func(string) (driver.Rows, error) {
		return newFakeRows(
			traceSpanRow(traceID, 1, "query", since.Add(-30*time.Second), since.Add(time.Second)),
			traceSpanRow(traceID, 2, "QueryPipelineEx::execute", since, since.Add(time.Second)),
		), nil
	})

	spans, err := reader.FetchSpansForTraceIDs(context.Background(), []string{traceID.String()}, 2, since, 1000, []string{"MergeTreeIndex"})
	if err != nil {
		t.Fatalf("FetchSpansForTraceIDs: %v", err)
	}
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want 2", len(spans))
	}
	if len(conn.sqls) != 1 {
		t.Fatalf("reads = %d, want 1 when no span started before the floor:\n%s", len(conn.sqls), strings.Join(conn.sqls, "\n---\n"))
	}
	sql := conn.sqls[0]
	if !strings.Contains(sql, "AND finish_time_us >= ?") || strings.Contains(sql, "finish_time_us < ?") {
		t.Errorf("span SQL must bound finish_time_us from below only:\n%s", sql)
	}
	args := conn.queries[0]
	floor := uint64(since.Add(-traceStartMargin).UnixMicro())
	want := []any{[]uuid.UUID{traceID}, 2, floor, "%MergeTreeIndex%", 1000}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want [traceIDs days floor blacklist limit]", args)
	}
	if got := args[0].([]uuid.UUID); len(got) != 1 || got[0] != traceID {
		t.Errorf("trace IDs arg = %v, want [%s]", got, traceID)
	}
	for i := 1; i < len(want); i++ {
		if args[i] != want[i] {
			t.Errorf("arg %d = %v, want %v", i, args[i], want[i])
		}
	}
}

// TestFetchSpansForTraceIDs_ReadsALongQueryWhole is a query that started two
// hours before since and finished after it. Its `query` span is in the first
// read; the spans it ran that finished early are reached by a second read of
// only the gap, back to one margin before the query started.
func TestFetchSpansForTraceIDs_ReadsALongQueryWhole(t *testing.T) {
	traceID := uuid.New()
	since := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	queryStart := since.Add(-2 * time.Hour)
	reader, conn := newTraceDrilldownReader(func(query string) (driver.Rows, error) {
		if strings.Contains(query, "finish_time_us < ?") {
			return newFakeRows(
				traceSpanRow(traceID, 3, "MergeTreeRead", queryStart.Add(time.Second), queryStart.Add(time.Minute)),
			), nil
		}
		return newFakeRows(
			traceSpanRow(traceID, 1, "query", queryStart, since.Add(5*time.Minute)),
			traceSpanRow(traceID, 2, "MergeTreeRead", since.Add(-time.Second), since.Add(time.Minute)),
		), nil
	})

	spans, err := reader.FetchSpansForTraceIDs(context.Background(), []string{traceID.String()}, 1, since, 500, nil)
	if err != nil {
		t.Fatalf("FetchSpansForTraceIDs: %v", err)
	}
	var ids []uint64
	for _, span := range spans {
		ids = append(ids, span.SpanID)
	}
	if fmt.Sprint(ids) != "[1 2 3]" {
		t.Fatalf("span IDs = %v, want [1 2 3]: the whole query", ids)
	}
	if len(conn.sqls) != 2 {
		t.Fatalf("reads = %d, want 2", len(conn.sqls))
	}
	if !strings.Contains(conn.sqls[1], "AND finish_time_us >= ? AND finish_time_us < ?") {
		t.Errorf("gap read must be bounded on both sides:\n%s", conn.sqls[1])
	}
	floor := uint64(since.Add(-traceStartMargin).UnixMicro())
	reach := uint64(queryStart.Add(-traceStartMargin).UnixMicro())
	args := conn.queries[1]
	if len(args) != 5 || args[1] != 1 || args[2] != reach || args[3] != floor || args[4] != 498 {
		t.Errorf("gap read args = %v, want [traceIDs 1 %d %d 498]", args, reach, floor)
	}
}

// TestFetchSpansForTraceIDs_ReadsAnOverlappingEarlierQueryWhole is an
// application trace with two queries: P ran 10:00-11:30 and Q 11:00-12:45,
// fetched by trace ID from a 12:00 window. Q's start reaches P's `query` span,
// whose own start reaches back further: the reads repeat until a read finds
// nothing that started before its floor, so P is not returned without the
// spans it ran early.
func TestFetchSpansForTraceIDs_ReadsAnOverlappingEarlierQueryWhole(t *testing.T) {
	traceID := uuid.New()
	at := func(h, m int) time.Time { return time.Date(2026, 6, 15, h, m, 0, 0, time.UTC) }
	reads := [][][]any{
		{traceSpanRow(traceID, 1, "query", at(11, 0), at(12, 45))},         // Q
		{traceSpanRow(traceID, 2, "query", at(10, 0), at(11, 30))},         // P
		{traceSpanRow(traceID, 3, "MergeTreeRead", at(10, 1), at(10, 30))}, // P's early span
	}
	calls := 0
	reader, conn := newTraceDrilldownReader(func(string) (driver.Rows, error) {
		calls++
		if calls > len(reads) {
			t.Fatalf("read %d: want %d reads", calls, len(reads))
		}
		return newFakeRows(reads[calls-1]...), nil
	})

	spans, err := reader.FetchSpansForTraceIDs(context.Background(), []string{traceID.String()}, 1, at(12, 0), 100, nil)
	if err != nil {
		t.Fatalf("FetchSpansForTraceIDs: %v", err)
	}
	if got := spanIDSet(spans); got != "[1 2 3]" {
		t.Fatalf("span IDs = %s, want [1 2 3]", got)
	}
	us := func(t time.Time) uint64 { return uint64(t.Add(-traceStartMargin).UnixMicro()) }
	wantBounds := [][2]uint64{
		{us(at(12, 0)), 0},
		{us(at(11, 0)), us(at(12, 0))},
		{us(at(10, 0)), us(at(11, 0))},
	}
	wantLimits := []int{100, 99, 98}
	for i, want := range wantBounds {
		args := conn.queries[i]
		if args[2] != want[0] {
			t.Errorf("read %d from = %v, want %d", i+1, args[2], want[0])
		}
		if want[1] == 0 {
			if len(args) != 4 || args[3] != wantLimits[i] {
				t.Errorf("read %d args = %v, want [traceIDs 1 %d %d]", i+1, args, want[0], wantLimits[i])
			}
			continue
		}
		if len(args) != 5 || args[3] != want[1] || args[4] != wantLimits[i] {
			t.Errorf("read %d args = %v, want [traceIDs 1 %d %d %d]", i+1, args, want[0], want[1], wantLimits[i])
		}
	}
}

// spanIDSet is a stable string of the span IDs in spans.
func spanIDSet(spans []model.OpenTelemetrySpan) string {
	ids := make([]uint64, len(spans))
	for i, span := range spans {
		ids[i] = span.SpanID
	}
	slices.Sort(ids)
	return fmt.Sprint(ids)
}

func TestServerTime_ReadsTheServerClock(t *testing.T) {
	want := time.Date(2026, 6, 15, 12, 0, 0, 123456000, time.UTC)
	conn := &fakeConn{row: fakeRow{vals: []any{want.UnixMicro()}}}
	reader := &ClickHouseReader{conn: conn, queryTimeout: time.Second}
	got, err := reader.ServerTime(context.Background())
	if err != nil {
		t.Fatalf("ServerTime: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("server time = %s, want %s", got, want)
	}
	if conn.lastQueryRow != serverTimeQuery {
		t.Errorf("query = %q, want %q", conn.lastQueryRow, serverTimeQuery)
	}

	conn.row = fakeRow{err: errors.New("TIMEOUT_EXCEEDED")}
	if _, err := reader.ServerTime(context.Background()); err == nil {
		t.Error("expected the query error to propagate")
	}
}

// TestFetchSpansForTraceIDs_NoGapReadAtTheLimit keeps a full first read from
// reading more: the limit is already reached.
func TestFetchSpansForTraceIDs_NoGapReadAtTheLimit(t *testing.T) {
	traceID := uuid.New()
	since := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	reader, conn := newTraceDrilldownReader(func(string) (driver.Rows, error) {
		return newFakeRows(
			traceSpanRow(traceID, 1, "query", since.Add(-2*time.Hour), since.Add(time.Minute)),
			traceSpanRow(traceID, 2, "MergeTreeRead", since, since.Add(time.Second)),
		), nil
	})
	spans, err := reader.FetchSpansForTraceIDs(context.Background(), []string{traceID.String()}, 1, since, 2, nil)
	if err != nil {
		t.Fatalf("FetchSpansForTraceIDs: %v", err)
	}
	if len(spans) != 2 || len(conn.sqls) != 1 {
		t.Errorf("spans/reads = %d/%d, want 2/1", len(spans), len(conn.sqls))
	}
}

func TestFetchSpansForTraceIDs_GapReadErrorPropagates(t *testing.T) {
	traceID := uuid.New()
	since := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	reader, _ := newTraceDrilldownReader(func(query string) (driver.Rows, error) {
		if strings.Contains(query, "finish_time_us < ?") {
			return nil, errors.New("TIMEOUT_EXCEEDED")
		}
		return newFakeRows(traceSpanRow(traceID, 1, "query", since.Add(-time.Hour), since.Add(time.Minute))), nil
	})
	spans, err := reader.FetchSpansForTraceIDs(context.Background(), []string{traceID.String()}, 1, since, 100, nil)
	if err == nil || spans != nil {
		t.Fatalf("spans, err = %v, %v; want the gap read's error and no spans", spans, err)
	}
}

// ---------------------------------------------------------------------------
// Recent candidate search builder (Phase 2)
// ---------------------------------------------------------------------------

func newRecentCandidateReader() *ClickHouseReader {
	return &ClickHouseReader{
		queryLogSelect: buildQueryLogSQL(queryLogSelectSQL, "system.query_log", true),
	}
}

func TestRecentQueryCandidatesBuilder_WindowAndType(t *testing.T) {
	r := newRecentCandidateReader()
	start := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	b := r.recentQueryCandidatesBuilder(RecentQueryCandidateOptions{
		StartTime: start,
		EndTime:   end,
		Limit:     20,
	})

	for _, want := range []string{
		"FROM system.query_log",
		"event_time >= ? AND event_time <= ?",
		"type IN ('QueryFinish', 'ExceptionWhileProcessing')",
		"ORDER BY query_duration_ms DESC, event_time DESC, query_id ASC",
		"LIMIT ?",
	} {
		if !strings.Contains(b.query, want) {
			t.Errorf("candidate SQL missing %q:\n%s", want, b.query)
		}
	}
	// The window must be the first two bound params, the limit the last.
	if len(b.params) < 3 {
		t.Fatalf("params = %v, want at least [start end ... limit]", b.params)
	}
	if b.params[0] != start || b.params[1] != end {
		t.Errorf("window params = %v, want [%s %s]", b.params[:2], start, end)
	}
	if b.params[len(b.params)-1] != 20 {
		t.Errorf("limit param = %v, want 20", b.params[len(b.params)-1])
	}
}

func TestRecentQueryCandidatesBuilder_PicksUpUserFilters(t *testing.T) {
	r := newRecentCandidateReader()
	r.whitelistUsers = []string{"app_frontend"}
	r.blacklistUsers = []string{"patient_records"}

	b := r.recentQueryCandidatesBuilder(RecentQueryCandidateOptions{
		StartTime: time.Now().Add(-time.Hour),
		EndTime:   time.Now(),
		Limit:     10,
	})
	if !strings.Contains(b.query, "AND if(initial_user != '', initial_user, user) IN ?") {
		t.Errorf("candidate builder did not splice whitelist clause\nSQL: %s", b.query)
	}
	if !strings.Contains(b.query, "AND user NOT IN ?") {
		t.Errorf("candidate builder did not splice blacklist clause\nSQL: %s", b.query)
	}
}

func TestRecentQueryCandidatesBuilder_NoUserFilterByDefault(t *testing.T) {
	r := newRecentCandidateReader()
	b := r.recentQueryCandidatesBuilder(RecentQueryCandidateOptions{
		StartTime: time.Now().Add(-time.Hour),
		EndTime:   time.Now(),
		Limit:     10,
	})
	if strings.Contains(b.query, "user IN") || strings.Contains(b.query, "user NOT IN") {
		t.Errorf("expected no user clauses with no filter configured\nSQL: %s", b.query)
	}
}

func TestRecentQueryCandidatesBuilder_DurationPosture(t *testing.T) {
	r := newRecentCandidateReader()
	b := r.recentQueryCandidatesBuilder(RecentQueryCandidateOptions{
		StartTime:      time.Now().Add(-time.Hour),
		EndTime:        time.Now(),
		MinDurationMs:  500,
		MaxDurationMs:  9000,
		MaxQueryLength: 4096,
		Limit:          5,
	})
	for _, want := range []string{
		"AND query_duration_ms >= ?",
		"AND query_duration_ms <= ?",
		"AND length(query) <= ?",
	} {
		if !strings.Contains(b.query, want) {
			t.Errorf("candidate SQL missing duration/length clause %q:\n%s", want, b.query)
		}
	}
}

// newRecentCandidateReaderNormalized builds a reader whose query_log SELECT
// carries the normalized columns and whose queryLogNormalized flag is set, so
// the builder emits the hash predicate and the normalizeQuery(query) match term.
func newRecentCandidateReaderNormalized() *ClickHouseReader {
	return &ClickHouseReader{
		queryLogSelect:     buildQueryLogSQL(queryLogSelectSQL, "system.query_log", true),
		queryLogNormalized: true,
	}
}

func TestRecentQueryCandidatesBuilder_MatchPushedToSQLBeforeLimit(t *testing.T) {
	r := newRecentCandidateReaderNormalized()
	start := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	b := r.recentQueryCandidatesBuilder(RecentQueryCandidateOptions{
		StartTime: start,
		EndTime:   end,
		Match:     "events",
		Limit:     20,
	})

	// The match OR-group must use case-insensitive position over each scalar
	// field and arrayExists over the array columns — and it must sit BEFORE the
	// ORDER BY ... LIMIT so the LIMIT bounds the matched rows, not a
	// cost-truncated set.
	for _, want := range []string{
		"positionCaseInsensitive(query_id, ?) > 0",
		"positionCaseInsensitive(user, ?) > 0",
		"positionCaseInsensitive(client_name, ?) > 0",
		"positionCaseInsensitive(client_hostname, ?) > 0",
		"positionCaseInsensitive(normalizeQuery(query), ?) > 0",
		"arrayExists(x -> positionCaseInsensitive(x, ?) > 0, tables)",
		"arrayExists(x -> positionCaseInsensitive(x, ?) > 0, databases)",
	} {
		if !strings.Contains(b.query, want) {
			t.Errorf("candidate SQL missing match term %q:\n%s", want, b.query)
		}
	}
	matchIdx := strings.Index(b.query, "positionCaseInsensitive")
	orderIdx := strings.Index(b.query, "ORDER BY query_duration_ms DESC")
	if matchIdx < 0 || orderIdx < 0 || matchIdx > orderIdx {
		t.Errorf("match predicate must appear in WHERE before ORDER BY/LIMIT:\n%s", b.query)
	}

	// Seven terms (4 scalar + normalized + 2 array) each bind the needle once, in
	// order, between the duration param and the trailing LIMIT: 2 window + 1
	// min-duration + 7 needles + 1 limit = 11.
	if len(b.params) != 11 {
		t.Fatalf("params = %v, want [start end minDur + 7 needles + limit] (len 11)", b.params)
	}
}

func TestRecentQueryCandidatesBuilder_MatchNeedleParamsBoundPerTerm(t *testing.T) {
	r := newRecentCandidateReaderNormalized()
	start := time.Now().Add(-time.Hour)
	end := time.Now()
	b := r.recentQueryCandidatesBuilder(RecentQueryCandidateOptions{
		StartTime: start,
		EndTime:   end,
		Match:     "needle",
		Limit:     7,
	})

	// Param order: [start, end, minDur(0), needle x7, limit]. Window first, LIMIT
	// last, exactly one needle per OR term in between (the always-present
	// min-duration param sits between the window and the needles).
	if len(b.params) != 11 {
		t.Fatalf("params = %v, want 11 (start end minDur + 7 needles + limit)", b.params)
	}
	if b.params[0] != start || b.params[1] != end {
		t.Errorf("window params = %v, want [%s %s]", b.params[:2], start, end)
	}
	needleCount := 0
	for _, p := range b.params {
		if p == "needle" {
			needleCount++
		}
	}
	if needleCount != 7 {
		t.Errorf("needle bound %d times, want 7 (one per OR term)", needleCount)
	}
	if b.params[len(b.params)-1] != 7 {
		t.Errorf("limit param = %v, want 7 (must be last)", b.params[len(b.params)-1])
	}
}

func TestRecentQueryCandidatesBuilder_MatchDropsNormalizedWhenUnsupported(t *testing.T) {
	// Without normalized support the match OR-group must fall back to metadata
	// terms only — the normalizeQuery(query) term must not appear (the column /
	// expression is unavailable).
	r := newRecentCandidateReader() // queryLogNormalized defaults false
	b := r.recentQueryCandidatesBuilder(RecentQueryCandidateOptions{
		StartTime: time.Now().Add(-time.Hour),
		EndTime:   time.Now(),
		Match:     "events",
		Limit:     5,
	})
	// The SELECT column list always carries normalizeQuery(query) AS
	// normalized_query; what must be dropped is the WHERE match term over it.
	if strings.Contains(b.query, "positionCaseInsensitive(normalizeQuery(query), ?)") {
		t.Errorf("unsupported-normalized match must drop the normalizeQuery match term:\n%s", b.query)
	}
	for _, want := range []string{
		"positionCaseInsensitive(query_id, ?) > 0",
		"arrayExists(x -> positionCaseInsensitive(x, ?) > 0, tables)",
	} {
		if !strings.Contains(b.query, want) {
			t.Errorf("metadata-only match SQL missing %q:\n%s", want, b.query)
		}
	}
	// 4 scalar + 2 array = 6 needles (no normalized term) + window(2) +
	// minDur(1) + limit(1) = 10.
	if len(b.params) != 10 {
		t.Fatalf("params = %v, want 10 (start end minDur + 6 needles + limit)", b.params)
	}
}

func TestRecentQueryCandidatesBuilder_HashPushedToSQL(t *testing.T) {
	r := newRecentCandidateReaderNormalized()
	start := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	b := r.recentQueryCandidatesBuilder(RecentQueryCandidateOptions{
		StartTime:           start,
		EndTime:             end,
		NormalizedQueryHash: 987654321,
		HasHash:             true,
		Limit:               20,
	})

	if !strings.Contains(b.query, "AND normalized_query_hash = ?") {
		t.Errorf("hash identity must be pushed into the WHERE clause:\n%s", b.query)
	}
	hashIdx := strings.Index(b.query, "normalized_query_hash = ?")
	orderIdx := strings.Index(b.query, "ORDER BY query_duration_ms DESC")
	if hashIdx < 0 || orderIdx < 0 || hashIdx > orderIdx {
		t.Errorf("hash predicate must appear before ORDER BY/LIMIT so LIMIT bounds the lookup:\n%s", b.query)
	}
	// Param order: [start, end, minDur(0), hash, limit]. The always-present
	// min-duration param sits between the window and the hash.
	if len(b.params) != 5 {
		t.Fatalf("params = %v, want [start end minDur hash limit]", b.params)
	}
	if b.params[3] != uint64(987654321) {
		t.Errorf("hash param = %v, want 987654321", b.params[3])
	}
	if b.params[len(b.params)-1] != 20 {
		t.Errorf("limit param = %v, want 20 (must be last)", b.params[len(b.params)-1])
	}
}

func TestRecentQueryCandidatesBuilder_HashSkippedWhenNormalizedUnsupported(t *testing.T) {
	// Defense in depth: even if HasHash reaches the builder without normalized
	// support, the column reference must NOT be emitted (the command gates this
	// case to an empty list + warning, but the builder must never produce a query
	// against a missing column).
	r := newRecentCandidateReader() // queryLogNormalized false
	b := r.recentQueryCandidatesBuilder(RecentQueryCandidateOptions{
		StartTime:           time.Now().Add(-time.Hour),
		EndTime:             time.Now(),
		NormalizedQueryHash: 42,
		HasHash:             true,
		Limit:               5,
	})
	if strings.Contains(b.query, "normalized_query_hash = ?") {
		t.Errorf("hash predicate must be skipped without normalized support:\n%s", b.query)
	}
}

func TestFetchRecentQueryCandidates_ScansRows(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 30, 0, 0, time.UTC)
	// queryLogSelect carries the raw query column for backfill, but the
	// candidate API must clear it and return only the normalized preview.
	conn := &fakeConn{rows: newFakeRows([]any{
		"qid-1", "QueryFinish", now, uint64(1500), "SELECT * FROM events WHERE id = 7",
		uint64(987654321), "SELECT * FROM events WHERE id = ?",
		"default", "default", "clickhouse-go", "app-1", "10.0.0.1", "10.0.0.1",
		[]string{"default"}, []string{"default.events"}, int32(0),
		uint64(100), uint64(200), uint64(0), uint64(0), uint64(10), uint64(20), uint64(300),
	})}
	r := &ClickHouseReader{
		conn:               conn,
		queryTimeout:       time.Second,
		queryLogSelect:     buildQueryLogSQL(queryLogSelectSQL, "system.query_log", true),
		queryLogNormalized: true,
	}

	rows, err := r.FetchRecentQueryCandidates(context.Background(), RecentQueryCandidateOptions{
		StartTime: now.Add(-time.Hour),
		EndTime:   now,
		Limit:     20,
	})
	if err != nil {
		t.Fatalf("FetchRecentQueryCandidates: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].QueryID != "qid-1" || rows[0].NormalizedQueryHash != 987654321 {
		t.Errorf("scanned candidate = %+v", rows[0])
	}
	if rows[0].NormalizedQuery != "SELECT * FROM events WHERE id = ?" {
		t.Errorf("normalized preview = %q", rows[0].NormalizedQuery)
	}
	if rows[0].Query != "" {
		t.Errorf("raw query = %q, want it cleared at the candidate API boundary", rows[0].Query)
	}
}

// ---------------------------------------------------------------------------
// Current candidate search builder (Phase 4: system.processes)
// ---------------------------------------------------------------------------

// newCurrentCandidateReader builds a reader whose system.processes SELECT omits
// the normalized preview column (no normalized support), matching a ClickHouse
// without normalizeQuery support.
func newCurrentCandidateReader() *ClickHouseReader {
	return &ClickHouseReader{
		currentQuerySelect: buildCurrentQuerySQL(currentQueryCandidatesSelectSQL, "system.processes", false),
	}
}

// newCurrentCandidateReaderNormalized builds a reader whose system.processes
// SELECT carries the normalizeQuery(query) preview column and whose normalized
// flag is set, so the builder emits the normalized match term.
func newCurrentCandidateReaderNormalized() *ClickHouseReader {
	return &ClickHouseReader{
		currentQuerySelect: buildCurrentQuerySQL(currentQueryCandidatesSelectSQL, "system.processes", true),
		queryLogNormalized: true,
	}
}

func TestCurrentQueryCandidatesBuilder_ColumnsAndOrder(t *testing.T) {
	r := newCurrentCandidateReaderNormalized()
	b := r.currentQueryCandidatesBuilder(CurrentQueryCandidateOptions{Limit: 20})

	for _, want := range []string{
		"FROM system.processes",
		"query_id",
		"toUInt64(elapsed * 1000) AS elapsed_ms",
		"normalizeQuery(query) AS normalized_query",
		"IPv6NumToString(address) AS client_address",
		"WHERE query_id != ''",
		"ORDER BY elapsed_ms DESC, query_id ASC",
		"LIMIT ?",
	} {
		if !strings.Contains(b.query, want) {
			t.Errorf("current candidate SQL missing %q:\n%s", want, b.query)
		}
	}
	// Raw query text must never be selected.
	if strings.Contains(b.query, " query,") || strings.Contains(b.query, "\tquery\n") {
		t.Errorf("current candidate SQL must not select raw query text:\n%s", b.query)
	}
	// system.processes exposes the client IP as `address`, not `client_address`;
	// selecting `client_address` would fail the query with an unknown identifier.
	if strings.Contains(b.query, "IPv6NumToString(client_address)") {
		t.Errorf("current candidate SQL must read the `address` column, not `client_address`:\n%s", b.query)
	}
	if len(b.params) != 1 || b.params[0] != 20 {
		t.Errorf("params = %v, want [20] (just the limit)", b.params)
	}
}

func TestCurrentQueryCandidatesBuilder_NormalizedPreviewGatedOnSupport(t *testing.T) {
	// Without normalized support, the preview column must NOT appear — raw query
	// text is never selected, so the candidate simply carries no preview.
	r := newCurrentCandidateReader()
	b := r.currentQueryCandidatesBuilder(CurrentQueryCandidateOptions{Limit: 5})
	if strings.Contains(b.query, "normalizeQuery(query)") {
		t.Errorf("normalized preview must be dropped without support:\n%s", b.query)
	}
	if strings.Contains(b.query, "AS normalized_query") {
		t.Errorf("normalized_query column must be omitted without support:\n%s", b.query)
	}
}

func TestCurrentQueryCandidatesBuilder_PicksUpUserFilters(t *testing.T) {
	r := newCurrentCandidateReaderNormalized()
	r.whitelistUsers = []string{"app_frontend"}
	r.blacklistUsers = []string{"patient_records"}
	b := r.currentQueryCandidatesBuilder(CurrentQueryCandidateOptions{Limit: 10})
	if !strings.Contains(b.query, "AND if(initial_user != '', initial_user, user) IN ?") {
		t.Errorf("current builder did not splice whitelist clause:\n%s", b.query)
	}
	if !strings.Contains(b.query, "AND user NOT IN ?") {
		t.Errorf("current builder did not splice blacklist clause:\n%s", b.query)
	}
}

func TestCurrentQueryCandidatesBuilder_MatchPushedBeforeLimit(t *testing.T) {
	r := newCurrentCandidateReaderNormalized()
	b := r.currentQueryCandidatesBuilder(CurrentQueryCandidateOptions{
		Match: "events",
		Limit: 20,
	})

	for _, want := range []string{
		"positionCaseInsensitive(query_id, ?) > 0",
		"positionCaseInsensitive(user, ?) > 0",
		"positionCaseInsensitive(client_name, ?) > 0",
		"positionCaseInsensitive(client_hostname, ?) > 0",
		"positionCaseInsensitive(normalizeQuery(query), ?) > 0",
	} {
		if !strings.Contains(b.query, want) {
			t.Errorf("current match SQL missing term %q:\n%s", want, b.query)
		}
	}
	// system.processes has no tables/databases arrays.
	if strings.Contains(b.query, "arrayExists") {
		t.Errorf("current match must not reference array columns:\n%s", b.query)
	}
	matchIdx := strings.Index(b.query, "positionCaseInsensitive")
	orderIdx := strings.Index(b.query, "ORDER BY elapsed_ms DESC")
	if matchIdx < 0 || orderIdx < 0 || matchIdx > orderIdx {
		t.Errorf("match predicate must appear before ORDER BY/LIMIT:\n%s", b.query)
	}
	// 5 needles (4 scalar + normalized) + 1 limit.
	if len(b.params) != 6 {
		t.Fatalf("params = %v, want 6 (5 needles + limit)", b.params)
	}
	needleCount := 0
	for _, p := range b.params {
		if p == "events" {
			needleCount++
		}
	}
	if needleCount != 5 {
		t.Errorf("needle bound %d times, want 5 (one per OR term)", needleCount)
	}
	if b.params[len(b.params)-1] != 20 {
		t.Errorf("limit param = %v, want 20 (must be last)", b.params[len(b.params)-1])
	}
}

func TestCurrentQueryCandidatesBuilder_MatchDropsNormalizedWhenUnsupported(t *testing.T) {
	r := newCurrentCandidateReader() // no normalized support
	b := r.currentQueryCandidatesBuilder(CurrentQueryCandidateOptions{
		Match: "events",
		Limit: 5,
	})
	if strings.Contains(b.query, "positionCaseInsensitive(normalizeQuery(query), ?)") {
		t.Errorf("unsupported-normalized match must drop the normalizeQuery term:\n%s", b.query)
	}
	// 4 scalar needles (no normalized term) + limit.
	if len(b.params) != 5 {
		t.Fatalf("params = %v, want 5 (4 needles + limit)", b.params)
	}
}

func TestFetchCurrentQueryCandidates_ScansRows(t *testing.T) {
	// With normalized support the scan includes the preview column.
	conn := &fakeConn{rows: newFakeRows([]any{
		"running-1",                  // query_id
		uint64(4200),                 // elapsed_ms
		"SELECT * FROM events WHERE", // normalized_query (preview)
		"default",                    // user
		"clickhouse-go",              // client_name
		"app-1",                      // client_hostname
		"10.0.0.1",                   // client_address
	})}
	r := &ClickHouseReader{
		conn:               conn,
		queryTimeout:       time.Second,
		currentQuerySelect: buildCurrentQuerySQL(currentQueryCandidatesSelectSQL, "system.processes", true),
		queryLogNormalized: true,
	}

	rows, err := r.FetchCurrentQueryCandidates(context.Background(), CurrentQueryCandidateOptions{Limit: 20})
	if err != nil {
		t.Fatalf("FetchCurrentQueryCandidates: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].QueryID != "running-1" || rows[0].ElapsedMs != 4200 {
		t.Errorf("scanned current candidate = %+v", rows[0])
	}
	if rows[0].User != "default" || rows[0].ClientName != "clickhouse-go" {
		t.Errorf("dimensions not scanned: %+v", rows[0])
	}
}

func TestFetchCurrentQueryCandidates_ScansRowsWithoutNormalized(t *testing.T) {
	// Without normalized support the scan omits the preview column entirely.
	conn := &fakeConn{rows: newFakeRows([]any{
		"running-2",  // query_id
		uint64(1500), // elapsed_ms
		"etl",        // user
		"native",     // client_name
		"host-x",     // client_hostname
		"10.0.0.2",   // client_address
	})}
	r := &ClickHouseReader{
		conn:               conn,
		queryTimeout:       time.Second,
		currentQuerySelect: buildCurrentQuerySQL(currentQueryCandidatesSelectSQL, "system.processes", false),
		queryLogNormalized: false,
	}

	rows, err := r.FetchCurrentQueryCandidates(context.Background(), CurrentQueryCandidateOptions{Limit: 20})
	if err != nil {
		t.Fatalf("FetchCurrentQueryCandidates: %v", err)
	}
	if len(rows) != 1 || rows[0].QueryID != "running-2" || rows[0].ElapsedMs != 1500 {
		t.Fatalf("scanned current candidate = %+v", rows)
	}
	if rows[0].NormalizedQuery != "" {
		t.Errorf("normalized preview should be empty without support, got %q", rows[0].NormalizedQuery)
	}
}

func TestFetchSpansForTraceIDs_EmptyAndInvalidInputs(t *testing.T) {
	reader, conn := newTraceDrilldownReader(nil)

	spans, err := reader.FetchSpansForTraceIDs(context.Background(), nil, 1, time.Time{}, 1000, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(spans) != 0 {
		t.Errorf("spans = %+v, want empty for no trace IDs", spans)
	}

	// All-invalid trace IDs must short-circuit without a query.
	spans, err = reader.FetchSpansForTraceIDs(context.Background(), []string{"not-a-uuid"}, 1, time.Time{}, 1000, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(spans) != 0 {
		t.Errorf("spans = %+v, want empty for all-invalid trace IDs", spans)
	}
	if len(conn.sqls) != 0 {
		t.Errorf("all-invalid trace IDs should not issue a query, got %d", len(conn.sqls))
	}
}
