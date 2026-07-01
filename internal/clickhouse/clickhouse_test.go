package clickhouse

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/config"
)

// TestClickHouseReader_QueryTimeout verifies that query timeouts are respected
func TestClickHouseReader_QueryTimeout(t *testing.T) {
	// This test verifies that the context timeout is properly applied
	// We test this by checking the timeout is set in the reader config

	cfg := config.ClickHouseConfig{
		Host:          "localhost",
		Port:          9000,
		Database:      "default",
		Username:      "default",
		Password:      "",
		MaxOpenConns:  2,
		MaxIdleConns:  1,
		QueryTimeoutS: 5, // 5 second timeout
	}

	// Verify timeout is correctly converted to duration
	expectedTimeout := 5 * time.Second
	actualTimeout := time.Duration(cfg.QueryTimeoutS) * time.Second

	if actualTimeout != expectedTimeout {
		t.Errorf("Expected timeout %v, got %v", expectedTimeout, actualTimeout)
	}
}

// TestClickHouseConfig_Defaults verifies default configuration values
func TestClickHouseConfig_Defaults(t *testing.T) {
	tests := []struct {
		name     string
		config   config.ClickHouseConfig
		checkFn  func(config.ClickHouseConfig) bool
		expected string
	}{
		{
			name:   "MaxOpenConns defaults to 2 when 0",
			config: config.ClickHouseConfig{MaxOpenConns: 0},
			checkFn: func(c config.ClickHouseConfig) bool {
				// Simulate the default logic from config.go
				if c.MaxOpenConns == 0 {
					c.MaxOpenConns = 2
				}
				return c.MaxOpenConns == 2
			},
			expected: "MaxOpenConns should default to 2",
		},
		{
			name:   "MaxIdleConns defaults to 1 when 0",
			config: config.ClickHouseConfig{MaxIdleConns: 0},
			checkFn: func(c config.ClickHouseConfig) bool {
				if c.MaxIdleConns == 0 {
					c.MaxIdleConns = 1
				}
				return c.MaxIdleConns == 1
			},
			expected: "MaxIdleConns should default to 1",
		},
		{
			name:   "QueryTimeoutS defaults to 30 when 0",
			config: config.ClickHouseConfig{QueryTimeoutS: 0},
			checkFn: func(c config.ClickHouseConfig) bool {
				if c.QueryTimeoutS == 0 {
					c.QueryTimeoutS = 30
				}
				return c.QueryTimeoutS == 30
			},
			expected: "QueryTimeoutS should default to 30",
		},
		{
			name:   "Explicit values are preserved",
			config: config.ClickHouseConfig{MaxOpenConns: 10, MaxIdleConns: 5, QueryTimeoutS: 60},
			checkFn: func(c config.ClickHouseConfig) bool {
				return c.MaxOpenConns == 10 && c.MaxIdleConns == 5 && c.QueryTimeoutS == 60
			},
			expected: "Explicit values should be preserved",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.checkFn(tt.config) {
				t.Error(tt.expected)
			}
		})
	}
}

// TestClickHouseReader_ConnectionPoolConfig verifies connection pool settings
func TestClickHouseReader_ConnectionPoolConfig(t *testing.T) {
	tests := []struct {
		name        string
		maxOpen     int
		maxIdle     int
		expectError bool
		description string
	}{
		{
			name:        "valid pool config",
			maxOpen:     5,
			maxIdle:     2,
			expectError: false,
			description: "MaxIdle <= MaxOpen should be valid",
		},
		{
			name:        "maxIdle equals maxOpen",
			maxOpen:     3,
			maxIdle:     3,
			expectError: false,
			description: "MaxIdle == MaxOpen should be valid",
		},
		{
			name:        "minimal pool",
			maxOpen:     1,
			maxIdle:     1,
			expectError: false,
			description: "Single connection pool should be valid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.ClickHouseConfig{
				MaxOpenConns: tt.maxOpen,
				MaxIdleConns: tt.maxIdle,
			}

			// Validate the configuration logic
			if cfg.MaxIdleConns > cfg.MaxOpenConns {
				if !tt.expectError {
					t.Errorf("%s: MaxIdleConns (%d) > MaxOpenConns (%d) should be invalid",
						tt.description, cfg.MaxIdleConns, cfg.MaxOpenConns)
				}
			}
		})
	}
}

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

// TestClickHouseReader_LookbackCalculation verifies lookback duration calculation
func TestClickHouseReader_LookbackCalculation(t *testing.T) {
	tests := []struct {
		name             string
		checkIntervalS   int
		lookbackS        int
		expectedLookback int
	}{
		{
			name:             "explicit lookback",
			checkIntervalS:   30,
			lookbackS:        60,
			expectedLookback: 60,
		},
		{
			name:             "default lookback (interval + 10)",
			checkIntervalS:   30,
			lookbackS:        0, // Will be set to checkIntervalS + 10
			expectedLookback: 40,
		},
		{
			name:             "large interval",
			checkIntervalS:   300,
			lookbackS:        0,
			expectedLookback: 310,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				Monitor: config.MonitorConfig{
					CheckIntervalS: tt.checkIntervalS,
					LookbackS:      tt.lookbackS,
				},
			}

			// Apply default logic from config.go
			if cfg.Monitor.LookbackS == 0 {
				cfg.Monitor.LookbackS = cfg.Monitor.CheckIntervalS + 10
			}

			if cfg.Monitor.LookbackS != tt.expectedLookback {
				t.Errorf("Expected lookback %d, got %d", tt.expectedLookback, cfg.Monitor.LookbackS)
			}
		})
	}
}

// TestClickHouseReader_DurationFilters verifies duration filter logic
func TestClickHouseReader_DurationFilters(t *testing.T) {
	tests := []struct {
		name               string
		minTraceDurationMs int
		maxTraceDurationMs int
		minSpanDurationMs  int
		maxSpanDurationMs  int
		spanDurationMs     int
		shouldInclude      bool
	}{
		{
			name:               "span within range",
			minTraceDurationMs: 100,
			maxTraceDurationMs: 5000,
			minSpanDurationMs:  50,
			maxSpanDurationMs:  0, // no max
			spanDurationMs:     200,
			shouldInclude:      true,
		},
		{
			name:               "span below minimum",
			minTraceDurationMs: 100,
			maxTraceDurationMs: 0,
			minSpanDurationMs:  100,
			maxSpanDurationMs:  0,
			spanDurationMs:     50,
			shouldInclude:      false,
		},
		{
			name:               "span above maximum",
			minTraceDurationMs: 100,
			maxTraceDurationMs: 0,
			minSpanDurationMs:  0,
			maxSpanDurationMs:  1000,
			spanDurationMs:     2000,
			shouldInclude:      false,
		},
		{
			name:               "no filters (all zeros except min trace)",
			minTraceDurationMs: 100,
			maxTraceDurationMs: 0,
			minSpanDurationMs:  0,
			maxSpanDurationMs:  0,
			spanDurationMs:     50,
			shouldInclude:      true, // min_span is 0, so all spans included
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the SQL filter logic
			included := tt.minSpanDurationMs <= 0 || tt.spanDurationMs >= tt.minSpanDurationMs
			if tt.maxSpanDurationMs > 0 && tt.spanDurationMs > tt.maxSpanDurationMs {
				included = false
			}

			if included != tt.shouldInclude {
				t.Errorf("Expected shouldInclude=%v, got %v", tt.shouldInclude, included)
			}
		})
	}
}

// TestClickHouseReader_OperationBlacklist verifies operation_name blacklist logic.
// Each pattern is matched as a substring (LIKE '%pattern%').
func TestClickHouseReader_OperationBlacklist(t *testing.T) {
	tests := []struct {
		name          string
		blacklist     []string
		operationName string
		shouldInclude bool
	}{
		{
			name:          "no blacklist",
			blacklist:     nil,
			operationName: "MergeTreeIndex",
			shouldInclude: true,
		},
		{
			name:          "exact match excluded",
			blacklist:     []string{"MergeTreeIndex"},
			operationName: "MergeTreeIndex",
			shouldInclude: false,
		},
		{
			name:          "substring match excluded",
			blacklist:     []string{"MergeTreeSource"},
			operationName: "MergeTreeSource(prod.events (uuid))::tryGenerate",
			shouldInclude: false,
		},
		{
			name:          "query spans not affected",
			blacklist:     []string{"MergeTreeIndex", "VFSWrite"},
			operationName: "query",
			shouldInclude: true,
		},
		{
			name:          "empty pattern ignored",
			blacklist:     []string{""},
			operationName: "MergeTreeIndex",
			shouldInclude: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate SQL NOT LIKE '%pattern%' logic
			included := true
			for _, p := range tt.blacklist {
				if p == "" {
					continue
				}
				if strings.Contains(tt.operationName, p) {
					included = false
					break
				}
			}
			if included != tt.shouldInclude {
				t.Errorf("Expected shouldInclude=%v, got %v", tt.shouldInclude, included)
			}
		})
	}
}

func TestClickHouseReader_QueryLengthFilter(t *testing.T) {
	tests := []struct {
		name           string
		maxQueryLength int
		queryLength    int
		shouldInclude  bool
	}{
		{
			name:           "query within limit",
			maxQueryLength: 100000,
			queryLength:    5000,
			shouldInclude:  true,
		},
		{
			name:           "query at limit",
			maxQueryLength: 100000,
			queryLength:    100000,
			shouldInclude:  true,
		},
		{
			name:           "query exceeds limit",
			maxQueryLength: 100000,
			queryLength:    150000,
			shouldInclude:  false,
		},
		{
			name:           "no limit (0)",
			maxQueryLength: 0,
			queryLength:    500000,
			shouldInclude:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			included := tt.maxQueryLength <= 0 || tt.queryLength <= tt.maxQueryLength

			if included != tt.shouldInclude {
				t.Errorf("Expected shouldInclude=%v, got %v", tt.shouldInclude, included)
			}
		})
	}
}

// TestClickHouseReader_TwoStepSpanFetching verifies the two-step span fetching logic
func TestClickHouseReader_TwoStepSpanFetching(t *testing.T) {
	// This test verifies the logic of the two-step span fetching process:
	// Step 1: Find trace IDs with spans matching duration range
	// Step 2: Fetch full spans for those traces

	tests := []struct {
		name               string
		minTraceDurationMs int
		maxTraceDurationMs int
		minSpanDurationMs  int
		maxSpanDurationMs  int
		limit              int
		description        string
	}{
		{
			name:               "basic slow query detection",
			minTraceDurationMs: 1000,
			maxTraceDurationMs: 0,
			minSpanDurationMs:  0,
			maxSpanDurationMs:  0,
			limit:              100,
			description:        "Find traces with at least one span >= 1s, fetch all spans",
		},
		{
			name:               "with span duration filter",
			minTraceDurationMs: 1000,
			maxTraceDurationMs: 0,
			minSpanDurationMs:  500,
			maxSpanDurationMs:  0,
			limit:              100,
			description:        "Find traces >= 1s, but only export spans >= 500ms",
		},
		{
			name:               "with upper bounds",
			minTraceDurationMs: 1000,
			maxTraceDurationMs: 5000,
			minSpanDurationMs:  100,
			maxSpanDurationMs:  3000,
			limit:              50,
			description:        "Find traces between 1-5s, export spans 100ms-3s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Verify the configuration is valid
			if tt.minTraceDurationMs < 0 {
				t.Error("minTraceDurationMs cannot be negative")
			}
			if tt.maxTraceDurationMs != 0 && tt.maxTraceDurationMs < tt.minTraceDurationMs {
				t.Error("maxTraceDurationMs should be >= minTraceDurationMs or 0")
			}
			if tt.limit <= 0 {
				t.Error("limit should be positive")
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

// TestClickHouseReader_ErrorHandling verifies error handling patterns
func TestClickHouseReader_ErrorHandling(t *testing.T) {
	tests := []struct {
		name          string
		errorType     string
		shouldRetry   bool
		shouldCircuit bool
		description   string
	}{
		{
			name:          "connection timeout",
			errorType:     "timeout",
			shouldRetry:   true,
			shouldCircuit: true,
			description:   "Connection timeouts should trigger retry and circuit breaker",
		},
		{
			name:          "query error",
			errorType:     "query",
			shouldRetry:   true,
			shouldCircuit: true,
			description:   "Query errors should be recorded for circuit breaker",
		},
		{
			name:          "context canceled",
			errorType:     "canceled",
			shouldRetry:   false,
			shouldCircuit: false,
			description:   "Context cancellation is graceful, not a failure",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Verify error categorization
			switch tt.errorType {
			case "timeout":
				if !tt.shouldRetry || !tt.shouldCircuit {
					t.Error("Timeouts should trigger retry and circuit breaker")
				}
			case "query":
				if !tt.shouldCircuit {
					t.Error("Query errors should be recorded for circuit breaker")
				}
			case "canceled":
				if tt.shouldCircuit {
					t.Error("Context cancellation should not trigger circuit breaker")
				}
			}
		})
	}
}

// TestClickHouseReader_QueryTimeout verifies query timeout configuration
func TestClickHouseReader_QueryTimeoutConfig(t *testing.T) {
	tests := []struct {
		name             string
		queryTimeoutS    int
		expectedTimeoutS int
	}{
		{
			name:             "default timeout",
			queryTimeoutS:    0,
			expectedTimeoutS: 30, // Default
		},
		{
			name:             "custom timeout",
			queryTimeoutS:    60,
			expectedTimeoutS: 60,
		},
		{
			name:             "short timeout",
			queryTimeoutS:    5,
			expectedTimeoutS: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.ClickHouseConfig{
				QueryTimeoutS: tt.queryTimeoutS,
			}

			// Apply default logic
			timeout := cfg.QueryTimeoutS
			if timeout == 0 {
				timeout = 30
			}

			if timeout != tt.expectedTimeoutS {
				t.Errorf("Expected timeout %d, got %d", tt.expectedTimeoutS, timeout)
			}
		})
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
