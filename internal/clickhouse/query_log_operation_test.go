package clickhouse

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDetectQueryLogOperationSupport(t *testing.T) {
	ctx := context.Background()

	conn := &fakeConn{row: fakeRow{count: 1}}
	if !detectQueryLogOperationSupport(ctx, conn, "", false) {
		t.Fatal("expected query operation support when system.columns count is positive")
	}
	for _, want := range []string{"system.columns", "database = 'system'", "table = 'query_log'", "name = 'query_kind'"} {
		if !strings.Contains(conn.lastQueryRow, want) {
			t.Errorf("probe SQL missing %q:\n%s", want, conn.lastQueryRow)
		}
	}

	conn = &fakeConn{row: fakeRow{count: 0}}
	if detectQueryLogOperationSupport(ctx, conn, "", false) {
		t.Fatal("expected no query operation support when system.columns count is zero")
	}

	conn = &fakeConn{row: fakeRow{err: errors.New("permission denied")}}
	if detectQueryLogOperationSupport(ctx, conn, "", false) {
		t.Fatal("expected probe errors to disable query operation enrichment")
	}

	conn = &fakeConn{row: fakeRow{vals: []any{uint64(3), uint64(3)}}}
	if !detectQueryLogOperationSupport(ctx, conn, "prod", true) {
		t.Fatal("expected cluster mode to enable query operations when all replicas support query_kind")
	}
	if !strings.Contains(conn.lastQueryRow, "clusterAllReplicas('prod', system.columns)") {
		t.Errorf("cluster probe SQL should use clusterAllReplicas:\n%s", conn.lastQueryRow)
	}

	conn = &fakeConn{row: fakeRow{vals: []any{uint64(3), uint64(2)}}}
	if detectQueryLogOperationSupport(ctx, conn, "prod", true) {
		t.Fatal("expected mixed cluster support to disable query operation enrichment")
	}

	conn = &fakeConn{row: fakeRow{count: 1}}
	if detectQueryLogOperationSupport(ctx, conn, "", true) {
		t.Fatal("cluster query mode without a cluster name should disable query operation enrichment")
	}
	if conn.queryRowCallCount != 0 {
		t.Errorf("empty-cluster mode should not probe system.columns, got %d calls", conn.queryRowCallCount)
	}
}

func TestBuildQueryLogSQL_QueryOperationColumn(t *testing.T) {
	for _, template := range []string{queryLogSelectSQL, queryLogEnrichSelectSQL} {
		got := buildQueryLogSQLWithCapabilities(template, "system.query_log", false, true)
		if !strings.Contains(got, "query_kind AS query_operation") {
			t.Errorf("operation-aware SQL missing query_kind projection:\n%s", got)
		}
		if strings.Contains(got, queryLogOperationColumn) {
			t.Errorf("operation placeholder leaked into SQL:\n%s", got)
		}

		got = buildQueryLogSQLWithCapabilities(template, "system.query_log", false, false)
		if strings.Contains(got, "query_kind AS query_operation") || strings.Contains(got, queryLogOperationColumn) {
			t.Errorf("fallback SQL unexpectedly contains query operation projection:\n%s", got)
		}
	}
}

func TestQueryLogOperationSupported(t *testing.T) {
	if !(&ClickHouseReader{queryLogOperation: true}).QueryLogOperationSupported() {
		t.Fatal("QueryLogOperationSupported returned false for an enabled reader")
	}
	if (&ClickHouseReader{}).QueryLogOperationSupported() {
		t.Fatal("QueryLogOperationSupported returned true for a disabled reader")
	}
}

func TestFetchQueryLogByQueryIDsScansQueryOperation(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	conn := &fakeConn{rows: newFakeRows([]any{
		"qid-1", "QueryFinish", "Select", now, uint64(1500),
		"default", "clickhouse-go", "app-1", "10.0.0.1", "10.0.0.1",
		[]string{"default"}, []string{"default.events"}, int32(0),
		uint64(100), uint64(200), uint64(0), uint64(0), uint64(10), uint64(20), uint64(300),
	})}
	reader := &ClickHouseReader{
		conn:              conn,
		queryTimeout:      time.Second,
		enrichSelect:      buildQueryLogSQLWithCapabilities(queryLogEnrichSelectSQL, "system.query_log", false, true),
		queryLogOperation: true,
	}
	reader.setUserFilter(nil, nil)

	logs, err := reader.FetchQueryLogByQueryIDs(context.Background(), []string{"qid-1"}, 1)
	if err != nil {
		t.Fatalf("FetchQueryLogByQueryIDs failed: %v", err)
	}
	if got := logs["qid-1"].QueryOperation; got != "Select" {
		t.Errorf("QueryOperation = %q, want Select", got)
	}
}

func TestFetchQueryLogByQueryIDsScansWithoutQueryOperation(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	conn := &fakeConn{rows: newFakeRows([]any{
		"qid-1", "QueryFinish", now, uint64(1500),
		"default", "clickhouse-go", "app-1", "10.0.0.1", "10.0.0.1",
		[]string{"default"}, []string{"default.events"}, int32(0),
		uint64(100), uint64(200), uint64(0), uint64(0), uint64(10), uint64(20), uint64(300),
	})}
	reader := &ClickHouseReader{
		conn:              conn,
		queryTimeout:      time.Second,
		enrichSelect:      buildQueryLogSQLWithCapabilities(queryLogEnrichSelectSQL, "system.query_log", false, false),
		queryLogOperation: false,
	}
	reader.setUserFilter(nil, nil)

	logs, err := reader.FetchQueryLogByQueryIDs(context.Background(), []string{"qid-1"}, 1)
	if err != nil {
		t.Fatalf("FetchQueryLogByQueryIDs failed: %v", err)
	}
	if got := logs["qid-1"].QueryOperation; got != "" {
		t.Errorf("QueryOperation = %q, want empty when query_kind is unsupported", got)
	}
	if got := logs["qid-1"].User; got != "default" {
		t.Errorf("User = %q, want default; disabled operation column shifted scan destinations", got)
	}
}
