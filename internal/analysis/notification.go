package analysis

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	// ConditionSchemaVersion versions the stable, window-independent finding
	// identity contract used by notification destinations.
	ConditionSchemaVersion = "analysis.condition.v1"

	// NotificationSchemaVersion versions the bounded, privacy-reviewed DTO
	// shared by every analysis notification destination.
	NotificationSchemaVersion = "analysis.notification.v1"

	// MaxNotificationFindingIdentities is the documented maximum number of
	// eligible condition identities included in a single destination payload.
	MaxNotificationFindingIdentities = 20

	// MaxNotificationHashesPerCondition bounds exact hash identifiers within
	// each included condition. The condition key still covers the full sorted
	// hash set, so truncating display identities cannot merge conditions.
	MaxNotificationHashesPerCondition = 10
)

// SeverityCounts is a fixed, low-cardinality count set for reports and
// baseline-relative new conditions.
type SeverityCounts struct {
	Critical int `json:"critical"`
	Warning  int `json:"warning"`
	Info     int `json:"info"`
}

// NotificationCondition is the only per-finding identity allowed to leave the
// local report. It intentionally excludes titles, summaries, recommendations,
// query previews, dimensions, evidence, paths, endpoints, and credentials.
type NotificationCondition struct {
	ConditionKey          string   `json:"condition_key"`
	Analyzer              string   `json:"analyzer"`
	Severity              Severity `json:"severity"`
	New                   bool     `json:"new"`
	FamilyID              string   `json:"family_id,omitempty"`
	NormalizedQueryHashes []string `json:"normalized_query_hashes,omitempty"`
	HashesTruncated       int      `json:"hashes_truncated,omitempty"`
}

// NotificationSummary is the single privacy-reviewed input consumed by both
// webhook and Datadog renderers.
type NotificationSummary struct {
	SchemaVersion           string                  `json:"schema_version"`
	ReportSchemaVersion     string                  `json:"report_schema_version"`
	Window                  AnalysisWindow          `json:"window"`
	HighestEligibleSeverity Severity                `json:"highest_eligible_severity"`
	TotalFindingCounts      SeverityCounts          `json:"total_finding_counts"`
	NewFindingCounts        SeverityCounts          `json:"new_finding_counts"`
	EligibleFindingCount    int                     `json:"eligible_finding_count"`
	IncludedFindingCount    int                     `json:"included_finding_count"`
	TruncatedFindingCount   int                     `json:"truncated_finding_count"`
	Conditions              []NotificationCondition `json:"conditions"`
}

// ConditionKey derives a deterministic identity without Finding.ID because
// Finding.ID includes the analysis window. The internal condition scope,
// family identity, and complete sorted hash set carry the subject without
// mutable prose or evidence.
func ConditionKey(f Finding) (string, error) {
	if f.Analyzer == "" || f.ConditionScope == "" {
		return "", fmt.Errorf("finding does not expose a valid analyzer and condition scope")
	}
	hashes, err := canonicalHashStrings(f.NormalizedQueryHashes)
	if err != nil {
		return "", fmt.Errorf("finding for analyzer %q: %w", f.Analyzer, err)
	}
	subject := f.FamilyID
	if subject == "" {
		subject = f.ConditionScope
	}
	input := strings.Join([]string{
		ConditionSchemaVersion,
		f.Analyzer,
		f.ConditionScope,
		subject,
		strings.Join(hashes, ","),
	}, "|")
	sum := sha256.Sum256([]byte(input))
	return ConditionSchemaVersion + ":" + hex.EncodeToString(sum[:])[:24], nil
}

// BuildNotificationSummary classifies new and eligible findings from the
// completed report. A usable comparison can prove a regression analyzer's
// condition new, or prove a family condition new when it contains a
// current-only exact hash. Without that proof, only critical findings are
// eligible and new counts remain zero.
func BuildNotificationSummary(report AnalysisReport, comparison *BaselineComparison) (NotificationSummary, error) {
	summary := NotificationSummary{
		SchemaVersion:       NotificationSchemaVersion,
		ReportSchemaVersion: report.SchemaVersion,
		Window:              report.Window,
		Conditions:          []NotificationCondition{},
	}

	conditions := make([]NotificationCondition, 0, len(report.Findings))
	for _, finding := range report.Findings {
		incrementSeverityCount(&summary.TotalFindingCounts, finding.Severity)
		isNew := comparisonFindingIsNew(comparison, finding)
		if isNew {
			incrementSeverityCount(&summary.NewFindingCounts, finding.Severity)
		}
		if finding.Severity == SeverityInfo || (finding.Severity != SeverityCritical && !isNew) {
			continue
		}

		key, err := ConditionKey(finding)
		if err != nil {
			return summary, err
		}
		hashes, err := canonicalHashStrings(finding.NormalizedQueryHashes)
		if err != nil {
			return summary, err
		}
		condition := NotificationCondition{
			ConditionKey: key,
			Analyzer:     finding.Analyzer,
			Severity:     finding.Severity,
			New:          isNew,
			FamilyID:     finding.FamilyID,
		}
		if len(hashes) > MaxNotificationHashesPerCondition {
			condition.HashesTruncated = len(hashes) - MaxNotificationHashesPerCondition
			hashes = hashes[:MaxNotificationHashesPerCondition]
		}
		condition.NormalizedQueryHashes = hashes
		conditions = append(conditions, condition)
	}

	sort.SliceStable(conditions, func(i, j int) bool {
		a, b := conditions[i], conditions[j]
		if severityRank(a.Severity) != severityRank(b.Severity) {
			return severityRank(a.Severity) < severityRank(b.Severity)
		}
		if a.Analyzer != b.Analyzer {
			return a.Analyzer < b.Analyzer
		}
		if a.FamilyID != b.FamilyID {
			return a.FamilyID < b.FamilyID
		}
		return a.ConditionKey < b.ConditionKey
	})
	summary.EligibleFindingCount = len(conditions)
	if len(conditions) > 0 {
		summary.HighestEligibleSeverity = conditions[0].Severity
	}
	if len(conditions) > MaxNotificationFindingIdentities {
		summary.TruncatedFindingCount = len(conditions) - MaxNotificationFindingIdentities
		conditions = conditions[:MaxNotificationFindingIdentities]
	}
	summary.Conditions = conditions
	summary.IncludedFindingCount = len(conditions)
	return summary, nil
}

func comparisonFindingIsNew(comparison *BaselineComparison, finding Finding) bool {
	if comparison == nil || !comparison.Usable() {
		return false
	}
	if isRegressionAnalyzer(finding.Analyzer) {
		return finding.Severity == SeverityWarning || finding.Severity == SeverityCritical
	}
	for _, raw := range finding.NormalizedQueryHashes {
		hash, err := strconv.ParseUint(raw, 10, 64)
		if err == nil && comparison.currentOnlyHash[hash] {
			return true
		}
	}
	return false
}

func incrementSeverityCount(counts *SeverityCounts, severity Severity) {
	switch severity {
	case SeverityCritical:
		counts.Critical++
	case SeverityWarning:
		counts.Warning++
	case SeverityInfo:
		counts.Info++
	}
}

func canonicalHashStrings(values []string) ([]string, error) {
	hashes := append([]string(nil), values...)
	for _, value := range hashes {
		if parsed, err := strconv.ParseUint(value, 10, 64); err != nil || parsed == 0 {
			return nil, fmt.Errorf("normalized query hash %q is not a non-zero uint64", value)
		}
	}
	sortHashStrings(hashes)
	unique := hashes[:0]
	for _, value := range hashes {
		if len(unique) == 0 || unique[len(unique)-1] != value {
			unique = append(unique, value)
		}
	}
	return unique, nil
}
