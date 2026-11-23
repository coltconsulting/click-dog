package clickhouse

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/model"
)

// TestFetchTraceNodes_GroupsNodesByTrace pins the trace filters'
// page-independent bridge from a trace to its queries and span tree: the read
// is scoped to the given traces, the lookback partition, and spans that
// finished from one margin before the caller's earliest span start; without an
// ancestor request it returns only spans carrying a query ID, and it groups
// each span's links, query flag, query ID, and statement per trace.
func TestFetchTraceNodes_GroupsNodesByTrace(t *testing.T) {
	traceA := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	traceB := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	conn := &captureConn{fakeConn: &fakeConn{}}
	conn.rowsFn = func(string) (driver.Rows, error) {
		return newFakeRows(
			[]any{traceA, uint64(1), uint64(0), uint8(1), "q-initial", "SELECT * FROM events"},
			[]any{traceA, uint64(2), uint64(9), uint8(1), "q-secondary", "SELECT * FROM events_local"},
			[]any{traceB, uint64(3), uint64(1), uint8(0), "q-other", ""},
		), nil
	}
	r := newTraceNodesReader(conn)

	const earliestStartUs = uint64(1_793_642_769_881_118)
	got, err := r.FetchTraceNodes(context.Background(), []uuid.UUID{traceA, traceB}, 0, earliestStartUs, model.AncestorRead{})
	if err != nil {
		t.Fatalf("FetchTraceNodes: %v", err)
	}
	if len(got[traceA]) != 2 || got[traceA][0] != (model.TraceNode{SpanID: 1, Query: true, QueryID: "q-initial", Statement: "SELECT * FROM events"}) ||
		got[traceA][1].ParentSpanID != 9 || got[traceA][1].Statement != "SELECT * FROM events_local" {
		t.Errorf("trace A nodes = %+v, want q-initial and q-secondary with links and statements", got[traceA])
	}
	if len(got[traceB]) != 1 || got[traceB][0].QueryID != "q-other" || got[traceB][0].Query {
		t.Errorf("trace B nodes = %+v, want [q-other], not a query span", got[traceB])
	}

	sql := conn.sqls[0]
	for _, want := range []string{"trace_id IN ?", "finish_date >= today() - INTERVAL ? DAY", "finish_time_us >= ?",
		"operation_name = 'query'", "AND (attribute['clickhouse.query_id'] != '')", "attribute['db.statement']"} {
		if !strings.Contains(sql, want) {
			t.Errorf("query lacks %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "parent_span_id FROM") {
		t.Errorf("query asks for ancestors nobody asked for:\n%s", sql)
	}
	args := conn.queries[0]
	wantFloor := earliestStartUs - uint64(time.Minute.Microseconds())
	if len(args) != 3 || args[1] != 1 || args[2] != wantFloor {
		t.Errorf("args = %v, want [traceIDs 1 %d] (lookback floored at one day, finish floored one minute before the earliest start)", args, wantFloor)
	}
}

// TestFetchTraceNodes_AsksForAncestors pins the ancestor request: spans that
// started by the asked time and are the parent of another span of the asked
// traces since the same floor, so leaves are never returned. The parent set is
// a GLOBAL subquery, so cluster mode runs it once, and the asked traces are
// bound once in it.
func TestFetchTraceNodes_AsksForAncestors(t *testing.T) {
	traceA := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	conn := &captureConn{fakeConn: &fakeConn{}}
	conn.rowsFn = func(string) (driver.Rows, error) { return newFakeRows(), nil }
	r := newTraceNodesReader(conn)

	const earliestStartUs = uint64(1_793_642_769_881_118)
	asked := []uuid.UUID{traceA}
	if _, err := r.FetchTraceNodes(context.Background(), []uuid.UUID{traceA}, 2, earliestStartUs, model.AncestorRead{Traces: asked, StartedByUs: 1_793_642_800_000_000}); err != nil {
		t.Fatalf("FetchTraceNodes: %v", err)
	}
	sql := conn.sqls[0]
	for _, want := range []string{
		"OR (start_time_us <= ? AND span_id GLOBAL IN (",
		"SELECT parent_span_id FROM system.opentelemetry_span_log",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("query lacks %q:\n%s", want, sql)
		}
	}
	if strings.Count(sql, "(") != strings.Count(sql, ")") {
		t.Errorf("unbalanced parentheses:\n%s", sql)
	}
	floor := earliestStartUs - uint64(time.Minute.Microseconds())
	want := []any{[]uuid.UUID{traceA}, 2, floor, uint64(1_793_642_800_000_000), asked, 2, floor}
	if !reflect.DeepEqual(conn.queries[0], want) {
		t.Errorf("args = %v, want %v", conn.queries[0], want)
	}
	if got := strings.Count(sql, "?"); got != len(want) {
		t.Errorf("SQL has %d placeholders for %d args:\n%s", got, len(want), sql)
	}
}

func newTraceNodesReader(conn driver.Conn) *ClickHouseReader {
	return &ClickHouseReader{
		conn:           conn,
		queryTimeout:   time.Second,
		traceNodes:     strings.ReplaceAll(traceNodesSelectSQL, tblSpanLog, "system.opentelemetry_span_log"),
		traceAncestors: strings.ReplaceAll(traceAncestorsSQL, tblSpanLog, "system.opentelemetry_span_log"),
	}
}

func TestSpanReadFloor_OneMinuteBeforeSaturatingAtZero(t *testing.T) {
	margin := uint64(time.Minute.Microseconds())
	for _, tt := range []struct {
		earliestStartUs, want uint64
	}{
		{0, 0},
		{margin - 1, 0},
		{margin, 0},
		{margin + 1, 1},
		{1_793_642_769_881_118, 1_793_642_769_881_118 - margin},
	} {
		if got := spanReadFloor(tt.earliestStartUs); got != tt.want {
			t.Errorf("spanReadFloor(%d) = %d, want %d", tt.earliestStartUs, got, tt.want)
		}
	}
}

func TestFetchTraceNodes_NoTracesSkipsQuery(t *testing.T) {
	conn := &captureConn{fakeConn: &fakeConn{}}
	r := &ClickHouseReader{conn: conn, queryTimeout: time.Second}
	got, err := r.FetchTraceNodes(context.Background(), nil, 1, 0, model.AncestorRead{})
	if err != nil || len(got) != 0 || len(conn.sqls) != 0 {
		t.Fatalf("empty input: got %v, err %v, %d queries; want no query", got, err, len(conn.sqls))
	}
}

// TestFetchQueryLogByQueryIDs_ScansInitialAddress pins the new column: the
// originating client lands in InitialAddress, distinct from the row's own
// address, in both the enrichment and the full query_log scans.
func TestFetchQueryLogByQueryIDs_ScansInitialAddress(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	conn := &fakeConn{rows: newFakeRows([]any{
		"qid-secondary", "QueryFinish", now, uint64(1500),
		"default", "app", "clickhouse-go", "app-1", "10.0.0.9", "192.168.4.7",
		[]string{"default"}, []string{"default.events"}, int32(0),
		uint64(100), uint64(200), uint64(0), uint64(0), uint64(10), uint64(20), uint64(300),
	})}
	reader := &ClickHouseReader{
		conn:         conn,
		queryTimeout: time.Second,
		enrichSelect: buildQueryLogSQLWithCapabilities(queryLogEnrichSelectSQL, "system.query_log", false, false),
	}
	reader.setUserFilter(nil, nil)

	logs, err := reader.FetchQueryLogByQueryIDs(context.Background(), []string{"qid-secondary"}, 1, 0)
	if err != nil {
		t.Fatalf("FetchQueryLogByQueryIDs: %v", err)
	}
	ql := logs["qid-secondary"]
	if ql.ClientAddress != "10.0.0.9" || ql.InitialAddress != "192.168.4.7" {
		t.Errorf("scanned client=%q initial=%q, want 10.0.0.9 / 192.168.4.7", ql.ClientAddress, ql.InitialAddress)
	}
	if ql.User != "default" || ql.InitialUser != "app" {
		t.Errorf("scanned user=%q initial_user=%q, want default / app", ql.User, ql.InitialUser)
	}
	if !strings.Contains(reader.enrichSelect, "IPv6NumToString(initial_address) as initial_address") {
		t.Errorf("enrichment SELECT lacks initial_address:\n%s", reader.enrichSelect)
	}
}

// TestFetchQueryOrigins_ReadsUsersAndAddresses pins the trace filters' origin
// lookup: it reads user, initial_user, and both addresses per query from
// every lifecycle row, and is never narrowed by the user whitelist splice.
func TestFetchQueryOrigins_ReadsUsersAndAddresses(t *testing.T) {
	conn := &captureConn{fakeConn: &fakeConn{}}
	conn.rowsFn = func(string) (driver.Rows, error) {
		return newFakeRows([]any{"q-shard", "default", "patient_records", "10.0.0.9", "192.168.4.8"}), nil
	}
	r := &ClickHouseReader{
		conn:               conn,
		queryTimeout:       time.Second,
		queryOriginsSelect: strings.ReplaceAll(queryOriginsSelectSQL, tblQueryLog, "system.query_log"),
	}

	const earliestStartUs = uint64(1_793_642_769_881_118)
	got, err := r.FetchQueryOrigins(context.Background(), []string{"q-shard"}, 0, earliestStartUs)
	if err != nil {
		t.Fatalf("FetchQueryOrigins: %v", err)
	}
	want := model.QueryOrigin{User: "default", InitialUser: "patient_records", Address: "10.0.0.9", InitialAddress: "192.168.4.8"}
	if got["q-shard"] != want {
		t.Errorf("q-shard origin = %+v, want %+v", got["q-shard"], want)
	}
	sql := conn.sqls[0]
	for _, want := range []string{"any(initial_user)", "any(IPv6NumToString(initial_address))", "query_id IN ?", "event_time >= toDateTime(?)", "GROUP BY query_id"} {
		if !strings.Contains(sql, want) {
			t.Errorf("query lacks %q:\n%s", want, sql)
		}
	}
	for _, unwanted := range []string{"type IN", "user IN", enrichSelectAnchor} {
		if strings.Contains(sql, unwanted) {
			t.Errorf("query must not contain %q:\n%s", unwanted, sql)
		}
	}
	// One second below the enrichment floor, so every query span
	// FetchTraceQueries returns from the same start has its finish row read.
	wantFloor := earliestStartUs/1_000_000 - 61
	if args := conn.queries[0]; len(args) != 3 || args[1] != 1 || args[2] != wantFloor {
		t.Errorf("args = %v, want [queryIDs 1 %d] (lookback floored at one day, event_time 61s before the earliest start)", args, wantFloor)
	}
}

func TestOriginsReadFloor_StaysBelowTheSpanFloor(t *testing.T) {
	for _, start := range []uint64{0, 30_000_000, 60_000_000, 61_000_000, 1_793_642_769_000_001, 1_793_642_769_999_999} {
		got := originsReadFloor(start)
		// A query span that finished at the span floor can have its finish
		// row stamped in the second before it.
		if span := spanReadFloor(start); span >= 1_000_000 && got > span/1_000_000-1 {
			t.Errorf("originsReadFloor(%d) = %d, above the second before the span floor %d", start, got, span)
		}
	}
	if got := originsReadFloor(0); got != 0 {
		t.Errorf("originsReadFloor(0) = %d, want 0", got)
	}
}
