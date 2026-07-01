package analysis

import (
	"context"
	"fmt"
)

const (
	// coverageQueryIDRatioThreshold is the clickhouse.query_id coverage below
	// which a warning is emitted.
	coverageQueryIDRatioThreshold = 0.80

	// coverageQueryIDMinSampleSpans suppresses the query-id ratio warning on
	// small samples where the ratio is too noisy to act on.
	coverageQueryIDMinSampleSpans = 50
)

// coverageAnalyzer emits report-level prerequisite findings so an operator
// can tell why deeper analysis would be incomplete. This is the install /
// coverage QA half of the Phase 1 value wedge.
type coverageAnalyzer struct{}

func (a *coverageAnalyzer) Name() string { return "coverage" }

func (a *coverageAnalyzer) Analyze(_ context.Context, input AnalysisInput) ([]Finding, error) {
	var findings []Finding
	cov := input.Coverage

	if !cov.NormalizedQuerySupported {
		findings = append(findings, a.finding(input,
			"normalized_query_hash_unsupported",
			SeverityWarning,
			"normalized_query_hash is unavailable",
			"system.query_log.normalized_query_hash is not available, so queries cannot be grouped into normalized families.",
			map[string]any{
				"normalized_query_supported": false,
				"lookback":                   input.Config.Lookback,
			},
			"Run `click-dog check` to verify query_log capabilities. Query-family analysis requires a ClickHouse version that exposes normalized_query_hash on every replica.",
		))
	}

	if !cov.QueryFamilyRollupsSupported {
		findings = append(findings, a.finding(input,
			"rollups_unsupported",
			SeverityWarning,
			"Query family rollups are unsupported",
			"Query family rollups could not be computed for this window, so resource, attribution, and skew analysis are unavailable.",
			map[string]any{
				"query_family_rollups_supported": false,
				"lookback":                       input.Config.Lookback,
			},
			"Run `click-dog check` to verify normalized_query_hash support; rollups require it on every replica in cluster-query mode.",
		))
	}

	if cov.NormalizedQuerySupported && cov.QueryFamilyRollupsSupported && cov.QueryFamilyCount == 0 {
		findings = append(findings, a.finding(input,
			"no_query_families",
			SeverityInfo,
			"No query families matched this window",
			fmt.Sprintf("No query families with at least %d executions were found in the lookback window.", input.Config.MinExecutions),
			map[string]any{
				"query_family_count": cov.QueryFamilyCount,
				"min_executions":     input.Config.MinExecutions,
				"lookback":           input.Config.Lookback,
			},
			"Widen -lookback or lower -min-executions if query traffic is expected in this window.",
		))
	}

	if !cov.SpanSampleAvailable {
		findings = append(findings, a.finding(input,
			"span_sample",
			SeverityWarning,
			"Span sampling is unavailable",
			"No span sample could be fetched, so clickhouse.query_id coverage and log_comment attribution analysis were skipped.",
			map[string]any{
				"span_sample_available": false,
				"lookback":              input.Config.Lookback,
			},
			"Run `click-dog check` to verify system.opentelemetry_span_log readability and that OpenTelemetry span logging is enabled.",
		))
	}

	if cov.SpanSampleAvailable &&
		cov.SpanSampleSize >= coverageQueryIDMinSampleSpans &&
		cov.QueryIDRatio < coverageQueryIDRatioThreshold {
		findings = append(findings, a.finding(input,
			"query_id_ratio",
			SeverityWarning,
			"Low clickhouse.query_id span coverage",
			fmt.Sprintf("Only %.1f%% of %d sampled spans carry clickhouse.query_id, limiting query_log enrichment and attribution analysis.",
				cov.QueryIDRatio*100, cov.SpanSampleSize),
			map[string]any{
				"span_sample_size":    cov.SpanSampleSize,
				"spans_with_query_id": cov.SpansWithQueryID,
				"query_id_ratio":      roundRatio(cov.QueryIDRatio),
				"threshold":           coverageQueryIDRatioThreshold,
				"lookback":            input.Config.Lookback,
			},
			"Run `click-dog check` and verify OpenTelemetry trace propagation so ClickHouse records query_id on query spans.",
		))
	}

	return findings, nil
}

// finding builds a non-family coverage finding with the stable
// "coverage:<scope>" subject.
func (a *coverageAnalyzer) finding(input AnalysisInput, scope string, severity Severity, title, summary string, evidence map[string]any, recommendation string) Finding {
	subject := a.Name() + ":" + scope
	return Finding{
		ID:             FindingID(a.Name(), scope, subject, input.Window.Start, nil),
		Analyzer:       a.Name(),
		Severity:       severity,
		Confidence:     1.0,
		Title:          title,
		Summary:        summary,
		Evidence:       evidence,
		Recommendation: recommendation,
	}
}
