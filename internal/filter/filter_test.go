package filter

import (
	"strings"
	"testing"

	"github.com/coltconsulting/click-dog/internal/config"
)

func TestQueryFilter_NewQueryFilter_EmptyConfig(t *testing.T) {
	cfg := config.FiltersConfig{}

	filter, err := NewQueryFilter(cfg)
	if err != nil {
		t.Fatalf("Failed to create filter with empty config: %v", err)
	}

	if filter.hasOperationWhitelist {
		t.Error("Empty config should not have operation whitelist")
	}
	if filter.hasIPWhitelist {
		t.Error("Empty config should not have IP whitelist")
	}
	if len(filter.operationPatterns) != 0 {
		t.Errorf("Expected 0 operation patterns, got %d", len(filter.operationPatterns))
	}
	if len(filter.queryBlacklistPatterns) != 0 {
		t.Errorf("Expected 0 blacklist patterns, got %d", len(filter.queryBlacklistPatterns))
	}
}

func TestQueryFilter_NewQueryFilter_InvalidRegex(t *testing.T) {
	tests := []struct {
		name    string
		config  config.FiltersConfig
		wantErr bool
	}{
		{
			name: "invalid blacklist regex",
			config: config.FiltersConfig{
				BlacklistQueries: []string{"[invalid(regex"},
			},
			wantErr: true,
		},
		{
			name: "valid blacklist regex",
			config: config.FiltersConfig{
				BlacklistQueries: []string{"^SELECT.*FROM"},
			},
			wantErr: false,
		},
		{
			name: "mixed valid and invalid",
			config: config.FiltersConfig{
				BlacklistQueries: []string{"valid", "[invalid"},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewQueryFilter(tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewQueryFilter() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestQueryFilter_WildcardConversion(t *testing.T) {
	tests := []struct {
		name          string
		patterns      []string
		operationName string
		shouldMatch   bool
	}{
		{
			name:          "exact match without wildcard",
			patterns:      []string{"DB::executeQuery()"},
			operationName: "DB::executeQuery()",
			shouldMatch:   true,
		},
		{
			name:          "no match without wildcard",
			patterns:      []string{"DB::executeQuery()"},
			operationName: "DB::otherQuery()",
			shouldMatch:   false,
		},
		{
			name:          "wildcard at end",
			patterns:      []string{"DB::Interpreter*"},
			operationName: "DB::InterpreterSelectQuery::execute()",
			shouldMatch:   true,
		},
		{
			name:          "wildcard at end - no match",
			patterns:      []string{"DB::Interpreter*"},
			operationName: "DB::OtherClass::execute()",
			shouldMatch:   false,
		},
		{
			name:          "wildcard in middle",
			patterns:      []string{"DB::*::execute()"},
			operationName: "DB::SomeClass::execute()",
			shouldMatch:   true,
		},
		{
			name:          "multiple wildcards",
			patterns:      []string{"*::Interpreter*::*"},
			operationName: "DB::InterpreterSelectQuery::execute()",
			shouldMatch:   true,
		},
		{
			name:          "wildcard matches empty string",
			patterns:      []string{"SELECT*"},
			operationName: "SELECT",
			shouldMatch:   true,
		},
		{
			name:          "special regex chars are escaped",
			patterns:      []string{"DB::func()"},
			operationName: "DB::func()",
			shouldMatch:   true,
		},
		{
			name:          "dots are escaped",
			patterns:      []string{"a.b.c"},
			operationName: "a.b.c",
			shouldMatch:   true,
		},
		{
			name:          "dots don't match arbitrary chars",
			patterns:      []string{"a.b"},
			operationName: "aXb",
			shouldMatch:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.FiltersConfig{
				WhitelistOperations: tt.patterns,
			}

			filter, err := NewQueryFilter(cfg)
			if err != nil {
				t.Fatalf("Failed to create filter: %v", err)
			}

			// ShouldFilter returns true if we should EXCLUDE the operation
			// So if operation matches whitelist, ShouldFilter returns false
			shouldFilter := filter.ShouldFilter(tt.operationName, "", "")
			matched := !shouldFilter

			if matched != tt.shouldMatch {
				t.Errorf("Operation %q with patterns %v: expected match=%v, got match=%v",
					tt.operationName, tt.patterns, tt.shouldMatch, matched)
			}
		})
	}
}

func TestQueryFilter_WhitelistOperations(t *testing.T) {
	cfg := config.FiltersConfig{
		WhitelistOperations: []string{
			"DB::InterpreterSelectQuery::execute()",
			"DB::InterpreterInsertQuery::execute()",
		},
	}

	filter, err := NewQueryFilter(cfg)
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	tests := []struct {
		operationName string
		shouldFilter  bool
		description   string
	}{
		{
			operationName: "DB::InterpreterSelectQuery::execute()",
			shouldFilter:  false,
			description:   "Whitelisted operation should not be filtered",
		},
		{
			operationName: "DB::InterpreterInsertQuery::execute()",
			shouldFilter:  false,
			description:   "Second whitelisted operation should not be filtered",
		},
		{
			operationName: "DB::InterpreterDeleteQuery::execute()",
			shouldFilter:  true,
			description:   "Non-whitelisted operation should be filtered",
		},
		{
			operationName: "SomeOtherOperation",
			shouldFilter:  true,
			description:   "Unrelated operation should be filtered",
		},
	}

	for _, tt := range tests {
		t.Run(tt.operationName, func(t *testing.T) {
			result := filter.ShouldFilter(tt.operationName, "", "")
			if result != tt.shouldFilter {
				t.Errorf("%s: expected shouldFilter=%v, got %v",
					tt.description, tt.shouldFilter, result)
			}
		})
	}
}

func TestQueryFilter_WhitelistIPs(t *testing.T) {
	cfg := config.FiltersConfig{
		WhitelistIPs: []string{"192.168.1.100", "10.0.0.1", "::1"},
	}

	filter, err := NewQueryFilter(cfg)
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	tests := []struct {
		clientIP     string
		shouldFilter bool
		description  string
	}{
		{
			clientIP:     "192.168.1.100",
			shouldFilter: false,
			description:  "Whitelisted IP should not be filtered",
		},
		{
			clientIP:     "10.0.0.1",
			shouldFilter: false,
			description:  "Second whitelisted IP should not be filtered",
		},
		{
			clientIP:     "::1",
			shouldFilter: false,
			description:  "IPv6 whitelisted IP should not be filtered",
		},
		{
			clientIP:     "192.168.1.200",
			shouldFilter: true,
			description:  "Non-whitelisted IP should be filtered",
		},
		{
			clientIP:     "8.8.8.8",
			shouldFilter: true,
			description:  "External IP should be filtered",
		},
	}

	for _, tt := range tests {
		t.Run(tt.clientIP, func(t *testing.T) {
			result := filter.ShouldFilter("", "", tt.clientIP)
			if result != tt.shouldFilter {
				t.Errorf("%s: expected shouldFilter=%v, got %v",
					tt.description, tt.shouldFilter, result)
			}
		})
	}
}

func TestQueryFilter_WhitelistIPs_CIDR(t *testing.T) {
	cfg := config.FiltersConfig{
		WhitelistIPs: []string{"10.0.0.0/8", "192.168.1.0/24", "::1"},
	}

	filter, err := NewQueryFilter(cfg)
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	tests := []struct {
		clientIP     string
		shouldFilter bool
		description  string
	}{
		{
			clientIP:     "10.1.2.3",
			shouldFilter: false,
			description:  "IP within 10.0.0.0/8 CIDR should not be filtered",
		},
		{
			clientIP:     "10.255.255.255",
			shouldFilter: false,
			description:  "Last IP in 10.0.0.0/8 CIDR should not be filtered",
		},
		{
			clientIP:     "192.168.1.50",
			shouldFilter: false,
			description:  "IP within 192.168.1.0/24 should not be filtered",
		},
		{
			clientIP:     "192.168.2.1",
			shouldFilter: true,
			description:  "IP outside 192.168.1.0/24 should be filtered",
		},
		{
			clientIP:     "::1",
			shouldFilter: false,
			description:  "Exact IPv6 match should not be filtered",
		},
		{
			clientIP:     "8.8.8.8",
			shouldFilter: true,
			description:  "IP outside all CIDRs should be filtered",
		},
		{
			clientIP:     "172.16.0.1",
			shouldFilter: true,
			description:  "IP not in any range should be filtered",
		},
	}

	for _, tt := range tests {
		t.Run(tt.clientIP, func(t *testing.T) {
			result := filter.ShouldFilter("", "", tt.clientIP)
			if result != tt.shouldFilter {
				t.Errorf("%s: expected shouldFilter=%v, got %v",
					tt.description, tt.shouldFilter, result)
			}
		})
	}
}

func TestQueryFilter_WhitelistIPs_InvalidCIDR(t *testing.T) {
	cfg := config.FiltersConfig{
		WhitelistIPs: []string{"not-a-cidr/99"},
	}

	_, err := NewQueryFilter(cfg)
	if err == nil {
		t.Error("Expected error for invalid CIDR, got nil")
	}
}

func TestQueryFilter_WhitelistIPs_MixedExactAndCIDR(t *testing.T) {
	cfg := config.FiltersConfig{
		WhitelistIPs: []string{"192.168.1.100", "10.0.0.0/8"},
	}

	filter, err := NewQueryFilter(cfg)
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	// Exact match
	if filter.ShouldFilter("", "", "192.168.1.100") {
		t.Error("Exact IP match should not be filtered")
	}

	// CIDR match
	if filter.ShouldFilter("", "", "10.5.5.5") {
		t.Error("IP within CIDR should not be filtered")
	}

	// Neither
	if !filter.ShouldFilter("", "", "172.16.0.1") {
		t.Error("IP outside both exact and CIDR should be filtered")
	}
}

func TestQueryFilter_BlacklistQueries(t *testing.T) {
	cfg := config.FiltersConfig{
		BlacklistQueries: []string{
			"^SELECT 1$",                 // Exact match
			"SYSTEM",                     // Contains SYSTEM
			"(?i)healthcheck",            // Case insensitive
			"^SELECT \\* FROM system\\.", // System tables
		},
	}

	filter, err := NewQueryFilter(cfg)
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	tests := []struct {
		queryText    string
		shouldFilter bool
		description  string
	}{
		{
			queryText:    "SELECT 1",
			shouldFilter: true,
			description:  "Exact match should be filtered",
		},
		{
			queryText:    "SELECT 1 + 1",
			shouldFilter: false,
			description:  "Similar but different query should not match exact pattern",
		},
		{
			queryText:    "SYSTEM FLUSH LOGS",
			shouldFilter: true,
			description:  "Query containing SYSTEM should be filtered",
		},
		{
			queryText:    "SELECT * FROM healthcheck",
			shouldFilter: true,
			description:  "Case insensitive match should work",
		},
		{
			queryText:    "SELECT * FROM HEALTHCHECK",
			shouldFilter: true,
			description:  "Case insensitive match uppercase",
		},
		{
			queryText:    "SELECT * FROM system.query_log",
			shouldFilter: true,
			description:  "System table query should be filtered",
		},
		{
			queryText:    "SELECT * FROM users",
			shouldFilter: false,
			description:  "Normal query should not be filtered",
		},
	}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			result := filter.ShouldFilter("", tt.queryText, "")
			if result != tt.shouldFilter {
				t.Errorf("Query %q: expected shouldFilter=%v, got %v",
					tt.queryText, tt.shouldFilter, result)
			}
		})
	}
}

func TestQueryFilter_CombinedFilters(t *testing.T) {
	cfg := config.FiltersConfig{
		WhitelistOperations: []string{"DB::*Query::execute()"},
		WhitelistIPs:        []string{"192.168.1.100"},
		BlacklistQueries:    []string{"SYSTEM"},
	}

	filter, err := NewQueryFilter(cfg)
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	tests := []struct {
		name          string
		operationName string
		queryText     string
		clientIP      string
		shouldFilter  bool
	}{
		{
			name:          "all conditions pass",
			operationName: "DB::SelectQuery::execute()",
			queryText:     "SELECT * FROM users",
			clientIP:      "192.168.1.100",
			shouldFilter:  false,
		},
		{
			name:          "IP not whitelisted",
			operationName: "DB::SelectQuery::execute()",
			queryText:     "SELECT * FROM users",
			clientIP:      "10.0.0.1",
			shouldFilter:  true,
		},
		{
			name:          "operation not whitelisted",
			operationName: "DB::OtherClass::method()",
			queryText:     "SELECT * FROM users",
			clientIP:      "192.168.1.100",
			shouldFilter:  true,
		},
		{
			name:          "query blacklisted",
			operationName: "DB::SelectQuery::execute()",
			queryText:     "SYSTEM FLUSH LOGS",
			clientIP:      "192.168.1.100",
			shouldFilter:  true,
		},
		{
			name:          "multiple failures - IP and operation",
			operationName: "BadOperation",
			queryText:     "SELECT * FROM users",
			clientIP:      "8.8.8.8",
			shouldFilter:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filter.ShouldFilter(tt.operationName, tt.queryText, tt.clientIP)
			if result != tt.shouldFilter {
				t.Errorf("Expected shouldFilter=%v, got %v", tt.shouldFilter, result)
			}
		})
	}
}

func TestQueryFilter_FilterOrder(t *testing.T) {
	// Test that filters are applied in the correct order:
	// 1. IP whitelist (if configured)
	// 2. Operation whitelist (if configured)
	// 3. Query blacklist

	// This test verifies early exit behavior

	cfg := config.FiltersConfig{
		WhitelistIPs:        []string{"192.168.1.100"},
		WhitelistOperations: []string{"AllowedOp"},
		BlacklistQueries:    []string{"BLOCKED"},
	}

	filter, err := NewQueryFilter(cfg)
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	// Test 1: Bad IP should be filtered immediately (before checking operation)
	result := filter.ShouldFilter("AllowedOp", "normal query", "bad.ip.address")
	if !result {
		t.Error("Bad IP should be filtered even with allowed operation")
	}

	// Test 2: Bad operation should be filtered (after IP check passes)
	result = filter.ShouldFilter("BadOp", "normal query", "192.168.1.100")
	if !result {
		t.Error("Bad operation should be filtered with good IP")
	}

	// Test 3: Blacklisted query should be filtered (after IP and operation pass)
	result = filter.ShouldFilter("AllowedOp", "BLOCKED query", "192.168.1.100")
	if !result {
		t.Error("Blacklisted query should be filtered with good IP and operation")
	}

	// Test 4: All pass
	result = filter.ShouldFilter("AllowedOp", "normal query", "192.168.1.100")
	if result {
		t.Error("Valid request should not be filtered")
	}
}

func TestQueryFilter_NoWhitelists(t *testing.T) {
	// When no whitelists are configured, only blacklist applies
	cfg := config.FiltersConfig{
		BlacklistQueries: []string{"BLOCKED"},
	}

	filter, err := NewQueryFilter(cfg)
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	// Any IP should be allowed
	result := filter.ShouldFilter("AnyOp", "normal query", "any.ip.address")
	if result {
		t.Error("Without whitelists, any IP/operation should be allowed")
	}

	// But blacklist still applies
	result = filter.ShouldFilter("AnyOp", "BLOCKED query", "any.ip.address")
	if !result {
		t.Error("Blacklist should still apply without whitelists")
	}
}

// ---------------------------------------------------------------------------
// RedactQuery
// ---------------------------------------------------------------------------

func TestRedactQuery_NoRules(t *testing.T) {
	filter, err := NewQueryFilter(config.FiltersConfig{})
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	input := "SELECT * FROM users WHERE password = 'secret'"
	got := filter.RedactQuery(input)
	if got != input {
		t.Errorf("RedactQuery with no rules should return input unchanged, got %q", got)
	}
}

func TestRedactQuery_SingleRule(t *testing.T) {
	filter, err := NewQueryFilter(config.FiltersConfig{
		RedactQueries: []config.RedactionRule{
			{Pattern: `(?i)identified\s+by\s+'[^']*'`, Replacement: "IDENTIFIED BY '[REDACTED]'"},
		},
	})
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	input := "CREATE USER foo IDENTIFIED BY 'supersecret'"
	want := "CREATE USER foo IDENTIFIED BY '[REDACTED]'"
	got := filter.RedactQuery(input)
	if got != want {
		t.Errorf("RedactQuery = %q, want %q", got, want)
	}
}

func TestRedactQuery_MultipleRules(t *testing.T) {
	filter, err := NewQueryFilter(config.FiltersConfig{
		RedactQueries: []config.RedactionRule{
			{Pattern: `(?i)identified\s+by\s+'[^']*'`, Replacement: "IDENTIFIED BY '[REDACTED]'"},
			{Pattern: `(?i)password\s*=\s*'[^']*'`, Replacement: "password='[REDACTED]'"},
		},
	})
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	input := "CREATE USER foo IDENTIFIED BY 'secret' AND password = 'abc123'"
	got := filter.RedactQuery(input)
	if !strings.Contains(got, "IDENTIFIED BY '[REDACTED]'") {
		t.Errorf("First rule should have matched, got %q", got)
	}
	if !strings.Contains(got, "password='[REDACTED]'") {
		t.Errorf("Second rule should have matched, got %q", got)
	}
}

func TestRedactQuery_DefaultReplacement(t *testing.T) {
	filter, err := NewQueryFilter(config.FiltersConfig{
		RedactQueries: []config.RedactionRule{
			{Pattern: `secret_value`}, // empty replacement -> default [REDACTED]
		},
	})
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	input := "SELECT secret_value FROM table"
	want := "SELECT [REDACTED] FROM table"
	got := filter.RedactQuery(input)
	if got != want {
		t.Errorf("RedactQuery = %q, want %q", got, want)
	}
}

func TestRedactQuery_NoMatch(t *testing.T) {
	filter, err := NewQueryFilter(config.FiltersConfig{
		RedactQueries: []config.RedactionRule{
			{Pattern: `IDENTIFIED BY`, Replacement: "***"},
		},
	})
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	input := "SELECT * FROM users"
	got := filter.RedactQuery(input)
	if got != input {
		t.Errorf("RedactQuery should return input unchanged when no match, got %q", got)
	}
}

func TestRedactQuery_WhitespaceTrimmed(t *testing.T) {
	// Simulates YAML block scalar with trailing newline
	filter, err := NewQueryFilter(config.FiltersConfig{
		RedactQueries: []config.RedactionRule{
			{Pattern: "  redacted_regex\n", Replacement: "***"},
		},
	})
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	input := "SELECT redacted_regex FROM table"
	want := "SELECT *** FROM table"
	got := filter.RedactQuery(input)
	if got != want {
		t.Errorf("RedactQuery = %q, want %q", got, want)
	}
}

func TestQueryFilter_ShouldFilterUser_NoFilter(t *testing.T) {
	qf, err := NewQueryFilter(config.FiltersConfig{})
	if err != nil {
		t.Fatalf("NewQueryFilter failed: %v", err)
	}
	if qf.HasUserFilter() {
		t.Error("HasUserFilter should be false with empty config")
	}
	for _, u := range []string{"", "alice", "bob"} {
		if qf.ShouldFilterUser(u) {
			t.Errorf("Unfiltered config should not filter user %q", u)
		}
	}
}

func TestQueryFilter_ShouldFilterUser_WhitelistOnly(t *testing.T) {
	qf, err := NewQueryFilter(config.FiltersConfig{
		WhitelistUsers: []string{"app_frontend", "app_analytics"},
	})
	if err != nil {
		t.Fatalf("NewQueryFilter failed: %v", err)
	}
	if !qf.HasUserFilter() {
		t.Error("HasUserFilter should be true when whitelist is set")
	}
	tests := []struct {
		user         string
		shouldFilter bool
	}{
		{"app_frontend", false},
		{"app_analytics", false},
		{"patient_records", true},
		{"", true}, // unknown user dropped under strict whitelist
	}
	for _, tt := range tests {
		if got := qf.ShouldFilterUser(tt.user); got != tt.shouldFilter {
			t.Errorf("ShouldFilterUser(%q) = %v, want %v", tt.user, got, tt.shouldFilter)
		}
	}
}

func TestQueryFilter_ShouldFilterUser_BlacklistOnly(t *testing.T) {
	qf, err := NewQueryFilter(config.FiltersConfig{
		BlacklistUsers: []string{"patient_records", "ml_training"},
	})
	if err != nil {
		t.Fatalf("NewQueryFilter failed: %v", err)
	}
	if !qf.HasUserFilter() {
		t.Error("HasUserFilter should be true when blacklist is set")
	}
	tests := []struct {
		user         string
		shouldFilter bool
	}{
		{"patient_records", true},
		{"ml_training", true},
		{"app_frontend", false},
		{"", false}, // unknown user passes when only a blacklist is set
	}
	for _, tt := range tests {
		if got := qf.ShouldFilterUser(tt.user); got != tt.shouldFilter {
			t.Errorf("ShouldFilterUser(%q) = %v, want %v", tt.user, got, tt.shouldFilter)
		}
	}
}

func TestQueryFilter_ShouldFilterUser_BothLists(t *testing.T) {
	// Whitelist and blacklist together: whitelist gates entry, blacklist
	// removes specific names from within the allowed set.
	qf, err := NewQueryFilter(config.FiltersConfig{
		WhitelistUsers: []string{"app_frontend", "app_analytics", "shared_user"},
		BlacklistUsers: []string{"shared_user"},
	})
	if err != nil {
		t.Fatalf("NewQueryFilter failed: %v", err)
	}
	tests := []struct {
		user         string
		shouldFilter bool
	}{
		{"app_frontend", false},   // whitelist hit, not blacklisted
		{"app_analytics", false},  // whitelist hit, not blacklisted
		{"shared_user", true},     // whitelist hit but blacklist trumps
		{"patient_records", true}, // not in whitelist
		{"", true},                // unknown user dropped by whitelist
	}
	for _, tt := range tests {
		if got := qf.ShouldFilterUser(tt.user); got != tt.shouldFilter {
			t.Errorf("ShouldFilterUser(%q) = %v, want %v", tt.user, got, tt.shouldFilter)
		}
	}
}

func TestNewQueryFilter_InvalidRedactRegex(t *testing.T) {
	_, err := NewQueryFilter(config.FiltersConfig{
		RedactQueries: []config.RedactionRule{
			{Pattern: "[invalid(regex"},
		},
	})
	if err == nil {
		t.Fatal("Expected error for invalid redaction regex, got nil")
	}
}

func TestQueryFilter_EmptyStrings(t *testing.T) {
	cfg := config.FiltersConfig{
		WhitelistOperations: []string{"ValidOp"},
		BlacklistQueries:    []string{"blocked"},
	}

	filter, err := NewQueryFilter(cfg)
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	// Empty operation name with whitelist configured
	result := filter.ShouldFilter("", "normal query", "192.168.1.1")
	if !result {
		t.Error("Empty operation should be filtered when whitelist is configured")
	}

	// Empty query text should not match blacklist
	result = filter.ShouldFilter("ValidOp", "", "192.168.1.1")
	if result {
		t.Error("Empty query should not match blacklist patterns")
	}
}
