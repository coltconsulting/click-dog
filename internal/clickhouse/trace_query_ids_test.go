package clickhouse

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
)

// TestFetchTraceQueryIDs_GroupsQueryIDsByTrace pins the whitelist's
// page-independent bridge from a trace to its query_log rows: the query is
// scoped to the given traces and the lookback partition, ignores spans with no
// query ID, and groups the IDs per trace.
func TestFetchTraceQueryIDs_GroupsQueryIDsByTrace(t *testing.T) {
	traceA := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	traceB := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	conn := &captureConn{fakeConn: &fakeConn{}}
	conn.rowsFn = func(string) (driver.Rows, error) {
		return newFakeRows(
			[]any{traceA, "q-initial"},
			[]any{traceA, "q-secondary"},
			[]any{traceB, "q-other"},
		), nil
	}
	r := &ClickHouseReader{
		conn:          conn,
		queryTimeout:  time.Second,
		traceQueryIDs: strings.ReplaceAll(traceQueryIDsSelectSQL, tblSpanLog, "system.opentelemetry_span_log"),
	}

	got, err := r.FetchTraceQueryIDs(context.Background(), []uuid.UUID{traceA, traceB}, 0)
	if err != nil {
		t.Fatalf("FetchTraceQueryIDs: %v", err)
	}
	if len(got[traceA]) != 2 || got[traceA][0] != "q-initial" || got[traceA][1] != "q-secondary" {
		t.Errorf("trace A query IDs = %v, want [q-initial q-secondary]", got[traceA])
	}
	if len(got[traceB]) != 1 || got[traceB][0] != "q-other" {
		t.Errorf("trace B query IDs = %v, want [q-other]", got[traceB])
	}

	sql := conn.sqls[0]
	for _, want := range []string{"trace_id IN ?", "finish_date >= today() - INTERVAL ? DAY", "attribute['clickhouse.query_id'] != ''"} {
		if !strings.Contains(sql, want) {
			t.Errorf("query lacks %q:\n%s", want, sql)
		}
	}
	args := conn.queries[0]
	if len(args) != 2 || args[1] != 1 {
		t.Errorf("args = %v, want [traceIDs 1] (lookback floored at one day)", args)
	}
}

func TestFetchTraceQueryIDs_NoTracesSkipsQuery(t *testing.T) {
	conn := &captureConn{fakeConn: &fakeConn{}}
	r := &ClickHouseReader{conn: conn, queryTimeout: time.Second}
	got, err := r.FetchTraceQueryIDs(context.Background(), nil, 1)
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
		"default", "clickhouse-go", "app-1", "10.0.0.9", "192.168.4.7",
		[]string{"default"}, []string{"default.events"}, int32(0),
		uint64(100), uint64(200), uint64(0), uint64(0), uint64(10), uint64(20), uint64(300),
	})}
	reader := &ClickHouseReader{
		conn:         conn,
		queryTimeout: time.Second,
		enrichSelect: buildQueryLogSQLWithCapabilities(queryLogEnrichSelectSQL, "system.query_log", false, false),
	}
	reader.setUserFilter(nil, nil)

	logs, err := reader.FetchQueryLogByQueryIDs(context.Background(), []string{"qid-secondary"}, 1)
	if err != nil {
		t.Fatalf("FetchQueryLogByQueryIDs: %v", err)
	}
	ql := logs["qid-secondary"]
	if ql.ClientAddress != "10.0.0.9" || ql.InitialAddress != "192.168.4.7" {
		t.Errorf("scanned client=%q initial=%q, want 10.0.0.9 / 192.168.4.7", ql.ClientAddress, ql.InitialAddress)
	}
	if !strings.Contains(reader.enrichSelect, "IPv6NumToString(initial_address) as initial_address") {
		t.Errorf("enrichment SELECT lacks initial_address:\n%s", reader.enrichSelect)
	}
}
