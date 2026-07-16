package analysis

import (
	"context"
	"fmt"
	"sort"

	"github.com/coltconsulting/click-dog/internal/model"
)

const (
	// resourceHogMinPeerFamilies is the minimum number of OTHER families with
	// a non-zero value for a metric before that metric is scored — a median
	// over fewer peers is too noisy to call anything an outlier.
	resourceHogMinPeerFamilies = 5

	// resourceHogCriticalRatio escalates a finding to critical when any
	// tripped metric is at least this many times the peer median.
	resourceHogCriticalRatio = 20.0

	// Absolute floors keep tiny relative outliers from becoming findings.
	resourceHogFloorP95ReadRows   = 1_000_000.0
	resourceHogFloorP95ReadBytes  = 128.0 * 1024 * 1024
	resourceHogFloorMaxMemory     = 512.0 * 1024 * 1024
	resourceHogFloorP95DurationMs = 1000.0

	// Ratio thresholds against the peer median.
	resourceHogRatioP95ReadRows   = 5.0
	resourceHogRatioP95ReadBytes  = 5.0
	resourceHogRatioMaxMemory     = 3.0
	resourceHogRatioP95DurationMs = 3.0
)

// resourceHogMetric describes one scored rollup metric.
type resourceHogMetric struct {
	key   string
	label string
	floor float64
	ratio float64
	value func(model.QueryFamilyStats) float64
}

// resourceHogMetrics is the fixed metric set, in stable evidence order.
var resourceHogMetrics = []resourceHogMetric{
	{
		key:   "p95_read_rows",
		label: "p95 read_rows",
		floor: resourceHogFloorP95ReadRows,
		ratio: resourceHogRatioP95ReadRows,
		value: func(s model.QueryFamilyStats) float64 { return s.P95ReadRows },
	},
	{
		key:   "p95_read_bytes",
		label: "p95 read_bytes",
		floor: resourceHogFloorP95ReadBytes,
		ratio: resourceHogRatioP95ReadBytes,
		value: func(s model.QueryFamilyStats) float64 { return s.P95ReadBytes },
	},
	{
		key:   "max_memory_usage",
		label: "max memory_usage",
		floor: resourceHogFloorMaxMemory,
		ratio: resourceHogRatioMaxMemory,
		value: func(s model.QueryFamilyStats) float64 { return float64(s.MaxMemoryUsage) },
	},
	{
		key:   "p95_duration_ms",
		label: "p95 duration",
		floor: resourceHogFloorP95DurationMs,
		ratio: resourceHogRatioP95DurationMs,
		value: func(s model.QueryFamilyStats) float64 { return s.P95DurationMs },
	},
}

// resourceHogAnalyzer flags families whose resource profile is far above peer
// families in the same window. One finding per family; multiple tripped
// metrics share the finding with the title reflecting the strongest signal.
type resourceHogAnalyzer struct{}

func (a *resourceHogAnalyzer) Name() string { return "resource_hog" }

func (a *resourceHogAnalyzer) Analyze(_ context.Context, input AnalysisInput) ([]Finding, error) {
	var findings []Finding

	for i := range input.Families {
		family := &input.Families[i]
		if family.Stats.ExecutionCount < input.Config.MinExecutions {
			continue
		}

		evidence := map[string]any{
			"execution_count": family.Stats.ExecutionCount,
		}
		severity := SeverityWarning
		var strongest *trippedMetric
		tripped := 0

		for _, metric := range resourceHogMetrics {
			value := metric.value(family.Stats)
			peers := peerValues(input.Families, i, metric.value)
			if len(peers) < resourceHogMinPeerFamilies {
				continue
			}
			median := medianOf(peers)
			if value < metric.floor || median <= 0 || value < median*metric.ratio {
				continue
			}

			ratio := value / median
			evidence[metric.key] = value
			evidence["peer_median_"+metric.key] = median
			evidence[metric.key+"_ratio"] = roundRatio(ratio)
			tripped++
			if ratio >= resourceHogCriticalRatio {
				severity = SeverityCritical
			}
			if strongest == nil || ratio > strongest.ratio {
				strongest = &trippedMetric{metric: metric, ratio: ratio}
			}
		}

		if strongest == nil {
			continue
		}
		metricLabel := "metrics"
		if tripped == 1 {
			metricLabel = "metric"
		}

		findings = append(findings, Finding{
			ID:                    FindingID(a.Name(), "family", family.FamilyID, input.Window.Start, family.MemberHashesSorted),
			Analyzer:              a.Name(),
			Severity:              severity,
			Confidence:            1.0,
			Title:                 fmt.Sprintf("Query family %s is %.1fx peer median", strongest.metric.label, strongest.ratio),
			Summary:               fmt.Sprintf("%s is %.1fx the peer-family median in this window (%d %s above threshold).", strongest.metric.label, strongest.ratio, tripped, metricLabel),
			FamilyID:              family.FamilyID,
			NormalizedQueryHashes: hashStrings(family.MemberHashesSorted),
			RepresentativeQuery:   family.RepresentativeQuery,
			Evidence:              evidence,
			Recommendation:        "Inspect predicates, partition pruning, joins, and table access pattern for this family, and check whether it is expected batch/reporting workload.",
		})
	}

	return findings, nil
}

type trippedMetric struct {
	metric resourceHogMetric
	ratio  float64
}

// peerValues returns the non-zero metric values of every family except the
// candidate at index self.
func peerValues(families []model.QueryFamilyRollup, self int, value func(model.QueryFamilyStats) float64) []float64 {
	var peers []float64
	for i := range families {
		if i == self {
			continue
		}
		if v := value(families[i].Stats); v > 0 {
			peers = append(peers, v)
		}
	}
	return peers
}

// medianOf returns the median of values (mean of the middle pair for even
// counts). values is copied before sorting.
func medianOf(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}
