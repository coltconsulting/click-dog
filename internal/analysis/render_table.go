package analysis

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// maxTableFindings caps table output on noisy clusters. JSON includes all
// findings in Phase 1.
const maxTableFindings = 50

// RenderTable renders the dense human-readable report. No ANSI color —
// output must stay friendly to logs and support bundles.
func RenderTable(report AnalysisReport) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Query analysis (lookback %s, %s..%s)\n",
		report.Config.Lookback,
		report.Window.Start.UTC().Format(time.RFC3339),
		report.Window.End.UTC().Format(time.RFC3339))

	b.WriteString("\nCoverage:\n")
	if report.Coverage.NormalizedQuerySupported {
		b.WriteString("  normalized_query_hash: ok\n")
	} else {
		b.WriteString("  normalized_query_hash: unsupported\n")
	}
	if !report.Coverage.QueryFamilyRollupsSupported {
		b.WriteString("  query_family_rollups: unsupported\n")
	}
	fmt.Fprintf(&b, "  families analyzed: %d\n", report.Coverage.QueryFamilyCount)
	switch {
	case !report.Coverage.SpanSampleAvailable:
		b.WriteString("  span query_id coverage: unavailable\n")
	case report.Coverage.SpanSampleSize == 0:
		b.WriteString("  span query_id coverage: no spans sampled\n")
	default:
		fmt.Fprintf(&b, "  span query_id coverage: %.1f%% (%d/%d)\n",
			report.Coverage.QueryIDRatio*100,
			report.Coverage.SpansWithQueryID,
			report.Coverage.SpanSampleSize)
	}
	for _, w := range report.Coverage.Warnings {
		fmt.Fprintf(&b, "  warning: %s\n", w)
	}

	if report.Comparison != nil {
		renderComparisonSummary(&b, *report.Comparison)
	}

	if len(report.Findings) == 0 {
		b.WriteString("\nNo findings for this window.\n")
		return b.String()
	}

	// Table order differs from JSON order on purpose: humans scan summaries,
	// machines diff around family identity.
	findings := append([]Finding(nil), report.Findings...)
	sort.SliceStable(findings, func(i, j int) bool {
		a, c := findings[i], findings[j]
		if severityRank(a.Severity) != severityRank(c.Severity) {
			return severityRank(a.Severity) < severityRank(c.Severity)
		}
		if a.Analyzer != c.Analyzer {
			return a.Analyzer < c.Analyzer
		}
		return a.Summary < c.Summary
	})

	var regressions, ordinary []Finding
	for _, finding := range findings {
		if isRegressionAnalyzer(finding.Analyzer) && report.Comparison != nil {
			regressions = append(regressions, finding)
		} else {
			ordinary = append(ordinary, finding)
		}
	}
	total := len(findings)
	remaining := maxTableFindings
	if len(regressions) > remaining {
		regressions = regressions[:remaining]
	}
	remaining -= len(regressions)
	if len(ordinary) > remaining {
		ordinary = ordinary[:remaining]
	}

	if report.Comparison != nil {
		renderRegressionFindings(&b, regressions)
	}
	if len(ordinary) > 0 {
		b.WriteString("\nFindings:\n")
		fmt.Fprintf(&b, "  %-8s %-16s %-18s %s\n", "SEV", "ANALYZER", "FAMILY", "SUMMARY")
	}
	for _, f := range ordinary {
		fmt.Fprintf(&b, "  %-8s %-16s %-18s %s\n", f.Severity, f.Analyzer, tableFamilyColumn(f.FamilyID), f.Summary)
		if f.Recommendation != "" {
			fmt.Fprintf(&b, "  %-8s %s\n", "", f.Recommendation)
		}
	}
	if total > maxTableFindings {
		fmt.Fprintf(&b, "\n(showing %d of %d findings; use -format json for the full report)\n", maxTableFindings, total)
	}

	return b.String()
}

func renderComparisonSummary(b *strings.Builder, comparison ComparisonSummary) {
	b.WriteString("\nComparison:\n")
	fmt.Fprintf(b, "  status: %s\n", comparison.Status)
	fmt.Fprintf(b, "  baseline: %s (%s..%s)\n",
		comparison.BaselineID,
		comparison.BaselineWindow.Start.UTC().Format(time.RFC3339),
		comparison.BaselineWindow.End.UTC().Format(time.RFC3339))
	fmt.Fprintf(b, "  hashes: %d matched, %d current-only, %d baseline-only\n",
		comparison.Counts.MatchedHashes,
		comparison.Counts.CurrentOnlyHashes,
		comparison.Counts.BaselineOnlyHashes)
	fmt.Fprintf(b, "  families: %d matched, %d current-only, %d baseline-only\n",
		comparison.Counts.MatchedFamilies,
		comparison.Counts.CurrentOnlyFamilies,
		comparison.Counts.BaselineOnlyFamilies)
	for _, warning := range comparison.Warnings {
		fmt.Fprintf(b, "  warning: %s\n", warning)
	}
}

func renderRegressionFindings(b *strings.Builder, findings []Finding) {
	b.WriteString("\nRegressions:\n")
	if len(findings) == 0 {
		b.WriteString("  No regression findings.\n")
		return
	}
	fmt.Fprintf(b, "  %-8s %-18s %-14s %-12s %-12s %s\n", "SEV", "FAMILY", "SIGNAL", "BASELINE", "CURRENT", "CHANGE")
	for _, finding := range findings {
		signal, baseline, current, change := regressionTableValues(finding)
		fmt.Fprintf(b, "  %-8s %-18s %-14s %-12s %-12s %s\n",
			finding.Severity,
			tableFamilyColumn(finding.FamilyID),
			signal,
			baseline,
			current,
			change)
		if finding.Recommendation != "" {
			fmt.Fprintf(b, "  %-8s %s\n", "", finding.Recommendation)
		}
	}
}

func regressionTableValues(finding Finding) (string, string, string, string) {
	if finding.Analyzer == "failure_spike" {
		baseline := evidenceFloat(finding.Evidence, "baseline_failure_rate") * 100
		current := evidenceFloat(finding.Evidence, "current_failure_rate") * 100
		delta := evidenceFloat(finding.Evidence, "failure_rate_delta_percentage_points")
		return "failure rate", fmt.Sprintf("%.1f%%", baseline), fmt.Sprintf("%.1f%%", current), fmt.Sprintf("+%.1f pp", delta)
	}
	signal, _ := finding.Evidence["signal"].(string)
	baseline := evidenceFloat(finding.Evidence, "baseline_duration_ms")
	current := evidenceFloat(finding.Evidence, "current_duration_ms")
	ratio := evidenceFloat(finding.Evidence, "duration_ratio")
	return strings.ReplaceAll(signal, "_", " "), formatTableDuration(baseline), formatTableDuration(current), fmt.Sprintf("%.1fx", ratio)
}

func evidenceFloat(evidence map[string]any, key string) float64 {
	switch value := evidence[key].(type) {
	case float64:
		return value
	case float32:
		return float64(value)
	case int:
		return float64(value)
	case uint64:
		return float64(value)
	default:
		return 0
	}
}

func formatTableDuration(milliseconds float64) string {
	if milliseconds >= 1000 {
		return fmt.Sprintf("%.2fs", milliseconds/1000)
	}
	return fmt.Sprintf("%.0fms", milliseconds)
}

func isRegressionAnalyzer(name string) bool {
	return name == "latency_regression" || name == "failure_spike"
}

func tableFamilyColumn(familyID string) string {
	if familyID == "" {
		return "-"
	}
	if len(familyID) > 18 {
		return familyID[:15] + "..."
	}
	return familyID
}
