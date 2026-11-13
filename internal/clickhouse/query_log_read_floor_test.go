package clickhouse

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestQueryLogReadFloor_OneMinuteBeforeInWholeSeconds(t *testing.T) {
	tests := []struct {
		name            string
		earliestStartUs uint64
		want            uint64
	}{
		{"unknown start bounds nothing", 0, 0},
		{"start inside the margin saturates", 59_999_999, 0},
		{"start at the margin saturates", 60_000_000, 0},
		{"one second past the margin", 61_000_000, 1},
		{"sub-second start truncates down", 100_999_999, 40},
		{"live page", 1_793_650_721_080_900, 1_793_650_661},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := queryLogReadFloor(tt.earliestStartUs); got != tt.want {
				t.Errorf("queryLogReadFloor(%d) = %d, want %d", tt.earliestStartUs, got, tt.want)
			}
		})
	}
}

// TestFetchQueryLogByQueryIDs_BoundsEventTime pins the enrichment read's
// granule bound: query_log is sorted by (event_date, event_time), so the read
// keeps the day-rounded event_date filter and adds an event_time floor a
// minute before the earliest span start, bound before the user whitelist.
func TestFetchQueryLogByQueryIDs_BoundsEventTime(t *testing.T) {
	conn := &captureConn{fakeConn: &fakeConn{}}
	r := &ClickHouseReader{
		conn:         conn,
		queryTimeout: time.Second,
		enrichSelect: buildQueryLogSQLWithCapabilities(queryLogEnrichSelectSQL, "system.query_log", true, true),
	}
	r.setUserFilter([]string{"app"}, nil)

	queryIDs := []string{"q-1", "q-2"}
	if _, err := r.FetchQueryLogByQueryIDs(context.Background(), queryIDs, 2, 1_793_650_721_080_900); err != nil {
		t.Fatalf("FetchQueryLogByQueryIDs: %v", err)
	}

	sql := conn.sqls[0]
	dateAt := strings.Index(sql, "AND event_date >= today() - ?")
	timeAt := strings.Index(sql, "AND event_time >= toDateTime(?)")
	userAt := strings.Index(sql, "AND if(initial_user != '', initial_user, user) IN ?")
	if dateAt < 0 || timeAt < 0 || userAt < 0 {
		t.Fatalf("enrichment SQL lacks the day floor, the event_time floor, or the user splice:\n%s", sql)
	}
	if dateAt > timeAt || timeAt > userAt {
		t.Errorf("predicates out of bind order (event_date, event_time, user):\n%s", sql)
	}
	args := conn.queries[0]
	want := []any{queryIDs, 2, uint64(1_793_650_661), []string{"app"}}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %#v, want %#v", args, want)
	}
	if got := strings.Count(sql, "?"); got != len(args) {
		t.Errorf("SQL has %d placeholders for %d args:\n%s", got, len(args), sql)
	}
}

func TestFetchQueryLogByQueryIDs_UnknownStartReadsToDayFloor(t *testing.T) {
	conn := &captureConn{fakeConn: &fakeConn{}}
	r := &ClickHouseReader{
		conn:         conn,
		queryTimeout: time.Second,
		enrichSelect: buildQueryLogSQLWithCapabilities(queryLogEnrichSelectSQL, "system.query_log", false, false),
	}
	r.setUserFilter(nil, nil)

	if _, err := r.FetchQueryLogByQueryIDs(context.Background(), []string{"q-1"}, 1, 0); err != nil {
		t.Fatalf("FetchQueryLogByQueryIDs: %v", err)
	}
	if args := conn.queries[0]; len(args) != 3 || args[2] != uint64(0) {
		t.Errorf("args = %#v, want an event_time floor of 0", args)
	}
}
