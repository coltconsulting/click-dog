package clickhouse

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// TestBuildTableRef verifies table reference generation
func TestBuildTableRef(t *testing.T) {
	tests := []struct {
		name       string
		cluster    string
		useCluster bool
		table      string
		expected   string
	}{
		{
			name:       "no cluster - local table",
			cluster:    "",
			useCluster: false,
			table:      "system.query_log",
			expected:   "system.query_log",
		},
		{
			name:       "cluster set but queries disabled",
			cluster:    "my_cluster",
			useCluster: false,
			table:      "system.query_log",
			expected:   "system.query_log",
		},
		{
			name:       "cluster queries enabled",
			cluster:    "my_cluster",
			useCluster: true,
			table:      "system.query_log",
			expected:   "cluster('my_cluster', system.query_log)",
		},
		{
			name:       "cluster queries enabled - opentelemetry table",
			cluster:    "production",
			useCluster: true,
			table:      "system.opentelemetry_span_log",
			expected:   "cluster('production', system.opentelemetry_span_log)",
		},
		{
			name:       "escapes single quotes in cluster name",
			cluster:    "it''s",
			useCluster: true,
			table:      "system.query_log",
			expected:   "cluster('it''''s', system.query_log)",
		},
		{
			name:       "escapes backslashes in cluster name",
			cluster:    `prod\cluster`,
			useCluster: true,
			table:      "system.query_log",
			expected:   `cluster('prod\\cluster', system.query_log)`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildTableRef(tt.table, tt.cluster, tt.useCluster)
			if result != tt.expected {
				t.Errorf("Expected %q, got %q", tt.expected, result)
			}
		})
	}
}

// TestClickHouseReader_SpanLimitCalculation pins the contract for
// monitor.max_spans_per_cycle: the configured value is the actual SQL LIMIT
// applied to the spans query, so no more than that many spans are fetched (or
// exported) per cycle. The only adjustment is the MaxSpanQueryLimit safety
// ceiling for unset/oversized values.
func TestClickHouseReader_SpanLimitCalculation(t *testing.T) {
	tests := []struct {
		name              string
		limit             int
		expectedSpanLimit int
	}{
		{
			name:              "standard limit is honored verbatim",
			limit:             100,
			expectedSpanLimit: 100,
		},
		{
			name:              "small limit is honored verbatim",
			limit:             10,
			expectedSpanLimit: 10,
		},
		{
			name:              "default limit is honored verbatim",
			limit:             1000,
			expectedSpanLimit: 1000,
		},
		{
			name:              "limit equal to safety ceiling is honored",
			limit:             MaxSpanQueryLimit,
			expectedSpanLimit: MaxSpanQueryLimit,
		},
		{
			name:              "limit above safety ceiling is clamped",
			limit:             MaxSpanQueryLimit + 1,
			expectedSpanLimit: MaxSpanQueryLimit,
		},
		{
			name:              "very large limit is clamped to safety ceiling",
			limit:             5_000_000,
			expectedSpanLimit: MaxSpanQueryLimit,
		},
		{
			name:              "zero limit falls back to safety ceiling",
			limit:             0,
			expectedSpanLimit: MaxSpanQueryLimit,
		},
		{
			name:              "negative limit falls back to safety ceiling",
			limit:             -1,
			expectedSpanLimit: MaxSpanQueryLimit,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveSpanLimit(tt.limit)
			if got != tt.expectedSpanLimit {
				t.Errorf("resolveSpanLimit(%d) = %d, want %d", tt.limit, got, tt.expectedSpanLimit)
			}
			if got > MaxSpanQueryLimit {
				t.Errorf("resolveSpanLimit(%d) = %d exceeds MaxSpanQueryLimit %d", tt.limit, got, MaxSpanQueryLimit)
			}
			// Per-cycle export cap: when the user configures a positive
			// value within the safety ceiling, the resolved SQL LIMIT must
			// not exceed it. This is the property the issue requires.
			if tt.limit > 0 && tt.limit <= MaxSpanQueryLimit && got > tt.limit {
				t.Errorf("resolveSpanLimit(%d) = %d exceeds the configured per-cycle cap", tt.limit, got)
			}
		})
	}
}

// TestClickHouseReader_SpanLimitNeverExceedsConfiguredCap is a property-style
// regression test for issue #91: the previous implementation computed
// `limit * 100`, which silently allowed up to 100x the configured cap. This
// test guards against any future re-introduction of that multiplier.
func TestClickHouseReader_SpanLimitNeverExceedsConfiguredCap(t *testing.T) {
	for _, limit := range []int{1, 10, 100, 500, 1000, 5000, 10_000, MaxSpanQueryLimit - 1, MaxSpanQueryLimit} {
		got := resolveSpanLimit(limit)
		if got > limit {
			t.Errorf("resolveSpanLimit(%d) = %d, exceeds configured per-cycle cap", limit, got)
		}
	}
}

// TestQueryLogBuilder_AddDurationFilters verifies SQL fragment and params produced by addDurationFilters.
func TestQueryLogBuilder_AddDurationFilters(t *testing.T) {
	tests := []struct {
		name           string
		minDurationMs  int
		maxDurationMs  int
		maxQueryLength int
		wantSQL        string
		wantParams     []interface{}
	}{
		{
			name:           "min only",
			minDurationMs:  1000,
			maxDurationMs:  0,
			maxQueryLength: 0,
			wantSQL:        " AND query_duration_ms >= ?",
			wantParams:     []interface{}{1000},
		},
		{
			name:           "min and max duration",
			minDurationMs:  500,
			maxDurationMs:  5000,
			maxQueryLength: 0,
			wantSQL:        " AND query_duration_ms >= ? AND query_duration_ms <= ?",
			wantParams:     []interface{}{500, 5000},
		},
		{
			name:           "all filters",
			minDurationMs:  100,
			maxDurationMs:  3000,
			maxQueryLength: 10000,
			wantSQL:        " AND query_duration_ms >= ? AND query_duration_ms <= ? AND length(query) <= ?",
			wantParams:     []interface{}{100, 3000, 10000},
		},
		{
			name:           "min and query length only",
			minDurationMs:  200,
			maxDurationMs:  0,
			maxQueryLength: 50000,
			wantSQL:        " AND query_duration_ms >= ? AND length(query) <= ?",
			wantParams:     []interface{}{200, 50000},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &queryLogBuilder{}
			b.addDurationFilters(tt.minDurationMs, tt.maxDurationMs, tt.maxQueryLength)

			if b.query != tt.wantSQL {
				t.Errorf("SQL mismatch\n got: %q\nwant: %q", b.query, tt.wantSQL)
			}
			if len(b.params) != len(tt.wantParams) {
				t.Fatalf("params length %d, want %d", len(b.params), len(tt.wantParams))
			}
			for i, want := range tt.wantParams {
				if b.params[i] != want {
					t.Errorf("params[%d] = %v, want %v", i, b.params[i], want)
				}
			}
		})
	}
}

// TestQueryLogBuilder_AddOrderAndLimit verifies ORDER BY and LIMIT output.
func TestQueryLogBuilder_AddOrderAndLimit(t *testing.T) {
	tests := []struct {
		name       string
		ascending  bool
		limit      int
		wantSQL    string
		wantParams []interface{}
	}{
		{
			name:       "descending no limit",
			ascending:  false,
			limit:      0,
			wantSQL:    " ORDER BY event_time DESC",
			wantParams: nil,
		},
		{
			name:       "ascending no limit",
			ascending:  true,
			limit:      0,
			wantSQL:    " ORDER BY event_time ASC",
			wantParams: nil,
		},
		{
			name:       "descending with limit",
			ascending:  false,
			limit:      100,
			wantSQL:    " ORDER BY event_time DESC LIMIT ?",
			wantParams: []interface{}{100},
		},
		{
			name:       "ascending with limit",
			ascending:  true,
			limit:      50,
			wantSQL:    " ORDER BY event_time ASC LIMIT ?",
			wantParams: []interface{}{50},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &queryLogBuilder{}
			b.addOrderAndLimit(tt.ascending, tt.limit)

			if b.query != tt.wantSQL {
				t.Errorf("SQL mismatch\n got: %q\nwant: %q", b.query, tt.wantSQL)
			}
			if len(b.params) != len(tt.wantParams) {
				t.Errorf("params length mismatch: got %d, want %d", len(b.params), len(tt.wantParams))
			}
			for i := range tt.wantParams {
				if i < len(b.params) && b.params[i] != tt.wantParams[i] {
					t.Errorf("params[%d] = %v, want %v", i, b.params[i], tt.wantParams[i])
				}
			}
		})
	}
}

// TestNewQueryLogBuilder_ParamsDefensiveCopy verifies that the builder does not alias the caller's slice.
func TestNewQueryLogBuilder_ParamsDefensiveCopy(t *testing.T) {
	reader := &ClickHouseReader{}
	original := []interface{}{1, 2}
	b := reader.newQueryLogBuilder("1=1", original)

	// Mutate original — builder params must be unaffected.
	original[0] = 999

	if b.params[0] == 999 {
		t.Error("builder params aliased caller slice; expected a defensive copy")
	}
}

func TestQueryLogBuilder_AddUserFilters(t *testing.T) {
	tests := []struct {
		name       string
		whitelist  []string
		blacklist  []string
		wantSQL    string
		wantParams int
	}{
		{
			name:       "no filters is a no-op",
			whitelist:  nil,
			blacklist:  nil,
			wantSQL:    "",
			wantParams: 0,
		},
		{
			name:       "whitelist only",
			whitelist:  []string{"app_frontend", "app_analytics"},
			wantSQL:    " AND user IN ?",
			wantParams: 1,
		},
		{
			name:       "blacklist only",
			blacklist:  []string{"patient_records"},
			wantSQL:    " AND user NOT IN ?",
			wantParams: 1,
		},
		{
			name:       "both lists",
			whitelist:  []string{"a"},
			blacklist:  []string{"b"},
			wantSQL:    " AND user IN ? AND user NOT IN ?",
			wantParams: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &queryLogBuilder{}
			b.addUserFilters(tt.whitelist, tt.blacklist)
			if b.query != tt.wantSQL {
				t.Errorf("SQL mismatch\n got: %q\nwant: %q", b.query, tt.wantSQL)
			}
			if len(b.params) != tt.wantParams {
				t.Errorf("params length = %d, want %d", len(b.params), tt.wantParams)
			}
			if len(tt.whitelist) > 0 && len(tt.blacklist) > 0 {
				wl := strings.Index(b.query, "AND user IN ?")
				bl := strings.Index(b.query, "AND user NOT IN ?")
				if wl < 0 || bl < 0 || wl >= bl {
					t.Errorf("whitelist clause must precede blacklist clause: %q", b.query)
				}
			}
		})
	}
}

// Guards the setUserFilter → slow-query-builder wiring against a silent regression.
func TestSlowQueriesBuilder_PicksUpUserFilters(t *testing.T) {
	r := &ClickHouseReader{
		whitelistUsers: []string{"app_frontend"},
		blacklistUsers: []string{"patient_records"},
	}

	b := r.slowQueriesBuilder(100, 0, 0, 60*time.Second, 10)
	if !strings.Contains(b.query, "AND user IN ?") {
		t.Errorf("slowQueriesBuilder did not splice whitelist clause\nSQL: %s", b.query)
	}
	if !strings.Contains(b.query, "AND user NOT IN ?") {
		t.Errorf("slowQueriesBuilder did not splice blacklist clause\nSQL: %s", b.query)
	}

	b = r.slowQueriesInRangeBuilder(100, 0, 0, time.Now().Add(-time.Hour), time.Now(), 10)
	if !strings.Contains(b.query, "AND user IN ?") {
		t.Errorf("slowQueriesInRangeBuilder did not splice whitelist clause\nSQL: %s", b.query)
	}
	if !strings.Contains(b.query, "AND user NOT IN ?") {
		t.Errorf("slowQueriesInRangeBuilder did not splice blacklist clause\nSQL: %s", b.query)
	}
}

func TestSlowQueriesBuilder_NoUserFilterByDefault(t *testing.T) {
	r := &ClickHouseReader{}
	b := r.slowQueriesBuilder(100, 0, 0, 60*time.Second, 10)
	if strings.Contains(b.query, "user IN") || strings.Contains(b.query, "user NOT IN") {
		t.Errorf("expected no user clauses when reader has no filter configured\nSQL: %s", b.query)
	}
}

func TestUserFilter_SecondCallPanics(t *testing.T) {
	r := &ClickHouseReader{enrichSelect: queryLogEnrichSelectSQL}
	r.setUserFilter([]string{"a"}, nil)

	defer func() {
		if recover() == nil {
			t.Fatal("expected setUserFilter to panic on second call")
		}
	}()
	r.setUserFilter([]string{"b"}, nil)
}

func TestUserFilter_SecondNoOpCallStaysSilent(t *testing.T) {
	// A real-then-empty call is a no-op by design (the early-return at the
	// top sees both lists empty before reaching the double-call guard).
	r := &ClickHouseReader{enrichSelect: queryLogEnrichSelectSQL}
	r.setUserFilter([]string{"a"}, nil)
	before := r.enrichSelect
	beforeParams := len(r.enrichExtraParams)

	r.setUserFilter(nil, nil) // must not panic

	if r.enrichSelect != before {
		t.Error("empty second call mutated enrichSelect")
	}
	if len(r.enrichExtraParams) != beforeParams {
		t.Error("empty second call mutated enrichExtraParams")
	}
}

func TestUserFilter_MissingAnchorPanics(t *testing.T) {
	r := &ClickHouseReader{enrichSelect: "SELECT user FROM system.query_log WHERE 1=1"}

	defer func() {
		if recover() == nil {
			t.Fatal("expected setUserFilter to panic when anchor is missing")
		}
	}()
	r.setUserFilter([]string{"a"}, nil)
}

func TestUserFilter_DuplicateAnchorPanics(t *testing.T) {
	// If a future template edit accidentally introduces a second splice
	// marker (copy-paste, etc.), strings.Replace would target the wrong
	// occurrence — fail loud rather than silently misplacing the clause.
	r := &ClickHouseReader{
		enrichSelect: "SELECT 1 FROM x -- {USER_FILTER_SPLICE} ORDER BY a UNION ALL SELECT 2 FROM y -- {USER_FILTER_SPLICE} ORDER BY b",
	}
	defer func() {
		if recover() == nil {
			t.Fatal("expected setUserFilter to panic on duplicate anchor")
		}
	}()
	r.setUserFilter([]string{"a"}, nil)
}

func TestUserFilter_DefensiveCopy(t *testing.T) {
	// Mutating the caller's slice after setUserFilter must not mutate the
	// reader's bindings — same invariant as TestNewQueryLogBuilder_ParamsDefensiveCopy.
	r := &ClickHouseReader{enrichSelect: queryLogEnrichSelectSQL}
	wl := []string{"app_frontend"}
	bl := []string{"patient_records"}
	r.setUserFilter(wl, bl)

	wl[0] = "MUTATED"
	bl[0] = "MUTATED"

	if r.whitelistUsers[0] == "MUTATED" {
		t.Error("reader.whitelistUsers aliased caller's slice")
	}
	if r.blacklistUsers[0] == "MUTATED" {
		t.Error("reader.blacklistUsers aliased caller's slice")
	}
	if got := r.enrichExtraParams[0].([]string)[0]; got == "MUTATED" {
		t.Error("enrichExtraParams[0] aliased caller's whitelist slice")
	}
}

func TestUserFilter_BlacklistNotInEnrichSelect(t *testing.T) {
	// Regression: blacklist must NOT be spliced into enrichSelect.
	// Pushing it down to SQL would strip blacklisted users' rows from the
	// enrichment map; resolveSpanUser would then return "" and the Go-level
	// blacklist (permissive on unknown users by design) would silently let
	// blacklisted spans through. Blacklist is enforced exclusively at the
	// Go layer for the enrichment path.
	r := &ClickHouseReader{enrichSelect: queryLogEnrichSelectSQL}
	r.setUserFilter([]string{"app_frontend"}, []string{"patient_records"})

	if strings.Contains(r.enrichSelect, "user NOT IN") {
		t.Errorf("blacklist must not appear in enrichSelect:\n%s", r.enrichSelect)
	}
	// Whitelist remains the only enrichment-side clause; one bound param.
	if len(r.enrichExtraParams) != 1 {
		t.Fatalf("expected 1 param (whitelist only), got %d", len(r.enrichExtraParams))
	}
	got, ok := r.enrichExtraParams[0].([]string)
	if !ok || !slices.Equal(got, []string{"app_frontend"}) {
		t.Errorf("enrichExtraParams[0] = %v, want whitelist", r.enrichExtraParams[0])
	}
	// Blacklist is still recorded for the slow-query builder path.
	if !slices.Equal(r.blacklistUsers, []string{"patient_records"}) {
		t.Errorf("blacklistUsers = %v, want stored for slow-query path", r.blacklistUsers)
	}
}

func TestUserFilter_EnrichmentParamCountMatchesPlaceholders(t *testing.T) {
	// FetchQueryLogByQueryIDs assembles params as
	//   {queryIDs, lookbackDays} ++ enrichExtraParams
	// so the count of ? placeholders in enrichSelect must equal
	// 2 + len(enrichExtraParams). If a future change adds a new fixed ?
	// param to the template without inserting it before the append of
	// enrichExtraParams, this test fires.
	cases := []struct {
		name      string
		whitelist []string
		blacklist []string
	}{
		{"no filter", nil, nil},
		{"whitelist only", []string{"a"}, nil},
		{"blacklist only", nil, []string{"b"}},
		{"both lists", []string{"a"}, []string{"b"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r := &ClickHouseReader{enrichSelect: queryLogEnrichSelectSQL}
			r.setUserFilter(tt.whitelist, tt.blacklist)

			placeholders := strings.Count(r.enrichSelect, "?")
			expected := 2 + len(r.enrichExtraParams)
			if placeholders != expected {
				t.Errorf("placeholder/param mismatch: %d ? in SQL, want %d (2 fixed + %d extras)\nSQL: %s",
					placeholders, expected, len(r.enrichExtraParams), r.enrichSelect)
			}
		})
	}
}

func TestUserFilter_AnchorStrippedFromFinalSQL(t *testing.T) {
	// The build-time splice marker must never appear in the SQL CH executes
	// — it would otherwise show up in system.query_log as a confusing
	// comment. Covers all four configurations.
	cases := []struct {
		name      string
		whitelist []string
		blacklist []string
	}{
		{"no filters", nil, nil},
		{"whitelist only", []string{"a"}, nil},
		{"blacklist only", nil, []string{"b"}},
		{"both lists", []string{"a"}, []string{"b"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r := &ClickHouseReader{enrichSelect: queryLogEnrichSelectSQL}
			r.setUserFilter(tt.whitelist, tt.blacklist)
			if strings.Contains(r.enrichSelect, enrichSelectAnchor) {
				t.Errorf("anchor leaked into final SQL:\n%s", r.enrichSelect)
			}
		})
	}
}

func TestUserFilter_SplicesEnrichSelect(t *testing.T) {
	tests := []struct {
		name              string
		whitelist         []string
		blacklist         []string
		wantContains      []string
		wantParamCount    int
		wantOrderByBefore string // SQL fragment that must precede ORDER BY
	}{
		{
			name:           "neither list is a no-op",
			whitelist:      nil,
			blacklist:      nil,
			wantContains:   nil,
			wantParamCount: 0,
		},
		{
			name:              "whitelist only",
			whitelist:         []string{"app_frontend"},
			wantContains:      []string{"AND user IN ?"},
			wantParamCount:    1,
			wantOrderByBefore: "AND user IN ?",
		},
		{
			// Blacklist intentionally does NOT touch enrichSelect — see
			// TestUserFilter_BlacklistNotInEnrichSelect for the rationale.
			name:           "blacklist only leaves enrichSelect untouched",
			blacklist:      []string{"patient_records"},
			wantContains:   nil,
			wantParamCount: 0,
		},
		{
			name:              "both lists splice whitelist only",
			whitelist:         []string{"a"},
			blacklist:         []string{"b"},
			wantContains:      []string{"AND user IN ?"},
			wantParamCount:    1,
			wantOrderByBefore: "AND user IN ?",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &ClickHouseReader{enrichSelect: queryLogEnrichSelectSQL}
			r.setUserFilter(tt.whitelist, tt.blacklist)

			for _, want := range tt.wantContains {
				if !strings.Contains(r.enrichSelect, want) {
					t.Errorf("enrichSelect missing %q\ngot: %s", want, r.enrichSelect)
				}
			}
			if len(r.enrichExtraParams) != tt.wantParamCount {
				t.Errorf("enrichExtraParams length = %d, want %d", len(r.enrichExtraParams), tt.wantParamCount)
			}
			// ORDER BY must remain after the user clauses, otherwise the SQL
			// is malformed.
			if tt.wantOrderByBefore != "" {
				userIdx := strings.Index(r.enrichSelect, tt.wantOrderByBefore)
				orderIdx := strings.Index(r.enrichSelect, "ORDER BY event_time")
				if userIdx < 0 || orderIdx < 0 || userIdx >= orderIdx {
					t.Errorf("user clause must precede ORDER BY\nuserIdx=%d orderIdx=%d\nSQL: %s", userIdx, orderIdx, r.enrichSelect)
				}
			}
		})
	}
}
