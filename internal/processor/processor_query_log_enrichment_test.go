package processor

import (
	"slices"
	"testing"

	"github.com/coltconsulting/click-dog/internal/model"
)

func TestEnrichSpanFromQueryLog_Basic(t *testing.T) {
	span := model.OpenTelemetrySpan{
		Attributes: map[string]string{
			"db.statement": "SELECT 1",
		},
	}
	ql := model.QueryLog{
		QueryID:          "qid-1",
		User:             "default",
		ClientName:       "clickhouse-go",
		ClientHostname:   "app-server-1",
		ClientAddress:    "10.0.0.1",
		QueryDurationMs:  1500,
		ReadRows:         10000,
		ReadBytes:        512000,
		WrittenRows:      0,
		WrittenBytes:     0,
		ResultRows:       100,
		ResultBytes:      4096,
		MemoryUsage:      2048000,
		DatabasesVisited: []string{"default", "system"},
		TablesVisited:    []string{"default.events", "system.numbers"},
	}

	EnrichSpanFromQueryLog(&span, ql, 100000)

	checks := map[string]string{
		"query_log.query_id":          "qid-1",
		"query_log.user":              "default",
		"query_log.client_name":       "clickhouse-go",
		"query_log.client_hostname":   "app-server-1",
		"query_log.client_address":    "10.0.0.1",
		"query_log.query_duration_ms": "1500",
		"query_log.read_rows":         "10000",
		"query_log.read_bytes":        "512000",
		"query_log.written_rows":      "0",
		"query_log.written_bytes":     "0",
		"query_log.result_rows":       "100",
		"query_log.result_bytes":      "4096",
		"query_log.memory_usage":      "2048000",
		"query_log.databases_csv":     "default,system",
		"query_log.tables_csv":        "default.events,system.numbers",
	}
	for k, want := range checks {
		if got := span.Attributes[k]; got != want {
			t.Errorf("%s: got %q, want %q", k, got, want)
		}
	}
	for _, key := range []string{"query_log.databases", "query_log.tables"} {
		if _, ok := span.Attributes[key]; ok {
			t.Errorf("%s should be an OTLP string-array attribute, not a string attribute", key)
		}
	}
	if got := span.StringSliceAttributes["query_log.databases"]; !slices.Equal(got, []string{"default", "system"}) {
		t.Errorf("query_log.databases slice: got %v, want [default system]", got)
	}
	if got := span.StringSliceAttributes["query_log.tables"]; !slices.Equal(got, []string{"default.events", "system.numbers"}) {
		t.Errorf("query_log.tables slice: got %v, want [default.events system.numbers]", got)
	}

	// Original attribute preserved
	if span.Attributes["db.statement"] != "SELECT 1" {
		t.Errorf("db.statement lost after enrichment")
	}
}

func TestEnrichSpanFromQueryLog_NoMatch(t *testing.T) {
	span := model.OpenTelemetrySpan{
		Attributes: map[string]string{
			"db.statement":        "SELECT 1",
			"clickhouse.query_id": "no-match-qid",
		},
	}

	// Simulate the pipeline-level check: queryLogMap miss → span unchanged
	queryLogMap := map[string]model.QueryLog{
		"other-qid": {QueryID: "other-qid", User: "admin"},
	}

	if qid := span.Attributes["clickhouse.query_id"]; qid != "" {
		if ql, ok := queryLogMap[qid]; ok {
			EnrichSpanFromQueryLog(&span, ql, 100000)
		}
	}

	// Span should be untouched (db.statement + clickhouse.query_id)
	if len(span.Attributes) != 2 {
		t.Errorf("expected 2 attributes, got %d", len(span.Attributes))
	}
	if span.Attributes["db.statement"] != "SELECT 1" {
		t.Error("db.statement was modified")
	}
	if _, ok := span.Attributes["query_log.user"]; ok {
		t.Error("query_log.user should not be set on unmatched span")
	}
}

func TestEnrichSpanFromQueryLog_PreservesExistingAttributes(t *testing.T) {
	span := model.OpenTelemetrySpan{
		Attributes: map[string]string{
			"db.statement":   "SELECT 1",
			"hostname":       "ch-node-1",
			"client.address": "10.0.0.1",
			"custom.tag":     "myvalue",
		},
	}
	ql := model.QueryLog{
		QueryID: "qid-1",
		User:    "admin",
	}

	EnrichSpanFromQueryLog(&span, ql, 100000)

	// All original attributes should still be present
	for _, key := range []string{"db.statement", "hostname", "client.address", "custom.tag"} {
		if _, ok := span.Attributes[key]; !ok {
			t.Errorf("original attribute %q lost after enrichment", key)
		}
	}
	if span.Attributes["query_log.user"] != "admin" {
		t.Errorf("enrichment attribute missing")
	}
}

func TestEnrichSpanFromQueryLog_DoesNotMutateOriginalMap(t *testing.T) {
	original := map[string]string{
		"db.statement": "SELECT 1",
	}
	originalSlices := map[string][]string{
		"existing.slice": {"before"},
	}
	span := model.OpenTelemetrySpan{
		Attributes:            original,
		StringSliceAttributes: originalSlices,
	}
	ql := model.QueryLog{
		QueryID:          "qid-1",
		User:             "default",
		TablesVisited:    []string{"default.events"},
		DatabasesVisited: []string{"default"},
	}

	EnrichSpanFromQueryLog(&span, ql, 100000)

	// Original map should not have been mutated
	if _, ok := original["query_log.user"]; ok {
		t.Error("original map was mutated by enrichment")
	}
	// But span should have the enriched value
	if span.Attributes["query_log.user"] != "default" {
		t.Error("span missing enriched attribute")
	}
	if _, ok := originalSlices["query_log.tables"]; ok {
		t.Error("original string-slice map was mutated by enrichment")
	}
	span.StringSliceAttributes["existing.slice"][0] = "after"
	if originalSlices["existing.slice"][0] != "before" {
		t.Error("original string-slice values were aliased by enrichment")
	}
}

func TestEnrichSpanFromQueryLog_ExceptionCodeOnlyWhenNonZero(t *testing.T) {
	span := model.OpenTelemetrySpan{
		Attributes: map[string]string{},
	}
	ql := model.QueryLog{ExceptionCode: 0}

	EnrichSpanFromQueryLog(&span, ql, 100000)

	if _, ok := span.Attributes["query_log.exception_code"]; ok {
		t.Error("exception_code should not be set when zero")
	}

	span2 := model.OpenTelemetrySpan{
		Attributes: map[string]string{},
	}
	ql2 := model.QueryLog{ExceptionCode: 62}

	EnrichSpanFromQueryLog(&span2, ql2, 100000)

	if span2.Attributes["query_log.exception_code"] != "62" {
		t.Errorf("expected exception_code=62, got %q", span2.Attributes["query_log.exception_code"])
	}
}

func TestEnrichSpanFromQueryLog_NormalizedQueryAttrsOnlyWhenPresent(t *testing.T) {
	normalized := "  SELECT * FROM events WHERE user_id = ? AND ts >= ?  "
	if got := truncateNormalizedQuery(normalized, 24); got != "SELECT * FROM events WHE..." {
		t.Fatalf("truncateNormalizedQuery() = %q, want whitespace-trimmed truncated preview", got)
	}
	if got := truncateNormalizedQuery(normalized, 0); got != "SELECT * FROM events WHERE user_id = ? AND ts >= ?" {
		t.Fatalf("truncateNormalizedQuery(maxLen=0) = %q, want whitespace-trimmed untruncated preview", got)
	}

	span := model.OpenTelemetrySpan{Attributes: map[string]string{}}
	ql := model.QueryLog{
		NormalizedQueryHash: 123456789,
		NormalizedQuery:     normalized,
	}

	EnrichSpanFromQueryLog(&span, ql, 24)

	if got := span.Attributes["query_log.normalized_query_hash"]; got != "123456789" {
		t.Errorf("normalized_query_hash = %q, want 123456789", got)
	}
	if got := span.Attributes["query_log.normalized_query"]; got != "SELECT * FROM events WHE..." {
		t.Errorf("normalized_query = %q, want truncated normalized preview", got)
	}

	span = model.OpenTelemetrySpan{Attributes: map[string]string{}}
	EnrichSpanFromQueryLog(&span, model.QueryLog{}, 24)

	for _, key := range []string{"query_log.normalized_query_hash", "query_log.normalized_query"} {
		if _, ok := span.Attributes[key]; ok {
			t.Errorf("%s should not be set when absent", key)
		}
	}
}

func TestEnrichSpanFromQueryLog_EmptyValuesOmitted(t *testing.T) {
	span := model.OpenTelemetrySpan{
		Attributes: map[string]string{},
	}
	ql := model.QueryLog{
		QueryID:          "qid-1",
		User:             "",
		ClientName:       "",
		ClientHostname:   "",
		ClientAddress:    "",
		ExceptionCode:    0,
		DatabasesVisited: nil,
		TablesVisited:    []string{},
	}

	EnrichSpanFromQueryLog(&span, ql, 100000)

	for _, key := range []string{
		"query_log.user",
		"query_log.client_name",
		"query_log.client_hostname",
		"query_log.client_address",
		"query_log.exception_code",
		"query_log.databases",
		"query_log.databases_csv",
		"query_log.tables",
		"query_log.tables_csv",
		"query_log.normalized_query_hash",
		"query_log.normalized_query",
	} {
		if _, ok := span.Attributes[key]; ok {
			t.Errorf("%s should not be set when empty/zero", key)
		}
	}
	for _, key := range []string{"query_log.databases", "query_log.tables"} {
		if _, ok := span.StringSliceAttributes[key]; ok {
			t.Errorf("%s string-slice attribute should not be set when empty", key)
		}
	}

	// Numeric stats are always set (even when zero)
	if span.Attributes["query_log.query_id"] != "qid-1" {
		t.Error("query_id should always be set")
	}
	if span.Attributes["query_log.read_rows"] != "0" {
		t.Error("read_rows should be set even when zero")
	}
}

func TestEnrichSpanFromQueryLog_MultipleSpansSameTrace(t *testing.T) {
	ql := model.QueryLog{
		QueryID:     "qid-1",
		User:        "default",
		ClientName:  "python-driver",
		ReadRows:    5000,
		MemoryUsage: 1024000,
	}

	spans := []model.OpenTelemetrySpan{
		{SpanID: 1, Attributes: map[string]string{"op": "execute"}},
		{SpanID: 2, Attributes: map[string]string{"op": "read"}},
		{SpanID: 3, Attributes: map[string]string{"op": "merge"}},
	}

	for i := range spans {
		EnrichSpanFromQueryLog(&spans[i], ql, 100000)
	}

	for i, span := range spans {
		if span.Attributes["query_log.user"] != "default" {
			t.Errorf("span %d: missing enrichment", i)
		}
		if span.Attributes["query_log.read_rows"] != "5000" {
			t.Errorf("span %d: wrong read_rows", i)
		}
	}
}

// TestQueryLogDedup_ExceptionPreference verifies the dedup logic used in
// FetchQueryLogByQueryIDs: ExceptionWhileProcessing should be preferred
// over QueryFinish regardless of which arrives first in the result set.
// This replicates the exact logic from reader.go to test both orderings.
func TestQueryLogDedup_ExceptionPreference(t *testing.T) {
	// dedup replicates the logic from FetchQueryLogByQueryIDs.
	// The map is keyed by ql.QueryID — test rows must populate QueryID to match.
	dedup := func(rows []model.QueryLog) map[string]model.QueryLog {
		result := make(map[string]model.QueryLog)
		for _, ql := range rows {
			if existing, ok := result[ql.QueryID]; ok {
				if existing.QueryKind == "ExceptionWhileProcessing" {
					continue
				}
			}
			result[ql.QueryID] = ql
		}
		return result
	}

	tests := []struct {
		name     string
		rows     []model.QueryLog
		wantKind string
	}{
		{
			name: "exception first, finish second — keeps exception",
			rows: []model.QueryLog{
				{QueryID: "q1", QueryKind: "ExceptionWhileProcessing", ExceptionCode: 62},
				{QueryID: "q1", QueryKind: "QueryFinish"},
			},
			wantKind: "ExceptionWhileProcessing",
		},
		{
			name: "finish first, exception second — overwrites with exception",
			rows: []model.QueryLog{
				{QueryID: "q1", QueryKind: "QueryFinish"},
				{QueryID: "q1", QueryKind: "ExceptionWhileProcessing", ExceptionCode: 62},
			},
			wantKind: "ExceptionWhileProcessing",
		},
		{
			name: "only finish — keeps finish",
			rows: []model.QueryLog{
				{QueryID: "q1", QueryKind: "QueryFinish"},
			},
			wantKind: "QueryFinish",
		},
		{
			name: "only exception — keeps exception",
			rows: []model.QueryLog{
				{QueryID: "q1", QueryKind: "ExceptionWhileProcessing", ExceptionCode: 62},
			},
			wantKind: "ExceptionWhileProcessing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := dedup(tt.rows)
			ql, ok := result["q1"]
			if !ok {
				t.Fatal("expected q1 in result")
			}
			if ql.QueryKind != tt.wantKind {
				t.Errorf("got QueryKind=%q, want %q", ql.QueryKind, tt.wantKind)
			}
		})
	}
}

func TestUniqueQueryIDs(t *testing.T) {
	spans := []model.OpenTelemetrySpan{
		{Attributes: map[string]string{"clickhouse.query_id": "a"}},
		{Attributes: map[string]string{"clickhouse.query_id": "b"}},
		{Attributes: map[string]string{"clickhouse.query_id": "a"}},
		{Attributes: map[string]string{"clickhouse.query_id": "c"}},
		{Attributes: map[string]string{"clickhouse.query_id": "b"}},
	}

	ids := UniqueQueryIDs(spans)

	if len(ids) != 3 {
		t.Fatalf("expected 3 unique query IDs, got %d", len(ids))
	}

	// Verify order preserved (first occurrence)
	expected := []string{"a", "b", "c"}
	for i, want := range expected {
		if ids[i] != want {
			t.Errorf("ids[%d] = %q, want %q", i, ids[i], want)
		}
	}
}

func TestUniqueQueryIDs_SkipsEmpty(t *testing.T) {
	spans := []model.OpenTelemetrySpan{
		{Attributes: map[string]string{"clickhouse.query_id": "a"}},
		{Attributes: map[string]string{}}, // no query_id
		{Attributes: map[string]string{"other": "value"}},
		{Attributes: map[string]string{"clickhouse.query_id": "b"}},
	}

	ids := UniqueQueryIDs(spans)

	if len(ids) != 2 {
		t.Fatalf("expected 2 unique query IDs (skipping empty), got %d: %v", len(ids), ids)
	}
}
