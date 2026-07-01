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

	total := len(findings)
	if total > maxTableFindings {
		findings = findings[:maxTableFindings]
	}

	b.WriteString("\nFindings:\n")
	fmt.Fprintf(&b, "  %-8s %-16s %-18s %s\n", "SEV", "ANALYZER", "FAMILY", "SUMMARY")
	for _, f := range findings {
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

func tableFamilyColumn(familyID string) string {
	if familyID == "" {
		return "-"
	}
	if len(familyID) > 18 {
		return familyID[:15] + "..."
	}
	return familyID
}
