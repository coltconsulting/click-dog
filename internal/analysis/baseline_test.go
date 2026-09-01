package analysis

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/model"
)

func testBaselineCompatibility() BaselineCompatibility {
	return BaselineCompatibility{
		FamilyAlgorithmVersion:    "query-family.v1",
		SimilarityThreshold:       0.75,
		FilterFingerprint:         "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		MinExecutions:             3,
		FamilyLimit:               200,
		QueryPreviewLength:        500,
		NormalizedQuerySupported:  true,
		QueryFamilyRollupsSupport: true,
	}
}

func testBaselineInput(window AnalysisWindow) AnalysisInput {
	return AnalysisInput{
		Window: window,
		Coverage: CoverageSummary{
			NormalizedQuerySupported:    true,
			QueryFamilyRollupsSupported: true,
		},
		Families: []model.QueryFamilyRollup{
			{
				FamilyID:            "qf_secret_family",
				RepresentativeQuery: "SELECT secret_column FROM private_table WHERE tenant = ?",
				MemberHashesSorted:  []uint64{100, 200},
				Members: []model.QueryFamilyMember{
					{
						NormalizedQueryHash: 100,
						NormalizedQuery:     "SELECT secret_column FROM private_table WHERE tenant = ?",
						ExecutionCount:      100,
						SuccessfulCount:     98,
						FailedCount:         2,
						P95DurationMs:       300,
						P99DurationMs:       500,
						TopExceptions:       []model.QueryExceptionCount{{Code: 241, Count: 2}},
					},
					{
						NormalizedQueryHash: 200,
						NormalizedQuery:     "SELECT other_secret FROM private_table WHERE tenant = ?",
						ExecutionCount:      50,
						SuccessfulCount:     50,
						P95DurationMs:       200,
						P99DurationMs:       350,
					},
				},
				Stats: model.QueryFamilyStats{ExecutionCount: 150},
			},
		},
		DimensionsByFamily: map[string]FamilyDimensionBreakdown{
			"qf_secret_family": {FamilyID: "qf_secret_family", Values: map[string][]DimensionHit{
				"user": {{Value: "secret_user", ExecutionCount: 150, Ratio: 1}},
			}},
		},
		Config: ReportConfig{ConfigPath: "/secret/config/path.yaml", ClickHouseHost: "secret-host.internal"},
	}
}

func TestBaseline_BuildRoundTripAtomicPrivateAndPrivacyReviewed(t *testing.T) {
	window := AnalysisWindow{
		Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC),
	}
	snapshot, err := BuildBaseline(testBaselineInput(window), window.End, testBaselineCompatibility())
	if err != nil {
		t.Fatalf("BuildBaseline failed: %v", err)
	}
	if snapshot.SchemaVersion != BaselineSchemaVersion || !strings.HasPrefix(snapshot.BaselineID, "bl_") {
		t.Fatalf("unexpected baseline identity: %+v", snapshot)
	}

	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"SELECT", "secret_column", "private_table", "secret_user", "secret-host", "/secret/config"} {
		if strings.Contains(string(data), forbidden) {
			t.Errorf("baseline leaked %q:\n%s", forbidden, data)
		}
	}

	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteBaselineAtomic(path, snapshot); err != nil {
		t.Fatalf("WriteBaselineAtomic failed: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("baseline permissions = %o, want 600", got)
	}
	loaded, err := LoadBaseline(path)
	if err != nil {
		t.Fatalf("LoadBaseline failed: %v", err)
	}
	if loaded.BaselineID != snapshot.BaselineID || len(loaded.Groups) != 2 {
		t.Fatalf("round trip drifted: %+v", loaded)
	}
	if loaded.Groups[0].NormalizedQueryHash != "100" || loaded.Groups[0].FailureRate != 0.02 {
		t.Errorf("first exact group = %+v", loaded.Groups[0])
	}
}

func TestBaseline_IDStableAcrossFamilyAndMemberOrdering(t *testing.T) {
	window := AnalysisWindow{Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)}
	inputA := testBaselineInput(window)
	inputB := testBaselineInput(window)
	inputB.Families[0].Members[0], inputB.Families[0].Members[1] = inputB.Families[0].Members[1], inputB.Families[0].Members[0]
	a, err := BuildBaseline(inputA, window.End, testBaselineCompatibility())
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildBaseline(inputB, window.End, testBaselineCompatibility())
	if err != nil {
		t.Fatal(err)
	}
	if a.BaselineID != b.BaselineID {
		t.Fatalf("ordering changed baseline ID: %s != %s", a.BaselineID, b.BaselineID)
	}
}

func TestBaseline_LoadRejectsUnknownFieldsAndTampering(t *testing.T) {
	window := AnalysisWindow{Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)}
	snapshot, err := BuildBaseline(testBaselineInput(window), window.End, testBaselineCompatibility())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("tampered identity", func(t *testing.T) {
		tampered := snapshot
		tampered.Groups = append([]BaselineExactGroup(nil), snapshot.Groups...)
		tampered.Groups[0].P95DurationMs++
		path := filepath.Join(t.TempDir(), "tampered.json")
		data, _ := json.Marshal(tampered)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadBaseline(path); err == nil || !strings.Contains(err.Error(), "does not match artifact contents") {
			t.Fatalf("LoadBaseline error = %v, want identity failure", err)
		}
	})

	t.Run("unknown field", func(t *testing.T) {
		data, _ := json.Marshal(snapshot)
		data = append(data[:len(data)-1], []byte(`,"surprise":true}`)...)
		path := filepath.Join(t.TempDir(), "unknown.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadBaseline(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("LoadBaseline error = %v, want unknown-field failure", err)
		}
	})
}

func TestBaseline_LoadIsSizeBoundedAndInvalidWritePreservesTarget(t *testing.T) {
	t.Run("size bound", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oversized.json")
		if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, int(BaselineMaxBytes+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadBaseline(path); err == nil || !strings.Contains(err.Error(), "byte limit") {
			t.Fatalf("LoadBaseline error = %v, want size-limit failure", err)
		}
	})

	t.Run("invalid replacement", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "baseline.json")
		original := []byte("known-good artifact")
		if err := os.WriteFile(path, original, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := WriteBaselineAtomic(path, BaselineSnapshot{}); err == nil {
			t.Fatal("WriteBaselineAtomic accepted an invalid snapshot")
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, original) {
			t.Fatalf("invalid write changed target: %q", got)
		}
	})
}

func TestBaselineComparison_ChangedMembershipAndCoverage(t *testing.T) {
	baselineWindow := AnalysisWindow{Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)}
	baselineInput := testBaselineInput(baselineWindow)
	baselineInput.Families[0].FamilyID = "baseline_family_a"
	baselineInput.Families[0].Members = baselineInput.Families[0].Members[:1]
	baselineInput.Families[0].MemberHashesSorted = []uint64{100}
	baselineInput.Families = append(baselineInput.Families, model.QueryFamilyRollup{
		FamilyID:           "baseline_family_b",
		MemberHashesSorted: []uint64{200},
		Members:            []model.QueryFamilyMember{{NormalizedQueryHash: 200, ExecutionCount: 50, SuccessfulCount: 50, P95DurationMs: 200, P99DurationMs: 350}},
		Stats:              model.QueryFamilyStats{ExecutionCount: 50},
	})
	snapshot, err := BuildBaseline(baselineInput, baselineWindow.End, testBaselineCompatibility())
	if err != nil {
		t.Fatal(err)
	}

	currentWindow := AnalysisWindow{Start: baselineWindow.End.Add(time.Hour), End: baselineWindow.End.Add(2 * time.Hour)}
	current := AnalysisInput{Window: currentWindow, Families: []model.QueryFamilyRollup{
		{
			FamilyID:           "current_reclustered_family",
			MemberHashesSorted: []uint64{100, 300},
			Members: []model.QueryFamilyMember{
				{NormalizedQueryHash: 100, ExecutionCount: 90, SuccessfulCount: 90},
				{NormalizedQueryHash: 300, ExecutionCount: 10, SuccessfulCount: 10},
			},
			Stats: model.QueryFamilyStats{ExecutionCount: 100},
		},
	}}
	comparison := BuildBaselineComparison(current, snapshot, testBaselineCompatibility())
	if !comparison.Usable() || comparison.Summary.Status != ComparisonCompatible {
		t.Fatalf("comparison = %+v, want usable", comparison.Summary)
	}
	want := ComparisonCounts{MatchedHashes: 1, CurrentOnlyHashes: 1, BaselineOnlyHashes: 1, MatchedFamilies: 1, BaselineOnlyFamilies: 1}
	if comparison.Summary.Counts != want {
		t.Fatalf("counts = %+v, want %+v", comparison.Summary.Counts, want)
	}
}

func TestBaselineComparison_StaleAndIncompatibleSuppressUse(t *testing.T) {
	baselineWindow := AnalysisWindow{Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)}
	snapshot, err := BuildBaseline(testBaselineInput(baselineWindow), baselineWindow.End, testBaselineCompatibility())
	if err != nil {
		t.Fatal(err)
	}

	staleInput := testBaselineInput(AnalysisWindow{Start: baselineWindow.End.Add(BaselineStaleAfter + time.Hour), End: baselineWindow.End.Add(BaselineStaleAfter + 2*time.Hour)})
	stale := BuildBaselineComparison(staleInput, snapshot, testBaselineCompatibility())
	if stale.Usable() || stale.Summary.Status != ComparisonStale {
		t.Fatalf("stale comparison = %+v", stale.Summary)
	}

	current := testBaselineInput(AnalysisWindow{Start: baselineWindow.End.Add(time.Hour), End: baselineWindow.End.Add(2 * time.Hour)})
	incompatibleSettings := testBaselineCompatibility()
	incompatibleSettings.FilterFingerprint = "sha256:different"
	incompatible := BuildBaselineComparison(current, snapshot, incompatibleSettings)
	if incompatible.Usable() || incompatible.Summary.Status != ComparisonIncompatible {
		t.Fatalf("incompatible comparison = %+v", incompatible.Summary)
	}
	if strings.Contains(strings.Join(incompatible.Summary.Warnings, " "), "sha256:") {
		t.Fatalf("compatibility warning exposed filter fingerprints: %v", incompatible.Summary.Warnings)
	}

	unsupportedSettings := testBaselineCompatibility()
	unsupportedSettings.NormalizedQuerySupported = false
	unsupported := BuildBaselineComparison(current, snapshot, unsupportedSettings)
	if unsupported.Usable() || unsupported.Summary.Status != ComparisonIncompatible {
		t.Fatalf("unsupported comparison = %+v", unsupported.Summary)
	}
}
