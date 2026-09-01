package analysis

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/coltconsulting/click-dog/internal/model"
)

const (
	regressionMinExecutions      = 20
	regressionMinCurrentCoverage = 0.80

	latencyRegressionRatioThreshold = 2.0
	latencyRegressionAbsoluteFloor  = 250.0

	failureSpikeMinFailures              = 3
	failureSpikeRatioThreshold           = 3.0
	failureSpikePercentagePointThreshold = 5.0
	failureSpikeZeroBaselineRate         = 0.10
	failureSpikeCriticalRate             = 0.15
)

type regressionMetrics struct {
	hashes                   []uint64
	currentExecutions        uint64
	baselineExecutions       uint64
	currentSuccessful        uint64
	currentFailed            uint64
	baselineSuccessful       uint64
	baselineFailed           uint64
	currentP95               float64
	currentP99               float64
	baselineP95              float64
	baselineP99              float64
	currentTopExceptions     []BaselineExceptionCount
	baselineTopExceptions    []BaselineExceptionCount
	currentExecutionCoverage float64
}

type latencyRegressionAnalyzer struct{}

func (a *latencyRegressionAnalyzer) Name() string { return "latency_regression" }

func (a *latencyRegressionAnalyzer) Analyze(_ context.Context, input AnalysisInput) ([]Finding, error) {
	if input.Comparison == nil || !input.Comparison.Usable() {
		return nil, nil
	}
	var findings []Finding
	for _, family := range input.Families {
		metrics := matchedRegressionMetrics(family, input.Comparison)
		if !metrics.eligibleVolumeAndCoverage() {
			continue
		}

		type signal struct {
			name     string
			baseline float64
			current  float64
			ratio    float64
			delta    float64
		}
		var strongest *signal
		for _, candidate := range []signal{
			{name: "p95_latency", baseline: metrics.baselineP95, current: metrics.currentP95},
			{name: "p99_latency", baseline: metrics.baselineP99, current: metrics.currentP99},
		} {
			if candidate.baseline <= 0 {
				continue
			}
			candidate.ratio = candidate.current / candidate.baseline
			candidate.delta = candidate.current - candidate.baseline
			if candidate.ratio < latencyRegressionRatioThreshold || candidate.delta < latencyRegressionAbsoluteFloor {
				continue
			}
			if strongest == nil || candidate.ratio > strongest.ratio {
				copy := candidate
				strongest = &copy
			}
		}
		if strongest == nil {
			continue
		}

		findings = append(findings, Finding{
			ID:                    FindingID(a.Name(), "family", family.FamilyID, input.Window.Start, metrics.hashes),
			Analyzer:              a.Name(),
			ConditionScope:        "family",
			Severity:              SeverityWarning,
			Confidence:            1.0,
			Title:                 fmt.Sprintf("Query family %s regressed %.1fx", strongest.name, strongest.ratio),
			Summary:               fmt.Sprintf("%s increased from %.0fms in the baseline to %.0fms in the current window (%.1fx, +%.0fms).", strongest.name, strongest.baseline, strongest.current, strongest.ratio, strongest.delta),
			FamilyID:              family.FamilyID,
			NormalizedQueryHashes: hashStrings(metrics.hashes),
			RepresentativeQuery:   family.RepresentativeQuery,
			Evidence: map[string]any{
				"signal":                             strongest.name,
				"baseline_duration_ms":               roundRatio(strongest.baseline),
				"current_duration_ms":                roundRatio(strongest.current),
				"duration_ratio":                     roundRatio(strongest.ratio),
				"absolute_delta_ms":                  roundRatio(strongest.delta),
				"baseline_execution_count":           metrics.baselineExecutions,
				"current_execution_count":            metrics.currentExecutions,
				"matched_hash_count":                 len(metrics.hashes),
				"matched_current_execution_coverage": roundRatio(metrics.currentExecutionCoverage),
				"ratio_threshold":                    latencyRegressionRatioThreshold,
				"absolute_delta_threshold_ms":        latencyRegressionAbsoluteFloor,
				"minimum_execution_count":            regressionMinExecutions,
			},
			Recommendation: regressionDrilldownRecommendation(metrics.hashes),
		})
	}
	return findings, nil
}

type failureSpikeAnalyzer struct{}

func (a *failureSpikeAnalyzer) Name() string { return "failure_spike" }

func (a *failureSpikeAnalyzer) Analyze(_ context.Context, input AnalysisInput) ([]Finding, error) {
	if input.Comparison == nil || !input.Comparison.Usable() {
		return nil, nil
	}
	var findings []Finding
	for _, family := range input.Families {
		metrics := matchedRegressionMetrics(family, input.Comparison)
		if !metrics.eligibleVolumeAndCoverage() || metrics.currentFailed < failureSpikeMinFailures || len(metrics.currentTopExceptions) == 0 {
			continue
		}
		baselineRate := ratio(metrics.baselineFailed, metrics.baselineSuccessful+metrics.baselineFailed)
		currentRate := ratio(metrics.currentFailed, metrics.currentSuccessful+metrics.currentFailed)
		deltaPP := (currentRate - baselineRate) * 100
		if deltaPP < failureSpikePercentagePointThreshold {
			continue
		}
		var rateRatio float64
		if baselineRate == 0 {
			if currentRate < failureSpikeZeroBaselineRate {
				continue
			}
		} else {
			rateRatio = currentRate / baselineRate
			if rateRatio < failureSpikeRatioThreshold {
				continue
			}
		}

		severity := SeverityWarning
		if currentRate >= failureSpikeCriticalRate || deltaPP >= 10 {
			severity = SeverityCritical
		}
		evidence := map[string]any{
			"signal":                               "failure_rate",
			"baseline_execution_count":             metrics.baselineExecutions,
			"current_execution_count":              metrics.currentExecutions,
			"baseline_failed_count":                metrics.baselineFailed,
			"current_failed_count":                 metrics.currentFailed,
			"baseline_failure_rate":                roundRatio(baselineRate),
			"current_failure_rate":                 roundRatio(currentRate),
			"failure_rate_delta_percentage_points": roundRatio(deltaPP),
			"minimum_failure_count":                failureSpikeMinFailures,
			"ratio_threshold":                      failureSpikeRatioThreshold,
			"percentage_point_threshold":           failureSpikePercentagePointThreshold,
			"matched_hash_count":                   len(metrics.hashes),
			"matched_current_execution_coverage":   roundRatio(metrics.currentExecutionCoverage),
			"current_top_exception_codes":          exceptionCodes(metrics.currentTopExceptions),
			"current_top_exception_counts":         exceptionCounts(metrics.currentTopExceptions),
			"baseline_top_exception_codes":         exceptionCodes(metrics.baselineTopExceptions),
			"baseline_top_exception_counts":        exceptionCounts(metrics.baselineTopExceptions),
		}
		if baselineRate == 0 {
			evidence["baseline_rate_was_zero"] = true
			evidence["zero_baseline_current_rate_threshold"] = failureSpikeZeroBaselineRate
		} else {
			evidence["failure_rate_ratio"] = roundRatio(rateRatio)
		}

		findings = append(findings, Finding{
			ID:                    FindingID(a.Name(), "family", family.FamilyID, input.Window.Start, metrics.hashes),
			Analyzer:              a.Name(),
			ConditionScope:        "family",
			Severity:              severity,
			Confidence:            1.0,
			Title:                 "Query family failure rate spiked",
			Summary:               fmt.Sprintf("Failure rate increased from %.1f%% in the baseline to %.1f%% in the current window (+%.1f percentage points; %d failures).", baselineRate*100, currentRate*100, deltaPP, metrics.currentFailed),
			FamilyID:              family.FamilyID,
			NormalizedQueryHashes: hashStrings(metrics.hashes),
			RepresentativeQuery:   family.RepresentativeQuery,
			Evidence:              evidence,
			Recommendation:        regressionDrilldownRecommendation(metrics.hashes),
		})
	}
	return findings, nil
}

func matchedRegressionMetrics(family model.QueryFamilyRollup, comparison *BaselineComparison) regressionMetrics {
	metrics := regressionMetrics{}
	currentExceptions := make(map[int32]uint64)
	baselineExceptionsByCode := make(map[int32]uint64)
	for _, member := range family.Members {
		baseline, ok := comparison.baselineByHash[member.NormalizedQueryHash]
		if !ok {
			continue
		}
		metrics.hashes = append(metrics.hashes, member.NormalizedQueryHash)
		successful, failed := normalizedOutcomeCounts(member.ExecutionCount, member.SuccessfulCount, member.FailedCount)
		metrics.currentExecutions += member.ExecutionCount
		metrics.currentSuccessful += successful
		metrics.currentFailed += failed
		metrics.currentP95 = math.Max(metrics.currentP95, member.P95DurationMs)
		metrics.currentP99 = math.Max(metrics.currentP99, member.P99DurationMs)
		metrics.baselineExecutions += baseline.ExecutionCount
		metrics.baselineSuccessful += baseline.SuccessfulCount
		metrics.baselineFailed += baseline.FailedCount
		metrics.baselineP95 = math.Max(metrics.baselineP95, baseline.P95DurationMs)
		metrics.baselineP99 = math.Max(metrics.baselineP99, baseline.P99DurationMs)
		for _, exception := range member.TopExceptions {
			currentExceptions[exception.Code] += exception.Count
		}
		for _, exception := range baseline.TopExceptions {
			baselineExceptionsByCode[exception.Code] += exception.Count
		}
	}
	sort.Slice(metrics.hashes, func(i, j int) bool { return metrics.hashes[i] < metrics.hashes[j] })
	if family.Stats.ExecutionCount > 0 {
		metrics.currentExecutionCoverage = float64(metrics.currentExecutions) / float64(family.Stats.ExecutionCount)
		if metrics.currentExecutionCoverage > 1 {
			metrics.currentExecutionCoverage = 1
		}
	}
	metrics.currentTopExceptions = sortedExceptionCounts(currentExceptions)
	metrics.baselineTopExceptions = sortedExceptionCounts(baselineExceptionsByCode)
	return metrics
}

func (m regressionMetrics) eligibleVolumeAndCoverage() bool {
	return len(m.hashes) > 0 &&
		m.currentExecutions >= regressionMinExecutions &&
		m.baselineExecutions >= regressionMinExecutions &&
		m.currentExecutionCoverage >= regressionMinCurrentCoverage
}

func sortedExceptionCounts(counts map[int32]uint64) []BaselineExceptionCount {
	out := make([]BaselineExceptionCount, 0, len(counts))
	for code, count := range counts {
		if count > 0 {
			out = append(out, BaselineExceptionCount{Code: code, Count: count})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Code < out[j].Code
	})
	if len(out) > MaxBaselineExceptionCodes {
		out = out[:MaxBaselineExceptionCodes]
	}
	return out
}

func exceptionCodes(exceptions []BaselineExceptionCount) []int {
	out := make([]int, len(exceptions))
	for i, exception := range exceptions {
		out[i] = int(exception.Code)
	}
	return out
}

func exceptionCounts(exceptions []BaselineExceptionCount) []uint64 {
	out := make([]uint64, len(exceptions))
	for i, exception := range exceptions {
		out[i] = exception.Count
	}
	return out
}

func regressionDrilldownRecommendation(hashes []uint64) string {
	if len(hashes) == 0 {
		return "Inspect the current query family and compare deployment or workload changes against the known-good window."
	}
	return fmt.Sprintf("Run click-dog analyze trace -normalized-query-hash %d, then compare deployment, exception, and workload changes against the known-good window.", hashes[0])
}

func ratio(numerator, denominator uint64) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}
