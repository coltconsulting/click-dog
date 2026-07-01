package analysis

import (
	"context"
	"testing"
)

func runCoverage(t *testing.T, cov CoverageSummary) []Finding {
	t.Helper()
	input := AnalysisInput{
		Window:   testWindow(),
		Config:   ReportConfig{Lookback: "1h0m0s", MinExecutions: 3},
		Coverage: cov,
	}
	findings, err := (&coverageAnalyzer{}).Analyze(context.Background(), input)
	if err != nil {
		t.Fatalf("coverage analyzer error: %v", err)
	}
	return findings
}

func findingByIDPrefix(findings []Finding, prefix string) *Finding {
	for i := range findings {
		if len(findings[i].ID) >= len(prefix) && findings[i].ID[:len(prefix)] == prefix {
			return &findings[i]
		}
	}
	return nil
}

func TestCoverageAnalyzer_MissingNormalizedHashSupport(t *testing.T) {
	findings := runCoverage(t, CoverageSummary{
		NormalizedQuerySupported:    false,
		QueryFamilyRollupsSupported: false,
		SpanSampleAvailable:         true,
	})

	f := findingByIDPrefix(findings, "coverage:normalized_query_hash_unsupported:")
	if f == nil {
		t.Fatalf("missing normalized_query_hash_unsupported finding: %+v", findings)
	}
	if f.Severity != SeverityWarning {
		t.Errorf("severity = %s, want warning", f.Severity)
	}
	if f.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0", f.Confidence)
	}
	if findingByIDPrefix(findings, "coverage:rollups_unsupported:") == nil {
		t.Error("rollups_unsupported should also fire when normalized hash is missing")
	}
	// no_query_families requires normalized support; it must not fire here.
	if findingByIDPrefix(findings, "coverage:no_query_families:") != nil {
		t.Error("no_query_families should not fire without normalized hash support")
	}
}

func TestCoverageAnalyzer_UnsupportedRollups(t *testing.T) {
	findings := runCoverage(t, CoverageSummary{
		NormalizedQuerySupported:    true,
		QueryFamilyRollupsSupported: false,
		SpanSampleAvailable:         true,
	})

	f := findingByIDPrefix(findings, "coverage:rollups_unsupported:")
	if f == nil {
		t.Fatalf("missing rollups_unsupported finding: %+v", findings)
	}
	if f.Severity != SeverityWarning {
		t.Errorf("severity = %s, want warning", f.Severity)
	}
	if findingByIDPrefix(findings, "coverage:normalized_query_hash_unsupported:") != nil {
		t.Error("normalized hash finding should not fire when support is present")
	}
}

func TestCoverageAnalyzer_NoQueryFamilies(t *testing.T) {
	findings := runCoverage(t, CoverageSummary{
		NormalizedQuerySupported:    true,
		QueryFamilyRollupsSupported: true,
		QueryFamilyCount:            0,
		SpanSampleAvailable:         true,
	})

	f := findingByIDPrefix(findings, "coverage:no_query_families:")
	if f == nil {
		t.Fatalf("missing no_query_families finding: %+v", findings)
	}
	if f.Severity != SeverityInfo {
		t.Errorf("severity = %s, want info", f.Severity)
	}
}

func TestCoverageAnalyzer_SpanSampleUnavailable(t *testing.T) {
	findings := runCoverage(t, CoverageSummary{
		NormalizedQuerySupported:    true,
		QueryFamilyRollupsSupported: true,
		QueryFamilyCount:            5,
		SpanSampleAvailable:         false,
	})

	f := findingByIDPrefix(findings, "coverage:span_sample:")
	if f == nil {
		t.Fatalf("missing span_sample finding: %+v", findings)
	}
	if f.Severity != SeverityWarning {
		t.Errorf("severity = %s, want warning", f.Severity)
	}
}

func TestCoverageAnalyzer_QueryIDRatio(t *testing.T) {
	tests := []struct {
		name        string
		sampleSize  int
		withQueryID int
		ratio       float64
		wantFinding bool
	}{
		{"low ratio over large sample", 100, 42, 0.42, true},
		{"ratio at threshold", 100, 80, 0.80, false},
		{"healthy ratio", 1000, 972, 0.972, false},
		{"low ratio but sample too small for a verdict", 49, 10, 0.2041, false},
		{"low ratio at minimum sample", 50, 10, 0.20, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := runCoverage(t, CoverageSummary{
				NormalizedQuerySupported:    true,
				QueryFamilyRollupsSupported: true,
				QueryFamilyCount:            5,
				SpanSampleAvailable:         true,
				SpanSampleSize:              tt.sampleSize,
				SpansWithQueryID:            tt.withQueryID,
				QueryIDRatio:                tt.ratio,
			})

			f := findingByIDPrefix(findings, "coverage:query_id_ratio:")
			if tt.wantFinding && f == nil {
				t.Fatalf("missing query_id_ratio finding: %+v", findings)
			}
			if !tt.wantFinding && f != nil {
				t.Fatalf("unexpected query_id_ratio finding: %+v", *f)
			}
			if f != nil {
				if f.Severity != SeverityWarning {
					t.Errorf("severity = %s, want warning", f.Severity)
				}
				if f.Evidence["span_sample_size"] != tt.sampleSize {
					t.Errorf("evidence span_sample_size = %v, want %d", f.Evidence["span_sample_size"], tt.sampleSize)
				}
			}
		})
	}
}

func TestCoverageAnalyzer_HealthyInputNoFindings(t *testing.T) {
	findings := runCoverage(t, CoverageSummary{
		NormalizedQuerySupported:    true,
		QueryFamilyRollupsSupported: true,
		QueryFamilyCount:            42,
		SpanSampleAvailable:         true,
		SpanSampleSize:              1000,
		SpansWithQueryID:            972,
		QueryIDRatio:                0.972,
	})
	if len(findings) != 0 {
		t.Errorf("healthy coverage produced findings: %+v", findings)
	}
}
