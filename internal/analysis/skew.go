package analysis

import (
	"context"
	"fmt"

	"github.com/coltconsulting/click-dog/internal/model"
)

const (
	// skewMinFamilyExecutions gates skew findings — dominance ratios over a
	// handful of executions are meaningless.
	skewMinFamilyExecutions = 20

	// skewDominanceRatioThreshold is the share of family executions one
	// dimension value must reach to be flagged.
	skewDominanceRatioThreshold = 0.80
)

// skewDimensions is the fixed dimension order. It must stay aligned with the
// dimension names the input bridge stores in FamilyDimensionBreakdown.Values.
var skewDimensions = []struct {
	name  string
	label string
}{
	{"user", "user"},
	{"client_name", "client"},
	{"client_hostname", "client host"},
}

// skewAnalyzer flags families dominated by one user, client, or host. One
// finding per family and dimension; the dimension name is the ID scope so a
// family skewed on user and host yields two distinct stable IDs.
type skewAnalyzer struct{}

func (a *skewAnalyzer) Name() string { return "skew" }

func (a *skewAnalyzer) Analyze(_ context.Context, input AnalysisInput) ([]Finding, error) {
	var findings []Finding

	for i := range input.Families {
		family := &input.Families[i]
		if family.Stats.ExecutionCount < skewMinFamilyExecutions {
			continue
		}
		breakdown, ok := input.DimensionsByFamily[family.FamilyID]
		if !ok {
			continue
		}

		for _, dim := range skewDimensions {
			hits := breakdown.Values[dim.name]
			if len(hits) == 0 {
				continue
			}
			top := hits[0]
			// Threshold comparisons use the stored clamped ratio so JSON
			// output and analyzer behavior agree.
			if top.Ratio < skewDominanceRatioThreshold {
				continue
			}

			severity := SeverityInfo
			if exceedsResourceFloor(family.Stats) {
				severity = SeverityWarning
			}

			findings = append(findings, Finding{
				ID:                    FindingID(a.Name(), dim.name, family.FamilyID, input.Window.Start, family.MemberHashesSorted),
				Analyzer:              a.Name(),
				ConditionScope:        dim.name,
				Severity:              severity,
				Confidence:            1.0,
				Title:                 fmt.Sprintf("One %s dominates query family executions", dim.label),
				Summary:               fmt.Sprintf("%s %q accounts for %.0f%% of %d executions in this window.", dim.label, top.Value, top.Ratio*100, family.Stats.ExecutionCount),
				FamilyID:              family.FamilyID,
				NormalizedQueryHashes: hashStrings(family.MemberHashesSorted),
				RepresentativeQuery:   family.RepresentativeQuery,
				Evidence: map[string]any{
					"dimension":              dim.name,
					"value":                  top.Value,
					"value_execution_count":  top.ExecutionCount,
					"family_execution_count": family.Stats.ExecutionCount,
					"ratio":                  roundRatio(top.Ratio),
				},
				Recommendation: "Verify ownership and workload routing: confirm the dominating " + dim.label + " is expected for this family. Concentration is not bad by itself.",
			})
		}
	}

	return findings, nil
}

// exceedsResourceFloor reports whether a family's raw rollup metrics exceed
// any resource-hog absolute floor. This intentionally reads only
// AnalysisInput — there is no cross-analyzer finding dependency.
func exceedsResourceFloor(stats model.QueryFamilyStats) bool {
	return stats.P95ReadRows >= resourceHogFloorP95ReadRows ||
		stats.P95ReadBytes >= resourceHogFloorP95ReadBytes ||
		float64(stats.MaxMemoryUsage) >= resourceHogFloorMaxMemory ||
		stats.P95DurationMs >= resourceHogFloorP95DurationMs
}
