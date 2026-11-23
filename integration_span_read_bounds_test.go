//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
)

// oldSpanRows is how many span-log rows TestIntegration_TraceScopedSpanReads
// seeds hours before its fixture. They finish inside the readers' day floor,
// so a read bounded only by finish_date reads every one of them.
const oldSpanRows = 300_000

// TestIntegration_TraceScopedSpanReadsSkipOlderGranules measures the span-log
// reads that filter by trace ID or by span attribute, whose column is last in
// (or absent from) the sort key (finish_date, finish_time_us, trace_id). It
// seeds rows from two to six hours ago, too short to qualify any trace, then
// runs Live Span Selection, the IP whitelist's trace query-ID lookup, and the
// drilldown bridge against a fresh traced query. Each read's own read_rows in
// system.query_log must stay far below the seeded rows: the finish_time_us
// floor prunes their granules, where a finish_date bound alone reads them all.
func TestIntegration_TraceScopedSpanReadsSkipOlderGranules(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()

	dayFloorRows := seedOldSpanLogRows(t, conn)

	fixture := seedTracedQueryFixture(t, conn, 1)
	traceID, queryID := fixture.TraceIDs[0], fixture.QueryIDs[0]

	reader := newCHReader(t, 1)
	defer reader.Close()
	tag := "click-dog-span-read-bounds-" + fixture.Marker
	ctx := clickhouse.Context(context.Background(), clickhouse.WithSettings(clickhouse.Settings{"log_comment": tag}))

	spans, err := reader.FetchOpenTelemetrySpans(ctx, 1, 0, 0, 0, 10*time.Minute, 1000)
	if err != nil {
		t.Fatalf("FetchOpenTelemetrySpans: %v", err)
	}
	if len(exactFixtureSpans(t, spans, fixture)) == 0 {
		t.Fatalf("Live Span Selection returned no spans of fixture trace %s", traceID)
	}
	byTrace, err := reader.FetchTraceNodes(ctx, fixture.TraceIDs, 1, uint64(fixture.WindowStart.UnixMicro()), model.AncestorRead{})
	if err != nil {
		t.Fatalf("FetchTraceNodes: %v", err)
	}
	if got := byTrace[traceID]; len(got) == 0 || got[0].QueryID != queryID {
		t.Fatalf("trace queries = %+v, want [%s]", got, queryID)
	}
	ids, _, err := reader.FetchTraceIDsByQueryID(ctx, queryID, 1, fixture.WindowStart, 10)
	if err != nil {
		t.Fatalf("FetchTraceIDsByQueryID: %v", err)
	}
	if len(ids) != 1 || ids[0] != traceID.String() {
		t.Fatalf("trace IDs for query %s = %v, want [%s]", queryID, ids, traceID)
	}

	if err := conn.Exec(context.Background(), "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("flush logs: %v", err)
	}
	rows, err := conn.Query(context.Background(), `
		SELECT query, read_rows, read_bytes
		FROM system.query_log
		WHERE log_comment = ? AND type = 'QueryFinish' AND event_date >= today() - 1
		  AND query LIKE '%opentelemetry_span_log%'`, tag)
	if err != nil {
		t.Fatalf("read query_log: %v", err)
	}
	defer rows.Close()

	reads := map[string]uint64{}
	for rows.Next() {
		var query string
		var readRows, readBytes uint64
		if err := rows.Scan(&query, &readRows, &readBytes); err != nil {
			t.Fatalf("scan query_log: %v", err)
		}
		var read string
		switch {
		case strings.Contains(query, "max_finish_time"):
			read = "trace selection"
		case strings.Contains(query, "attribute['clickhouse.query_id'] != ''"):
			read = "trace query IDs"
		case strings.Contains(query, "attribute['clickhouse.query_id'] = "):
			read = "query-id bridge"
		case strings.Contains(query, "trace_id IN"):
			read = "trace spans"
		default:
			continue
		}
		reads[read] = readRows
		t.Logf("%s: read_rows=%d read_bytes=%d (rows since the day floor: %d)", read, readRows, readBytes, dayFloorRows)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate query_log: %v", err)
	}
	for _, read := range []string{"trace selection", "trace spans", "trace query IDs", "query-id bridge"} {
		got, ok := reads[read]
		if !ok {
			t.Errorf("no %s read with log_comment %q in system.query_log", read, tag)
			continue
		}
		if got >= oldSpanRows/2 {
			t.Errorf("%s read %d rows, want well under the %d seeded rows hours older than its floor", read, got, oldSpanRows)
		}
	}
}

// spanLogDateColumn returns the span log's stored date column: event_date from
// ClickHouse 26.10, where finish_date became an ALIAS of it, and finish_date
// before. An INSERT must name the stored column; ClickHouse rejects an ALIAS.
func spanLogDateColumn(t *testing.T, conn driver.Conn) string {
	t.Helper()
	var name string
	if err := conn.QueryRow(context.Background(), `
		SELECT name FROM system.columns
		WHERE database = 'system' AND table = 'opentelemetry_span_log'
			AND name IN ('event_date', 'finish_date') AND default_kind = ''
		ORDER BY name = 'event_date' DESC
		LIMIT 1`).Scan(&name); err != nil {
		t.Fatalf("find the span log's stored date column: %v", err)
	}
	return name
}

// seedOldSpanLogRows inserts oldSpanRows span-log rows that finished two to six
// hours ago and returns how many rows now finish since the one-day floor.
func seedOldSpanLogRows(t *testing.T, conn driver.Conn) uint64 {
	t.Helper()
	seed := fmt.Sprintf(`
		INSERT INTO system.opentelemetry_span_log
			(hostname, trace_id, span_id, parent_span_id, operation_name, kind, start_time_us, finish_time_us, %s, attribute)
		SELECT 'click-dog-integration-old', generateUUIDv4(number), number + 1, 0, 'IntegrationOldSpan', 'INTERNAL',
			finish_us - 50, finish_us, toDate(toDateTime(intDiv(finish_us, 1000000))), map()
		FROM (
			SELECT number, toUnixTimestamp64Micro(now64(6)) - 7200000000 - intDiv(number * 14400000000, %d) AS finish_us
			FROM numbers(%d)
		)`, spanLogDateColumn(t, conn), oldSpanRows, oldSpanRows)
	if err := conn.Exec(context.Background(), seed); err != nil {
		t.Fatalf("seed old span-log rows: %v", err)
	}
	var dayFloorRows uint64
	if err := conn.QueryRow(context.Background(),
		"SELECT count() FROM system.opentelemetry_span_log WHERE finish_date >= today() - INTERVAL 1 DAY",
	).Scan(&dayFloorRows); err != nil {
		t.Fatalf("count span-log rows since the day floor: %v", err)
	}
	return dayFloorRows
}

// spanLogReads returns the finished span-log reads system.query_log records
// under the extra condition, after flushing the logs.
func spanLogReads(t *testing.T, conn driver.Conn, condition string, args ...any) []spanLogRead {
	t.Helper()
	if err := conn.Exec(context.Background(), "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("flush logs: %v", err)
	}
	rows, err := conn.Query(context.Background(), `
		SELECT log_comment, query, read_rows, read_bytes
		FROM system.query_log
		WHERE type = 'QueryFinish' AND event_date >= today() - 1
		  AND query LIKE '%opentelemetry_span_log%'
		  AND query NOT LIKE '%system.query_log%'
		  AND `+condition+`
		ORDER BY event_time_microseconds`, args...)
	if err != nil {
		t.Fatalf("read query_log: %v", err)
	}
	defer rows.Close()
	var reads []spanLogRead
	for rows.Next() {
		var read spanLogRead
		if err := rows.Scan(&read.tag, &read.query, &read.rows, &read.bytes); err != nil {
			t.Fatalf("scan query_log: %v", err)
		}
		reads = append(reads, read)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate query_log: %v", err)
	}
	return reads
}

type spanLogRead struct {
	tag, query  string
	rows, bytes uint64
}

// TestIntegration_TraceSpanFetchReadsFromSince measures FetchSpansForTraceIDs
// for each of its callers against rows seeded two to six hours earlier:
//
//   - analyze trace by query ID, from the bridge's query start;
//   - test tracing, the real command, from its canary's start;
//   - the same fetch with the floor forced to 0, which is the read before the
//     bound: every row since the day floor;
//   - analyze trace by a bare trace ID, from the window start, of a query that
//     started three hours before the window and whose early spans finished
//     long before it. It must still return every span of the query. It
//     finished 15 minutes ago, outside every Live Span Selection lookback in
//     this suite: a trace that long would otherwise pull those tests' span
//     reads three hours back.
func TestIntegration_TraceSpanFetchReadsFromSince(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()

	dayFloorRows := seedOldSpanLogRows(t, conn)
	longTrace := uuid.New()
	if err := conn.Exec(context.Background(), fmt.Sprintf(`
		INSERT INTO system.opentelemetry_span_log
			(hostname, trace_id, span_id, parent_span_id, operation_name, kind, start_time_us, finish_time_us, %s, attribute)
		SELECT 'click-dog-integration-long', toUUID('%s'), number + 1, if(number = 0, 0, 1),
			if(number = 0, 'query', 'IntegrationLongChild'), 'INTERNAL',
			start_us, finish_us, toDate(toDateTime(intDiv(finish_us, 1000000))), map()
		FROM (
			WITH toUnixTimestamp64Micro(now64(6)) AS now_us
			SELECT number, now_us - 10800000000 + number AS start_us,
				multiIf(number = 0, now_us - 900000000, number <= 5, now_us - 10200000000, now_us - 960000000) AS finish_us
			FROM numbers(11)
		)`, spanLogDateColumn(t, conn), longTrace)); err != nil {
		t.Fatalf("seed long trace: %v", err)
	}
	window := time.Now().Add(-20 * time.Minute)

	fixture := seedTracedQueryFixture(t, conn, 1)
	reader := newCHReader(t, 1)
	defer reader.Close()
	tag := "click-dog-trace-span-fetch-" + fixture.Marker
	tagged := func(read string) context.Context {
		return clickhouse.Context(context.Background(), clickhouse.WithSettings(clickhouse.Settings{"log_comment": tag + "/" + read}))
	}

	ids, queryStart, err := reader.FetchTraceIDsByQueryID(tagged("bridge"), fixture.QueryIDs[0], 1, fixture.WindowStart, 10)
	if err != nil || len(ids) != 1 || ids[0] != fixture.TraceIDs[0].String() {
		t.Fatalf("FetchTraceIDsByQueryID = %v, %v; want [%s]", ids, err, fixture.TraceIDs[0])
	}
	bounded, err := reader.FetchSpansForTraceIDs(tagged("query-id spans"), ids, 1, queryStart, 1000, nil)
	if err != nil {
		t.Fatalf("FetchSpansForTraceIDs from the query start: %v", err)
	}
	unbounded, err := reader.FetchSpansForTraceIDs(tagged("query-id spans, floor 0"), ids, 1, time.Time{}, 1000, nil)
	if err != nil {
		t.Fatalf("FetchSpansForTraceIDs with no floor: %v", err)
	}
	if got, want := spanIDSet(bounded), spanIDSet(unbounded); len(bounded) == 0 || got != want {
		t.Fatalf("bounded fetch spans %s, want the unbounded fetch's %s", got, want)
	}

	long, err := reader.FetchSpansForTraceIDs(tagged("long query by trace ID"), []string{longTrace.String()}, 1, window, 1000, nil)
	if err != nil {
		t.Fatalf("FetchSpansForTraceIDs of the long query: %v", err)
	}
	if len(long) != 11 {
		t.Fatalf("long query returned %d spans, want all 11: %s", len(long), spanIDSet(long))
	}

	identity, err := generateTracingIdentity()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		ClickHouse: config.ClickHouseConfig{
			Host: integrationCHHost(1), Port: integrationCHPort(1), Database: "default", Username: "default",
			MaxOpenConns: 2, MaxIdleConns: 1, QueryTimeoutS: 30, MaxMemoryUsage: 104857600,
		},
		Monitor: config.MonitorConfig{ExportTimeoutS: 5},
	}
	deps := productionTracingTestDependencies()
	deps.buildExporters = func(*config.Config) []builtExporter {
		return []builtExporter{{Label: "recording[0]", Endpoint: "memory", Exporter: &tracingIntegrationExporter{}}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out bytes.Buffer
	if code := executeTracingTest(ctx, cfg, identity, deps, &out); code != 0 {
		t.Fatalf("tracing command exit = %d:\n%s", code, out.String())
	}

	reads := map[string][]uint64{}
	for _, read := range spanLogReads(t, conn, "startsWith(log_comment, ?)", tag+"/") {
		name := strings.TrimPrefix(read.tag, tag+"/")
		reads[name] = append(reads[name], read.rows)
		t.Logf("%s: read_rows=%d read_bytes=%d (rows since the day floor: %d)", name, read.rows, read.bytes, dayFloorRows)
	}
	polls := spanLogReads(t, conn, "position(query, ?) > 0", identity.TraceID.String())
	for i, read := range polls {
		t.Logf("test tracing poll %d: read_rows=%d read_bytes=%d", i+1, read.rows, read.bytes)
		reads["test tracing"] = append(reads["test tracing"], read.rows)
	}

	for _, read := range []string{"bridge", "query-id spans", "test tracing"} {
		if len(reads[read]) == 0 {
			t.Errorf("no %s read in system.query_log", read)
		}
		for _, got := range reads[read] {
			if got >= oldSpanRows/2 {
				t.Errorf("%s read %d rows, want well under the %d seeded rows hours older than its floor", read, got, oldSpanRows)
			}
		}
	}
	if got := reads["query-id spans, floor 0"]; len(got) != 1 || got[0] < oldSpanRows {
		t.Errorf("floor-0 fetch read %v rows, want every one of the %d seeded rows: the comparison is void", got, oldSpanRows)
	}
	// The long query is read in two parts: the window, then the gap back to
	// a minute before the query started, three hours ago. It reaches about a
	// quarter of the seeded rows, not the day.
	if got := reads["long query by trace ID"]; len(got) != 2 {
		t.Errorf("long query reads = %v, want the window read and one gap read", got)
	} else if total := got[0] + got[1]; total >= dayFloorRows/2 {
		t.Errorf("long query read %d rows, want under half the %d rows since the day floor", total, dayFloorRows)
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
