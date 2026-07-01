package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/analysis"
	"github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
)

// fakeAnalysisSource is a canned queryAnalysisSource for the report path. No
// mock framework is needed: each method returns a configured value.
type fakeAnalysisSource struct {
	normalized      bool
	families        []model.QueryFamilyRollup
	familiesErr     error
	spans           []model.OpenTelemetrySpan
	spansErr        error
	queryLog        map[string]model.QueryLog
	queryLogErr     error
	dimensionCounts []clickhouse.QueryFamilyDimensionCount
	dimensionErr    error
}

func (f *fakeAnalysisSource) QueryLogNormalizedSupported() bool { return f.normalized }

func (f *fakeAnalysisSource) FetchQueryFamilyRollups(_ context.Context, _ clickhouse.QueryFamilyRollupOptions) ([]model.QueryFamilyRollup, error) {
	return f.families, f.familiesErr
}

func (f *fakeAnalysisSource) FetchOpenTelemetrySpansWithOpts(_ context.Context, _ int, _ time.Duration, _ int, _ clickhouse.FetchOpts) ([]model.OpenTelemetrySpan, error) {
	return f.spans, f.spansErr
}

func (f *fakeAnalysisSource) FetchQueryLogByQueryIDs(_ context.Context, _ []string, _ int) (map[string]model.QueryLog, error) {
	return f.queryLog, f.queryLogErr
}

func (f *fakeAnalysisSource) FetchQueryFamilyDimensionCounts(_ context.Context, _ clickhouse.QueryFamilyDimensionCountOptions) ([]clickhouse.QueryFamilyDimensionCount, error) {
	return f.dimensionCounts, f.dimensionErr
}

func testAnalyzeConfig() *config.Config {
	cfg := &config.Config{}
	cfg.ClickHouse.Host = "ch.example.internal"
	cfg.Monitor.MinTraceDurationMs = 1000
	cfg.Monitor.MaxSpansPerCycle = 1000
	return cfg
}

func defaultAnalyzeOptions() analyzeQueriesOptions {
	return analyzeQueriesOptions{
		Lookback:           time.Hour,
		Timeout:            time.Minute,
		MinExecutions:      3,
		FamilyLimit:        200,
		QueryPreviewLength: 500,
	}
}

func TestRunAnalyze_NoNestedVerbExits2(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runAnalyze(nil, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "Subcommands:") || !strings.Contains(errOut.String(), "queries") {
		t.Errorf("usage missing from stderr:\n%s", errOut.String())
	}
}

func TestRunAnalyze_UnknownVerbExits2(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runAnalyze([]string{"foo"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), `unknown command "foo"`) {
		t.Errorf("stderr missing unknown-command line:\n%s", errOut.String())
	}
}

func TestRunAnalyze_HelpExits0(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runAnalyze([]string{"--help"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "analyze") {
		t.Errorf("help missing from stdout:\n%s", out.String())
	}
}

func TestAnalyzeQueries_HelpExits0(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runAnalyzeQueries([]string{"--help"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 for --help", code)
	}
	if !strings.Contains(errOut.String(), "analyze queries") {
		t.Errorf("help text missing:\n%s", errOut.String())
	}
}

func TestAnalyzeQueries_BadFormatExits2(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runAnalyzeQueries([]string{"-format", "yaml"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "invalid -format") {
		t.Errorf("stderr missing invalid-format message:\n%s", errOut.String())
	}
}

func TestAnalyzeQueries_BadFlagExits2(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runAnalyzeQueries([]string{"-nonsense"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestAnalyzeQueries_JSONOutputIsValid(t *testing.T) {
	src := &fakeAnalysisSource{normalized: true}
	var out, errOut bytes.Buffer

	code := analyzeQueriesWithSource(src, testAnalyzeConfig(), "/etc/click-dog/click-dog.yaml", defaultAnalyzeOptions(), "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (findings are not failures); stderr:\n%s", code, errOut.String())
	}

	var report analysis.AnalysisReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	if report.SchemaVersion != analysis.ReportSchemaVersion {
		t.Errorf("schema_version = %q, want %q", report.SchemaVersion, analysis.ReportSchemaVersion)
	}
	// Normalized supported but zero rollups → info coverage finding.
	if len(report.Findings) == 0 {
		t.Error("expected at least a coverage finding for an empty window")
	}
}

func TestAnalyzeQueries_OutputPathKeepsStdoutQuiet(t *testing.T) {
	src := &fakeAnalysisSource{normalized: true}
	var out, errOut bytes.Buffer
	path := filepath.Join(t.TempDir(), "report.txt")

	code := analyzeQueriesWithSource(src, testAnalyzeConfig(), "", defaultAnalyzeOptions(), "table", path, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout should be quiet when -output is set, got:\n%s", out.String())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("report file not written: %v", err)
	}
	if !strings.Contains(string(data), "Query analysis") {
		t.Errorf("report file missing table header:\n%s", data)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("report file perms = %o, want 600", perm)
	}
}

func TestAnalyzeQueries_JSONToFile(t *testing.T) {
	src := &fakeAnalysisSource{normalized: true}
	var out, errOut bytes.Buffer
	path := filepath.Join(t.TempDir(), "report.json")

	code := analyzeQueriesWithSource(src, testAnalyzeConfig(), "", defaultAnalyzeOptions(), "json", path, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout should be quiet when -output is set, got:\n%s", out.String())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("report file not written: %v", err)
	}
	var report analysis.AnalysisReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("file is not valid JSON: %v\n%s", err, data)
	}
}

func TestAnalyzeQueries_RollupsUnsupportedStillSucceeds(t *testing.T) {
	src := &fakeAnalysisSource{
		normalized:  false,
		familiesErr: clickhouse.ErrQueryFamilyRollupsUnsupported,
	}
	var out, errOut bytes.Buffer

	code := analyzeQueriesWithSource(src, testAnalyzeConfig(), "", defaultAnalyzeOptions(), "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("unsupported rollups must not fail the command; got code %d, stderr:\n%s", code, errOut.String())
	}
	var report analysis.AnalysisReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if report.Coverage.QueryFamilyRollupsSupported {
		t.Error("coverage should record rollups as unsupported")
	}
}

func TestAnalyzeQueries_RequiredQueryErrorFailsCommand(t *testing.T) {
	src := &fakeAnalysisSource{
		normalized:  true,
		familiesErr: errors.New("connection reset"),
	}
	var out, errOut bytes.Buffer

	code := analyzeQueriesWithSource(src, testAnalyzeConfig(), "", defaultAnalyzeOptions(), "table", "", &out, &errOut)
	if code != 1 {
		t.Fatalf("a required query failure should exit 1, got %d", code)
	}
	if !strings.Contains(errOut.String(), "connection reset") {
		t.Errorf("stderr should surface the query error:\n%s", errOut.String())
	}
}

func TestAnalyzeQueries_RedactDimensionsOmitsRawValues(t *testing.T) {
	families := []model.QueryFamilyRollup{
		{
			FamilyID:            "qf_0000000000000001",
			RepresentativeQuery: "SELECT 1",
			MemberHashesSorted:  []uint64{100},
			Stats:               model.QueryFamilyStats{ExecutionCount: 100},
		},
	}
	dimCounts := []clickhouse.QueryFamilyDimensionCount{
		{NormalizedQueryHash: 100, Dimension: clickhouse.QueryFamilyDimensionUser, Value: "secret_etl_user", ExecutionCount: 95},
		{NormalizedQueryHash: 100, Dimension: clickhouse.QueryFamilyDimensionUser, Value: "other_user", ExecutionCount: 5},
	}
	src := &fakeAnalysisSource{normalized: true, families: families, dimensionCounts: dimCounts}

	opts := defaultAnalyzeOptions()
	opts.RedactDimensions = true

	var out, errOut bytes.Buffer
	code := analyzeQueriesWithSource(src, testAnalyzeConfig(), "", opts, "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}

	output := out.String()
	if strings.Contains(output, "secret_etl_user") || strings.Contains(output, "other_user") {
		t.Errorf("redacted report leaked raw dimension values:\n%s", output)
	}
	if !strings.Contains(output, "user_1") {
		t.Errorf("redacted report should use stable labels like user_1:\n%s", output)
	}
	// Counts must survive redaction (the skew finding reports the dominant share).
	if !strings.Contains(output, `"value_execution_count": 95`) {
		t.Errorf("redaction should preserve counts:\n%s", output)
	}
}

func TestAnalyzeQueries_NotRedactedKeepsRawValues(t *testing.T) {
	families := []model.QueryFamilyRollup{
		{
			FamilyID:            "qf_0000000000000001",
			RepresentativeQuery: "SELECT 1",
			MemberHashesSorted:  []uint64{100},
			Stats:               model.QueryFamilyStats{ExecutionCount: 100},
		},
	}
	dimCounts := []clickhouse.QueryFamilyDimensionCount{
		{NormalizedQueryHash: 100, Dimension: clickhouse.QueryFamilyDimensionUser, Value: "etl_user", ExecutionCount: 95},
	}
	src := &fakeAnalysisSource{normalized: true, families: families, dimensionCounts: dimCounts}

	var out, errOut bytes.Buffer
	code := analyzeQueriesWithSource(src, testAnalyzeConfig(), "", defaultAnalyzeOptions(), "json", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "etl_user") {
		t.Errorf("default (non-redacted) report should keep raw dimension values:\n%s", out.String())
	}
}

func TestResolveAnalysisSpanSampleLimit(t *testing.T) {
	tests := []struct {
		name             string
		flagLimit        int
		maxSpansPerCycle int
		want             int
	}{
		{"auto default", 0, 0, 1000},
		{"capped by max spans per cycle", 0, 250, 250},
		{"max above auto keeps auto", 0, 5000, 1000},
		{"explicit flag overrides", 200, 50, 200},
		{"explicit flag above cap", 9000, 250, 9000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveAnalysisSpanSampleLimit(tt.flagLimit, tt.maxSpansPerCycle); got != tt.want {
				t.Errorf("resolveAnalysisSpanSampleLimit(%d, %d) = %d, want %d", tt.flagLimit, tt.maxSpansPerCycle, got, tt.want)
			}
		})
	}
}

func TestAnalysisQueryLogLookbackDays(t *testing.T) {
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

func TestBuildAttributionCoverage_MapsSpansToFamilies(t *testing.T) {
	families := []model.QueryFamilyRollup{
		{FamilyID: "qf_1", MemberHashesSorted: []uint64{100}},
	}
	spans := []model.OpenTelemetrySpan{
		// Two spans map to qf_1 via query_id -> normalized hash 100.
		{Attributes: map[string]string{"clickhouse.query_id": "q1", "log_comment": `{"app":"billing","query_name":"invoice"}`}},
		{Attributes: map[string]string{"clickhouse.query_id": "q2", "log_comment": `{"app":"billing"}`}},
		// No query_id — skipped.
		{Attributes: map[string]string{}},
	}
	src := &fakeAnalysisSource{
		queryLog: map[string]model.QueryLog{
			"q1": {QueryID: "q1", NormalizedQueryHash: 100},
			"q2": {QueryID: "q2", NormalizedQueryHash: 100},
		},
	}

	attr, warning := buildAttributionCoverage(context.Background(), src, spans, families, 1)
	if warning != "" {
		t.Fatalf("unexpected warning: %s", warning)
	}
	cov, ok := attr["qf_1"]
	if !ok {
		t.Fatalf("qf_1 missing from attribution: %+v", attr)
	}
	if cov.SampledSpans != 2 {
		t.Errorf("sampled spans = %d, want 2", cov.SampledSpans)
	}
	if cov.WithLogCommentApp != 2 {
		t.Errorf("with app = %d, want 2", cov.WithLogCommentApp)
	}
	if cov.WithLogCommentQueryName != 1 {
		t.Errorf("with query_name = %d, want 1", cov.WithLogCommentQueryName)
	}
	if cov.AppRatio != 1.0 {
		t.Errorf("app ratio = %v, want 1.0", cov.AppRatio)
	}
	if cov.QueryNameRatio != 0.5 {
		t.Errorf("query_name ratio = %v, want 0.5", cov.QueryNameRatio)
	}
}

func TestBuildAttributionCoverage_EnrichmentErrorWarns(t *testing.T) {
	families := []model.QueryFamilyRollup{{FamilyID: "qf_1", MemberHashesSorted: []uint64{100}}}
	spans := []model.OpenTelemetrySpan{{Attributes: map[string]string{"clickhouse.query_id": "q1"}}}
	src := &fakeAnalysisSource{queryLogErr: errors.New("ACCESS_DENIED")}

	attr, warning := buildAttributionCoverage(context.Background(), src, spans, families, 1)
	if attr != nil {
		t.Errorf("attribution should be nil on enrichment failure, got %+v", attr)
	}
	if !strings.Contains(warning, "ACCESS_DENIED") {
		t.Errorf("warning should surface the enrichment error, got %q", warning)
	}
}

func TestMainUsageListsAnalyze(t *testing.T) {
	var out bytes.Buffer
	printUsage(&out)
	if !strings.Contains(out.String(), "analyze") {
		t.Errorf("main usage should list the analyze command:\n%s", out.String())
	}
}
