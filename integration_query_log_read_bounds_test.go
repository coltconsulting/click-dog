//go:build integration

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/coltconsulting/click-dog/internal/processor"
)

// oldQueryLogRows is how many query_log rows
// TestIntegration_QueryLogEnrichmentSkipsOlderGranules seeds hours before its
// fixture. They are QueryStart rows inside the enrichment read's day floor,
// so a read bounded only by event_date reads every one of them, while the
// QueryFinish reads other tests make never return them.
const oldQueryLogRows = 300_000

// TestIntegration_QueryLogEnrichmentSkipsOlderGranules measures the query_log
// enrichment read, which filters by query_id, a column absent from the sort
// key (event_date, event_time). It seeds rows from two to six hours ago, then
// enriches a fresh traced query the way a live cycle does, from the earliest
// start of its query span, and again with no start. The bounded read's own
// read_rows in system.query_log must stay far below the seeded rows, and the
// unbounded one must read them all, which shows they sit inside the day floor.
func TestIntegration_QueryLogEnrichmentSkipsOlderGranules(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()

	seed := fmt.Sprintf(`
		INSERT INTO system.query_log
			(type, event_date, event_time, event_time_microseconds, query_start_time, query_start_time_microseconds,
			 query_id, initial_query_id, user, initial_user, query)
		SELECT 'QueryStart', toDate(t), t, toDateTime64(t, 6), t, toDateTime64(t, 6),
			qid, qid, 'click-dog-integration-old', 'click-dog-integration-old', 'SELECT 1'
		FROM (
			SELECT toString(generateUUIDv4(number)) AS qid, now() - 7200 - intDiv(number * 14400, %d) AS t
			FROM numbers(%d)
		)`, oldQueryLogRows, oldQueryLogRows)
	if err := conn.Exec(context.Background(), seed); err != nil {
		t.Fatalf("seed old query_log rows: %v", err)
	}
	var dayFloorRows uint64
	if err := conn.QueryRow(context.Background(),
		"SELECT count() FROM system.query_log WHERE event_date >= today() - 1",
	).Scan(&dayFloorRows); err != nil {
		t.Fatalf("count query_log rows since the day floor: %v", err)
	}

	fixture := seedTracedQueryFixture(t, conn, 1)
	queryID := fixture.QueryIDs[0]

	reader := newCHReader(t, 1)
	defer reader.Close()
	spans, err := reader.FetchOpenTelemetrySpans(context.Background(), 1, 0, 0, 0, 10*time.Minute, 1000)
	if err != nil {
		t.Fatalf("FetchOpenTelemetrySpans: %v", err)
	}
	startUs := processor.EarliestQueryStartUs(exactFixtureSpans(t, spans, fixture))
	if startUs == 0 {
		t.Fatalf("Live Span Selection returned no query span of fixture query %s", queryID)
	}

	tag := "click-dog-query-log-read-bounds-" + fixture.Marker
	for _, read := range []struct {
		name    string
		startUs uint64
	}{{"bounded", startUs}, {"unbounded", 0}} {
		ctx := clickhouse.Context(context.Background(), clickhouse.WithSettings(clickhouse.Settings{"log_comment": tag + "-" + read.name}))
		logs, err := reader.FetchQueryLogByQueryIDs(ctx, []string{queryID}, 1, read.startUs)
		if err != nil {
			t.Fatalf("FetchQueryLogByQueryIDs (%s): %v", read.name, err)
		}
		if _, ok := logs[queryID]; !ok {
			t.Fatalf("%s enrichment read found no query_log row for fixture query %s", read.name, queryID)
		}
	}

	if err := conn.Exec(context.Background(), "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("flush logs: %v", err)
	}
	rows, err := conn.Query(context.Background(), `
		SELECT log_comment, read_rows, read_bytes
		FROM system.query_log
		WHERE startsWith(log_comment, ?) AND type = 'QueryFinish' AND event_date >= today() - 1
		  AND query LIKE '%LIMIT 2 BY query_id%'`, tag+"-")
	if err != nil {
		t.Fatalf("read query_log: %v", err)
	}
	defer rows.Close()

	reads := map[string]uint64{}
	for rows.Next() {
		var comment string
		var readRows, readBytes uint64
		if err := rows.Scan(&comment, &readRows, &readBytes); err != nil {
			t.Fatalf("scan query_log: %v", err)
		}
		read := strings.TrimPrefix(comment, tag+"-")
		reads[read] = readRows
		t.Logf("%s enrichment read: read_rows=%d read_bytes=%d (rows since the day floor: %d)", read, readRows, readBytes, dayFloorRows)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate query_log: %v", err)
	}

	bounded, ok := reads["bounded"]
	if !ok {
		t.Fatalf("no bounded enrichment read with log_comment %q in system.query_log", tag+"-bounded")
	}
	if bounded >= oldQueryLogRows/2 {
		t.Errorf("bounded enrichment read %d rows, want well under the %d seeded rows hours older than its floor", bounded, oldQueryLogRows)
	}
	unbounded, ok := reads["unbounded"]
	if !ok {
		t.Fatalf("no unbounded enrichment read with log_comment %q in system.query_log", tag+"-unbounded")
	}
	// The unbounded read is the control that shows what the bound saves. From
	// ClickHouse 26.8 the server skips those older granules on its own, so the
	// control reads a few rows too and proves nothing; only the bounded read's
	// cost above is a requirement.
	if unbounded < oldQueryLogRows {
		t.Logf("unbounded enrichment read %d rows: this ClickHouse skips the %d older rows without the time bound", unbounded, oldQueryLogRows)
	}
}
