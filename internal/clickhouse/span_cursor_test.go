package clickhouse

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
)

func spanCursorTestRow(traceID uuid.UUID, spanID, finish uint64) []any {
	return []any{
		"host-a",
		traceID,
		spanID,
		uint64(0),
		"SELECT",
		"internal",
		finish - 10,
		finish,
		time.Unix(0, 0).UTC(),
		map[string]string{},
	}
}

func TestFetchOpenTelemetrySpansWithOpts_AdvancesKeysetsBeforeWrapping(t *testing.T) {
	traceA := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	traceB := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	traceC := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	var traceCalls, spanCalls int
	conn := &captureConn{fakeConn: &fakeConn{}}
	conn.rowsFn = func(query string) (driver.Rows, error) {
		switch {
		case strings.Contains(query, "SELECT trace_id, max(finish_time)"):
			traceCalls++
			switch traceCalls {
			case 1:
				return newFakeRows([]any{traceA, uint64(100)}, []any{traceB, uint64(90)}), nil
			case 2:
				return newFakeRows([]any{traceC, uint64(80)}), nil
			case 3:
				return newFakeRows(), nil
			case 4:
				return newFakeRows([]any{traceA, uint64(100)}), nil
			default:
				t.Fatalf("unexpected trace query %d", traceCalls)
			}
		case strings.Contains(query, "WHERE trace_id IN ?"):
			spanCalls++
			switch spanCalls {
			case 1:
				return newFakeRows(spanCursorTestRow(traceA, 10, 100), spanCursorTestRow(traceA, 9, 99)), nil
			case 2:
				return newFakeRows(spanCursorTestRow(traceB, 8, 98)), nil
			case 3:
				return newFakeRows(spanCursorTestRow(traceC, 7, 80)), nil
			case 4:
				return newFakeRows(), nil
			default:
				t.Fatalf("unexpected span query %d", spanCalls)
			}
		default:
			t.Fatalf("unexpected SQL:\n%s", query)
		}
		return nil, nil
	}

	reader := &ClickHouseReader{
		conn:         conn,
		queryTimeout: time.Second,
		traceIDSelect: strings.ReplaceAll(
			traceIDSelectSQL,
			tblSpanLog,
			"system.opentelemetry_span_log",
		),
		spanSelect: strings.ReplaceAll(spanSelectSQL, tblSpanLog, "system.opentelemetry_span_log"),
	}
	fetch := func() int {
		spans, err := reader.FetchOpenTelemetrySpansWithOpts(context.Background(), 1000, time.Minute, 2, FetchOpts{})
		if err != nil {
			t.Fatalf("FetchOpenTelemetrySpansWithOpts: %v", err)
		}
		return len(spans)
	}

	if got := fetch(); got != 2 {
		t.Fatalf("first page spans = %d, want 2", got)
	}
	if traceCalls != 1 || spanCalls != 1 {
		t.Fatalf("first page calls = trace:%d span:%d, want 1/1", traceCalls, spanCalls)
	}
	if !strings.Contains(conn.sqls[0], "ORDER BY max_finish_time DESC, trace_id DESC") {
		t.Errorf("trace page lacks stable ordering:\n%s", conn.sqls[0])
	}
	if !strings.Contains(conn.sqls[1], "ORDER BY finish_time_us DESC, trace_id DESC, span_id DESC") {
		t.Errorf("span page lacks stable ordering:\n%s", conn.sqls[1])
	}

	if got := fetch(); got != 1 {
		t.Fatalf("continued span page spans = %d, want 1", got)
	}
	if traceCalls != 1 {
		t.Fatalf("continued span page re-queried trace IDs: %d calls", traceCalls)
	}
	continuedSpanSQL := conn.sqls[2]
	if !strings.Contains(continuedSpanSQL, "(finish_time_us, trace_id, span_id) < (?, ?, ?)") {
		t.Errorf("continued span page lacks keyset predicate:\n%s", continuedSpanSQL)
	}
	continuedSpanArgs := conn.queries[2]
	if len(continuedSpanArgs) != 6 || continuedSpanArgs[2] != uint64(99) || continuedSpanArgs[3] != traceA || continuedSpanArgs[4] != uint64(9) {
		t.Errorf("continued span args = %v, want cursor [99 %s 9]", continuedSpanArgs, traceA)
	}

	if got := fetch(); got != 1 {
		t.Fatalf("next trace page spans = %d, want 1", got)
	}
	continuedTraceSQL := conn.sqls[3]
	if !strings.Contains(continuedTraceSQL, "HAVING (max_finish_time, trace_id) < (?, ?)") {
		t.Errorf("continued trace page lacks keyset predicate:\n%s", continuedTraceSQL)
	}
	continuedTraceArgs := conn.queries[3]
	if len(continuedTraceArgs) != 6 || continuedTraceArgs[3] != uint64(90) || continuedTraceArgs[4] != traceB {
		t.Errorf("continued trace args = %v, want cursor [90 %s]", continuedTraceArgs, traceB)
	}

	if got := fetch(); got != 0 {
		t.Fatalf("end-of-walk spans = %d, want 0", got)
	}
	if spanCalls != 3 {
		t.Fatalf("end-of-walk should not query spans, got %d calls", spanCalls)
	}

	if got := fetch(); got != 0 {
		t.Fatalf("wrapped page spans = %d, want 0", got)
	}
	wrappedTraceSQL := conn.sqls[6]
	if strings.Contains(wrappedTraceSQL, "HAVING (max_finish_time, trace_id) <") {
		t.Errorf("wrapped trace page retained stale cursor:\n%s", wrappedTraceSQL)
	}
}
