package analysis

import (
	"context"
	"fmt"
	"math"
	"sort"
)

const (
	// attributionGapMinSampledSpans gates findings on enough mapped spans to
	// avoid noisy ownership warnings. Intentionally separate from the
	// -min-executions gate: families with 3-9 executions can still appear in
	// resource findings.
	attributionGapMinSampledSpans = 10

	// attributionGapRatioThreshold is the log_comment coverage below which a
	// family is flagged.
	attributionGapRatioThreshold = 0.50

	// attributionGapMinP95DurationMs qualifies slow families regardless of
	// execution-count rank.
	attributionGapMinP95DurationMs = 1000.0

	// attributionGapTopExecutionFraction qualifies the busiest families by
	// execution count.
	attributionGapTopExecutionFraction = 0.20
)

// attributionGapAnalyzer flags expensive or busy families whose sampled spans
// are missing log_comment.app / log_comment.query_name ownership tagging.
type attributionGapAnalyzer struct{}

func (a *attributionGapAnalyzer) Name() string { return "attribution_gap" }

func (a *attributionGapAnalyzer) Analyze(_ context.Context, input AnalysisInput) ([]Finding, error) {
	var findings []Finding
	topFamilies := topExecutionFamilies(input)

	for i := range input.Families {
		family := &input.Families[i]
		if family.Stats.ExecutionCount < input.Config.MinExecutions {
			continue
		}
		attr, ok := input.AttributionByFamily[family.FamilyID]
		if !ok || attr.SampledSpans < attributionGapMinSampledSpans {
			continue
		}
		if family.Stats.P95DurationMs < attributionGapMinP95DurationMs && !topFamilies[family.FamilyID] {
			continue
		}

		appLow := attr.AppRatio < attributionGapRatioThreshold
		nameLow := attr.QueryNameRatio < attributionGapRatioThreshold
		if !appLow && !nameLow {
			continue
		}

		findings = append(findings, Finding{
			ID:                    FindingID(a.Name(), "family", family.FamilyID, input.Window.Start, family.MemberHashesSorted),
			Analyzer:              a.Name(),
			ConditionScope:        "family",
			Severity:              SeverityWarning,
			Confidence:            1.0,
			Title:                 attributionGapTitle(attr, appLow, nameLow),
			Summary:               fmt.Sprintf("%.0f%% of %d sampled spans carry log_comment.app and %.0f%% carry log_comment.query_name.", attr.AppRatio*100, attr.SampledSpans, attr.QueryNameRatio*100),
			FamilyID:              family.FamilyID,
			NormalizedQueryHashes: hashStrings(family.MemberHashesSorted),
			RepresentativeQuery:   family.RepresentativeQuery,
			Evidence: map[string]any{
				"sampled_spans":               attr.SampledSpans,
				"with_log_comment_app":        attr.WithLogCommentApp,
				"with_log_comment_query_name": attr.WithLogCommentQueryName,
				"app_ratio":                   roundRatio(attr.AppRatio),
				"query_name_ratio":            roundRatio(attr.QueryNameRatio),
				"execution_count":             family.Stats.ExecutionCount,
			},
			Recommendation: "Add or standardize ClickHouse client log_comment tagging (app, query_name) for the owning service so this family can be routed to an owner.",
		})
	}

	return findings, nil
}

// attributionGapTitle distinguishes absent attribution (ratio exactly zero)
// from merely low attribution.
func attributionGapTitle(attr FamilyAttributionCoverage, appLow, nameLow bool) string {
	switch {
	case appLow && nameLow:
		if attr.WithLogCommentApp == 0 && attr.WithLogCommentQueryName == 0 {
			return "Query family has no log_comment app or query_name attribution"
		}
		return "Query family has low log_comment app and query_name attribution"
	case appLow:
		if attr.WithLogCommentApp == 0 {
			return "Query family has no log_comment.app attribution"
		}
		return "Query family has low log_comment.app attribution"
	default:
		if attr.WithLogCommentQueryName == 0 {
			return "Query family has no log_comment.query_name attribution"
		}
		return "Query family has low log_comment.query_name attribution"
	}
}

// topExecutionFamilies returns the set of family IDs in the top
// attributionGapTopExecutionFraction by execution count, with FamilyID as the
// deterministic tie-break.
func topExecutionFamilies(input AnalysisInput) map[string]bool {
	n := len(input.Families)
	if n == 0 {
		return nil
	}
	type rank struct {
		familyID string
		count    uint64
	}
	ranks := make([]rank, n)
	for i := range input.Families {
		ranks[i] = rank{familyID: input.Families[i].FamilyID, count: input.Families[i].Stats.ExecutionCount}
	}
	sort.Slice(ranks, func(i, j int) bool {
		if ranks[i].count != ranks[j].count {
			return ranks[i].count > ranks[j].count
		}
		return ranks[i].familyID < ranks[j].familyID
	})

	topN := int(math.Ceil(float64(n) * attributionGapTopExecutionFraction))
	if topN < 1 {
		topN = 1
	}
	top := make(map[string]bool, topN)
	for i := 0; i < topN && i < n; i++ {
		top[ranks[i].familyID] = true
	}
	return top
}
