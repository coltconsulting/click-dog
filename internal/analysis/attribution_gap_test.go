package analysis

import (
	"context"
	"testing"

	"github.com/coltconsulting/click-dog/internal/model"
)

func runAttributionGap(t *testing.T, families []model.QueryFamilyRollup, attribution map[string]FamilyAttributionCoverage, minExec uint64) []Finding {
	t.Helper()
	input := AnalysisInput{
		Window:              testWindow(),
		Config:              ReportConfig{Lookback: "1h0m0s", MinExecutions: minExec},
		Families:            families,
		AttributionByFamily: attribution,
	}
	findings, err := (&attributionGapAnalyzer{}).Analyze(context.Background(), input)
	if err != nil {
		t.Fatalf("attribution gap analyzer error: %v", err)
	}
	return findings
}

// slowFamily qualifies via p95_duration_ms >= 1000.
func slowFamily(id string, hash, execs uint64) model.QueryFamilyRollup {
	return family(id, hash, execs, model.QueryFamilyStats{P95DurationMs: 1500})
}

func TestAttributionGapAnalyzer_LowAppCoverage(t *testing.T) {
	families := []model.QueryFamilyRollup{slowFamily("qf_1", 10, 50)}
	attr := map[string]FamilyAttributionCoverage{
		"qf_1": {FamilyID: "qf_1", SampledSpans: 20, WithLogCommentApp: 4, WithLogCommentQueryName: 20, AppRatio: 0.20, QueryNameRatio: 1.0},
	}

	findings := runAttributionGap(t, families, attr, 3)
	f := findingForFamily(findings, "qf_1")
	if f == nil {
		t.Fatalf("expected low app-coverage finding: %+v", findings)
	}
	if f.Severity != SeverityWarning {
		t.Errorf("severity = %s, want warning", f.Severity)
	}
	if f.Title != "Query family has low log_comment.app attribution" {
		t.Errorf("title = %q", f.Title)
	}
}

func TestAttributionGapAnalyzer_AbsentVsLow(t *testing.T) {
	families := []model.QueryFamilyRollup{slowFamily("qf_1", 10, 50)}
	attr := map[string]FamilyAttributionCoverage{
		"qf_1": {FamilyID: "qf_1", SampledSpans: 20, WithLogCommentApp: 0, WithLogCommentQueryName: 20, AppRatio: 0.0, QueryNameRatio: 1.0},
	}

	f := findingForFamily(runAttributionGap(t, families, attr, 3), "qf_1")
	if f == nil {
		t.Fatalf("expected finding")
	}
	if f.Title != "Query family has no log_comment.app attribution" {
		t.Errorf("title = %q, want absent wording for ratio 0", f.Title)
	}
}

func TestAttributionGapAnalyzer_LowQueryNameCoverage(t *testing.T) {
	families := []model.QueryFamilyRollup{slowFamily("qf_1", 10, 50)}
	attr := map[string]FamilyAttributionCoverage{
		"qf_1": {FamilyID: "qf_1", SampledSpans: 30, WithLogCommentApp: 30, WithLogCommentQueryName: 3, AppRatio: 1.0, QueryNameRatio: 0.1},
	}

	f := findingForFamily(runAttributionGap(t, families, attr, 3), "qf_1")
	if f == nil {
		t.Fatalf("expected low query-name finding: %+v", attr)
	}
	if f.Title != "Query family has low log_comment.query_name attribution" {
		t.Errorf("title = %q", f.Title)
	}
}

func TestAttributionGapAnalyzer_SkipWhenSampleTooSmall(t *testing.T) {
	families := []model.QueryFamilyRollup{slowFamily("qf_1", 10, 50)}
	attr := map[string]FamilyAttributionCoverage{
		"qf_1": {FamilyID: "qf_1", SampledSpans: 9, WithLogCommentApp: 0, WithLogCommentQueryName: 0, AppRatio: 0, QueryNameRatio: 0},
	}

	if findingForFamily(runAttributionGap(t, families, attr, 3), "qf_1") != nil {
		t.Error("fewer than 10 sampled spans should not produce a finding")
	}
}

func TestAttributionGapAnalyzer_GoodCoverageNoFinding(t *testing.T) {
	families := []model.QueryFamilyRollup{slowFamily("qf_1", 10, 50)}
	attr := map[string]FamilyAttributionCoverage{
		"qf_1": {FamilyID: "qf_1", SampledSpans: 20, WithLogCommentApp: 18, WithLogCommentQueryName: 20, AppRatio: 0.90, QueryNameRatio: 1.0},
	}

	if findingForFamily(runAttributionGap(t, families, attr, 3), "qf_1") != nil {
		t.Error("good attribution coverage should not produce a finding")
	}
}

func TestAttributionGapAnalyzer_RequiresSlowOrTopExecution(t *testing.T) {
	// A fast family with median execution count is neither slow nor top 20%.
	fast := family("qf_fast", 10, 50, model.QueryFamilyStats{P95DurationMs: 100})
	// Pad with families that are clearly busier so qf_fast isn't in the top 20%.
	families := []model.QueryFamilyRollup{
		fast,
		family("qf_a", 11, 5000, model.QueryFamilyStats{}),
		family("qf_b", 12, 4000, model.QueryFamilyStats{}),
		family("qf_c", 13, 3000, model.QueryFamilyStats{}),
		family("qf_d", 14, 2000, model.QueryFamilyStats{}),
	}
	attr := map[string]FamilyAttributionCoverage{
		"qf_fast": {FamilyID: "qf_fast", SampledSpans: 20, WithLogCommentApp: 0, WithLogCommentQueryName: 0, AppRatio: 0, QueryNameRatio: 0},
	}

	if findingForFamily(runAttributionGap(t, families, attr, 3), "qf_fast") != nil {
		t.Error("a non-slow, non-top-20% family should be skipped even with zero attribution")
	}
}

func TestAttributionGapAnalyzer_TopExecutionQualifies(t *testing.T) {
	// Fast but busiest family — qualifies via top 20% execution count.
	families := []model.QueryFamilyRollup{
		family("qf_busy", 10, 10000, model.QueryFamilyStats{P95DurationMs: 50}),
		family("qf_a", 11, 100, model.QueryFamilyStats{}),
		family("qf_b", 12, 90, model.QueryFamilyStats{}),
		family("qf_c", 13, 80, model.QueryFamilyStats{}),
		family("qf_d", 14, 70, model.QueryFamilyStats{}),
	}
	attr := map[string]FamilyAttributionCoverage{
		"qf_busy": {FamilyID: "qf_busy", SampledSpans: 40, WithLogCommentApp: 0, WithLogCommentQueryName: 0, AppRatio: 0, QueryNameRatio: 0},
	}

	f := findingForFamily(runAttributionGap(t, families, attr, 3), "qf_busy")
	if f == nil {
		t.Fatalf("busiest family with no attribution should produce a finding")
	}
	if f.Title != "Query family has no log_comment app or query_name attribution" {
		t.Errorf("title = %q, want combined absent wording", f.Title)
	}
}
