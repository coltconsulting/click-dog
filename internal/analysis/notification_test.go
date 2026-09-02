package analysis

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestConditionKey_StableAcrossWindowsAndHashOrdering(t *testing.T) {
	base := Finding{
		ID:                    FindingID("resource_hog", "family", "qf_1", time.Unix(100, 0), []uint64{300, 100, 200}),
		Analyzer:              "resource_hog",
		ConditionScope:        "family",
		Severity:              SeverityWarning,
		Title:                 "old title",
		FamilyID:              "qf_1",
		NormalizedQueryHashes: []string{"300", "100", "200"},
		WindowStart:           time.Unix(100, 0),
		WindowEnd:             time.Unix(200, 0),
	}
	later := base
	// The window-specific Finding.ID is deliberately unrelated: condition
	// identity must not parse or otherwise depend on it.
	later.ID = "resource_hog:family:a-different-window-specific-digest"
	later.NormalizedQueryHashes = []string{"200", "300", "100"}
	later.WindowStart = time.Unix(1000, 0)
	later.WindowEnd = time.Unix(1100, 0)
	later.Severity = SeverityCritical
	later.Title = "new title"
	later.Summary = "mutable prose"
	later.Evidence = map[string]any{"mutable": true}
	later.Recommendation = "changed recommendation"

	a, err := ConditionKey(base)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ConditionKey(later)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("condition key changed across window/hash order: %q != %q", a, b)
	}
	if !strings.HasPrefix(a, ConditionSchemaVersion+":") {
		t.Fatalf("condition key %q is not versioned", a)
	}
}

func TestConditionKey_ChangesForMeaningfulIdentity(t *testing.T) {
	base := Finding{ID: "resource_hog:family:windowed", Analyzer: "resource_hog", ConditionScope: "family", FamilyID: "qf_1", NormalizedQueryHashes: []string{"100"}}
	baseKey, err := ConditionKey(base)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*Finding)
	}{
		{name: "analyzer", mutate: func(f *Finding) { f.Analyzer = "skew"; f.ID = "skew:family:windowed" }},
		{name: "scope", mutate: func(f *Finding) { f.ConditionScope = "user" }},
		{name: "family", mutate: func(f *Finding) { f.FamilyID = "qf_2" }},
		{name: "hash", mutate: func(f *Finding) { f.NormalizedQueryHashes = []string{"101"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := base
			tt.mutate(&changed)
			got, err := ConditionKey(changed)
			if err != nil {
				t.Fatal(err)
			}
			if got == baseKey {
				t.Fatalf("condition key did not change for %s", tt.name)
			}
		})
	}
}

func TestBuildNotificationSummary_NewRepeatedAndNoBaselineEligibility(t *testing.T) {
	findings := []Finding{
		notificationTestFinding("critical", SeverityCritical, "qf_critical", "100"),
		notificationTestFinding("new_warning", SeverityWarning, "qf_new", "200"),
		notificationTestFinding("repeated_warning", SeverityWarning, "qf_repeat", "300"),
		notificationTestFinding("info", SeverityInfo, "qf_info", "400"),
	}
	report := AnalysisReport{SchemaVersion: ReportSchemaVersion, Findings: findings}

	withoutBaseline, err := BuildNotificationSummary(report, nil)
	if err != nil {
		t.Fatal(err)
	}
	if withoutBaseline.EligibleFindingCount != 1 || withoutBaseline.Conditions[0].FamilyID != "qf_critical" {
		t.Fatalf("without baseline eligible conditions = %+v, want critical only", withoutBaseline.Conditions)
	}
	if withoutBaseline.NewFindingCounts != (SeverityCounts{}) {
		t.Fatalf("without baseline new counts = %+v, want zero", withoutBaseline.NewFindingCounts)
	}

	comparison := &BaselineComparison{
		usable:          true,
		currentOnlyHash: map[uint64]bool{200: true, 400: true},
	}
	withBaseline, err := BuildNotificationSummary(report, comparison)
	if err != nil {
		t.Fatal(err)
	}
	if withBaseline.EligibleFindingCount != 2 {
		t.Fatalf("eligible count = %d, want critical + new warning", withBaseline.EligibleFindingCount)
	}
	if withBaseline.Conditions[0].Severity != SeverityCritical || withBaseline.Conditions[1].Severity != SeverityWarning {
		t.Fatalf("condition severity order = %+v, want critical then warning", withBaseline.Conditions)
	}
	if withBaseline.NewFindingCounts.Warning != 1 || withBaseline.NewFindingCounts.Info != 1 {
		t.Fatalf("new counts = %+v", withBaseline.NewFindingCounts)
	}
	for _, condition := range withBaseline.Conditions {
		if condition.FamilyID == "qf_repeat" || condition.Severity == SeverityInfo {
			t.Fatalf("ineligible repeated/info condition escaped: %+v", condition)
		}
	}
}

func TestBuildNotificationSummary_RegressionFindingIsNewWithUsableComparison(t *testing.T) {
	finding := notificationTestFinding("latency_regression", SeverityWarning, "qf_regressed", "100")
	report := AnalysisReport{SchemaVersion: ComparisonReportSchemaVersion, Findings: []Finding{finding}}
	comparison := &BaselineComparison{usable: true, currentOnlyHash: map[uint64]bool{}}

	summary, err := BuildNotificationSummary(report, comparison)
	if err != nil {
		t.Fatal(err)
	}
	if summary.EligibleFindingCount != 1 || !summary.Conditions[0].New || summary.NewFindingCounts.Warning != 1 {
		t.Fatalf("regression summary = %+v", summary)
	}
}

func TestBuildNotificationSummary_BoundedDeterministicAndPrivate(t *testing.T) {
	const (
		secretSQL  = "SELECT card_number FROM payments"
		secretPath = "/etc/click-dog/secret.yaml"
		secretUser = "customer-admin"
	)
	report := AnalysisReport{
		SchemaVersion: ReportSchemaVersion,
		Window:        AnalysisWindow{Start: time.Unix(100, 0).UTC(), End: time.Unix(200, 0).UTC()},
		Config:        ReportConfig{ConfigPath: secretPath},
	}
	for i := MaxNotificationFindingIdentities + 5; i >= 0; i-- {
		hashes := make([]string, MaxNotificationHashesPerCondition+3)
		for j := range hashes {
			hashes[j] = fmt.Sprintf("%d", (i+1)*1000+j+1)
		}
		finding := notificationTestFinding("resource_hog", SeverityCritical, fmt.Sprintf("qf_%02d", i), hashes...)
		finding.RepresentativeQuery = secretSQL
		finding.Title = secretUser
		finding.Summary = "webhook=https://hooks.example/secret"
		finding.Recommendation = "api_key=secret-value"
		finding.Evidence = map[string]any{"user": secretUser, "path": secretPath}
		report.Findings = append(report.Findings, finding)
	}

	a, err := BuildNotificationSummary(report, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildNotificationSummary(report, nil)
	if err != nil {
		t.Fatal(err)
	}
	aJSON, _ := json.Marshal(a)
	bJSON, _ := json.Marshal(b)
	if string(aJSON) != string(bJSON) {
		t.Fatal("notification summary is not deterministic")
	}
	if len(a.Conditions) != MaxNotificationFindingIdentities || a.TruncatedFindingCount != 6 {
		t.Fatalf("summary bounds = included %d truncated %d", len(a.Conditions), a.TruncatedFindingCount)
	}
	for _, condition := range a.Conditions {
		if len(condition.NormalizedQueryHashes) != MaxNotificationHashesPerCondition || condition.HashesTruncated != 3 {
			t.Fatalf("condition hash bounds = %+v", condition)
		}
	}
	for _, forbidden := range []string{secretSQL, secretPath, secretUser, "secret-value", "hooks.example"} {
		if strings.Contains(string(aJSON), forbidden) {
			t.Fatalf("notification summary leaked %q: %s", forbidden, aJSON)
		}
	}
}

func notificationTestFinding(analyzer string, severity Severity, family string, hashes ...string) Finding {
	scope := "family"
	return Finding{
		ID:                    analyzer + ":" + scope + ":windowed",
		Analyzer:              analyzer,
		ConditionScope:        scope,
		Severity:              severity,
		FamilyID:              family,
		NormalizedQueryHashes: hashes,
		Evidence:              map[string]any{},
	}
}
