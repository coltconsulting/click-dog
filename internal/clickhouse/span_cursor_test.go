package clickhouse

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/model"
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

// TestFetchOpenTelemetrySpansWithOpts_ExhaustedWalkWrapsInTheSameCall pins the
// wrap semantics that the live 3-node run showed to matter: a walk that runs
// past its last page must re-read the newest rows in the same call, and a short
// trace page must end the walk outright. Before this, the exhausted-cursor page
// returned nothing and the wrap waited for the next cycle, so every other cycle
// was blind to new arrivals — the poll interval doubled and, with
// lookback = interval + buffer, roughly a third of qualifying traces at the
// defaults were never exported.
func TestFetchOpenTelemetrySpansWithOpts_ExhaustedWalkWrapsInTheSameCall(t *testing.T) {
	traceA := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	traceB := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	traceNew := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")

	var traceCalls, spanCalls int
	conn := &captureConn{fakeConn: &fakeConn{}}
	conn.rowsFn = func(query string) (driver.Rows, error) {
		switch {
		case strings.Contains(query, "SELECT trace_id, max(finish_time)"):
			traceCalls++
			cursor := strings.Contains(query, "HAVING (max_finish_time, trace_id) <")
			switch traceCalls {
			case 1: // full page (limit 2): the walk continues next call
				return newFakeRows([]any{traceA, uint64(100)}, []any{traceB, uint64(90)}), nil
			case 2: // cursor page: nothing older — the walk is exhausted
				if !cursor {
					t.Fatalf("second trace page should continue the keyset walk:\n%s", query)
				}
				return newFakeRows(), nil
			case 3: // wrapped page in the SAME call: the trace that arrived meanwhile
				if cursor {
					t.Fatalf("wrapped trace page retained the stale cursor:\n%s", query)
				}
				return newFakeRows([]any{traceNew, uint64(120)}), nil
			case 4: // short page ended the walk: next cycle starts fresh, no cursor
				if cursor {
					t.Fatalf("a short trace page must end the walk, got a cursor query:\n%s", query)
				}
				return newFakeRows([]any{traceNew, uint64(120)}), nil
			default:
				t.Fatalf("unexpected trace query %d", traceCalls)
			}
		case strings.Contains(query, "WHERE trace_id IN ?"):
			spanCalls++
			switch spanCalls {
			case 1:
				return newFakeRows(spanCursorTestRow(traceA, 10, 100)), nil
			case 2, 3:
				return newFakeRows(spanCursorTestRow(traceNew, 7, 120)), nil
			default:
				t.Fatalf("unexpected span query %d", spanCalls)
			}
		default:
			t.Fatalf("unexpected SQL:\n%s", query)
		}
		return nil, nil
	}

	reader := &ClickHouseReader{
		conn:          conn,
		queryTimeout:  time.Second,
		traceIDSelect: strings.ReplaceAll(traceIDSelectSQL, tblSpanLog, "system.opentelemetry_span_log"),
		spanSelect:    strings.ReplaceAll(spanSelectSQL, tblSpanLog, "system.opentelemetry_span_log"),
	}
	fetch := func() []model.OpenTelemetrySpan {
		spans, err := reader.FetchOpenTelemetrySpansWithOpts(context.Background(), 1000, time.Minute, 2, FetchOpts{})
		if err != nil {
			t.Fatalf("FetchOpenTelemetrySpansWithOpts: %v", err)
		}
		return spans
	}

	// Cycle 1: full trace page, span page under the limit → trace cursor armed.
	if got := fetch(); len(got) != 1 || got[0].TraceID != traceA {
		t.Fatalf("cycle 1 spans = %v, want traceA only", got)
	}
	if traceCalls != 1 || spanCalls != 1 {
		t.Fatalf("cycle 1 calls = trace:%d span:%d, want 1/1", traceCalls, spanCalls)
	}

	// Cycle 2: the cursor page is empty, so the same call wraps and returns
	// the newly arrived trace instead of an empty page.
	got := fetch()
	if len(got) != 1 || got[0].TraceID != traceNew {
		t.Fatalf("cycle 2 spans = %v, want the newly arrived trace in the same call", got)
	}
	if traceCalls != 3 || spanCalls != 2 {
		t.Fatalf("cycle 2 calls = trace:%d span:%d, want 3/2 (cursor page + wrapped page)", traceCalls, spanCalls)
	}

	// Cycle 3: the wrapped page was short, so this cycle starts from the
	// newest rows without a cursor query.
	if got := fetch(); len(got) != 1 || got[0].TraceID != traceNew {
		t.Fatalf("cycle 3 spans = %v, want the newest page", got)
	}
	if traceCalls != 4 || spanCalls != 3 {
		t.Fatalf("cycle 3 calls = trace:%d span:%d, want 4/3", traceCalls, spanCalls)
	}
}

// TestFetchOpenTelemetrySpansWithOpts_ExactlyFullSpanPageDoesNotBlindTheNextCall
// pins the review finding on the same-call wrap: a short trace page whose spans
// fill the cap exactly arms the span cursor, and the next call's continuation
// comes back empty. That call must then select again rather than return an
// empty page, or a trace arriving between the two calls waits a full extra
// cycle — long enough to age out of the lookback at the defaults.
func TestFetchOpenTelemetrySpansWithOpts_ExactlyFullSpanPageDoesNotBlindTheNextCall(t *testing.T) {
	traceA := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	traceNew := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")

	var traceCalls, spanCalls int
	conn := &captureConn{fakeConn: &fakeConn{}}
	conn.rowsFn = func(query string) (driver.Rows, error) {
		switch {
		case strings.Contains(query, "SELECT trace_id, max(finish_time)"):
			traceCalls++
			cursor := strings.Contains(query, "HAVING (max_finish_time, trace_id) <")
			switch traceCalls {
			case 1: // short page: the walk ends with it
				return newFakeRows([]any{traceA, uint64(100)}), nil
			case 2: // selected again in the SAME call after the empty continuation
				if cursor {
					t.Fatalf("re-selection after an empty span continuation must start fresh:\n%s", query)
				}
				return newFakeRows([]any{traceNew, uint64(120)}), nil
			default:
				t.Fatalf("unexpected trace query %d", traceCalls)
			}
		case strings.Contains(query, "WHERE trace_id IN ?"):
			spanCalls++
			cursor := strings.Contains(query, "(finish_time_us, trace_id, span_id) < (?, ?, ?)")
			switch spanCalls {
			case 1: // exactly the cap: arms the span cursor
				return newFakeRows(spanCursorTestRow(traceA, 10, 100), spanCursorTestRow(traceA, 9, 99)), nil
			case 2: // continuation past the last span: nothing left
				if !cursor {
					t.Fatalf("second span query should continue the keyset:\n%s", query)
				}
				return newFakeRows(), nil
			case 3: // the new trace's spans, still within this call
				if cursor {
					t.Fatalf("span query for the re-selected page retained the stale cursor:\n%s", query)
				}
				return newFakeRows(spanCursorTestRow(traceNew, 7, 120)), nil
			default:
				t.Fatalf("unexpected span query %d", spanCalls)
			}
		default:
			t.Fatalf("unexpected SQL:\n%s", query)
		}
		return nil, nil
	}

	reader := &ClickHouseReader{
		conn:          conn,
		queryTimeout:  time.Second,
		traceIDSelect: strings.ReplaceAll(traceIDSelectSQL, tblSpanLog, "system.opentelemetry_span_log"),
		spanSelect:    strings.ReplaceAll(spanSelectSQL, tblSpanLog, "system.opentelemetry_span_log"),
	}
	fetch := func() []model.OpenTelemetrySpan {
		spans, err := reader.FetchOpenTelemetrySpansWithOpts(context.Background(), 1000, time.Minute, 2, FetchOpts{})
		if err != nil {
			t.Fatalf("FetchOpenTelemetrySpansWithOpts: %v", err)
		}
		return spans
	}

	if got := fetch(); len(got) != 2 {
		t.Fatalf("cycle 1 spans = %d, want the full page of 2", len(got))
	}
	if traceCalls != 1 || spanCalls != 1 {
		t.Fatalf("cycle 1 calls = trace:%d span:%d, want 1/1", traceCalls, spanCalls)
	}

	got := fetch()
	if len(got) != 1 || got[0].TraceID != traceNew {
		t.Fatalf("cycle 2 spans = %v, want the newly arrived trace in the same call", got)
	}
	if traceCalls != 2 || spanCalls != 3 {
		t.Fatalf("cycle 2 calls = trace:%d span:%d, want 2/3 (empty continuation, then a fresh selection)", traceCalls, spanCalls)
	}
}
