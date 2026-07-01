package analysis

import (
	"context"
	"testing"

	"github.com/coltconsulting/click-dog/internal/model"
)

func runSkew(t *testing.T, families []model.QueryFamilyRollup, dims map[string]FamilyDimensionBreakdown) []Finding {
	t.Helper()
	input := AnalysisInput{
		Window:             testWindow(),
		Config:             ReportConfig{Lookback: "1h0m0s", MinExecutions: 3},
		Families:           families,
		DimensionsByFamily: dims,
	}
	findings, err := (&skewAnalyzer{}).Analyze(context.Background(), input)
	if err != nil {
		t.Fatalf("skew analyzer error: %v", err)
	}
	return findings
}

func breakdown(familyID, dimension string, hits ...DimensionHit) map[string]FamilyDimensionBreakdown {
	return map[string]FamilyDimensionBreakdown{
		familyID: {FamilyID: familyID, Values: map[string][]DimensionHit{dimension: hits}},
	}
}

func TestSkewAnalyzer_UserDominance(t *testing.T) {
	families := []model.QueryFamilyRollup{family("qf_1", 10, 100, model.QueryFamilyStats{})}
	dims := breakdown("qf_1", "user", DimensionHit{Value: "etl", ExecutionCount: 90, Ratio: 0.90})

	f := findingForFamily(runSkew(t, families, dims), "qf_1")
	if f == nil {
		t.Fatalf("expected user-dominance finding")
	}
	if f.Severity != SeverityInfo {
		t.Errorf("severity = %s, want info (no resource floor exceeded)", f.Severity)
	}
	if f.Evidence["dimension"] != "user" {
		t.Errorf("evidence dimension = %v, want user", f.Evidence["dimension"])
	}
	if f.Evidence["value"] != "etl" {
		t.Errorf("evidence value = %v, want etl", f.Evidence["value"])
	}
}

func TestSkewAnalyzer_ClientAndHostDominance(t *testing.T) {
	for _, dim := range []string{"client_name", "client_hostname"} {
		t.Run(dim, func(t *testing.T) {
			families := []model.QueryFamilyRollup{family("qf_1", 10, 100, model.QueryFamilyStats{})}
			dims := breakdown("qf_1", dim, DimensionHit{Value: "one", ExecutionCount: 85, Ratio: 0.85})

			f := findingForFamily(runSkew(t, families, dims), "qf_1")
			if f == nil {
				t.Fatalf("expected %s-dominance finding", dim)
			}
			if f.Evidence["dimension"] != dim {
				t.Errorf("evidence dimension = %v, want %s", f.Evidence["dimension"], dim)
			}
		})
	}
}

func TestSkewAnalyzer_ResourceHeavySkewIsWarning(t *testing.T) {
	// Family exceeds the memory floor → warning instead of info.
	families := []model.QueryFamilyRollup{family("qf_1", 10, 100, model.QueryFamilyStats{
		MaxMemoryUsage: 600 * 1024 * 1024,
	})}
	dims := breakdown("qf_1", "user", DimensionHit{Value: "etl", ExecutionCount: 95, Ratio: 0.95})

	f := findingForFamily(runSkew(t, families, dims), "qf_1")
	if f == nil {
		t.Fatalf("expected finding")
	}
	if f.Severity != SeverityWarning {
		t.Errorf("severity = %s, want warning for resource-heavy skew", f.Severity)
	}
}

func TestSkewAnalyzer_BelowThresholdNoFinding(t *testing.T) {
	families := []model.QueryFamilyRollup{family("qf_1", 10, 100, model.QueryFamilyStats{})}
	dims := breakdown("qf_1", "user", DimensionHit{Value: "etl", ExecutionCount: 70, Ratio: 0.70})

	if findingForFamily(runSkew(t, families, dims), "qf_1") != nil {
		t.Error("ratio below 0.80 should not produce a finding")
	}
}

func TestSkewAnalyzer_BelowMinExecutionsSkipped(t *testing.T) {
	families := []model.QueryFamilyRollup{family("qf_1", 10, 19, model.QueryFamilyStats{})}
	dims := breakdown("qf_1", "user", DimensionHit{Value: "etl", ExecutionCount: 19, Ratio: 1.0})

	if findingForFamily(runSkew(t, families, dims), "qf_1") != nil {
		t.Error("family below 20 executions should be skipped by skew")
	}
}

func TestSkewAnalyzer_MultipleDimensionsDistinctIDs(t *testing.T) {
	families := []model.QueryFamilyRollup{family("qf_1", 10, 100, model.QueryFamilyStats{})}
	dims := map[string]FamilyDimensionBreakdown{
		"qf_1": {FamilyID: "qf_1", Values: map[string][]DimensionHit{
			"user":            {{Value: "etl", ExecutionCount: 90, Ratio: 0.90}},
			"client_hostname": {{Value: "host-1", ExecutionCount: 95, Ratio: 0.95}},
		}},
	}

	findings := runSkew(t, families, dims)
	if len(findings) != 2 {
		t.Fatalf("want 2 findings (user + host), got %d: %+v", len(findings), findings)
	}
	if findings[0].ID == findings[1].ID {
		t.Errorf("per-dimension findings must have distinct IDs: %s", findings[0].ID)
	}
}
