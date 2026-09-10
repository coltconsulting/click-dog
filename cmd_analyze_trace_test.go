package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/analysis"
	"github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
)

// fakeTraceSource is a canned traceDrilldownSource. Each method records its
// inputs and returns configured values so the report path runs without a live
// ClickHouse.
type fakeTraceSource struct {
	traceIDs    []string
	traceIDsErr error
	spans       []model.OpenTelemetrySpan
	spansErr    error
	queryLog    map[string]model.QueryLog
	queryLogErr error

	candidates    []model.QueryLog
	candidatesErr error
	normalized    bool
	families      []model.QueryFamilyRollup
	familiesErr   error

	// analysisSpans and dimensionCounts back the findings fan-out's reuse of the
	// `analyze queries` buildAnalysisInput path; they are only read when
	// -fanout findings is requested. analysisSpansErr degrades the span sample to
	// a coverage warning (non-fatal); dimensionErr is a required read whose
	// failure makes the inline analysis-input build fail (so the fan-out degrades
	// to a warning rather than failing the command).
	analysisSpans    []model.OpenTelemetrySpan
	analysisSpansErr error
	dimensionCounts  []clickhouse.QueryFamilyDimensionCount
	dimensionErr     error

	// currentCandidates is the system.processes-backed `current` source.
	// currentCandidatesErr forces the required-search failure. Each call to
	// FetchTraceIDsByQueryID can be made to return increasingly-populated trace
	// IDs via traceIDsByCall to model -wait polling materialization.
	currentCandidates    []model.QueryLog
	currentCandidatesErr error
	traceIDsByCall       [][]string
	traceIDCallCount     int

	traceIDQueryID   string
	spanTraceIDs     []string
	spanBlacklist    []string
	queryLogQueryIDs []string
	candidateOpts    clickhouse.RecentQueryCandidateOptions
	currentOpts      clickhouse.CurrentQueryCandidateOptions
	rollupOpts       clickhouse.QueryFamilyRollupOptions
}

func (f *fakeTraceSource) FetchTraceIDsByQueryID(_ context.Context, queryID string, _, _ int) ([]string, error) {
	f.traceIDQueryID = queryID
	call := f.traceIDCallCount
	f.traceIDCallCount++
	// traceIDsByCall models -wait polling: each successive call returns the
	// next configured result (the last entry repeats once exhausted), so a test
	// can make the trace materialize on the Nth poll.
	if f.traceIDsByCall != nil {
		if call >= len(f.traceIDsByCall) {
			call = len(f.traceIDsByCall) - 1
		}
		return f.traceIDsByCall[call], f.traceIDsErr
	}
	return f.traceIDs, f.traceIDsErr
}

func (f *fakeTraceSource) FetchSpansForTraceIDs(_ context.Context, traceIDs []string, _, _ int, blacklist []string) ([]model.OpenTelemetrySpan, error) {
	f.spanTraceIDs = traceIDs
	f.spanBlacklist = blacklist
	return f.spans, f.spansErr
}

func (f *fakeTraceSource) FetchQueryLogByQueryIDs(_ context.Context, queryIDs []string, _ int) (map[string]model.QueryLog, error) {
	f.queryLogQueryIDs = queryIDs
	return f.queryLog, f.queryLogErr
}

// FetchRecentQueryCandidates honors the SQL-level predicates the real builder
// now pushes into the WHERE clause: the -normalized-query-hash exact identity,
// the -match case-insensitive substring OR-group, and the cost-ordered LIMIT.
// Modeling them here (rather than returning the raw slice) keeps the fake
// faithful to the reader after the push-down fix, so the command tests prove the
// LIMIT bounds the matched rows — not a cost-truncated set the command then
// shrinks. f.normalized controls whether the normalized term participates, the
// same gate as the reader's queryLogNormalized.
func (f *fakeTraceSource) FetchRecentQueryCandidates(_ context.Context, opts clickhouse.RecentQueryCandidateOptions) ([]model.QueryLog, error) {
	f.candidateOpts = opts
	if f.candidatesErr != nil {
		return nil, f.candidatesErr
	}

	matched := make([]model.QueryLog, 0, len(f.candidates))
	for _, c := range f.candidates {
		if opts.HasHash && c.NormalizedQueryHash != opts.NormalizedQueryHash {
			continue
		}
		if opts.Match != "" && !fakeCandidateMatches(c, opts.Match, f.normalized) {
			continue
		}
		matched = append(matched, c)
	}
	if opts.Limit > 0 && len(matched) > opts.Limit {
		matched = matched[:opts.Limit]
	}
	return matched, nil
}

// fakeCandidateMatches mirrors the SQL OR-group in recentQueryCandidatesBuilder:
// a case-insensitive substring over query_id, user, client_name,
// client_hostname, tables, databases, plus the normalized query when normalized
// support is present.
func fakeCandidateMatches(ql model.QueryLog, needle string, normalized bool) bool {
	n := strings.ToLower(needle)
	fields := []string{ql.QueryID, ql.User, ql.ClientName, ql.ClientHostname}
	fields = append(fields, ql.TablesVisited...)
	fields = append(fields, ql.DatabasesVisited...)
	if normalized {
		fields = append(fields, ql.NormalizedQuery)
	}
	for _, fld := range fields {
		if fld != "" && strings.Contains(strings.ToLower(fld), n) {
			return true
		}
	}
	return false
}

// FetchCurrentQueryCandidates mirrors the system.processes search: it records
// the options and applies the same -match OR-group over the running-query
// metadata (and normalized preview when supported) plus the candidate-limit, so
// the command tests prove the LIMIT bounds the matched rows.
func (f *fakeTraceSource) FetchCurrentQueryCandidates(_ context.Context, opts clickhouse.CurrentQueryCandidateOptions) ([]model.QueryLog, error) {
	f.currentOpts = opts
	if f.currentCandidatesErr != nil {
		return nil, f.currentCandidatesErr
	}
	matched := make([]model.QueryLog, 0, len(f.currentCandidates))
	for _, c := range f.currentCandidates {
		if opts.Match != "" && !fakeCurrentMatches(c, opts.Match, f.normalized) {
			continue
		}
		matched = append(matched, c)
	}
	if opts.Limit > 0 && len(matched) > opts.Limit {
		matched = matched[:opts.Limit]
	}
	return matched, nil
}

// fakeCurrentMatches mirrors the SQL OR-group in currentQueryCandidatesBuilder:
// a case-insensitive substring over query_id, user, client_name,
// client_hostname, plus the normalized query when normalized support is present.
// system.processes has no tables/databases arrays.
func fakeCurrentMatches(ql model.QueryLog, needle string, normalized bool) bool {
	n := strings.ToLower(needle)
	fields := []string{ql.QueryID, ql.User, ql.ClientName, ql.ClientHostname}
	if normalized {
		fields = append(fields, ql.NormalizedQuery)
	}
	for _, fld := range fields {
		if fld != "" && strings.Contains(strings.ToLower(fld), n) {
			return true
		}
	}
	return false
}

func (f *fakeTraceSource) QueryLogNormalizedSupported() bool { return f.normalized }

func (f *fakeTraceSource) FetchQueryFamilyRollups(_ context.Context, opts clickhouse.QueryFamilyRollupOptions) ([]model.QueryFamilyRollup, error) {
	f.rollupOpts = opts
	return f.families, f.familiesErr
}

// FetchOpenTelemetrySpansWithOpts and FetchQueryFamilyDimensionCounts let the
// fake satisfy queryAnalysisSource (embedded in traceDrilldownSource) so the
// findings fan-out can drive the SAME buildAnalysisInput path as `analyze
// queries`. They are only exercised when -fanout findings is requested.
func (f *fakeTraceSource) FetchOpenTelemetrySpansWithOpts(_ context.Context, _ int, _ time.Duration, _ int, _ clickhouse.FetchOpts) ([]model.OpenTelemetrySpan, error) {
	return f.analysisSpans, f.analysisSpansErr
}

func (f *fakeTraceSource) FetchQueryFamilyDimensionCounts(_ context.Context, _ clickhouse.QueryFamilyDimensionCountOptions) ([]clickhouse.QueryFamilyDimensionCount, error) {
	return f.dimensionCounts, f.dimensionErr
}

func testTraceConfig() *config.Config {
	cfg := &config.Config{}
	cfg.ClickHouse.Host = "ch.example.internal"
	cfg.Filters.BlacklistOperations = []string{"MergeTreeIndex"}
	return cfg
}

func defaultTraceOptions() analyzeTraceOptions {
	return analyzeTraceOptions{
		Lookback:       time.Hour,
		Timeout:        time.Minute,
		Source:         "other",
		QueryID:        "q-123",
		CandidateLimit: defaultTraceCandidateLimit,
		SpanLimit:      1000,
		Fanout:         fanoutSet{Trace: true, Stats: true},
	}
}

func makeSpan(traceID uuid.UUID, spanID, parent uint64, op string, startUs, finishUs uint64) model.OpenTelemetrySpan {
	return model.OpenTelemetrySpan{
		Hostname:      "host-1",
		TraceID:       traceID,
		SpanID:        spanID,
		ParentSpanID:  parent,
		OperationName: op,
		Kind:          "server",
		StartTimeUs:   startUs,
		FinishTimeUs:  finishUs,
		FinishDate:    time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC),
		Attributes:    map[string]string{},
	}
}

// ---------------------------------------------------------------------------
// Flag validation and dispatch (exit codes 0 / 2)
// ---------------------------------------------------------------------------

func TestRunAnalyze_TraceVerbDispatches(t *testing.T) {
	// An invalid --source value is rejected as a usage error (exit 2) before any
	// config load — proves dispatch reached runAnalyzeTrace rather than the
	// unknown-command path. (With Phase 2, -source recent is the default and a
	// bare `trace` is a valid invocation, so it no longer fails on usage.)
	var out, errOut bytes.Buffer
	code := runAnalyze([]string{"trace", "-source", "bogus"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "invalid --source") {
		t.Errorf("stderr missing invalid-source rejection:\n%s", errOut.String())
	}
}

func TestRunAnalyze_TraceHelpListsTrace(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runAnalyze([]string{"--help"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "trace") {
		t.Errorf("analyze help should list the trace verb:\n%s", out.String())
	}
}

func TestAnalyzeTrace_HelpExits0(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runAnalyzeTrace([]string{"--help"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 for --help", code)
	}
	if !strings.Contains(errOut.String(), "analyze trace") {
		t.Errorf("help text missing:\n%s", errOut.String())
	}
}

func TestAnalyzeTrace_BadFormatExits2(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runAnalyzeTrace([]string{"-query-id", "q1", "-format", "yaml"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "invalid --format") {
		t.Errorf("stderr missing invalid-format message:\n%s", errOut.String())
	}
}

func TestAnalyzeTrace_BadFlagExits2(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runAnalyzeTrace([]string{"-nonsense"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestAnalyzeTrace_SourceOtherWithoutIdentityExits2(t *testing.T) {
	// -source other means "drill an explicit identity"; supplying it with no
	// identity flag is a usage error (exit 2).
	var out, errOut bytes.Buffer
	code := runAnalyzeTrace([]string{"-source", "other"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "--source other requires an identity flag") {
		t.Errorf("stderr missing required-identity message:\n%s", errOut.String())
	}
}

func TestAnalyzeTrace_NonPositiveSpanLimitExits2(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runAnalyzeTrace([]string{"-query-id", "q1", "-span-limit", "0"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "-span-limit must be positive") {
		t.Errorf("stderr missing span-limit message:\n%s", errOut.String())
	}
}

// ---------------------------------------------------------------------------
// Report path scenarios (exit 0 with degradation warnings; exit 1 on required
// failures)
// ---------------------------------------------------------------------------

func TestAnalyzeTrace_TraceFoundProducesReport(t *testing.T) {
	traceID := uuid.New()
	src := &fakeTraceSource{
		traceIDs: []string{traceID.String()},
		spans: []model.OpenTelemetrySpan{
			makeSpan(traceID, 1, 0, "Query", 1_000_000, 5_000_000),    // root, 4000ms
			makeSpan(traceID, 2, 1, "ReadData", 1_500_000, 2_000_000), // child, 500ms
		},
		queryLog: map[string]model.QueryLog{
			"q-123": {
				QueryID:             "q-123",
				QueryKind:           "QueryFinish",
				QueryDurationMs:     4200,
				ReadRows:            1000,
				User:                "etl_user",
				NormalizedQueryHash: 777,
				NormalizedQuery:     "SELECT ? FROM events",
			},
		},
	}

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", defaultTraceOptions(), "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}

	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	if report.SchemaVersion != analysis.TraceDrilldownSchemaVersion {
		t.Errorf("schema_version = %q, want %q", report.SchemaVersion, analysis.TraceDrilldownSchemaVersion)
	}
	if report.Trace == nil || report.Trace.SpanCount != 2 {
		t.Fatalf("trace summary missing or wrong span count: %+v", report.Trace)
	}
	if report.Trace.RootOperation != "Query" {
		t.Errorf("root operation = %q, want Query", report.Trace.RootOperation)
	}
	if len(report.Trace.SlowestSpans) == 0 || report.Trace.SlowestSpans[0].OperationName != "Query" {
		t.Errorf("slowest span should be the 4000ms root Query, got %+v", report.Trace.SlowestSpans)
	}
	if report.QueryLog == nil || report.QueryLog.User != "etl_user" {
		t.Errorf("query-log summary missing user: %+v", report.QueryLog)
	}
	if len(report.Warnings) != 0 {
		t.Errorf("trace-found run should have no warnings, got %v", report.Warnings)
	}
	// The blacklist must be threaded into the span fetch.
	if len(src.spanBlacklist) != 1 || src.spanBlacklist[0] != "MergeTreeIndex" {
		t.Errorf("blacklist not threaded to span fetch: %v", src.spanBlacklist)
	}
}

func TestAnalyzeTrace_QueryIDWithNoSpanMatchWarnsExit0(t *testing.T) {
	src := &fakeTraceSource{
		traceIDs: nil, // no trace IDs for this query ID
		queryLog: map[string]model.QueryLog{
			"q-123": {QueryID: "q-123", QueryDurationMs: 10, User: "u"},
		},
	}

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", defaultTraceOptions(), "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("missing trace must still exit 0; got %d, stderr:\n%s", code, errOut.String())
	}

	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if report.Trace != nil {
		t.Errorf("trace section should be omitted when no spans, got %+v", report.Trace)
	}
	if report.QueryLog == nil {
		t.Error("query-log metadata should still be present when the trace is missing")
	}
	if !containsSubstring(report.Warnings, "no trace IDs found") {
		t.Errorf("expected a missing-trace warning, got %v", report.Warnings)
	}
}

func TestAnalyzeTrace_MissingQueryLogRowWarns(t *testing.T) {
	traceID := uuid.New()
	src := &fakeTraceSource{
		traceIDs: []string{traceID.String()},
		spans:    []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog: map[string]model.QueryLog{}, // no row for q-123
	}

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", defaultTraceOptions(), "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}

	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if report.Trace == nil {
		t.Error("trace section should be present even without a query-log row")
	}
	if report.QueryLog != nil {
		t.Errorf("query-log section should be omitted when no row found, got %+v", report.QueryLog)
	}
	if !containsSubstring(report.Warnings, "no query_log row found") {
		t.Errorf("expected a missing-query-log warning, got %v", report.Warnings)
	}
}

func TestAnalyzeTrace_QueryLogEnrichmentErrorIsNonFatal(t *testing.T) {
	traceID := uuid.New()
	src := &fakeTraceSource{
		traceIDs:    []string{traceID.String()},
		spans:       []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLogErr: errors.New("ACCESS_DENIED"),
	}

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", defaultTraceOptions(), "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("query-log enrichment failure must be non-fatal; got %d", code)
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !containsSubstring(report.Warnings, "ACCESS_DENIED") {
		t.Errorf("expected enrichment warning, got %v", report.Warnings)
	}
}

func TestAnalyzeTrace_TraceIDLookupFailureExits1(t *testing.T) {
	src := &fakeTraceSource{traceIDsErr: errors.New("connection reset")}
	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", defaultTraceOptions(), "table", "", &out, &errOut)
	if code != 1 {
		t.Fatalf("required trace-id lookup failure should exit 1, got %d", code)
	}
	if !strings.Contains(errOut.String(), "connection reset") {
		t.Errorf("stderr should surface the query error:\n%s", errOut.String())
	}
}

func TestAnalyzeTrace_DirectTraceIDSkipsQueryIDLookup(t *testing.T) {
	traceID := uuid.New()
	src := &fakeTraceSource{
		spans: []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
	}
	opts := analyzeTraceOptions{
		Lookback:  time.Hour,
		Timeout:   time.Minute,
		TraceID:   traceID.String(),
		SpanLimit: 1000,
	}

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	if src.traceIDQueryID != "" {
		t.Errorf("query-id lookup should be skipped when -trace-id is set; saw queryID %q", src.traceIDQueryID)
	}
	if len(src.spanTraceIDs) != 1 || src.spanTraceIDs[0] != traceID.String() {
		t.Errorf("span fetch should use the supplied trace ID, got %v", src.spanTraceIDs)
	}
	// No query ID was supplied, so no query-log lookup should run.
	if src.queryLogQueryIDs != nil {
		t.Errorf("no query-log lookup should run without a query ID, got %v", src.queryLogQueryIDs)
	}
}

func TestAnalyzeTrace_RedactDimensionsOmitsRawValues(t *testing.T) {
	traceID := uuid.New()
	src := &fakeTraceSource{
		traceIDs: []string{traceID.String()},
		spans:    []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog: map[string]model.QueryLog{
			"q-123": {QueryID: "q-123", User: "secret_etl_user", ClientName: "secret_client"},
		},
	}
	opts := defaultTraceOptions()
	opts.RedactDimensions = true

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	output := out.String()
	if strings.Contains(output, "secret_etl_user") || strings.Contains(output, "secret_client") {
		t.Errorf("redacted report leaked raw dimension values:\n%s", output)
	}
	if !strings.Contains(output, "redacted_user") {
		t.Errorf("redacted report should use stable placeholders:\n%s", output)
	}
}

func TestAnalyzeTrace_OutputPathWrites0600(t *testing.T) {
	traceID := uuid.New()
	src := &fakeTraceSource{
		traceIDs: []string{traceID.String()},
		spans:    []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
	}
	path := filepath.Join(t.TempDir(), "trace.json")

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", defaultTraceOptions(), "json", path, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout should be quiet when -output is set, got:\n%s", out.String())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("report file not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("report file perms = %o, want 600", perm)
	}
}

// TestAnalyzeTrace_OutputReassertsModeOnExistingFile is the drilldown half of
// the report-permission contract; see the queries-side test for the rationale.
func TestAnalyzeTrace_OutputReassertsModeOnExistingFile(t *testing.T) {
	traceID := uuid.New()
	src := &fakeTraceSource{
		traceIDs: []string{traceID.String()},
		spans:    []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
	}
	path := filepath.Join(t.TempDir(), "trace.json")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", defaultTraceOptions(), "json", path, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("report perms after overwriting an existing file = %o, want 600", perm)
	}
}

func TestAnalyzeTrace_TableOutputIsCompact(t *testing.T) {
	traceID := uuid.New()
	src := &fakeTraceSource{
		traceIDs: []string{traceID.String()},
		spans:    []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 5_000_000)},
		queryLog: map[string]model.QueryLog{
			"q-123": {QueryID: "q-123", QueryDurationMs: 4200, User: "etl_user"},
		},
	}
	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", defaultTraceOptions(), "table", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	output := out.String()
	for _, want := range []string{"Trace drilldown", "Selected query:", "Trace:", "span_count: 1", "Query log:"} {
		if !strings.Contains(output, want) {
			t.Errorf("table output missing %q:\n%s", want, output)
		}
	}
}

// ---------------------------------------------------------------------------
// Lookback -> day rounding (shared rule reuse)
// ---------------------------------------------------------------------------

func TestAnalyzeTrace_LookbackDayRoundingReusesSharedRule(t *testing.T) {
	tests := []struct {
		lookback time.Duration
		want     int
	}{
		{time.Hour, 1},
		{24 * time.Hour, 1},
		{25 * time.Hour, 2},
		{48 * time.Hour, 2},
		{time.Minute, 1},
	}
	for _, tt := range tests {
		if got := analysisQueryLogLookbackDays(tt.lookback); got != tt.want {
			t.Errorf("analysisQueryLogLookbackDays(%s) = %d, want %d", tt.lookback, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Phase 2: source/fanout flag parsing and identity precedence
// ---------------------------------------------------------------------------

func TestResolveTraceSource_Table(t *testing.T) {
	tests := []struct {
		name       string
		source     string
		queryID    string
		traceID    string
		hash       string
		wantSource string
		wantErr    string
	}{
		{name: "recent default no identity", source: "recent", wantSource: "recent"},
		{name: "recent with identity becomes other", source: "recent", queryID: "q1", wantSource: "other"},
		{name: "current no identity", source: "current", wantSource: "current"},
		{name: "current with identity becomes other", source: "current", queryID: "q1", wantSource: "other"},
		{name: "other with identity", source: "other", traceID: "t1", wantSource: "other"},
		{name: "other without identity errors", source: "other", wantErr: "requires an identity flag"},
		{name: "unknown rejected", source: "bogus", wantErr: "invalid --source"},
		{name: "hash counts as identity", source: "recent", hash: "123", wantSource: "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveTraceSource(tt.source, tt.queryID, tt.traceID, tt.hash)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantSource {
				t.Errorf("source = %q, want %q", got, tt.wantSource)
			}
		})
	}
}

func TestParseFanout_Table(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		want    fanoutSet
		wantErr string
	}{
		{name: "default", spec: "trace,stats", want: fanoutSet{Trace: true, Stats: true}},
		{name: "similar", spec: "trace,stats,similar", want: fanoutSet{Trace: true, Stats: true, Similar: true}},
		{name: "trace always implied", spec: "stats", want: fanoutSet{Trace: true, Stats: true}},
		{name: "stray commas", spec: "trace,,stats,", want: fanoutSet{Trace: true, Stats: true}},
		{name: "findings accepted", spec: "trace,findings", want: fanoutSet{Trace: true, Findings: true}},
		{name: "all views", spec: "stats,similar,findings", want: fanoutSet{Trace: true, Stats: true, Similar: true, Findings: true}},
		{name: "unknown rejected", spec: "trace,bogus", wantErr: "invalid --fanout view"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFanout(tt.spec)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("fanout = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestAnalyzeTrace_NegativeWaitExits2(t *testing.T) {
	// A negative -wait is bad usage (exit 2), validated before any config load.
	var out, errOut bytes.Buffer
	code := runAnalyzeTrace([]string{"-source", "current", "-wait", "-5s"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "-wait must not be negative") {
		t.Errorf("stderr missing negative-wait rejection:\n%s", errOut.String())
	}
}

func TestAnalyzeTrace_FanoutFindingsAccepted(t *testing.T) {
	// findings is now an accepted -fanout view (Phase 5). With a bogus config
	// path the run fails later at config load (exit 1), but it must NOT be
	// rejected as a usage error (exit 2) the way an unknown view is.
	var out, errOut bytes.Buffer
	code := runAnalyzeTrace([]string{"-config", filepath.Join(t.TempDir(), "missing.yaml"), "-query-id", "q1", "-fanout", "trace,findings"}, &out, &errOut)
	if code == 2 {
		t.Fatalf("findings must be accepted, not a usage error; got exit 2:\n%s", errOut.String())
	}
	if strings.Contains(errOut.String(), "invalid --fanout") || strings.Contains(errOut.String(), "not supported") {
		t.Errorf("findings should not be rejected at parse:\n%s", errOut.String())
	}
}

// ---------------------------------------------------------------------------
// Phase 2: recent candidate search + selection (exit 0, no blocking)
// ---------------------------------------------------------------------------

func recentSearchOptions(match string) analyzeTraceOptions {
	return analyzeTraceOptions{
		Lookback:       time.Hour,
		Timeout:        time.Minute,
		Source:         "recent",
		Match:          match,
		CandidateLimit: defaultTraceCandidateLimit,
		SpanLimit:      1000,
		Fanout:         fanoutSet{Trace: true, Stats: true},
	}
}

func TestAnalyzeTrace_SingleCandidateAutoSelects(t *testing.T) {
	traceID := uuid.New()
	src := &fakeTraceSource{
		normalized: true,
		candidates: []model.QueryLog{
			{QueryID: "q-only", NormalizedQueryHash: 42, NormalizedQuery: "SELECT ? FROM events", User: "etl"},
		},
		traceIDs: []string{traceID.String()},
		spans:    []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog: map[string]model.QueryLog{
			"q-only": {QueryID: "q-only", QueryDurationMs: 1000, NormalizedQueryHash: 42, NormalizedQuery: "SELECT ? FROM events", User: "etl"},
		},
	}

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", recentSearchOptions("events"), "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("single-candidate output should be a full report, not a list: %v\n%s", err, out.String())
	}
	if report.SelectedCandidate == nil || report.SelectedCandidate.QueryID != "q-only" {
		t.Fatalf("expected the single candidate to be selected and drilled, got %+v", report.SelectedCandidate)
	}
	// The search-discovered query_id must appear on the SELECTED candidate, not
	// be misreported as a SUPPLIED identity (the operator supplied -match).
	if report.SuppliedIdentity != nil && report.SuppliedIdentity.QueryID != "" {
		t.Errorf("supplied identity must not carry the search-discovered query_id, got %+v", report.SuppliedIdentity)
	}
	if report.Source != "recent" {
		t.Errorf("source should record the recent search, got %q", report.Source)
	}
	// The query-id lookup must have run for the auto-selected candidate.
	if src.traceIDQueryID != "q-only" {
		t.Errorf("trace lookup should use the selected query_id, got %q", src.traceIDQueryID)
	}
	// The candidate search window must bound to the lookback.
	if src.candidateOpts.Limit != defaultTraceCandidateLimit {
		t.Errorf("candidate limit = %d, want %d", src.candidateOpts.Limit, defaultTraceCandidateLimit)
	}
}

func TestAnalyzeTrace_MultiCandidateListsExit0NoBlock(t *testing.T) {
	src := &fakeTraceSource{
		normalized: true,
		candidates: []model.QueryLog{
			{QueryID: "q-a", QueryDurationMs: 9000, NormalizedQueryHash: 1, NormalizedQuery: "SELECT ? FROM events a"},
			{QueryID: "q-b", QueryDurationMs: 8000, NormalizedQueryHash: 2, NormalizedQuery: "SELECT ? FROM events b"},
		},
	}

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", recentSearchOptions("events"), "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("multi-candidate must exit 0 (no blocking), got %d; stderr:\n%s", code, errOut.String())
	}
	// A multi-candidate run must NOT drill: no trace lookup, no span fetch.
	if src.traceIDQueryID != "" {
		t.Errorf("multi-candidate run must not drill into a trace, drilled %q", src.traceIDQueryID)
	}
	var list traceCandidateList
	if err := json.Unmarshal(out.Bytes(), &list); err != nil {
		t.Fatalf("multi-candidate output should be a candidate list: %v\n%s", err, out.String())
	}
	if len(list.Candidates) != 2 {
		t.Fatalf("candidate list = %d, want 2", len(list.Candidates))
	}
	if !strings.Contains(list.Note, "narrow") {
		t.Errorf("candidate list should note narrowing, got %q", list.Note)
	}
	// Raw query text must never leak: the previews are already-normalized.
	if strings.Contains(out.String(), "db.statement") {
		t.Errorf("candidate list leaked raw statement text:\n%s", out.String())
	}
}

func TestAnalyzeTrace_NormalizedHashIdentitySelects(t *testing.T) {
	traceID := uuid.New()
	src := &fakeTraceSource{
		normalized: true,
		candidates: []model.QueryLog{
			{QueryID: "q-a", NormalizedQueryHash: 100, NormalizedQuery: "SELECT ? FROM a"},
			{QueryID: "q-b", NormalizedQueryHash: 200, NormalizedQuery: "SELECT ? FROM b"},
		},
		traceIDs: []string{traceID.String()},
		spans:    []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog: map[string]model.QueryLog{
			"q-b": {QueryID: "q-b", NormalizedQueryHash: 200, NormalizedQuery: "SELECT ? FROM b"},
		},
	}
	opts := recentSearchOptions("")
	opts.Source = "other"
	opts.NormalizedQueryHash = "200"

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("expected a full report for the resolved hash: %v\n%s", err, out.String())
	}
	if report.SelectedCandidate == nil || report.SelectedCandidate.QueryID != "q-b" {
		t.Fatalf("normalized-hash 200 should resolve to q-b, got %+v", report.SelectedCandidate)
	}
	if report.SuppliedIdentity == nil || report.SuppliedIdentity.NormalizedQueryHash != "200" {
		t.Errorf("supplied identity should record the normalized hash, got %+v", report.SuppliedIdentity)
	}
}

func TestAnalyzeTrace_IdentityPrecedenceTraceIDWinsTraceResolution(t *testing.T) {
	// trace-id beats query-id for trace resolution: with both set, the span
	// fetch uses the supplied trace-id directly and the query-id -> trace-id
	// lookup is skipped. The recent candidate search is also skipped because an
	// explicit identity is present.
	traceID := uuid.New()
	src := &fakeTraceSource{
		spans:      []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		candidates: []model.QueryLog{{QueryID: "should-not-be-searched"}},
	}
	opts := analyzeTraceOptions{
		Lookback:            time.Hour,
		Timeout:             time.Minute,
		Source:              "other",
		TraceID:             traceID.String(),
		QueryID:             "q-co",
		NormalizedQueryHash: "999",
		CandidateLimit:      defaultTraceCandidateLimit,
		SpanLimit:           1000,
		Fanout:              fanoutSet{Trace: true, Stats: true},
	}

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	// trace-id wins trace resolution: the query-id -> trace-id lookup is skipped.
	if src.traceIDQueryID != "" {
		t.Errorf("trace-id path must skip the query-id -> trace-id lookup, drilled %q", src.traceIDQueryID)
	}
	if len(src.spanTraceIDs) != 1 || src.spanTraceIDs[0] != traceID.String() {
		t.Errorf("span fetch should use the supplied trace ID, got %v", src.spanTraceIDs)
	}
	// An explicit identity skips the recent candidate search entirely.
	if !src.candidateOpts.StartTime.IsZero() {
		t.Errorf("explicit identity must skip the recent candidate search, but it ran: %+v", src.candidateOpts)
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	// All supplied identities are recorded for transparency.
	if report.SuppliedIdentity == nil ||
		report.SuppliedIdentity.TraceID != traceID.String() ||
		report.SuppliedIdentity.QueryID != "q-co" ||
		report.SuppliedIdentity.NormalizedQueryHash != "999" {
		t.Errorf("supplied identity should record all supplied flags, got %+v", report.SuppliedIdentity)
	}
}

func TestAnalyzeTrace_NoCandidatesListsExit0(t *testing.T) {
	src := &fakeTraceSource{normalized: true, candidates: nil}
	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", recentSearchOptions("nomatch"), "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("no-candidate run must exit 0, got %d", code)
	}
	var list traceCandidateList
	if err := json.Unmarshal(out.Bytes(), &list); err != nil {
		t.Fatalf("expected a candidate list: %v\n%s", err, out.String())
	}
	if len(list.Candidates) != 0 || !strings.Contains(list.Note, "no candidates") {
		t.Errorf("expected an empty list with a narrowing note, got %+v", list)
	}
}

func TestAnalyzeTrace_MatchHitBelowCostLimitStillSelected(t *testing.T) {
	// The defect: -match was filtered in Go AFTER the SQL ORDER BY duration DESC
	// LIMIT candidate-limit, so a match ranking below the top candidate-limit by
	// cost was silently missed. The fix pushes the predicate into SQL so the
	// LIMIT bounds the MATCHED rows. With the predicate in SQL (modeled by the
	// fake), a candidate-limit of 1 must still surface the (cheap) matching query
	// even though several more-expensive non-matching queries exist.
	traceID := uuid.New()
	src := &fakeTraceSource{
		normalized: true,
		candidates: []model.QueryLog{
			// Most expensive, but does NOT match "audit_export".
			{QueryID: "q-expensive-1", QueryDurationMs: 9000, NormalizedQuery: "SELECT ? FROM hot_path_a"},
			{QueryID: "q-expensive-2", QueryDurationMs: 8000, NormalizedQuery: "SELECT ? FROM hot_path_b"},
			// The only match — and the cheapest row. Under the old post-limit
			// filtering with candidate-limit=1 it would be dropped.
			{QueryID: "q-cheap-match", QueryDurationMs: 5, NormalizedQuery: "SELECT ? FROM audit_export"},
		},
		traceIDs: []string{traceID.String()},
		spans:    []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog: map[string]model.QueryLog{
			"q-cheap-match": {QueryID: "q-cheap-match", QueryDurationMs: 5, NormalizedQuery: "SELECT ? FROM audit_export"},
		},
	}
	opts := recentSearchOptions("audit_export")
	opts.CandidateLimit = 1

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	// The match must drill into the cheap matching query — proving the LIMIT now
	// applies to matched rows, not a cost-truncated top-N.
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("expected a full report for the single match, got:\n%s\nerr: %v", out.String(), err)
	}
	if report.SelectedCandidate == nil || report.SelectedCandidate.QueryID != "q-cheap-match" {
		t.Fatalf("expected the low-cost matching query to be selected, got %+v", report.SelectedCandidate)
	}
	// The match predicate must be pushed to the source (SQL), not applied post-fetch.
	if src.candidateOpts.Match != "audit_export" {
		t.Errorf("match needle should be pushed into the candidate options, got %q", src.candidateOpts.Match)
	}
	if src.candidateOpts.Limit != 1 {
		t.Errorf("candidate limit should be passed to the source unchanged, got %d", src.candidateOpts.Limit)
	}
}

func TestAnalyzeTrace_NormalizedHashResolvesNonMostExpensive(t *testing.T) {
	// -normalized-query-hash is an explicit identity, not a post-filter on a
	// cost-truncated set: the hash predicate is pushed into SQL so a less-
	// expensive query with the requested hash still resolves even with a tight
	// candidate-limit.
	traceID := uuid.New()
	src := &fakeTraceSource{
		normalized: true,
		candidates: []model.QueryLog{
			{QueryID: "q-expensive", QueryDurationMs: 9000, NormalizedQueryHash: 111, NormalizedQuery: "SELECT ? FROM a"},
			{QueryID: "q-target", QueryDurationMs: 12, NormalizedQueryHash: 222, NormalizedQuery: "SELECT ? FROM b"},
		},
		traceIDs: []string{traceID.String()},
		spans:    []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog: map[string]model.QueryLog{
			"q-target": {QueryID: "q-target", NormalizedQueryHash: 222, NormalizedQuery: "SELECT ? FROM b"},
		},
	}
	opts := recentSearchOptions("")
	opts.Source = "other"
	opts.NormalizedQueryHash = "222"
	opts.CandidateLimit = 1

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("expected a full report for the resolved hash, got:\n%s\nerr: %v", out.String(), err)
	}
	if report.SelectedCandidate == nil || report.SelectedCandidate.QueryID != "q-target" {
		t.Fatalf("hash 222 should resolve to the cheaper q-target, got %+v", report.SelectedCandidate)
	}
	// The hash must be pushed into the source options as an explicit identity.
	if !src.candidateOpts.HasHash || src.candidateOpts.NormalizedQueryHash != 222 {
		t.Errorf("hash identity should be pushed into the candidate options, got %+v", src.candidateOpts)
	}
}

func TestAnalyzeTrace_MatchUnsupportedNormalizedFallsBackWithWarning(t *testing.T) {
	// Without normalized support, -match still runs but over metadata only; the
	// fake drops the normalized term to mirror the SQL, and the command emits the
	// previews-unavailable warning.
	traceID := uuid.New()
	src := &fakeTraceSource{
		normalized: false,
		candidates: []model.QueryLog{
			// Would only match on the normalized text (unavailable) — must be dropped.
			{QueryID: "q-norm-only", QueryDurationMs: 9000, NormalizedQuery: "SELECT ? FROM secret_audit"},
			// Matches on a metadata field (user) — must survive.
			{QueryID: "q-meta-match", QueryDurationMs: 5, User: "secret_audit_user"},
		},
		traceIDs: []string{traceID.String()},
		spans:    []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog: map[string]model.QueryLog{
			"q-meta-match": {QueryID: "q-meta-match", User: "secret_audit_user"},
		},
	}
	opts := recentSearchOptions("secret_audit")

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("expected a full report for the single metadata match, got:\n%s\nerr: %v", out.String(), err)
	}
	if report.SelectedCandidate == nil || report.SelectedCandidate.QueryID != "q-meta-match" {
		t.Fatalf("metadata-only match should select q-meta-match, got %+v", report.SelectedCandidate)
	}
	if !containsSubstring(report.Warnings, "-match falls back to candidate metadata only") {
		t.Errorf("expected the metadata-only fallback warning, got %v", report.Warnings)
	}
}

func TestAnalyzeTrace_NormalizedHashUnsupportedEmptyListWithWarning(t *testing.T) {
	// -normalized-query-hash against a ClickHouse without normalized support must
	// NOT run a query against the missing column. The command short-circuits to
	// an empty candidate list with an "unsupported" warning and exits 0.
	src := &fakeTraceSource{
		normalized: false,
		// Even though a row exists, the search must not consult it: the column it
		// would filter on does not exist on this ClickHouse.
		candidates: []model.QueryLog{{QueryID: "q-x", NormalizedQueryHash: 222}},
	}
	opts := recentSearchOptions("")
	opts.Source = "other"
	opts.NormalizedQueryHash = "222"

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("unsupported-hash run must exit 0, got %d; stderr:\n%s", code, errOut.String())
	}
	// Must NOT drill: the source must not have been queried for candidates.
	if !src.candidateOpts.StartTime.IsZero() {
		t.Errorf("unsupported-hash search must short-circuit before the candidate query, but it ran: %+v", src.candidateOpts)
	}
	if src.traceIDQueryID != "" {
		t.Errorf("unsupported-hash run must not drill into a trace, drilled %q", src.traceIDQueryID)
	}
	var list traceCandidateList
	if err := json.Unmarshal(out.Bytes(), &list); err != nil {
		t.Fatalf("expected a candidate list, got:\n%s\nerr: %v", out.String(), err)
	}
	if len(list.Candidates) != 0 {
		t.Errorf("expected an empty candidate list, got %d", len(list.Candidates))
	}
	if !containsSubstring(list.Warnings, "normalized_query_hash unsupported on this ClickHouse") {
		t.Errorf("expected an unsupported-hash warning, got %v", list.Warnings)
	}
}

func TestAnalyzeTrace_CandidateSearchFailureExits1(t *testing.T) {
	src := &fakeTraceSource{normalized: true, candidatesErr: errors.New("connection reset")}
	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", recentSearchOptions("x"), "table", "", &out, &errOut)
	if code != 1 {
		t.Fatalf("required candidate-search failure should exit 1, got %d", code)
	}
	if !strings.Contains(errOut.String(), "connection reset") {
		t.Errorf("stderr should surface the search error:\n%s", errOut.String())
	}
}

func TestAnalyzeTrace_InvalidNormalizedHashExits1(t *testing.T) {
	// A non-numeric -normalized-query-hash reaches the builder (flag parse
	// accepts any string) and fails the required search step.
	src := &fakeTraceSource{normalized: true}
	opts := recentSearchOptions("")
	opts.Source = "other"
	opts.NormalizedQueryHash = "not-a-number"
	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "table", "", &out, &errOut)
	if code != 1 {
		t.Fatalf("invalid hash should fail the search step (exit 1), got %d", code)
	}
	if !strings.Contains(errOut.String(), "invalid --normalized-query-hash") {
		t.Errorf("stderr should explain the bad hash:\n%s", errOut.String())
	}
}

// ---------------------------------------------------------------------------
// Phase 2: similar query-family fan-out
// ---------------------------------------------------------------------------

func TestAnalyzeTrace_SimilarMapsToFamily(t *testing.T) {
	traceID := uuid.New()
	src := &fakeTraceSource{
		normalized: true,
		traceIDs:   []string{traceID.String()},
		spans:      []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog: map[string]model.QueryLog{
			"q-123": {QueryID: "q-123", NormalizedQueryHash: 777, NormalizedQuery: "SELECT ? FROM events"},
		},
		families: []model.QueryFamilyRollup{
			{
				FamilyID:            "fam-1",
				RepresentativeQuery: "SELECT ? FROM events",
				MemberHashesSorted:  []uint64{555, 777, 999},
			},
		},
	}
	opts := defaultTraceOptions()
	opts.Fanout.Similar = true

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if report.Family == nil || report.Family.FamilyID != "fam-1" {
		t.Fatalf("similar fan-out should map hash 777 to fam-1, got %+v", report.Family)
	}
	if len(report.Family.MemberHashes) != 3 || report.Family.MemberHashes[1] != "777" {
		t.Errorf("family member hashes = %v, want [555 777 999] as strings", report.Family.MemberHashes)
	}
}

func TestAnalyzeTrace_SimilarUnsupportedRollupsWarnsExit0(t *testing.T) {
	traceID := uuid.New()
	src := &fakeTraceSource{
		normalized:  true,
		traceIDs:    []string{traceID.String()},
		spans:       []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog:    map[string]model.QueryLog{"q-123": {QueryID: "q-123", NormalizedQueryHash: 777}},
		familiesErr: clickhouse.ErrQueryFamilyRollupsUnsupported,
	}
	opts := defaultTraceOptions()
	opts.Fanout.Similar = true

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("unsupported rollups must degrade to a warning, got %d", code)
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if report.Family != nil {
		t.Errorf("family should be nil when rollups are unsupported, got %+v", report.Family)
	}
	if !containsSubstring(report.Warnings, "query-family rollups are unsupported") {
		t.Errorf("expected an unsupported-rollups warning, got %v", report.Warnings)
	}
}

func TestAnalyzeTrace_SimilarNoMatchingFamilyWarns(t *testing.T) {
	traceID := uuid.New()
	src := &fakeTraceSource{
		normalized: true,
		traceIDs:   []string{traceID.String()},
		spans:      []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog:   map[string]model.QueryLog{"q-123": {QueryID: "q-123", NormalizedQueryHash: 777}},
		families: []model.QueryFamilyRollup{
			{FamilyID: "fam-other", MemberHashesSorted: []uint64{111, 222}},
		},
	}
	opts := defaultTraceOptions()
	opts.Fanout.Similar = true

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if report.Family != nil {
		t.Errorf("family should be nil when no rollup contains the hash, got %+v", report.Family)
	}
	if !containsSubstring(report.Warnings, "no query-family rollup contains normalized_query_hash 777") {
		t.Errorf("expected a no-matching-family warning, got %v", report.Warnings)
	}
}

func TestAnalyzeTrace_SimilarMissingHashWarns(t *testing.T) {
	// A selected query with no normalized hash (unsupported CH) can't map to a
	// family; the command warns and continues.
	traceID := uuid.New()
	src := &fakeTraceSource{
		normalized: false,
		traceIDs:   []string{traceID.String()},
		spans:      []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog:   map[string]model.QueryLog{"q-123": {QueryID: "q-123"}}, // hash 0
	}
	opts := defaultTraceOptions()
	opts.Fanout.Similar = true

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !containsSubstring(report.Warnings, "normalized_query_hash is not supported") {
		t.Errorf("expected an unsupported-hash warning, got %v", report.Warnings)
	}
}

// ---------------------------------------------------------------------------
// JSON stability golden for analysis.trace_drilldown.v1
// ---------------------------------------------------------------------------

func TestBuildTraceDrilldownReport_JSONGolden(t *testing.T) {
	// A fixed trace UUID so the golden output is reproducible.
	traceID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	// Realistic microsecond timestamps so the golden time range is meaningful.
	base := uint64(time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC).UnixMicro())
	sec := uint64(time.Second.Microseconds())
	src := &fakeTraceSource{
		traceIDs: []string{traceID.String()},
		spans: []model.OpenTelemetrySpan{
			makeSpan(traceID, 100, 0, "Query", base+1*sec, base+5*sec),      // 4000ms root
			makeSpan(traceID, 200, 100, "ReadData", base+2*sec, base+3*sec), // 1000ms child
		},
		queryLog: map[string]model.QueryLog{
			"q-123": {
				QueryID:             "q-123",
				QueryKind:           "QueryFinish",
				EventTime:           time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC),
				QueryDurationMs:     4200,
				ReadRows:            1000,
				ReadBytes:           2048,
				ResultRows:          10,
				MemoryUsage:         4096,
				User:                "etl_user",
				ClientName:          "clickhouse-client",
				TablesVisited:       []string{"db.events"},
				NormalizedQueryHash: 777,
				NormalizedQuery:     "SELECT ? FROM events",
			},
		},
		normalized: true,
		families: []model.QueryFamilyRollup{
			{
				FamilyID:            "fam-1",
				RepresentativeQuery: "SELECT ? FROM events",
				MemberHashesSorted:  []uint64{555, 777},
			},
		},
	}

	// The Phase 2 golden exercises the full populated report: selected
	// candidate, query-log stats, and the similar query-family fan-out.
	opts := defaultTraceOptions()
	opts.NormalizedQueryHash = "777"
	opts.Fanout.Similar = true

	report, err := buildTraceDrilldownReport(context.Background(), src, testTraceConfig(), "", opts)
	if err != nil {
		t.Fatalf("buildTraceDrilldownReport: %v", err)
	}
	// GeneratedAt and Window are wall-clock; pin them for the golden compare.
	fixedEnd := time.Date(2026, 6, 15, 1, 0, 0, 0, time.UTC)
	report.GeneratedAt = fixedEnd
	report.Window = analysis.AnalysisWindow{Start: fixedEnd.Add(-time.Hour), End: fixedEnd}

	got, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}

	want := `{
  "schema_version": "analysis.trace_drilldown.v1",
  "generated_at": "2026-06-15T01:00:00Z",
  "window": {
    "start": "2026-06-15T00:00:00Z",
    "end": "2026-06-15T01:00:00Z"
  },
  "source": "other",
  "supplied_identity": {
    "query_id": "q-123",
    "normalized_query_hash": "777"
  },
  "selected_candidate": {
    "source": "other",
    "query_id": "q-123",
    "trace_ids": [
      "11111111-2222-3333-4444-555555555555"
    ],
    "event_time": "2026-06-15T00:00:00Z",
    "query_duration_ms": 4200,
    "query_kind": "QueryFinish",
    "user": "etl_user",
    "client_name": "clickhouse-client",
    "normalized_query_hash": 777,
    "normalized_query": "SELECT ? FROM events"
  },
  "trace": {
    "trace_ids": [
      "11111111-2222-3333-4444-555555555555"
    ],
    "span_count": 2,
    "earliest_start": "2026-06-15T00:00:01Z",
    "latest_finish": "2026-06-15T00:00:05Z",
    "root_operation": "Query",
    "slowest_spans": [
      {
        "trace_id": "11111111-2222-3333-4444-555555555555",
        "span_id": "100",
        "operation_name": "Query",
        "kind": "server",
        "duration_ms": 4000,
        "hostname": "host-1"
      },
      {
        "trace_id": "11111111-2222-3333-4444-555555555555",
        "span_id": "200",
        "parent_span_id": "100",
        "operation_name": "ReadData",
        "kind": "server",
        "duration_ms": 1000,
        "hostname": "host-1"
      }
    ]
  },
  "query_log": {
    "query_id": "q-123",
    "query_kind": "QueryFinish",
    "event_time": "2026-06-15T00:00:00Z",
    "query_duration_ms": 4200,
    "read_rows": 1000,
    "read_bytes": 2048,
    "result_rows": 10,
    "memory_usage": 4096,
    "user": "etl_user",
    "client_name": "clickhouse-client",
    "tables": [
      "db.events"
    ],
    "normalized_query_hash": 777,
    "normalized_query": "SELECT ? FROM events"
  },
  "family": {
    "family_id": "fam-1",
    "representative_query": "SELECT ? FROM events",
    "member_hashes": [
      "555",
      "777"
    ]
  }
}`
	if string(got) != want {
		t.Errorf("trace drilldown JSON drifted from golden:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// ---------------------------------------------------------------------------
// Phase 4: current source (system.processes) + -wait polling
// ---------------------------------------------------------------------------

func currentSearchOptions(match string) analyzeTraceOptions {
	return analyzeTraceOptions{
		Lookback:       time.Hour,
		Timeout:        time.Minute,
		Source:         "current",
		Match:          match,
		CandidateLimit: defaultTraceCandidateLimit,
		SpanLimit:      1000,
		Fanout:         fanoutSet{Trace: true, Stats: true},
	}
}

func TestAnalyzeTrace_CurrentSingleCandidateDrillsWithElapsed(t *testing.T) {
	traceID := uuid.New()
	src := &fakeTraceSource{
		normalized: true,
		currentCandidates: []model.QueryLog{
			{QueryID: "run-1", ElapsedMs: 4200, User: "etl", NormalizedQuery: "SELECT ? FROM events"},
		},
		traceIDs: []string{traceID.String()},
		spans:    []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		// Running query: no query-log row yet.
		queryLog: map[string]model.QueryLog{},
	}

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", currentSearchOptions("events"), "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("single current candidate should produce a full report: %v\n%s", err, out.String())
	}
	if report.Source != "current" {
		t.Errorf("source = %q, want current", report.Source)
	}
	if report.SelectedCandidate == nil || report.SelectedCandidate.QueryID != "run-1" {
		t.Fatalf("expected run-1 selected, got %+v", report.SelectedCandidate)
	}
	// The running metadata must survive even with no query-log row.
	if report.SelectedCandidate.ElapsedMs != 4200 {
		t.Errorf("elapsed_ms = %d, want 4200", report.SelectedCandidate.ElapsedMs)
	}
	if report.SelectedCandidate.User != "etl" {
		t.Errorf("running user lost: %+v", report.SelectedCandidate)
	}
	// No query-log row -> warning, but still exit 0.
	if !containsSubstring(report.Warnings, "no query_log row found") {
		t.Errorf("expected a missing-query-log warning for the running query, got %v", report.Warnings)
	}
	// The current search must have been used (not the recent one).
	if src.currentOpts.Limit != defaultTraceCandidateLimit {
		t.Errorf("current search limit = %d, want %d", src.currentOpts.Limit, defaultTraceCandidateLimit)
	}
	if !src.candidateOpts.StartTime.IsZero() {
		t.Errorf("recent search must not run for the current source, but it did: %+v", src.candidateOpts)
	}
}

func TestAnalyzeTrace_CurrentMultiCandidateListsExit0(t *testing.T) {
	src := &fakeTraceSource{
		normalized: true,
		currentCandidates: []model.QueryLog{
			{QueryID: "run-a", ElapsedMs: 9000, NormalizedQuery: "SELECT ? FROM events a"},
			{QueryID: "run-b", ElapsedMs: 8000, NormalizedQuery: "SELECT ? FROM events b"},
		},
	}

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", currentSearchOptions("events"), "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("multi current candidate must exit 0, got %d; stderr:\n%s", code, errOut.String())
	}
	if src.traceIDQueryID != "" {
		t.Errorf("multi-candidate run must not drill, drilled %q", src.traceIDQueryID)
	}
	var list traceCandidateList
	if err := json.Unmarshal(out.Bytes(), &list); err != nil {
		t.Fatalf("multi current candidate output should be a candidate list: %v\n%s", err, out.String())
	}
	if len(list.Candidates) != 2 || list.Source != "current" {
		t.Fatalf("candidate list = %+v, want 2 current candidates", list)
	}
	if list.Candidates[0].ElapsedMs != 9000 {
		t.Errorf("first candidate elapsed_ms = %d, want 9000", list.Candidates[0].ElapsedMs)
	}
}

func TestAnalyzeTrace_CurrentSearchFailureExits1(t *testing.T) {
	src := &fakeTraceSource{normalized: true, currentCandidatesErr: errors.New("connection reset")}
	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", currentSearchOptions("x"), "table", "", &out, &errOut)
	if code != 1 {
		t.Fatalf("required current-search failure should exit 1, got %d", code)
	}
	if !strings.Contains(errOut.String(), "connection reset") {
		t.Errorf("stderr should surface the search error:\n%s", errOut.String())
	}
}

func TestAnalyzeTrace_CurrentIncompleteDegradesStatsAndSimilar(t *testing.T) {
	// A running query has no query-log stats and no normalized_query_hash yet:
	// both stats and similar fan-outs degrade to warnings, exit 0.
	src := &fakeTraceSource{
		normalized: true,
		currentCandidates: []model.QueryLog{
			{QueryID: "run-1", ElapsedMs: 1000, User: "etl"},
		},
		traceIDs: nil,                         // trace not materialized yet
		queryLog: map[string]model.QueryLog{}, // no query-log row yet
		families: []model.QueryFamilyRollup{{FamilyID: "fam-x", MemberHashesSorted: []uint64{1, 2}}},
	}
	opts := currentSearchOptions("")
	opts.Fanout.Similar = true

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("incomplete current query must exit 0, got %d; stderr:\n%s", code, errOut.String())
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if report.Trace != nil {
		t.Errorf("trace should be absent when not materialized, got %+v", report.Trace)
	}
	if report.QueryLog != nil {
		t.Errorf("query-log stats should be absent for a running query, got %+v", report.QueryLog)
	}
	if report.Family != nil {
		t.Errorf("similar family should be absent without a normalized hash, got %+v", report.Family)
	}
	// All three degradation warnings must be present.
	if !containsSubstring(report.Warnings, "no trace IDs found") {
		t.Errorf("expected a missing-trace warning, got %v", report.Warnings)
	}
	if !containsSubstring(report.Warnings, "no query_log row found") {
		t.Errorf("expected a missing-query-log warning, got %v", report.Warnings)
	}
	if !containsSubstring(report.Warnings, "normalized_query_hash") {
		t.Errorf("expected a similar-fanout degradation warning, got %v", report.Warnings)
	}
}

func TestAnalyzeTrace_WaitZeroDoesNoPolling(t *testing.T) {
	src := &fakeTraceSource{
		normalized:        true,
		currentCandidates: []model.QueryLog{{QueryID: "run-1", ElapsedMs: 100}},
		traceIDs:          nil, // never materializes
		queryLog:          map[string]model.QueryLog{},
	}
	opts := currentSearchOptions("")
	opts.Wait = 0

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	// -wait 0s means exactly one lookup: no polling.
	if src.traceIDCallCount != 1 {
		t.Errorf("with -wait 0s the trace lookup must run exactly once, ran %d times", src.traceIDCallCount)
	}
}

func TestAnalyzeTrace_WaitMaterializesWithinBound(t *testing.T) {
	prev := traceWaitPollInterval
	traceWaitPollInterval = time.Millisecond
	defer func() { traceWaitPollInterval = prev }()

	traceID := uuid.New()
	src := &fakeTraceSource{
		normalized:        true,
		currentCandidates: []model.QueryLog{{QueryID: "run-1", ElapsedMs: 100}},
		// First two lookups: empty; third lookup: trace materialized.
		traceIDsByCall: [][]string{nil, nil, {traceID.String()}},
		spans:          []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog:       map[string]model.QueryLog{},
	}
	opts := currentSearchOptions("")
	opts.Wait = 5 * time.Second

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	// The trace materialized within the wait, so it must appear in the report.
	if report.Trace == nil || report.Trace.SpanCount != 1 {
		t.Fatalf("expected the materialized trace in the report, got %+v", report.Trace)
	}
	if src.traceIDCallCount < 3 {
		t.Errorf("expected polling to re-check until materialization (>=3 calls), got %d", src.traceIDCallCount)
	}
	if containsSubstring(report.Warnings, "no trace IDs found") {
		t.Errorf("materialized trace should not warn about a missing trace: %v", report.Warnings)
	}
}

func TestAnalyzeTrace_WaitBelowPollIntervalStillPolls(t *testing.T) {
	// Regression: a -wait shorter than the poll interval must still do a lookup
	// inside its window. With the interval far larger than -wait, the recheck is
	// scheduled at the remaining wait (not the full interval), so a trace that
	// materializes within the wait is found instead of being missed.
	prev := traceWaitPollInterval
	traceWaitPollInterval = time.Hour
	defer func() { traceWaitPollInterval = prev }()

	traceID := uuid.New()
	src := &fakeTraceSource{
		normalized:        true,
		currentCandidates: []model.QueryLog{{QueryID: "run-1", ElapsedMs: 100}},
		// Empty on the initial lookup; materialized on the first poll.
		traceIDsByCall: [][]string{nil, {traceID.String()}},
		spans:          []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog:       map[string]model.QueryLog{},
	}
	opts := currentSearchOptions("")
	opts.Wait = 30 * time.Millisecond

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if report.Trace == nil || report.Trace.SpanCount != 1 {
		t.Fatalf("trace materializing within a sub-interval -wait must appear, got %+v", report.Trace)
	}
	if src.traceIDCallCount < 2 {
		t.Errorf("expected a recheck within the wait window (>=2 calls), got %d", src.traceIDCallCount)
	}
}

func TestAnalyzeTrace_WaitTimesOutCleanly(t *testing.T) {
	prev := traceWaitPollInterval
	traceWaitPollInterval = time.Millisecond
	defer func() { traceWaitPollInterval = prev }()

	src := &fakeTraceSource{
		normalized:        true,
		currentCandidates: []model.QueryLog{{QueryID: "run-1", ElapsedMs: 100}},
		traceIDs:          nil, // never materializes
		queryLog:          map[string]model.QueryLog{},
	}
	opts := currentSearchOptions("")
	opts.Wait = 30 * time.Millisecond

	start := time.Now()
	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "json", "", &out, &errOut)
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("a wait that never materializes must still exit 0, got %d", code)
	}
	// The poll must not run forever: it is bounded by -wait.
	if elapsed > 2*time.Second {
		t.Errorf("wait should be bounded by -wait (30ms), took %s", elapsed)
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if report.Trace != nil {
		t.Errorf("trace should be absent after a timed-out wait, got %+v", report.Trace)
	}
	if !containsSubstring(report.Warnings, "no trace IDs found") {
		t.Errorf("expected a missing-trace warning after the wait timed out, got %v", report.Warnings)
	}
	// Polling happened (more than the single initial lookup).
	if src.traceIDCallCount < 2 {
		t.Errorf("expected the wait to poll more than once, got %d calls", src.traceIDCallCount)
	}
}

func TestAnalyzeTrace_WaitHonorsTimeoutOverWait(t *testing.T) {
	// -timeout is shorter than -wait: the command must stop at -timeout, never
	// overrunning it, and still exit 0 with a missing-trace warning.
	prev := traceWaitPollInterval
	traceWaitPollInterval = 5 * time.Millisecond
	defer func() { traceWaitPollInterval = prev }()

	src := &fakeTraceSource{
		normalized:        true,
		currentCandidates: []model.QueryLog{{QueryID: "run-1", ElapsedMs: 100}},
		traceIDs:          nil, // never materializes
		queryLog:          map[string]model.QueryLog{},
	}
	opts := currentSearchOptions("")
	opts.Timeout = 50 * time.Millisecond // much shorter than the wait
	opts.Wait = 10 * time.Second

	start := time.Now()
	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "json", "", &out, &errOut)
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	// Must stop near -timeout, well before the 10s -wait.
	if elapsed > 2*time.Second {
		t.Errorf("command must not overrun -timeout (50ms) waiting out the 10s -wait, took %s", elapsed)
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !containsSubstring(report.Warnings, "no trace IDs found") {
		t.Errorf("expected a missing-trace warning when -timeout bounds the wait, got %v", report.Warnings)
	}
}

// ---------------------------------------------------------------------------
// Phase 5: findings fan-out (reuse the analyze-queries registry, filter to the
// selected family, degrade gracefully)
// ---------------------------------------------------------------------------

// skewFamily builds a query-family rollup that trips the real skew analyzer: at
// least skewMinFamilyExecutions executions with one dimension value dominating
// (paired with skewDimCounts below). Using a real analyzer keeps the findings
// path faithful — the trace command runs the SAME compiled-in registry.
func skewFamily(familyID string, hash uint64, repr string) model.QueryFamilyRollup {
	return model.QueryFamilyRollup{
		FamilyID:            familyID,
		RepresentativeQuery: repr,
		MemberHashesSorted:  []uint64{hash},
		Stats:               model.QueryFamilyStats{ExecutionCount: 100},
	}
}

// skewDimCount builds a dominating user-dimension count for a family's hash:
// one user accounts for all 100 executions, far above skewDominanceRatioThreshold.
func skewDimCount(hash uint64, user string) clickhouse.QueryFamilyDimensionCount {
	return clickhouse.QueryFamilyDimensionCount{
		NormalizedQueryHash: hash,
		Dimension:           clickhouse.QueryFamilyDimensionUser,
		Value:               user,
		ExecutionCount:      100,
	}
}

// findingsTraceSource wires a selected query (q-sel, hash selHash) to a trace
// plus a multi-family analysis input where every family trips skew. The
// findings fan-out should keep only the selected family's finding.
func findingsTraceSource(t *testing.T, selHash uint64) (*fakeTraceSource, uuid.UUID) {
	t.Helper()
	traceID := uuid.New()
	return &fakeTraceSource{
		normalized: true,
		traceIDs:   []string{traceID.String()},
		spans:      []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog: map[string]model.QueryLog{
			"q-sel": {QueryID: "q-sel", NormalizedQueryHash: selHash, NormalizedQuery: "SELECT ? FROM events"},
		},
		// Three families, all skew-tripping. fam-sel owns the selected hash.
		families: []model.QueryFamilyRollup{
			skewFamily("fam-sel", selHash, "SELECT ? FROM events"),
			skewFamily("fam-other-1", 111, "SELECT ? FROM other_a"),
			skewFamily("fam-other-2", 222, "SELECT ? FROM other_b"),
		},
		dimensionCounts: []clickhouse.QueryFamilyDimensionCount{
			skewDimCount(selHash, "etl_user"),
			skewDimCount(111, "other_user_a"),
			skewDimCount(222, "other_user_b"),
		},
	}, traceID
}

func findingsTraceOptions() analyzeTraceOptions {
	opts := defaultTraceOptions()
	opts.QueryID = "q-sel"
	opts.Fanout = fanoutSet{Trace: true, Findings: true}
	return opts
}

func TestAnalyzeTrace_FindingsFilteredToSelectedFamily(t *testing.T) {
	const selHash = 777
	src, _ := findingsTraceSource(t, selHash)

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", findingsTraceOptions(), "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if len(report.Findings) == 0 {
		t.Fatalf("expected findings for the selected family, got none; warnings: %v", report.Warnings)
	}
	// Every surviving finding must belong to the selected family — none of the
	// other families' findings may leak through.
	for _, f := range report.Findings {
		if f.FamilyID != "fam-sel" {
			t.Errorf("finding for non-selected family leaked: id=%s family=%s", f.ID, f.FamilyID)
		}
		if !findingHasHash(f, strconv.FormatUint(selHash, 10)) && f.FamilyID == "" {
			t.Errorf("finding %s neither matches the family id nor the selected hash", f.ID)
		}
	}
}

func TestAnalyzeTrace_FindingsReuseCompiledRegistry(t *testing.T) {
	// The trace findings fan-out must run the SAME compiled-in registry as
	// analyze queries — no trace-specific analyzer/path. Prove it by building the
	// equivalent analysis input independently, running analysis.NewRegistry()
	// over it, filtering to the selected family, and asserting the trace path
	// produced the identical findings.
	const selHash = 777
	src, _ := findingsTraceSource(t, selHash)

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", findingsTraceOptions(), "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}

	// Independently build the same analysis input the command would and run the
	// compiled-in registry directly. Use a fresh source clone so the command's
	// recorded call state doesn't interfere.
	indep, _ := findingsTraceSource(t, selHash)
	window := report.Window
	queriesOpts := analyzeQueriesOptions{
		Lookback:           time.Hour,
		Timeout:            time.Minute,
		MinExecutions:      3,
		FamilyLimit:        200,
		QueryPreviewLength: traceNormalizedQueryPreviewLength,
	}
	input, err := buildAnalysisInput(context.Background(), indep, testTraceConfig(), "", queriesOpts, window)
	if err != nil {
		t.Fatalf("buildAnalysisInput: %v", err)
	}
	reg := analysis.NewRegistry()
	// The registry must be the documented compiled-in set — no trace analyzer.
	wantAnalyzers := []string{"coverage", "resource_hog", "attribution_gap", "skew"}
	if got := reg.AnalyzerNames(); strings.Join(got, ",") != strings.Join(wantAnalyzers, ",") {
		t.Fatalf("registry analyzers = %v, want the compiled-in set %v", got, wantAnalyzers)
	}
	allFindings, _ := reg.Run(context.Background(), input)
	wantFindings := filterFindingsToFamily(allFindings, selHash, report.Family)
	analysis.SortFindings(wantFindings)

	gotJSON, _ := json.Marshal(report.Findings)
	wantJSON, _ := json.Marshal(wantFindings)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("trace findings differ from the directly-run registry's filtered findings:\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
	// Sanity: the directly-run registry must have produced findings for ALL three
	// families (so the filtering is doing real work), but the report kept only one.
	if len(allFindings) <= len(report.Findings) {
		t.Errorf("expected the registry to emit more findings (%d) than survive filtering (%d)", len(allFindings), len(report.Findings))
	}
}

func TestAnalyzeTrace_FindingsMissingHashWarnsExit0(t *testing.T) {
	// A selected query with no normalized hash (unsupported CH) can't be matched
	// to a family; findings degrade to a warning and an empty section, exit 0.
	traceID := uuid.New()
	src := &fakeTraceSource{
		normalized: false,
		traceIDs:   []string{traceID.String()},
		spans:      []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog:   map[string]model.QueryLog{"q-sel": {QueryID: "q-sel"}}, // hash 0
	}
	opts := findingsTraceOptions()

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("missing-hash findings must exit 0, got %d", code)
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(report.Findings) != 0 {
		t.Errorf("findings should be empty without a normalized hash, got %+v", report.Findings)
	}
	if !containsSubstring(report.Warnings, "normalized_query_hash is not supported") {
		t.Errorf("expected a findings-skip warning, got %v", report.Warnings)
	}
}

func TestAnalyzeTrace_FindingsUnsupportedRollupsDegradesExit0(t *testing.T) {
	// Rollups unsupported -> buildAnalysisInput still succeeds (rollups become a
	// coverage gap). No family-specific finding can exist, and strict family-only
	// filtering also excludes the window-scoped coverage finding, so the findings
	// section is empty. The command exits 0 and records a warning so the operator
	// still learns findings could not be matched.
	traceID := uuid.New()
	src := &fakeTraceSource{
		normalized:  true,
		traceIDs:    []string{traceID.String()},
		spans:       []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog:    map[string]model.QueryLog{"q-sel": {QueryID: "q-sel", NormalizedQueryHash: 777}},
		familiesErr: clickhouse.ErrQueryFamilyRollupsUnsupported,
	}
	opts := findingsTraceOptions()

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("unsupported rollups in findings must exit 0, got %d", code)
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	// Strict family-only: nothing survives without a family match — not even the
	// window-scoped coverage finding. Findings empty, exit 0, with a warning so
	// the operator still learns findings could not be matched.
	if len(report.Findings) != 0 {
		t.Errorf("strict family-only findings must be empty with unsupported rollups, got %+v", report.Findings)
	}
	sawWarn := false
	for _, w := range report.Warnings {
		if strings.Contains(w, "no findings matched") {
			sawWarn = true
		}
	}
	if !sawWarn {
		t.Errorf("expected a 'no findings matched' warning, got %v", report.Warnings)
	}
}

func TestAnalyzeTrace_FindingsNoMatchingFamilyEmptyWarns(t *testing.T) {
	// Families trip findings, but none owns the selected hash: empty findings
	// section plus a warning, exit 0.
	traceID := uuid.New()
	src := &fakeTraceSource{
		normalized: true,
		traceIDs:   []string{traceID.String()},
		spans:      []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog:   map[string]model.QueryLog{"q-sel": {QueryID: "q-sel", NormalizedQueryHash: 999}}, // hash not in any family
		families: []model.QueryFamilyRollup{
			skewFamily("fam-other-1", 111, "SELECT ? FROM other_a"),
			skewFamily("fam-other-2", 222, "SELECT ? FROM other_b"),
		},
		dimensionCounts: []clickhouse.QueryFamilyDimensionCount{
			skewDimCount(111, "other_user_a"),
			skewDimCount(222, "other_user_b"),
		},
	}
	opts := findingsTraceOptions()

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("no-matching-family findings must exit 0, got %d", code)
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(report.Findings) != 0 {
		t.Errorf("findings should be empty when no family owns the hash, got %+v", report.Findings)
	}
	if !containsSubstring(report.Warnings, "no findings matched the selected family (normalized_query_hash 999)") {
		t.Errorf("expected a no-match findings warning, got %v", report.Warnings)
	}
}

func TestAnalyzeTrace_FindingsInputErrorDegradesNotFatal(t *testing.T) {
	// A required analysis read (dimension counts) failing must NOT fail the
	// drilldown: the findings fan-out degrades to a warning, exit 0.
	const selHash = 777
	src, _ := findingsTraceSource(t, selHash)
	src.dimensionErr = errors.New("ACCESS_DENIED")

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", findingsTraceOptions(), "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("findings analysis-input failure must be non-fatal, got %d; stderr:\n%s", code, errOut.String())
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(report.Findings) != 0 {
		t.Errorf("findings should be empty when the analysis input failed, got %+v", report.Findings)
	}
	if !containsSubstring(report.Warnings, "findings fan-out unavailable") {
		t.Errorf("expected a findings-unavailable warning, got %v", report.Warnings)
	}
}

func TestAnalyzeTrace_FindingsTableOutput(t *testing.T) {
	// The compact table view must render each finding's severity/id/title.
	const selHash = 777
	src, _ := findingsTraceSource(t, selHash)

	var out, errOut bytes.Buffer
	code := analyzeTraceWithSource(src, testTraceConfig(), "", findingsTraceOptions(), "table", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	output := out.String()
	if !strings.Contains(output, "Findings (") {
		t.Errorf("table output missing a Findings section:\n%s", output)
	}
	// The skew finding's severity, analyzer-prefixed id, and title must appear.
	if !strings.Contains(output, "[info]") || !strings.Contains(output, "skew:user:") {
		t.Errorf("findings row should show severity and id:\n%s", output)
	}
	if !strings.Contains(output, "One user dominates query family executions") {
		t.Errorf("findings row should show the title:\n%s", output)
	}
	// Other families' findings must not leak into the table.
	if strings.Contains(output, "fam-other") {
		t.Errorf("table leaked a non-selected family:\n%s", output)
	}
}

func TestFilterFindingsToFamily_Table(t *testing.T) {
	const selHash = 777
	hashStr := strconv.FormatUint(selHash, 10)
	family := &analysis.FamilySummary{FamilyID: "fam-sel", MemberHashes: []string{hashStr}}

	findings := []analysis.Finding{
		{ID: "by-family", FamilyID: "fam-sel"},                                              // matches by family id
		{ID: "by-hash", FamilyID: "fam-merged", NormalizedQueryHashes: []string{hashStr}},   // matches by member hash
		{ID: "coverage", FamilyID: "", NormalizedQueryHashes: nil},                          // window-scoped: excluded (strict family-only)
		{ID: "other-family", FamilyID: "fam-other", NormalizedQueryHashes: []string{"111"}}, // excluded
	}

	got := filterFindingsToFamily(findings, selHash, family)
	gotIDs := make([]string, 0, len(got))
	for _, f := range got {
		gotIDs = append(gotIDs, f.ID)
	}
	want := map[string]bool{"by-family": true, "by-hash": true}
	if len(gotIDs) != len(want) {
		t.Fatalf("kept findings = %v, want exactly %v", gotIDs, want)
	}
	for _, id := range gotIDs {
		if !want[id] {
			t.Errorf("unexpected finding kept: %s", id)
		}
	}
}

func TestBuildTraceDrilldownReport_FindingsJSONGolden(t *testing.T) {
	// The Phase 5 golden exercises a populated findings array under the stable
	// analysis.trace_drilldown.v1 contract. A single skew-tripping selected
	// family keeps the golden deterministic.
	traceID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	base := uint64(time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC).UnixMicro())
	sec := uint64(time.Second.Microseconds())
	const selHash = 777
	src := &fakeTraceSource{
		normalized: true,
		traceIDs:   []string{traceID.String()},
		spans: []model.OpenTelemetrySpan{
			makeSpan(traceID, 100, 0, "Query", base+1*sec, base+5*sec), // 4000ms root
		},
		queryLog: map[string]model.QueryLog{
			"q-sel": {
				QueryID:             "q-sel",
				QueryKind:           "QueryFinish",
				EventTime:           time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC),
				QueryDurationMs:     4200,
				NormalizedQueryHash: selHash,
				NormalizedQuery:     "SELECT ? FROM events",
			},
		},
		families: []model.QueryFamilyRollup{
			skewFamily("fam-sel", selHash, "SELECT ? FROM events"),
		},
		dimensionCounts: []clickhouse.QueryFamilyDimensionCount{
			skewDimCount(selHash, "etl_user"),
		},
	}

	opts := defaultTraceOptions()
	opts.QueryID = "q-sel"
	opts.NormalizedQueryHash = strconv.FormatUint(selHash, 10)
	opts.Fanout = fanoutSet{Trace: true, Stats: true, Findings: true}

	// Pin the command-start clock so the window — and therefore the analysis
	// registry's window-hashed finding IDs — are deterministic for the golden.
	fixedEnd := time.Date(2026, 6, 15, 1, 0, 0, 0, time.UTC)
	prevNow := traceNowFunc
	traceNowFunc = func() time.Time { return fixedEnd }
	defer func() { traceNowFunc = prevNow }()

	report, err := buildTraceDrilldownReport(context.Background(), src, testTraceConfig(), "", opts)
	if err != nil {
		t.Fatalf("buildTraceDrilldownReport: %v", err)
	}

	got, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}

	want := `{
  "schema_version": "analysis.trace_drilldown.v1",
  "generated_at": "2026-06-15T01:00:00Z",
  "window": {
    "start": "2026-06-15T00:00:00Z",
    "end": "2026-06-15T01:00:00Z"
  },
  "source": "other",
  "supplied_identity": {
    "query_id": "q-sel",
    "normalized_query_hash": "777"
  },
  "selected_candidate": {
    "source": "other",
    "query_id": "q-sel",
    "trace_ids": [
      "11111111-2222-3333-4444-555555555555"
    ],
    "event_time": "2026-06-15T00:00:00Z",
    "query_duration_ms": 4200,
    "query_kind": "QueryFinish",
    "normalized_query_hash": 777,
    "normalized_query": "SELECT ? FROM events"
  },
  "trace": {
    "trace_ids": [
      "11111111-2222-3333-4444-555555555555"
    ],
    "span_count": 1,
    "earliest_start": "2026-06-15T00:00:01Z",
    "latest_finish": "2026-06-15T00:00:05Z",
    "root_operation": "Query",
    "slowest_spans": [
      {
        "trace_id": "11111111-2222-3333-4444-555555555555",
        "span_id": "100",
        "operation_name": "Query",
        "kind": "server",
        "duration_ms": 4000,
        "hostname": "host-1"
      }
    ]
  },
  "query_log": {
    "query_id": "q-sel",
    "query_kind": "QueryFinish",
    "event_time": "2026-06-15T00:00:00Z",
    "query_duration_ms": 4200,
    "normalized_query_hash": 777,
    "normalized_query": "SELECT ? FROM events"
  },
  "findings": [
    {
      "schema_version": "analysis.finding.v1",
      "id": "skew:user:41d74ab389c5",
      "analyzer": "skew",
      "severity": "info",
      "confidence": 1,
      "title": "One user dominates query family executions",
      "summary": "user \"etl_user\" accounts for 100% of 100 executions in this window.",
      "family_id": "fam-sel",
      "normalized_query_hashes": [
        "777"
      ],
      "representative_query": "SELECT ? FROM events",
      "window_start": "2026-06-15T00:00:00Z",
      "window_end": "2026-06-15T01:00:00Z",
      "evidence": {
        "dimension": "user",
        "family_execution_count": 100,
        "ratio": 1,
        "value": "etl_user",
        "value_execution_count": 100
      },
      "recommendation": "Verify ownership and workload routing: confirm the dominating user is expected for this family. Concentration is not bad by itself."
    }
  ]
}`
	if string(got) != want {
		t.Errorf("trace drilldown findings JSON drifted from golden:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func containsSubstring(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}
