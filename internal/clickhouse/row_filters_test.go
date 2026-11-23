package clickhouse

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAddRowFilters_SQLAndParams(t *testing.T) {
	r := &ClickHouseReader{
		whitelistUsers:   []string{"app"},
		blacklistUsers:   []string{"backup"},
		blacklistQueries: []string{"secret", "(?i)password"},
		clientPrefixes:   []string{"10.0.0.0/8", "2001:db8::/32"},
	}
	start := time.Unix(1000, 0).UTC()
	b := &queryLogBuilder{}
	b.addRowFilters(r, initialQueryScope{table: "system.query_log", since: " AND event_time >= ?", params: []interface{}{start}})

	want := " AND if(initial_user != '', initial_user, user) IN ?" +
		" AND user NOT IN ? AND initial_user NOT IN ?" +
		" AND NOT match(query, ?) AND NOT match(query, ?)" +
		" AND (is_initial_query OR initial_query_id NOT IN (SELECT query_id FROM system.query_log" +
		" WHERE is_initial_query AND event_time >= ? AND (match(query, ?) OR match(query, ?))))" +
		" AND (isIPAddressInRange(" + originatingAddressSQL + ", ?) OR isIPAddressInRange(" + originatingAddressSQL + ", ?))"
	if b.query != want {
		t.Errorf("SQL =\n%s\nwant\n%s", b.query, want)
	}
	wantParams := []interface{}{
		[]string{"app"}, []string{"backup"}, []string{"backup"},
		"secret", "(?i)password",
		start, "secret", "(?i)password",
		"10.0.0.0/8", "2001:db8::/32",
	}
	if !reflect.DeepEqual(b.params, wantParams) {
		t.Errorf("params = %#v, want %#v", b.params, wantParams)
	}
}

func TestAddRowFilters_NoneConfigured(t *testing.T) {
	b := &queryLogBuilder{}
	b.addRowFilters(&ClickHouseReader{}, initialQueryScope{table: "system.query_log"})
	if b.query != "" || len(b.params) != 0 {
		t.Errorf("no filters should add nothing, got %q %v", b.query, b.params)
	}
}

// Every builder that returns query_log or system.processes rows or
// aggregates must carry the row filters: analyze, its notifications, and
// backfill read them directly, and an aggregate cannot be filtered later.
func TestRowFilters_ReachEveryRowBuilder(t *testing.T) {
	start := time.Unix(1000, 0).UTC()
	end := time.Unix(2000, 0).UTC()
	builders := []struct {
		name    string
		clauses int
		table   string // where the initial-query lookup reads
		build   func(*ClickHouseReader) *queryLogBuilder
	}{
		{"slow queries", 1, "system.query_log", func(r *ClickHouseReader) *queryLogBuilder {
			return r.slowQueriesBuilder(100, 0, 0, time.Hour, 10)
		}},
		{"slow queries in range, first page", 1, "system.query_log", func(r *ClickHouseReader) *queryLogBuilder {
			return r.slowQueriesInRangePageBuilder(100, 0, 0, start, end, nil, 10)
		}},
		{"slow queries in range, later page", 1, "system.query_log", func(r *ClickHouseReader) *queryLogBuilder {
			return r.slowQueriesInRangePageBuilder(100, 0, 0, start, end, &QueryLogCursor{EventTime: start, QueryID: "q1"}, 10)
		}},
		{"recent candidates", 1, "system.query_log", func(r *ClickHouseReader) *queryLogBuilder {
			return r.recentQueryCandidatesBuilder(RecentQueryCandidateOptions{StartTime: start, EndTime: end, Limit: 10})
		}},
		{"current candidates", 1, "system.processes", func(r *ClickHouseReader) *queryLogBuilder {
			return r.currentQueryCandidatesBuilder(CurrentQueryCandidateOptions{Limit: 10})
		}},
		{"query family exact groups", 1, "system.query_log", func(r *ClickHouseReader) *queryLogBuilder {
			return r.queryFamilyExactGroupsBuilder(QueryFamilyRollupOptions{StartTime: start, EndTime: end, MinExecutionCount: 1, ExactGroupLimit: 10})
		}},
		// The outer rows and the top-family subquery are both filtered, so
		// a family's dimensions and its rank agree.
		{"query family dimension counts", 2, "system.query_log", func(r *ClickHouseReader) *queryLogBuilder {
			return r.queryFamilyDimensionCountsBuilder(dimensionCountOpts())
		}},
	}
	for _, tt := range builders {
		t.Run(tt.name, func(t *testing.T) {
			r := &ClickHouseReader{
				queryLogSelect:     buildQueryLogSQL(queryLogSelectSQL, "system.query_log", true),
				queryFamilySelect:  strings.ReplaceAll(queryFamilyExactGroupsSQL, tblQueryLog, "system.query_log"),
				currentQuerySelect: buildCurrentQuerySQL(currentQueryCandidatesSelectSQL, "system.processes", true),
				queryLogRef:        "system.query_log",
				processesRef:       "system.processes",
				queryLogNormalized: true,
				blacklistQueries:   []string{"secret"},
				clientPrefixes:     []string{"10.0.0.0/8"},
			}
			b := tt.build(r)
			if got := strings.Count(b.query, "AND NOT match(query, ?)"); got != tt.clauses {
				t.Errorf("blacklist_queries clauses = %d, want %d:\n%s", got, tt.clauses, b.query)
			}
			if got := strings.Count(b.query, "isIPAddressInRange("+originatingAddressSQL+", ?)"); got != tt.clauses {
				t.Errorf("whitelist_ips clauses = %d, want %d:\n%s", got, tt.clauses, b.query)
			}
			initialLookup := "initial_query_id NOT IN (SELECT query_id FROM " + tt.table + " WHERE is_initial_query"
			if got := strings.Count(b.query, initialLookup); got != tt.clauses {
				t.Errorf("initial-query lookups on %s = %d, want %d:\n%s", tt.table, got, tt.clauses, b.query)
			}
			// Each pattern is bound once on the row's text and once on its
			// initial query's.
			if got := countParam(b.params, "secret"); got != 2*tt.clauses {
				t.Errorf("pattern params = %d, want %d: %v", got, 2*tt.clauses, b.params)
			}
			if got := countParam(b.params, "10.0.0.0/8"); got != tt.clauses {
				t.Errorf("prefix params = %d, want %d: %v", got, tt.clauses, b.params)
			}
			if got, want := strings.Count(b.query, "?"), len(b.params); got != want {
				t.Errorf("placeholders = %d, params = %d:\n%s", got, want, b.query)
			}
		})
	}
}

func countParam(params []interface{}, want string) int {
	n := 0
	for _, p := range params {
		if s, ok := p.(string); ok && s == want {
			n++
		}
	}
	return n
}

func TestSetRowFilters_NormalizesClientPrefixes(t *testing.T) {
	r := &ClickHouseReader{}
	if err := r.setRowFilters([]string{"secret"}, []string{"::ffff:10.0.0.1", "10.1.2.3/8", "2001:DB8::1"}); err != nil {
		t.Fatalf("setRowFilters: %v", err)
	}
	want := []string{"10.0.0.1/32", "10.0.0.0/8", "2001:db8::1/128"}
	if !reflect.DeepEqual(r.clientPrefixes, want) {
		t.Errorf("clientPrefixes = %v, want %v", r.clientPrefixes, want)
	}
	if !reflect.DeepEqual(r.blacklistQueries, []string{"secret"}) {
		t.Errorf("blacklistQueries = %v", r.blacklistQueries)
	}
	if err := r.setRowFilters(nil, []string{"not-an-ip"}); err == nil {
		t.Error("an invalid whitelist_ips entry should fail")
	}
}
