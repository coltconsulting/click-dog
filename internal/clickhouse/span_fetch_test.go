package clickhouse

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/model"
)

func newSpanFetchTestReader(rowsFn func(string) (driver.Rows, error)) (*ClickHouseReader, *captureConn) {
	conn := &captureConn{fakeConn: &fakeConn{}, rowsFn: rowsFn}
	return &ClickHouseReader{
		conn:          conn,
		queryTimeout:  time.Second,
		traceIDSelect: strings.ReplaceAll(traceIDSelectSQL, tblSpanLog, "system.opentelemetry_span_log"),
		spanSelect:    strings.ReplaceAll(spanSelectSQL, tblSpanLog, "system.opentelemetry_span_log"),
	}, conn
}

func TestFetchOpenTelemetrySpansWithOpts_BuildsAndExecutesTwoStepQueries(t *testing.T) {
	traceID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	finishDate := time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)
	queryCall := 0
	reader, conn := newSpanFetchTestReader(func(string) (driver.Rows, error) {
		queryCall++
		if queryCall == 1 {
			return newFakeRows([]any{traceID, uint64(200)}), nil
		}
		return newFakeRows([]any{
			"clickhouse-1",
			traceID,
			uint64(7),
			uint64(3),
			"query",
			"server",
			uint64(100),
			uint64(200),
			finishDate,
			map[string]string{"clickhouse.query_id": "q-1"},
		}), nil
	})

	const limit = 50
	lookback := 25 * time.Hour
	spans, err := reader.FetchOpenTelemetrySpansWithOpts(
		context.Background(),
		1000,
		lookback,
		limit,
		FetchOpts{
			MaxTraceDurationMs:  5000,
			MinSpanDurationMs:   100,
			MaxSpanDurationMs:   3000,
			BlacklistOperations: []string{"", "MergeTreeIndex", "VFSWrite"},
		},
	)
	if err != nil {
		t.Fatalf("FetchOpenTelemetrySpansWithOpts: %v", err)
	}
	wantSpans := []model.OpenTelemetrySpan{{
		Hostname:      "clickhouse-1",
		TraceID:       traceID,
		SpanID:        7,
		ParentSpanID:  3,
		OperationName: "query",
		Kind:          "server",
		StartTimeUs:   100,
		FinishTimeUs:  200,
		FinishDate:    finishDate,
		Attributes:    map[string]string{"clickhouse.query_id": "q-1"},
	}}
	if !reflect.DeepEqual(spans, wantSpans) {
		t.Errorf("scanned spans = %#v, want %#v", spans, wantSpans)
	}

	if len(conn.sqls) != 2 {
		t.Fatalf("queries = %d, want trace selection followed by span fetch", len(conn.sqls))
	}
	traceSQL := conn.sqls[0]
	for _, want := range []string{
		"FROM system.opentelemetry_span_log",
		"(finish_time_us - start_time_us) >= ? * 1000",
		"(finish_time_us - start_time_us) <= ? * 1000",
		"GROUP BY trace_id",
		"ORDER BY max_finish_time DESC",
		"LIMIT ?",
	} {
		if !strings.Contains(traceSQL, want) {
			t.Errorf("trace-selection SQL missing %q:\n%s", want, traceSQL)
		}
	}
	if strings.Contains(traceSQL, "operation_name NOT LIKE") {
		t.Errorf("trace-selection SQL must not apply the operation blacklist:\n%s", traceSQL)
	}
	wantTraceArgs := []any{2, int(lookback.Seconds()), 1000, 5000, limit}
	if !reflect.DeepEqual(conn.queries[0], wantTraceArgs) {
		t.Errorf("trace-selection args = %#v, want %#v", conn.queries[0], wantTraceArgs)
	}

	spanSQL := conn.sqls[1]
	for _, want := range []string{
		"FROM system.opentelemetry_span_log",
		"trace_id IN ?",
		"(finish_time_us - start_time_us) >= ? * 1000",
		"(finish_time_us - start_time_us) <= ? * 1000",
		"operation_name NOT LIKE ?",
		"LIMIT ?",
	} {
		if !strings.Contains(spanSQL, want) {
			t.Errorf("span-fetch SQL missing %q:\n%s", want, spanSQL)
		}
	}
	if got := strings.Count(spanSQL, "operation_name NOT LIKE ?"); got != 2 {
		t.Errorf("operation blacklist clauses = %d, want 2 (empty patterns must be ignored)", got)
	}
	wantSpanArgs := []any{
		[]uuid.UUID{traceID},
		2,
		100,
		3000,
		"%MergeTreeIndex%",
		"%VFSWrite%",
		limit,
	}
	if !reflect.DeepEqual(conn.queries[1], wantSpanArgs) {
		t.Errorf("span-fetch args = %#v, want %#v", conn.queries[1], wantSpanArgs)
	}
}

func TestFetchOpenTelemetrySpansWithOpts_ZeroOptionsOmitOptionalFilters(t *testing.T) {
	traceID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")
	queryCall := 0
	reader, conn := newSpanFetchTestReader(func(string) (driver.Rows, error) {
		queryCall++
		if queryCall == 1 {
			return newFakeRows([]any{traceID, uint64(200)}), nil
		}
		return newFakeRows([]any{
			"clickhouse-1", traceID, uint64(1), uint64(0), "query", "server",
			uint64(100), uint64(200), time.Unix(0, 0).UTC(), map[string]string{},
		}), nil
	})

	const limit = 100
	lookback := time.Hour
	spans, err := reader.FetchOpenTelemetrySpansWithOpts(context.Background(), 1000, lookback, limit, FetchOpts{})
	if err != nil {
		t.Fatalf("FetchOpenTelemetrySpansWithOpts: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("spans = %v, want one span", spans)
	}
	if len(conn.sqls) != 2 {
		t.Fatalf("queries = %d, want trace selection followed by span fetch", len(conn.sqls))
	}

	traceSQL := conn.sqls[0]
	if strings.Contains(traceSQL, "(finish_time_us - start_time_us) <= ? * 1000") {
		t.Errorf("zero max trace duration emitted an upper-bound filter:\n%s", traceSQL)
	}
	wantTraceArgs := []any{1, int(lookback.Seconds()), 1000, limit}
	if !reflect.DeepEqual(conn.queries[0], wantTraceArgs) {
		t.Errorf("trace-selection args = %#v, want %#v", conn.queries[0], wantTraceArgs)
	}

	spanSQL := conn.sqls[1]
	for _, unwanted := range []string{
		"(finish_time_us - start_time_us) >= ? * 1000",
		"(finish_time_us - start_time_us) <= ? * 1000",
		"operation_name NOT LIKE ?",
	} {
		if strings.Contains(spanSQL, unwanted) {
			t.Errorf("zero options emitted optional clause %q:\n%s", unwanted, spanSQL)
		}
	}
	wantSpanArgs := []any{[]uuid.UUID{traceID}, 1, limit}
	if !reflect.DeepEqual(conn.queries[1], wantSpanArgs) {
		t.Errorf("span-fetch args = %#v, want %#v", conn.queries[1], wantSpanArgs)
	}
}

func TestFetchOpenTelemetrySpansWithOpts_EmptyTraceSelectionSkipsSpanQuery(t *testing.T) {
	reader, conn := newSpanFetchTestReader(func(string) (driver.Rows, error) {
		return newFakeRows(), nil
	})

	spans, err := reader.FetchOpenTelemetrySpansWithOpts(context.Background(), 1000, time.Hour, 100, FetchOpts{})
	if err != nil {
		t.Fatalf("FetchOpenTelemetrySpansWithOpts: %v", err)
	}
	if len(spans) != 0 {
		t.Fatalf("spans = %v, want empty result", spans)
	}
	if len(conn.sqls) != 1 {
		t.Errorf("queries = %d, want only trace selection", len(conn.sqls))
	}
}

func TestFetchOpenTelemetrySpansWithOpts_ReportsQueryStage(t *testing.T) {
	traceID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	t.Run("trace selection", func(t *testing.T) {
		reader, _ := newSpanFetchTestReader(func(string) (driver.Rows, error) {
			return nil, errors.New("trace query failed")
		})
		_, err := reader.FetchOpenTelemetrySpansWithOpts(context.Background(), 1000, time.Hour, 100, FetchOpts{})
		if err == nil || !strings.Contains(err.Error(), "failed to query trace IDs: trace query failed") {
			t.Fatalf("error = %v, want trace-selection context", err)
		}
	})

	t.Run("span fetch", func(t *testing.T) {
		queryCall := 0
		reader, _ := newSpanFetchTestReader(func(string) (driver.Rows, error) {
			queryCall++
			if queryCall == 1 {
				return newFakeRows([]any{traceID, uint64(200)}), nil
			}
			return nil, errors.New("span query failed")
		})
		_, err := reader.FetchOpenTelemetrySpansWithOpts(context.Background(), 1000, time.Hour, 100, FetchOpts{})
		if err == nil || !strings.Contains(err.Error(), "failed to query spans: span query failed") {
			t.Fatalf("error = %v, want span-fetch context", err)
		}
	})
}
