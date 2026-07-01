package analysis

import (
	"context"
	"testing"

	"github.com/coltconsulting/click-dog/internal/model"
)

func family(id string, hash uint64, execs uint64, stats model.QueryFamilyStats) model.QueryFamilyRollup {
	stats.ExecutionCount = execs
	return model.QueryFamilyRollup{
		FamilyID:            id,
		MemberHashesSorted:  []uint64{hash},
		RepresentativeQuery: "SELECT 1",
		Stats:               stats,
	}
}

func runResourceHog(t *testing.T, families []model.QueryFamilyRollup, minExec uint64) []Finding {
	t.Helper()
	input := AnalysisInput{
		Window:   testWindow(),
		Config:   ReportConfig{Lookback: "1h0m0s", MinExecutions: minExec},
		Families: families,
	}
	findings, err := (&resourceHogAnalyzer{}).Analyze(context.Background(), input)
	if err != nil {
		t.Fatalf("resource hog analyzer error: %v", err)
	}
	return findings
}

// peerFamilies builds n peer families with a fixed p95_read_rows so the
// candidate has a stable median to be scored against.
func peerFamilies(n int, p95ReadRows float64) []model.QueryFamilyRollup {
	out := make([]model.QueryFamilyRollup, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, family(
			"qf_peer_"+string(rune('a'+i)),
			uint64(1000+i),
			50,
			model.QueryFamilyStats{P95ReadRows: p95ReadRows},
		))
	}
	return out
}

func TestResourceHogAnalyzer_PeerMedianMath(t *testing.T) {
	// 5 peers at 1M rows → median 1M. Candidate at 8M rows = 8x ≥ 5x ratio
	// and ≥ 1M floor → warning.
	families := append(peerFamilies(5, 1_000_000), family("qf_hot", 99, 120, model.QueryFamilyStats{P95ReadRows: 8_000_000}))

	findings := runResourceHog(t, families, 3)
	f := findingForFamily(findings, "qf_hot")
	if f == nil {
		t.Fatalf("expected a resource hog finding for qf_hot: %+v", findings)
	}
	if f.Severity != SeverityWarning {
		t.Errorf("severity = %s, want warning", f.Severity)
	}
	if got := f.Evidence["p95_read_rows"]; got != float64(8_000_000) {
		t.Errorf("evidence p95_read_rows = %v, want 8000000", got)
	}
	if got := f.Evidence["peer_median_p95_read_rows"]; got != float64(1_000_000) {
		t.Errorf("evidence peer_median_p95_read_rows = %v, want 1000000", got)
	}
	if got := f.Evidence["p95_read_rows_ratio"]; got != 8.0 {
		t.Errorf("evidence p95_read_rows_ratio = %v, want 8", got)
	}
}

func TestResourceHogAnalyzer_FloorPreventsTinyRelativeOutliers(t *testing.T) {
	// 5 peers at 100 rows → median 100. Candidate at 50k rows = 500x ratio,
	// but below the 1M floor → no finding.
	families := append(peerFamilies(5, 100), family("qf_small", 99, 120, model.QueryFamilyStats{P95ReadRows: 50_000}))

	findings := runResourceHog(t, families, 3)
	if findingForFamily(findings, "qf_small") != nil {
		t.Errorf("floor should suppress a tiny absolute outlier: %+v", findings)
	}
}

func TestResourceHogAnalyzer_RequiresFivePeers(t *testing.T) {
	// Only 4 peers with a non-zero value → metric not scored.
	families := append(peerFamilies(4, 1_000_000), family("qf_hot", 99, 120, model.QueryFamilyStats{P95ReadRows: 50_000_000}))

	findings := runResourceHog(t, families, 3)
	if findingForFamily(findings, "qf_hot") != nil {
		t.Errorf("fewer than 5 peers should not produce a finding: %+v", findings)
	}
}

func TestResourceHogAnalyzer_CriticalAtTwentyX(t *testing.T) {
	families := append(peerFamilies(5, 1_000_000), family("qf_hot", 99, 120, model.QueryFamilyStats{P95ReadRows: 25_000_000}))

	findings := runResourceHog(t, families, 3)
	f := findingForFamily(findings, "qf_hot")
	if f == nil {
		t.Fatalf("expected finding: %+v", findings)
	}
	if f.Severity != SeverityCritical {
		t.Errorf("severity = %s, want critical at >=20x", f.Severity)
	}
}

func TestResourceHogAnalyzer_OneFindingPerFamilyMultipleMetrics(t *testing.T) {
	// Peers carry non-zero values for two metrics so both can be scored.
	var families []model.QueryFamilyRollup
	for i := 0; i < 5; i++ {
		families = append(families, family(
			"qf_peer_"+string(rune('a'+i)),
			uint64(1000+i),
			50,
			model.QueryFamilyStats{P95ReadRows: 1_000_000, P95DurationMs: 100},
		))
	}
	// Candidate trips both p95_read_rows (8x) and p95_duration_ms (30x → critical).
	families = append(families, family("qf_hot", 99, 120, model.QueryFamilyStats{
		P95ReadRows:   8_000_000,
		P95DurationMs: 3_000,
	}))

	findings := runResourceHog(t, families, 3)
	matches := 0
	var f *Finding
	for i := range findings {
		if findings[i].FamilyID == "qf_hot" {
			matches++
			f = &findings[i]
		}
	}
	if matches != 1 {
		t.Fatalf("want exactly 1 finding for qf_hot, got %d: %+v", matches, findings)
	}
	if _, ok := f.Evidence["p95_read_rows"]; !ok {
		t.Error("evidence missing p95_read_rows")
	}
	if _, ok := f.Evidence["p95_duration_ms"]; !ok {
		t.Error("evidence missing p95_duration_ms")
	}
	// Strongest signal (duration, 30x) drives severity and title.
	if f.Severity != SeverityCritical {
		t.Errorf("severity = %s, want critical", f.Severity)
	}
}

func TestResourceHogAnalyzer_BelowMinExecutionsSkipped(t *testing.T) {
	families := append(peerFamilies(5, 1_000_000), family("qf_hot", 99, 2, model.QueryFamilyStats{P95ReadRows: 8_000_000}))

	findings := runResourceHog(t, families, 3)
	if findingForFamily(findings, "qf_hot") != nil {
		t.Errorf("family below min executions should be skipped: %+v", findings)
	}
}

func findingForFamily(findings []Finding, familyID string) *Finding {
	for i := range findings {
		if findings[i].FamilyID == familyID {
			return &findings[i]
		}
	}
	return nil
}
