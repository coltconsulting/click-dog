package analysis

import (
	"context"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/model"
)

func regressionInput(t *testing.T, baselineMember, currentMember model.QueryFamilyMember) AnalysisInput {
	t.Helper()
	baselineWindow := AnalysisWindow{Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)}
	baselineInput := AnalysisInput{Window: baselineWindow, Families: []model.QueryFamilyRollup{{
		FamilyID:           "old_family",
		MemberHashesSorted: []uint64{baselineMember.NormalizedQueryHash},
		Members:            []model.QueryFamilyMember{baselineMember},
		Stats:              model.QueryFamilyStats{ExecutionCount: baselineMember.ExecutionCount},
	}}}
	snapshot, err := BuildBaseline(baselineInput, baselineWindow.End, testBaselineCompatibility())
	if err != nil {
		t.Fatal(err)
	}
	currentWindow := AnalysisWindow{Start: baselineWindow.End.Add(time.Hour), End: baselineWindow.End.Add(2 * time.Hour)}
	input := AnalysisInput{
		Window: currentWindow,
		Config: ReportConfig{MinExecutions: 3},
		Families: []model.QueryFamilyRollup{{
			FamilyID:            "new_family_after_recluster",
			RepresentativeQuery: "SELECT normalized FROM app.events WHERE id = ?",
			MemberHashesSorted:  []uint64{currentMember.NormalizedQueryHash},
			Members:             []model.QueryFamilyMember{currentMember},
			Stats:               model.QueryFamilyStats{ExecutionCount: currentMember.ExecutionCount},
		}},
	}
	comparison := BuildBaselineComparison(input, snapshot, testBaselineCompatibility())
	input.Comparison = &comparison
	return input
}

func TestLatencyRegression_RatioFloorVolumeAndChangedFamilyMembership(t *testing.T) {
	baseline := model.QueryFamilyMember{NormalizedQueryHash: 100, ExecutionCount: 100, SuccessfulCount: 100, P95DurationMs: 300, P99DurationMs: 500}
	current := model.QueryFamilyMember{NormalizedQueryHash: 100, ExecutionCount: 120, SuccessfulCount: 120, P95DurationMs: 900, P99DurationMs: 1100}
	input := regressionInput(t, baseline, current)

	findings, err := (&latencyRegressionAnalyzer{}).Analyze(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want one latency regression", findings)
	}
	if findings[0].FamilyID != "new_family_after_recluster" || findings[0].Severity != SeverityWarning {
		t.Fatalf("finding = %+v", findings[0])
	}
	if got := findings[0].NormalizedQueryHashes; len(got) != 1 || got[0] != "100" {
		t.Fatalf("matched hashes = %v", got)
	}

	t.Run("absolute floor", func(t *testing.T) {
		current := current
		current.P95DurationMs = 500 // ratio passes, +200ms does not
		current.P99DurationMs = 700 // ratio does not pass
		input := regressionInput(t, baseline, current)
		findings, _ := (&latencyRegressionAnalyzer{}).Analyze(context.Background(), input)
		if len(findings) != 0 {
			t.Fatalf("small absolute delta produced findings: %+v", findings)
		}
	})

	t.Run("low volume", func(t *testing.T) {
		baseline := baseline
		current := current
		baseline.ExecutionCount, baseline.SuccessfulCount = 19, 19
		current.ExecutionCount, current.SuccessfulCount = 19, 19
		input := regressionInput(t, baseline, current)
		findings, _ := (&latencyRegressionAnalyzer{}).Analyze(context.Background(), input)
		if len(findings) != 0 {
			t.Fatalf("low volume produced findings: %+v", findings)
		}
	})
}

func TestLatencyRegression_PartialCurrentCoverageSuppressesFinding(t *testing.T) {
	baseline := model.QueryFamilyMember{NormalizedQueryHash: 100, ExecutionCount: 100, SuccessfulCount: 100, P95DurationMs: 300, P99DurationMs: 500}
	current := model.QueryFamilyMember{NormalizedQueryHash: 100, ExecutionCount: 70, SuccessfulCount: 70, P95DurationMs: 1000, P99DurationMs: 1500}
	input := regressionInput(t, baseline, current)
	input.Families[0].Members = append(input.Families[0].Members, model.QueryFamilyMember{NormalizedQueryHash: 999, ExecutionCount: 30, SuccessfulCount: 30})
	input.Families[0].MemberHashesSorted = []uint64{100, 999}
	input.Families[0].Stats.ExecutionCount = 100

	findings, _ := (&latencyRegressionAnalyzer{}).Analyze(context.Background(), input)
	if len(findings) != 0 {
		t.Fatalf("70%% matched execution coverage produced findings: %+v", findings)
	}
}

func TestLatencyRegression_P99OnlySignal(t *testing.T) {
	baseline := model.QueryFamilyMember{NormalizedQueryHash: 100, ExecutionCount: 100, SuccessfulCount: 100, P95DurationMs: 300, P99DurationMs: 400}
	current := model.QueryFamilyMember{NormalizedQueryHash: 100, ExecutionCount: 100, SuccessfulCount: 100, P95DurationMs: 450, P99DurationMs: 1000}
	input := regressionInput(t, baseline, current)
	findings, _ := (&latencyRegressionAnalyzer{}).Analyze(context.Background(), input)
	if len(findings) != 1 || findings[0].Evidence["signal"] != "p99_latency" {
		t.Fatalf("findings = %+v, want p99-only regression", findings)
	}
}

func TestFailureSpike_ZeroBaselineAndBoundedExceptionEvidence(t *testing.T) {
	baseline := model.QueryFamilyMember{NormalizedQueryHash: 100, ExecutionCount: 100, SuccessfulCount: 100, P95DurationMs: 100, P99DurationMs: 150}
	current := model.QueryFamilyMember{
		NormalizedQueryHash: 100,
		ExecutionCount:      100,
		SuccessfulCount:     82,
		FailedCount:         18,
		P95DurationMs:       100,
		P99DurationMs:       150,
		TopExceptions: []model.QueryExceptionCount{
			{Code: 241, Count: 10},
			{Code: 60, Count: 8},
		},
	}
	input := regressionInput(t, baseline, current)

	findings, err := (&failureSpikeAnalyzer{}).Analyze(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Severity != SeverityCritical {
		t.Fatalf("findings = %+v, want one critical failure spike", findings)
	}
	if _, ok := findings[0].Evidence["failure_rate_ratio"]; ok {
		t.Fatalf("zero baseline must not emit an infinite/fictional ratio: %+v", findings[0].Evidence)
	}
	if zero, _ := findings[0].Evidence["baseline_rate_was_zero"].(bool); !zero {
		t.Fatalf("zero-baseline evidence missing: %+v", findings[0].Evidence)
	}
	codes, _ := findings[0].Evidence["current_top_exception_codes"].([]int)
	if len(codes) != 2 || codes[0] != 241 || codes[1] != 60 {
		t.Fatalf("exception codes = %v, want deterministic count order", codes)
	}
}

func TestFailureSpike_NoExceptionEvidenceOrRepeatedRateIsSuppressed(t *testing.T) {
	baseline := model.QueryFamilyMember{NormalizedQueryHash: 100, ExecutionCount: 100, SuccessfulCount: 90, FailedCount: 10}
	current := model.QueryFamilyMember{NormalizedQueryHash: 100, ExecutionCount: 100, SuccessfulCount: 80, FailedCount: 20}
	input := regressionInput(t, baseline, current)
	findings, _ := (&failureSpikeAnalyzer{}).Analyze(context.Background(), input)
	if len(findings) != 0 {
		t.Fatalf("missing exception evidence produced findings: %+v", findings)
	}

	current.TopExceptions = []model.QueryExceptionCount{{Code: 241, Count: 20}}
	input = regressionInput(t, baseline, current)
	findings, _ = (&failureSpikeAnalyzer{}).Analyze(context.Background(), input)
	if len(findings) != 0 {
		t.Fatalf("2x repeated failure rate produced findings below 3x threshold: %+v", findings)
	}
}

func TestRegressionRegistryAndReportV2RunState(t *testing.T) {
	baseline := model.QueryFamilyMember{NormalizedQueryHash: 100, ExecutionCount: 100, SuccessfulCount: 100, P95DurationMs: 300, P99DurationMs: 500}
	current := model.QueryFamilyMember{NormalizedQueryHash: 100, ExecutionCount: 100, SuccessfulCount: 100, P95DurationMs: 300, P99DurationMs: 500}
	input := regressionInput(t, baseline, current)
	report := BuildReport(context.Background(), NewRegistryWithRegression(), input, input.Window.End, nil)
	if report.SchemaVersion != ComparisonReportSchemaVersion || report.Comparison == nil {
		t.Fatalf("report = %+v, want v2 comparison", report)
	}
	if len(report.AnalyzerRuns) != 6 {
		t.Fatalf("analyzer runs = %+v, want six", report.AnalyzerRuns)
	}
	if report.AnalyzerRuns[4].Name != "latency_regression" || report.AnalyzerRuns[5].Name != "failure_spike" {
		t.Fatalf("regression run order = %+v", report.AnalyzerRuns)
	}
}
