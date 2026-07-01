package analysis

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// recordingAnalyzer is a registry test double with canned output.
type recordingAnalyzer struct {
	name     string
	findings []Finding
	err      error
	order    *[]string
}

func (a *recordingAnalyzer) Name() string { return a.name }
func (a *recordingAnalyzer) Analyze(_ context.Context, _ AnalysisInput) ([]Finding, error) {
	if a.order != nil {
		*a.order = append(*a.order, a.name)
	}
	return a.findings, a.err
}

func testWindow() AnalysisWindow {
	end := time.Date(2026, 6, 12, 2, 0, 0, 0, time.UTC)
	return AnalysisWindow{Start: end.Add(-time.Hour), End: end}
}

func TestRegistry_ExecutionOrderSequentialAndStable(t *testing.T) {
	var order []string
	reg := &Registry{analyzers: []Analyzer{
		&recordingAnalyzer{name: "first", order: &order},
		&recordingAnalyzer{name: "second", order: &order},
		&recordingAnalyzer{name: "third", order: &order},
	}}

	_, runs := reg.Run(context.Background(), AnalysisInput{Window: testWindow()})

	want := []string{"first", "second", "third"}
	if len(order) != len(want) {
		t.Fatalf("execution order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("execution order = %v, want %v", order, want)
		}
		if runs[i].Name != want[i] {
			t.Fatalf("run order = %v, want %v", runs, want)
		}
	}
}

func TestRegistry_Phase1AnalyzerOrder(t *testing.T) {
	reg := NewRegistry()
	_, runs := reg.Run(context.Background(), AnalysisInput{Window: testWindow()})

	want := []string{"coverage", "resource_hog", "attribution_gap", "skew"}
	if len(runs) != len(want) {
		t.Fatalf("runs = %d, want %d", len(runs), len(want))
	}
	for i := range want {
		if runs[i].Name != want[i] {
			t.Errorf("runs[%d] = %s, want %s", i, runs[i].Name, want[i])
		}
	}
}

func TestRegistry_FindingsWithErrorDiscarded(t *testing.T) {
	reg := &Registry{analyzers: []Analyzer{
		&recordingAnalyzer{
			name:     "broken",
			findings: []Finding{{ID: "broken:family:abc", Severity: SeverityWarning}},
			err:      errors.New("dimension fetch raced"),
		},
		&recordingAnalyzer{
			name:     "healthy",
			findings: []Finding{{ID: "healthy:family:def", Severity: SeverityInfo}},
		},
	}}

	findings, runs := reg.Run(context.Background(), AnalysisInput{Window: testWindow()})

	if len(findings) != 1 || findings[0].ID != "healthy:family:def" {
		t.Fatalf("findings = %+v, want only the healthy analyzer's finding", findings)
	}
	if runs[0].Findings != 0 {
		t.Errorf("broken run Findings = %d, want 0 (count reflects included findings, not produced)", runs[0].Findings)
	}
	if runs[0].Error != "dimension fetch raced" {
		t.Errorf("broken run Error = %q", runs[0].Error)
	}
	if runs[1].Findings != 1 || runs[1].Error != "" {
		t.Errorf("healthy run = %+v, want 1 finding, no error", runs[1])
	}
}

func TestRegistry_InvalidEvidenceRejectedBeforeRender(t *testing.T) {
	tests := []struct {
		name     string
		evidence map[string]any
	}{
		{"NaN", map[string]any{"ratio": math.NaN()}},
		{"+Inf", map[string]any{"ratio": math.Inf(1)}},
		{"-Inf", map[string]any{"ratio": math.Inf(-1)}},
		{"NaN in array", map[string]any{"ratios": []float64{1.0, math.NaN()}}},
		{"nested map", map[string]any{"nested": map[string]any{"x": 1}}},
		{"nil value", map[string]any{"missing": nil}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &Registry{analyzers: []Analyzer{
				&recordingAnalyzer{name: "noisy", findings: []Finding{
					{ID: "noisy:family:bad", Evidence: tt.evidence},
					{ID: "noisy:family:good", Evidence: map[string]any{"count": 1}},
				}},
			}}

			findings, runs := reg.Run(context.Background(), AnalysisInput{Window: testWindow()})

			if len(findings) != 1 || findings[0].ID != "noisy:family:good" {
				t.Fatalf("findings = %+v, want only the valid finding", findings)
			}
			if runs[0].Findings != 1 {
				t.Errorf("run Findings = %d, want 1", runs[0].Findings)
			}
			if !strings.Contains(runs[0].Error, "noisy:family:bad") {
				t.Errorf("run error should name the rejected finding ID, got %q", runs[0].Error)
			}
			// The surviving report must marshal cleanly.
			if _, err := json.Marshal(findings); err != nil {
				t.Errorf("surviving findings failed to marshal: %v", err)
			}
		})
	}
}

func TestRegistry_NormalizesCommonFields(t *testing.T) {
	window := testWindow()
	reg := &Registry{analyzers: []Analyzer{
		&recordingAnalyzer{name: "a", findings: []Finding{
			{ID: "a:family:x", NormalizedQueryHashes: []string{"100", "20", "3"}},
		}},
	}}

	findings, _ := reg.Run(context.Background(), AnalysisInput{Window: window})

	f := findings[0]
	if f.SchemaVersion != FindingSchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", f.SchemaVersion, FindingSchemaVersion)
	}
	if !f.WindowStart.Equal(window.Start) || !f.WindowEnd.Equal(window.End) {
		t.Errorf("window stamps = %s..%s, want %s..%s", f.WindowStart, f.WindowEnd, window.Start, window.End)
	}
	want := []string{"3", "20", "100"}
	for i := range want {
		if f.NormalizedQueryHashes[i] != want[i] {
			t.Fatalf("NormalizedQueryHashes = %v, want %v", f.NormalizedQueryHashes, want)
		}
	}
	if f.Evidence == nil {
		t.Error("nil evidence should be normalized to an empty map")
	}
}

func TestSortFindings_SeverityAnalyzerFamilyID(t *testing.T) {
	findings := []Finding{
		{ID: "z", Severity: SeverityInfo, Analyzer: "skew", FamilyID: "qf_b"},
		{ID: "a", Severity: SeverityCritical, Analyzer: "resource_hog", FamilyID: "qf_b"},
		{ID: "m", Severity: SeverityWarning, Analyzer: "coverage"},
		{ID: "b", Severity: SeverityWarning, Analyzer: "attribution_gap", FamilyID: "qf_a"},
		{ID: "c", Severity: SeverityWarning, Analyzer: "attribution_gap", FamilyID: "qf_b"},
	}
	SortFindings(findings)

	wantIDs := []string{"a", "b", "c", "m", "z"}
	for i, want := range wantIDs {
		if findings[i].ID != want {
			t.Fatalf("sorted IDs = %v, want %v", findingIDs(findings), wantIDs)
		}
	}
}

func findingIDs(findings []Finding) []string {
	ids := make([]string, len(findings))
	for i, f := range findings {
		ids[i] = f.ID
	}
	return ids
}

// TestBuildReport_JSONGolden pins the report JSON contract: field names,
// timestamp formatting, finding order, and evidence keys must stay stable for
// downstream consumers.
func TestBuildReport_JSONGolden(t *testing.T) {
	window := testWindow()
	reg := &Registry{analyzers: []Analyzer{
		&recordingAnalyzer{name: "demo", findings: []Finding{
			{
				ID:             "demo:family:abcdef123456",
				Analyzer:       "demo",
				Severity:       SeverityWarning,
				Confidence:     1.0,
				Title:          "Demo finding",
				Summary:        "demo summary",
				FamilyID:       "qf_0011223344556677",
				Evidence:       map[string]any{"execution_count": 42, "ratio": 0.5},
				Recommendation: "inspect the demo",
			},
		}},
	}}

	input := AnalysisInput{
		Window: window,
		Config: ReportConfig{
			ConfigPath:         "/etc/click-dog/click-dog.yaml",
			ClickHouseHost:     "ch.example.internal",
			Lookback:           "1h0m0s",
			Timeout:            "2m0s",
			MinTraceDurationMs: 1000,
			MinExecutions:      3,
			FamilyLimit:        200,
			SpanSampleLimit:    1000,
			QueryPreviewLength: 500,
		},
		Coverage: CoverageSummary{
			NormalizedQuerySupported:    true,
			QueryFamilyRollupsSupported: true,
			QueryFamilyCount:            1,
			SpanSampleAvailable:         true,
			SpanSampleSize:              100,
			SpansWithQueryID:            97,
			QueryIDRatio:                0.97,
			QueryLogByIDLookbackDays:    1,
			AttributionFamilyCount:      1,
		},
	}

	report := BuildReport(context.Background(), reg, input, window.End, []string{"env warning"})
	// DurationMs is wall-clock and the only non-deterministic field; pin it
	// for the golden comparison.
	for i := range report.AnalyzerRuns {
		report.AnalyzerRuns[i].DurationMs = 0
	}

	got, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}

	want := `{
  "schema_version": "analysis.report.v1",
  "generated_at": "2026-06-12T02:00:00Z",
  "window": {
    "start": "2026-06-12T01:00:00Z",
    "end": "2026-06-12T02:00:00Z"
  },
  "config": {
    "config_path": "/etc/click-dog/click-dog.yaml",
    "clickhouse_host": "ch.example.internal",
    "use_cluster_queries": false,
    "lookback": "1h0m0s",
    "timeout": "2m0s",
    "redact_dimensions": false,
    "min_trace_duration_ms": 1000,
    "min_executions": 3,
    "family_limit": 200,
    "span_sample_limit": 1000,
    "query_preview_length": 500
  },
  "coverage": {
    "normalized_query_supported": true,
    "query_family_rollups_supported": true,
    "query_family_count": 1,
    "span_sample_available": true,
    "span_sample_size": 100,
    "spans_with_query_id": 97,
    "query_id_ratio": 0.97,
    "query_log_by_id_lookback_days": 1,
    "attribution_family_count": 1
  },
  "findings": [
    {
      "schema_version": "analysis.finding.v1",
      "id": "demo:family:abcdef123456",
      "analyzer": "demo",
      "severity": "warning",
      "confidence": 1,
      "title": "Demo finding",
      "summary": "demo summary",
      "family_id": "qf_0011223344556677",
      "window_start": "2026-06-12T01:00:00Z",
      "window_end": "2026-06-12T02:00:00Z",
      "evidence": {
        "execution_count": 42,
        "ratio": 0.5
      },
      "recommendation": "inspect the demo"
    }
  ],
  "analyzer_runs": [
    {
      "name": "demo",
      "findings": 1,
      "duration_ms": 0
    }
  ],
  "warnings": [
    "env warning"
  ]
}`
	if string(got) != want {
		t.Errorf("report JSON drifted from golden:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestBuildReport_DeterministicAcrossRuns(t *testing.T) {
	input := AnalysisInput{Window: testWindow(), Config: ReportConfig{Lookback: "1h0m0s"}}

	a := BuildReport(context.Background(), NewRegistry(), input, input.Window.End, nil)
	b := BuildReport(context.Background(), NewRegistry(), input, input.Window.End, nil)
	for i := range a.AnalyzerRuns {
		a.AnalyzerRuns[i].DurationMs = 0
		b.AnalyzerRuns[i].DurationMs = 0
	}

	aJSON, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal a: %v", err)
	}
	bJSON, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal b: %v", err)
	}
	if string(aJSON) != string(bJSON) {
		t.Errorf("same input produced different JSON:\n%s\n%s", aJSON, bJSON)
	}
}
