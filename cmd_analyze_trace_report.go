package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/coltconsulting/click-dog/internal/analysis"
	"github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
)

// buildTraceDrilldownReport runs the read-only drilldown pipeline for a single
// selected query:
//
//  1. resolve trace IDs (from -trace-id directly, or by query-id lookup);
//  2. fetch spans for those trace IDs without duration-based trace selection;
//  3. enrich with the query-log row for the selected query ID (non-fatal);
//  4. fan out to the similar query-family rollup when requested;
//  5. fan out to analysis findings filtered to the selected family when
//     requested (-fanout findings).
//
// Missing pieces degrade to warnings; only the required trace-id lookup
// failing returns an error that fails the command. resolvedPath is threaded
// through only for the findings fan-out's ReportConfig.
func buildTraceDrilldownReport(ctx context.Context, src traceDrilldownSource, cfg *config.Config, resolvedPath string, opts analyzeTraceOptions) (analysis.TraceDrilldownReport, error) {
	end := traceNowFunc()
	window := analysis.AnalysisWindow{Start: end.Add(-opts.Lookback), End: end}
	report := analysis.TraceDrilldownReport{
		SchemaVersion: analysis.TraceDrilldownSchemaVersion,
		GeneratedAt:   end,
		Window:        window,
		Source:        opts.Source,
	}
	if report.Source == "" {
		report.Source = "other"
	}
	report.SuppliedIdentity = &analysis.TraceIdentity{
		QueryID:             opts.QueryID,
		TraceID:             opts.TraceID,
		NormalizedQueryHash: opts.NormalizedQueryHash,
	}

	lookbackDays := analysisQueryLogLookbackDays(opts.Lookback)

	candidate := &analysis.QueryCandidate{Source: report.Source, QueryID: opts.QueryID}
	// Seed the candidate from the selected current-query row (if any) so the
	// running-query metadata (elapsed runtime, user/client/host, normalized
	// preview) is present in the report even when no query-log row materializes.
	// A later query-log row overlays this with the finished stats.
	if opts.currentCandidate != nil {
		seeded := candidateFromCurrentQuery(*opts.currentCandidate, opts.RedactDimensions)
		seeded.QueryID = opts.QueryID
		candidate = &seeded
	}

	// Step 1: resolve trace IDs.
	var traceIDs []string
	if opts.TraceID != "" {
		// Direct path: the operator named the exact trace. Skip the query-id
		// lookup entirely.
		traceIDs = []string{opts.TraceID}
	} else {
		// The query-id -> trace-id lookup is the one required query. For the
		// current source, -wait optionally polls (bounded by both -wait and the
		// command -timeout via ctx) for the trace to materialize; -wait 0s does a
		// single shot, identical to the recent/other path.
		ids, err := resolveTraceIDsWithWait(ctx, src, opts, lookbackDays)
		if err != nil {
			return analysis.TraceDrilldownReport{}, fmt.Errorf("trace-id lookup failed: %w", err)
		}
		traceIDs = ids
		if len(traceIDs) == 0 {
			warn := fmt.Sprintf("no trace IDs found for query_id %q in the span log within the lookback window", opts.QueryID)
			if opts.Source == "current" {
				warn += " (the query may still be running or its spans may not be flushed yet)"
			}
			report.Warnings = append(report.Warnings, warn)
		}
	}
	candidate.TraceIDs = traceIDs

	// Step 2: fetch spans for the resolved trace IDs (no duration selection).
	if len(traceIDs) > 0 {
		spans, err := src.FetchSpansForTraceIDs(ctx, traceIDs, lookbackDays, opts.SpanLimit, cfg.Filters.BlacklistOperations)
		if err != nil {
			return analysis.TraceDrilldownReport{}, fmt.Errorf("span fetch failed: %w", err)
		}
		if len(spans) == 0 {
			report.Warnings = append(report.Warnings,
				"trace IDs resolved but no spans were returned (they may be outside the lookback window or fully blacklisted)")
		} else {
			report.Trace = summarizeTrace(traceIDs, spans)
		}
	}

	// Step 3: query-log enrichment / stats fan-out for the selected query ID.
	// Stats is on by default (-fanout trace,stats); turning it off skips the
	// query-log section but the candidate is still enriched for the similar
	// fan-out and identity block.
	if opts.QueryID != "" {
		queryLogMap, err := src.FetchQueryLogByQueryIDs(ctx, []string{opts.QueryID}, lookbackDays)
		if err != nil {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("query_log enrichment unavailable: %v", err))
		} else if ql, ok := queryLogMap[opts.QueryID]; ok {
			if opts.Fanout.Stats {
				report.QueryLog = summarizeQueryLog(ql, opts.RedactDimensions)
			}
			applyQueryLogToCandidate(candidate, ql, opts.RedactDimensions)
		} else {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("no query_log row found for query_id %q within the lookback window", opts.QueryID))
		}
	}

	report.SelectedCandidate = candidate

	// Step 4: similar query-family fan-out. Unsupported rollups or a missing
	// normalized hash degrade to warnings rather than failing the command.
	if opts.Fanout.Similar {
		family, warns := buildSimilarFamily(ctx, src, candidate.NormalizedQueryHash, window)
		report.Family = family
		report.Warnings = append(report.Warnings, warns...)
	}

	// Step 5: findings fan-out. Reuse the `analyze queries` analysis pipeline
	// (buildAnalysisInput + analysis.NewRegistry()) over this command's lookback
	// window, then filter the produced findings to the selected query's family.
	// Missing normalized hash, unsupported rollups, an analysis-input failure, or
	// zero matching findings all degrade to a warning and an empty findings
	// section — findings fan-out must never turn a produced report into a failure.
	if opts.Fanout.Findings {
		findings, warns := buildTraceFindings(ctx, src, cfg, resolvedPath, opts, window, candidate.NormalizedQueryHash, report.Family)
		report.Findings = findings
		report.Warnings = append(report.Warnings, warns...)
	}

	return report, nil
}

// buildTraceFindings runs the `analyze queries` analysis inline over the
// command's lookback window and filters the produced findings to the selected
// query's family. It deliberately reuses, rather than forks, the shared path:
//
//   - buildAnalysisInput (cmd_analyze.go) builds the identical AnalysisInput
//     `analyze queries` uses — query-family rollups, the bounded span sample +
//     query-id coverage, attribution, and dimension counts;
//   - analysis.NewRegistry() is the same compiled-in analyzer registry, run via
//     the same reg.Run, so no trace-specific analyzer or parallel analysis path
//     exists.
//
// Findings are matched to the selected family by family id (the rollup the
// `similar` path already resolved) when available, and otherwise by the
// selected query's normalized_query_hash against each finding's
// NormalizedQueryHashes. The report is strictly family-scoped: window-scoped
// coverage/prerequisite findings (no family id, no member hashes) are excluded,
// and the rollup/coverage state they describe is surfaced as report warnings
// instead. Everything degrades to a warning: a zero hash, an analysis-input
// error, or zero matches yields an empty findings section, never a failure.
func buildTraceFindings(ctx context.Context, src traceDrilldownSource, cfg *config.Config, resolvedPath string, opts analyzeTraceOptions, window analysis.AnalysisWindow, normalizedHash uint64, family *analysis.FamilySummary) ([]analysis.Finding, []string) {
	if normalizedHash == 0 {
		if !src.QueryLogNormalizedSupported() {
			return nil, []string{"findings fan-out skipped: normalized_query_hash is not supported on this ClickHouse"}
		}
		return nil, []string{"findings fan-out skipped: the selected query has no normalized_query_hash to match findings against"}
	}

	// Reuse the exact AnalysisInput build path `analyze queries` uses. The
	// command's lookback/timeout/redaction map onto the analyze-queries options;
	// the remaining knobs take the analyze-queries defaults so the inline run is
	// the same analysis, just scoped to this command's window.
	queriesOpts := analyzeQueriesOptions{
		Lookback:           opts.Lookback,
		Timeout:            opts.Timeout,
		RedactDimensions:   opts.RedactDimensions,
		MinExecutions:      3,
		FamilyLimit:        200,
		QueryPreviewLength: traceNormalizedQueryPreviewLength,
	}
	input, err := buildAnalysisInput(ctx, src, cfg, resolvedPath, queriesOpts, window)
	if err != nil {
		// Non-fatal: the rest of the drilldown report is still useful.
		return nil, []string{fmt.Sprintf("findings fan-out unavailable: %v", err)}
	}

	// Run the SAME compiled-in registry as `analyze queries`. No trace-specific
	// analyzer is introduced.
	findings, _ := analysis.NewRegistry().Run(ctx, input)

	matched := filterFindingsToFamily(findings, normalizedHash, family)
	analysis.SortFindings(matched)
	if len(matched) == 0 {
		return nil, []string{fmt.Sprintf("findings fan-out: no findings matched the selected family (normalized_query_hash %d) in the window", normalizedHash)}
	}
	return matched, nil
}

// filterFindingsToFamily keeps only the findings that describe the selected
// query's family. A finding matches when its resolved family id equals the
// selected family's id, OR when its NormalizedQueryHashes contain the selected
// query's hash (the family-id fallback for findings the rollup grouped under a
// different representative). The report is strictly family-scoped: window-scoped
// coverage/prerequisite findings (no family id, no member hashes) are excluded —
// the rollup/coverage state they describe is already surfaced as report warnings.
func filterFindingsToFamily(findings []analysis.Finding, normalizedHash uint64, family *analysis.FamilySummary) []analysis.Finding {
	hashStr := strconv.FormatUint(normalizedHash, 10)
	var familyID string
	if family != nil {
		familyID = family.FamilyID
	}

	matched := make([]analysis.Finding, 0, len(findings))
	for _, f := range findings {
		if familyID != "" && f.FamilyID == familyID {
			matched = append(matched, f)
			continue
		}
		if findingHasHash(f, hashStr) {
			matched = append(matched, f)
		}
	}
	return matched
}

// findingHasHash reports whether a finding's normalized-query-hash set contains
// the selected query's hash (decimal string, the same encoding findings use).
func findingHasHash(f analysis.Finding, hashStr string) bool {
	for _, h := range f.NormalizedQueryHashes {
		if h == hashStr {
			return true
		}
	}
	return false
}

// resolveTraceIDsWithWait runs the query-id -> trace-id lookup, optionally
// polling until the trace materializes. The poll is bounded by BOTH -wait and
// the command -timeout (carried on ctx): it stops at whichever deadline is
// nearer, and it stops immediately when ctx is canceled. The cadence is the
// fixed traceWaitPollInterval. With -wait 0s it does exactly one fetch (no
// polling), identical to the recent/other path; that one-shot result is
// returned even if empty. A non-empty result returns as soon as it appears, so
// a fast-materializing trace is reported without waiting out the full -wait.
//
// The initial (one-shot) lookup error is returned so the caller fails the
// required lookup exactly as the recent/other path does. During the wait poll,
// a context-cancellation error from a fetch (the -timeout firing mid-poll) is a
// clean stop — expected when waiting — and degrades to a missing-trace warning;
// any other poll error still propagates as a required-lookup failure.
func resolveTraceIDsWithWait(ctx context.Context, src traceDrilldownSource, opts analyzeTraceOptions, lookbackDays int) ([]string, error) {
	ids, err := src.FetchTraceIDsByQueryID(ctx, opts.QueryID, lookbackDays, opts.SpanLimit)
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 || opts.Wait <= 0 {
		return ids, nil
	}

	// Bound the poll by -wait. Each recheck is scheduled at
	// min(traceWaitPollInterval, time left until the -wait deadline), so a wait
	// shorter than the poll interval still gets a lookup inside its window rather
	// than overshooting to the first full tick. ctx carries the -timeout
	// deadline, so the select on ctx.Done() never overruns the command timeout.
	waitDeadline := time.Now().Add(opts.Wait)
	for {
		remaining := time.Until(waitDeadline)
		if remaining <= 0 {
			// -wait elapsed without the trace materializing: stop polling.
			return ids, nil
		}
		sleep := traceWaitPollInterval
		if remaining < sleep {
			sleep = remaining
		}
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			// -timeout (or an outer cancellation) reached: return what we have
			// (empty) so the caller degrades to a missing-trace warning, not a
			// failure. The drilldown is best-effort for running queries.
			timer.Stop()
			return ids, nil
		case <-timer.C:
		}
		polled, perr := src.FetchTraceIDsByQueryID(ctx, opts.QueryID, lookbackDays, opts.SpanLimit)
		if perr != nil {
			// A ctx-cancellation mid-poll is the expected -timeout-bounded stop:
			// degrade to the last-known (empty) result rather than fail.
			if ctx.Err() != nil {
				return ids, nil
			}
			return nil, perr
		}
		ids = polled
		if len(ids) > 0 {
			return ids, nil
		}
	}
}

// buildSimilarFamily resolves the query-family rollup that contains the
// selected normalized query hash. It reuses FetchQueryFamilyRollups (the same
// rollup path as analyze queries) and finds the family whose member hashes
// include the selected hash. Unsupported rollups, missing normalized support,
// a zero hash, or no containing family all degrade to a warning and a nil
// family — never a command failure.
func buildSimilarFamily(ctx context.Context, src traceDrilldownSource, normalizedHash uint64, window analysis.AnalysisWindow) (*analysis.FamilySummary, []string) {
	if normalizedHash == 0 {
		if !src.QueryLogNormalizedSupported() {
			return nil, []string{"similar fan-out skipped: normalized_query_hash is not supported on this ClickHouse"}
		}
		return nil, []string{"similar fan-out skipped: the selected query has no normalized_query_hash"}
	}

	families, err := src.FetchQueryFamilyRollups(ctx, clickhouse.QueryFamilyRollupOptions{
		StartTime:        window.Start,
		EndTime:          window.End,
		MaxPreviewLength: traceNormalizedQueryPreviewLength,
	})
	switch {
	case errors.Is(err, clickhouse.ErrQueryFamilyRollupsUnsupported):
		return nil, []string{"similar fan-out skipped: query-family rollups are unsupported on this ClickHouse"}
	case err != nil:
		// Non-fatal: the rest of the report is still useful.
		return nil, []string{fmt.Sprintf("similar fan-out unavailable: %v", err)}
	}

	for i := range families {
		for _, h := range families[i].MemberHashesSorted {
			if h == normalizedHash {
				return familySummaryFromRollup(families[i]), nil
			}
		}
	}
	return nil, []string{fmt.Sprintf("similar fan-out: no query-family rollup contains normalized_query_hash %d in the window", normalizedHash)}
}

// familySummaryFromRollup maps a rollup to the bounded FamilySummary. The
// representative query is bounded with the same preview posture as the rest of
// the report; member hashes are rendered as decimal strings for stable JSON.
func familySummaryFromRollup(rollup model.QueryFamilyRollup) *analysis.FamilySummary {
	hashes := make([]string, 0, len(rollup.MemberHashesSorted))
	for _, h := range rollup.MemberHashesSorted {
		hashes = append(hashes, strconv.FormatUint(h, 10))
	}
	return &analysis.FamilySummary{
		FamilyID:            rollup.FamilyID,
		RepresentativeQuery: boundedPreview(rollup.RepresentativeQuery, traceNormalizedQueryPreviewLength),
		MemberHashes:        hashes,
	}
}

// summarizeTrace reduces the fetched spans to a bounded trace summary: the
// time range, the root operation (the span whose parent is absent from the
// fetched set), and the slowest spans by duration.
func summarizeTrace(traceIDs []string, spans []model.OpenTelemetrySpan) *analysis.TraceSummary {
	summary := &analysis.TraceSummary{
		TraceIDs:  sortedUniqueStrings(traceIDs),
		SpanCount: len(spans),
	}

	spanIDs := make(map[uint64]bool, len(spans))
	for i := range spans {
		spanIDs[spans[i].SpanID] = true
	}

	for i := range spans {
		s := spans[i]
		start := microsToTime(s.StartTimeUs)
		finish := microsToTime(s.FinishTimeUs)
		if summary.EarliestStart.IsZero() || start.Before(summary.EarliestStart) {
			summary.EarliestStart = start
		}
		if finish.After(summary.LatestFinish) {
			summary.LatestFinish = finish
		}
		// The root span is the one whose parent is not present in the fetched
		// span set (parent 0, or a parent that did not come back in this trace).
		if summary.RootOperation == "" && (s.ParentSpanID == 0 || !spanIDs[s.ParentSpanID]) {
			summary.RootOperation = s.OperationName
		}
	}

	briefs := make([]analysis.TraceSpanBrief, 0, len(spans))
	for i := range spans {
		s := spans[i]
		briefs = append(briefs, analysis.TraceSpanBrief{
			TraceID:       s.TraceID.String(),
			SpanID:        strconv.FormatUint(s.SpanID, 10),
			ParentSpanID:  parentSpanIDString(s.ParentSpanID),
			OperationName: s.OperationName,
			Kind:          s.Kind,
			DurationMs:    spanDurationMs(s),
			Hostname:      s.Hostname,
		})
	}
	sort.SliceStable(briefs, func(i, j int) bool {
		if briefs[i].DurationMs != briefs[j].DurationMs {
			return briefs[i].DurationMs > briefs[j].DurationMs
		}
		if briefs[i].OperationName != briefs[j].OperationName {
			return briefs[i].OperationName < briefs[j].OperationName
		}
		return briefs[i].SpanID < briefs[j].SpanID
	})
	if len(briefs) > traceSlowestSpanLimit {
		briefs = briefs[:traceSlowestSpanLimit]
	}
	summary.SlowestSpans = briefs
	return summary
}

// summarizeQueryLog maps a query-log row into the bounded report summary. Raw
// query text is never copied; only the normalized preview (truncated) is
// carried. Redaction blanks the user/client/host dimension values.
func summarizeQueryLog(ql model.QueryLog, redact bool) *analysis.QueryLogSummary {
	summary := &analysis.QueryLogSummary{
		QueryID:             ql.QueryID,
		QueryKind:           ql.QueryKind,
		EventTime:           ql.EventTime.UTC(),
		QueryDurationMs:     ql.QueryDurationMs,
		ReadRows:            ql.ReadRows,
		ReadBytes:           ql.ReadBytes,
		WrittenRows:         ql.WrittenRows,
		WrittenBytes:        ql.WrittenBytes,
		ResultRows:          ql.ResultRows,
		ResultBytes:         ql.ResultBytes,
		MemoryUsage:         ql.MemoryUsage,
		ExceptionCode:       ql.ExceptionCode,
		Databases:           ql.DatabasesVisited,
		Tables:              ql.TablesVisited,
		NormalizedQueryHash: ql.NormalizedQueryHash,
		NormalizedQuery:     boundedPreview(ql.NormalizedQuery, traceNormalizedQueryPreviewLength),
	}
	if redact {
		summary.User = redactValue(ql.User, "user")
		summary.ClientName = redactValue(ql.ClientName, "client_name")
		summary.ClientHostname = redactValue(ql.ClientHostname, "client_hostname")
		summary.ClientAddress = redactValue(ql.ClientAddress, "client_address")
	} else {
		summary.User = ql.User
		summary.ClientName = ql.ClientName
		summary.ClientHostname = ql.ClientHostname
		summary.ClientAddress = ql.ClientAddress
	}
	return summary
}

// applyQueryLogToCandidate copies the query-log metadata onto the selected
// candidate so the candidate identity block is populated even when search
// (later phases) did not provide it.
func applyQueryLogToCandidate(c *analysis.QueryCandidate, ql model.QueryLog, redact bool) {
	c.EventTime = ql.EventTime.UTC()
	c.QueryDurationMs = ql.QueryDurationMs
	c.QueryKind = ql.QueryKind
	c.NormalizedQueryHash = ql.NormalizedQueryHash
	c.NormalizedQuery = boundedPreview(ql.NormalizedQuery, traceNormalizedQueryPreviewLength)
	if redact {
		c.User = redactValue(ql.User, "user")
		c.ClientName = redactValue(ql.ClientName, "client_name")
		c.ClientHostname = redactValue(ql.ClientHostname, "client_hostname")
		c.ClientAddress = redactValue(ql.ClientAddress, "client_address")
	} else {
		c.User = ql.User
		c.ClientName = ql.ClientName
		c.ClientHostname = ql.ClientHostname
		c.ClientAddress = ql.ClientAddress
	}
}

// redactValue blanks a non-empty dimension value to a stable placeholder. The
// posture matches analyze queries: raw values are omitted, presence is kept.
func redactValue(value, dimension string) string {
	if value == "" {
		return ""
	}
	return "redacted_" + dimension
}

// boundedPreview truncates a (already normalized) preview string to at most max
// runes, appending an ellipsis marker when truncated. It never returns raw
// query text — callers pass normalized previews only.
func boundedPreview(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func sortedUniqueStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func microsToTime(us uint64) time.Time {
	if us == 0 {
		return time.Time{}
	}
	return time.UnixMicro(int64(us)).UTC()
}

func spanDurationMs(s model.OpenTelemetrySpan) uint64 {
	if s.FinishTimeUs <= s.StartTimeUs {
		return 0
	}
	return (s.FinishTimeUs - s.StartTimeUs) / 1000
}

func parentSpanIDString(id uint64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatUint(id, 10)
}
