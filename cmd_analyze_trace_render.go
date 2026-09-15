package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/coltconsulting/click-dog/internal/analysis"
)

// renderTraceDrilldownOutcome renders whichever outcome the build produced: a
// full report when a single query was selected, or the bounded candidate list
// when the recent search matched more than one. Both render in the requested
// format and both exit 0.
func renderTraceDrilldownOutcome(outcome traceDrilldownOutcome, format string) (string, error) {
	if outcome.CandidateList != nil {
		return renderTraceCandidateList(*outcome.CandidateList, format)
	}
	if outcome.Report != nil {
		return renderTraceDrilldownReport(*outcome.Report, format)
	}
	// Defensive: a build always populates exactly one branch.
	return "", errors.New("empty drilldown outcome")
}

func renderTraceDrilldownReport(report analysis.TraceDrilldownReport, format string) (string, error) {
	if format == "json" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return "", err
		}
		return string(data) + "\n", nil
	}
	return renderTraceDrilldownTable(report), nil
}

// renderTraceCandidateList renders the bounded multi/zero-candidate result. The
// table view lists each candidate's identity and bounded metadata with a note
// to narrow the search; JSON carries the same fields under the candidate-list
// schema marker.
func renderTraceCandidateList(list traceCandidateList, format string) (string, error) {
	if format == "json" {
		data, err := json.MarshalIndent(list, "", "  ")
		if err != nil {
			return "", err
		}
		return string(data) + "\n", nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Trace drilldown candidates (source %s, %s..%s)\n",
		list.Source,
		list.Window.Start.UTC().Format(time.RFC3339),
		list.Window.End.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "  %s\n", list.Note)
	if len(list.Candidates) == 0 {
		b.WriteString("\nNo candidates matched.\n")
	} else {
		fmt.Fprintf(&b, "\n%d candidate(s):\n", len(list.Candidates))
		for _, c := range list.Candidates {
			fmt.Fprintf(&b, "  query_id=%s", c.QueryID)
			if c.QueryDurationMs > 0 {
				fmt.Fprintf(&b, " duration_ms=%d", c.QueryDurationMs)
			}
			if c.ElapsedMs > 0 {
				fmt.Fprintf(&b, " elapsed_ms=%d", c.ElapsedMs)
			}
			if !c.EventTime.IsZero() {
				fmt.Fprintf(&b, " event_time=%s", c.EventTime.UTC().Format(time.RFC3339))
			}
			if c.NormalizedQueryHash != 0 {
				fmt.Fprintf(&b, " normalized_query_hash=%d", c.NormalizedQueryHash)
			}
			if c.User != "" {
				fmt.Fprintf(&b, " user=%s", c.User)
			}
			b.WriteString("\n")
			if c.NormalizedQuery != "" {
				fmt.Fprintf(&b, "    %s\n", truncateOp(c.NormalizedQuery, 100))
			}
		}
	}
	if len(list.Warnings) > 0 {
		b.WriteString("\nWarnings:\n")
		for _, w := range list.Warnings {
			fmt.Fprintf(&b, "  warning: %s\n", w)
		}
	}
	return b.String(), nil
}

// renderTraceDrilldownTable renders the compact, uncolored human view: selected
// candidate identity, trace summary, query-log stats, then warnings. Output
// must stay friendly to logs and support bundles (no ANSI color).
func renderTraceDrilldownTable(report analysis.TraceDrilldownReport) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Trace drilldown (source %s, %s..%s)\n",
		report.Source,
		report.Window.Start.UTC().Format(time.RFC3339),
		report.Window.End.UTC().Format(time.RFC3339))

	if c := report.SelectedCandidate; c != nil {
		b.WriteString("\nSelected query:\n")
		if c.QueryID != "" {
			fmt.Fprintf(&b, "  query_id: %s\n", c.QueryID)
		}
		if c.QueryKind != "" {
			fmt.Fprintf(&b, "  kind: %s\n", c.QueryKind)
		}
		if !c.EventTime.IsZero() {
			fmt.Fprintf(&b, "  event_time: %s\n", c.EventTime.UTC().Format(time.RFC3339))
		}
		if c.QueryDurationMs > 0 {
			fmt.Fprintf(&b, "  query_duration_ms: %d\n", c.QueryDurationMs)
		}
		if c.ElapsedMs > 0 {
			fmt.Fprintf(&b, "  elapsed_ms: %d\n", c.ElapsedMs)
		}
		if c.NormalizedQueryHash != 0 {
			fmt.Fprintf(&b, "  normalized_query_hash: %d\n", c.NormalizedQueryHash)
		}
		if c.NormalizedQuery != "" {
			fmt.Fprintf(&b, "  normalized_query: %s\n", c.NormalizedQuery)
		}
		if c.User != "" {
			fmt.Fprintf(&b, "  user: %s\n", c.User)
		}
	}

	if t := report.Trace; t != nil {
		b.WriteString("\nTrace:\n")
		fmt.Fprintf(&b, "  trace_ids: %s\n", strings.Join(t.TraceIDs, ", "))
		fmt.Fprintf(&b, "  span_count: %d\n", t.SpanCount)
		if !t.EarliestStart.IsZero() {
			fmt.Fprintf(&b, "  time_range: %s..%s\n",
				t.EarliestStart.UTC().Format(time.RFC3339),
				t.LatestFinish.UTC().Format(time.RFC3339))
		}
		if t.RootOperation != "" {
			fmt.Fprintf(&b, "  root_operation: %s\n", t.RootOperation)
		}
		if len(t.SlowestSpans) > 0 {
			b.WriteString("  slowest spans:\n")
			for _, s := range t.SlowestSpans {
				parent := s.ParentSpanID
				if parent == "" {
					parent = "-"
				}
				fmt.Fprintf(&b, "    %8dms  %-28s span=%s parent=%s\n",
					s.DurationMs, truncateOp(s.OperationName, 28), s.SpanID, parent)
			}
		}
	} else {
		b.WriteString("\nTrace: no spans available\n")
	}

	if q := report.QueryLog; q != nil {
		b.WriteString("\nQuery log:\n")
		fmt.Fprintf(&b, "  duration_ms: %d\n", q.QueryDurationMs)
		fmt.Fprintf(&b, "  read_rows: %d  read_bytes: %d\n", q.ReadRows, q.ReadBytes)
		fmt.Fprintf(&b, "  result_rows: %d  memory_usage: %d\n", q.ResultRows, q.MemoryUsage)
		if q.ExceptionCode != 0 {
			fmt.Fprintf(&b, "  exception_code: %d\n", q.ExceptionCode)
		}
		if len(q.Tables) > 0 {
			fmt.Fprintf(&b, "  tables: %s\n", strings.Join(q.Tables, ", "))
		}
		if q.User != "" {
			fmt.Fprintf(&b, "  user: %s  client_name: %s\n", q.User, q.ClientName)
		}
	}

	if f := report.Family; f != nil {
		b.WriteString("\nSimilar family:\n")
		if f.FamilyID != "" {
			fmt.Fprintf(&b, "  family_id: %s\n", f.FamilyID)
		}
		if len(f.MemberHashes) > 0 {
			fmt.Fprintf(&b, "  member_hashes: %s\n", strings.Join(f.MemberHashes, ", "))
		}
		if f.RepresentativeQuery != "" {
			fmt.Fprintf(&b, "  representative_query: %s\n", f.RepresentativeQuery)
		}
	}

	if len(report.Findings) > 0 {
		fmt.Fprintf(&b, "\nFindings (%d):\n", len(report.Findings))
		shown := report.Findings
		if len(shown) > traceFindingsTableLimit {
			shown = shown[:traceFindingsTableLimit]
		}
		for _, f := range shown {
			fmt.Fprintf(&b, "  [%s] %s: %s\n", f.Severity, f.ID, truncateOp(f.Title, 80))
		}
		if len(report.Findings) > traceFindingsTableLimit {
			fmt.Fprintf(&b, "  (showing %d of %d findings; use --format json for the full report)\n",
				traceFindingsTableLimit, len(report.Findings))
		}
	}

	if len(report.Warnings) > 0 {
		b.WriteString("\nWarnings:\n")
		for _, w := range report.Warnings {
			fmt.Fprintf(&b, "  warning: %s\n", w)
		}
	}

	return b.String()
}

func truncateOp(s string, max int) string {
	if len(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
