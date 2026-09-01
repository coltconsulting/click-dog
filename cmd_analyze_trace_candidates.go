package main

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/coltconsulting/click-dog/internal/analysis"
	"github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
)

// traceCandidateList is the bounded multi-candidate result rendered when the
// recent search matched more than one query and no identity flag selected one.
type traceCandidateList struct {
	SchemaVersion string                    `json:"schema_version"`
	GeneratedAt   time.Time                 `json:"generated_at"`
	Window        analysis.AnalysisWindow   `json:"window"`
	Source        string                    `json:"source"`
	Note          string                    `json:"note"`
	Candidates    []analysis.QueryCandidate `json:"candidates"`
	Warnings      []string                  `json:"warnings,omitempty"`
}

// buildTraceDrilldown is the Phase 2 entry point. It first resolves the
// drilldown subject:
//
//   - an identity flag (precedence trace-id > query-id > normalized-query-hash)
//     selects directly;
//   - otherwise the recent source searches system.query_log and selects the
//     single match, or returns the bounded candidate list to narrow.
//
// Once a single query is selected it delegates to buildTraceDrilldownReport for
// the trace/stats/similar fan-out. Headless runs never block: a multi-candidate
// search returns a list outcome rather than prompting.
func buildTraceDrilldown(ctx context.Context, src traceDrilldownSource, cfg *config.Config, resolvedPath string, opts analyzeTraceOptions) (traceDrilldownOutcome, error) {
	end := time.Now().UTC().Truncate(time.Second)
	window := analysis.AnalysisWindow{Start: end.Add(-opts.Lookback), End: end}

	selection, err := selectTraceSubject(ctx, src, opts, window)
	if err != nil {
		return traceDrilldownOutcome{}, err
	}
	if selection.list != nil {
		selection.list.GeneratedAt = end
		return traceDrilldownOutcome{CandidateList: selection.list}, nil
	}

	// buildTraceDrilldownReport stamps its own GeneratedAt/Window from the same
	// clock; the report path is the single source of truth for those fields.
	report, err := buildTraceDrilldownReport(ctx, src, cfg, resolvedPath, selection.opts)
	if err != nil {
		return traceDrilldownOutcome{}, err
	}
	// Record the operator-supplied identity rather than the drill identity, so a
	// query_id discovered by the recent search is not misreported as supplied.
	// A pure -match recent run has no supplied identity, so omit the section.
	if selection.supplied == (analysis.TraceIdentity{}) {
		report.SuppliedIdentity = nil
	} else {
		supplied := selection.supplied
		report.SuppliedIdentity = &supplied
	}
	// Carry forward any warnings raised during candidate selection (e.g. a
	// normalized-hash search where normalized support is missing).
	report.Warnings = append(selection.warnings, report.Warnings...)
	return traceDrilldownOutcome{Report: &report}, nil
}

// traceSubjectSelection is the result of candidate resolution: either a single
// selected query (carried as a resolved opts with QueryID/TraceID set) or a
// bounded candidate list to narrow. supplied records what the operator actually
// typed (so a search-discovered query_id is not misreported as supplied), and
// warnings carries any non-fatal notes raised during selection (e.g. missing
// normalized support) so the report can surface them.
type traceSubjectSelection struct {
	opts     analyzeTraceOptions
	supplied analysis.TraceIdentity
	list     *traceCandidateList
	warnings []string
}

// selectTraceSubject resolves the drilldown subject without ever blocking on
// input. Identity flags win in precedence order; the recent source searches
// query_log and either selects the single match or returns a candidate list.
func selectTraceSubject(ctx context.Context, src traceDrilldownSource, opts analyzeTraceOptions, window analysis.AnalysisWindow) (traceSubjectSelection, error) {
	// A pre-resolved selection (the wizard's operator-picked recent candidate)
	// is a *discovered* identity, not a supplied one: drill its query_id
	// directly with an empty supplied identity, exactly as the single-candidate
	// auto-select below does. This keeps the guided recent path byte-identical to
	// the flag recent path that discovered the same query_id.
	if opts.selectedQueryID != "" {
		resolved := opts
		resolved.QueryID = opts.selectedQueryID
		// resolved.currentCandidate (if the wizard picked a current candidate) is
		// preserved by the struct copy, so the report keeps the running metadata.
		return traceSubjectSelection{opts: resolved}, nil
	}

	// The supplied identity reflects what the operator typed; a query_id
	// discovered by the recent search is a discovered identity carried on the
	// selected candidate, not a supplied one.
	supplied := analysis.TraceIdentity{
		QueryID:             opts.QueryID,
		TraceID:             opts.TraceID,
		NormalizedQueryHash: opts.NormalizedQueryHash,
	}

	// Identity precedence: trace-id > query-id > normalized-query-hash. trace-id
	// and query-id are direct drill paths: when either is present the recent
	// search is skipped entirely. Trace resolution itself honors precedence in
	// buildTraceDrilldownReport (trace-id skips the query-id lookup); a
	// co-supplied query-id still enriches the report with its query-log stats.
	if opts.TraceID != "" || opts.QueryID != "" {
		return traceSubjectSelection{opts: opts, supplied: supplied}, nil
	}

	// normalized-query-hash and the recent search both go through query_log; the
	// current source goes through system.processes. Dispatch on the resolved
	// source so each path searches its own table with its own posture, then feed
	// the shared candidate-selection switch below.
	candidates, warnings, err := searchTraceCandidates(ctx, src, opts, window)
	if err != nil {
		return traceSubjectSelection{}, err
	}

	switch len(candidates) {
	case 0:
		// No candidate found: still a successful run that produces a report
		// (empty trace) so the operator sees the window and a warning.
		note := "no candidates matched the search; widen -lookback or adjust -match"
		return traceSubjectSelection{list: &traceCandidateList{
			SchemaVersion: analysis.TraceDrilldownSchemaVersion,
			Window:        window,
			Source:        opts.Source,
			Note:          note,
			Candidates:    []analysis.QueryCandidate{},
			Warnings:      warnings,
		}}, nil
	case 1:
		// Exactly one candidate: select it and drill in by its query_id. The
		// report path re-reads the query-log row by ID so the exception-row
		// dedup and enrichment posture stay identical to the direct-query-id
		// path. For the current source the candidate's system.processes metadata
		// is carried forward so the report still shows the running query's
		// elapsed/user/client even when no query-log row exists yet.
		resolved := opts
		resolved.QueryID = candidates[0].QueryID
		if opts.Source == "current" {
			row := candidates[0]
			resolved.currentCandidate = &row
		}
		return traceSubjectSelection{opts: resolved, supplied: supplied, warnings: warnings}, nil
	default:
		// Multiple candidates: render the bounded list and stop. Never block.
		list := &traceCandidateList{
			SchemaVersion: analysis.TraceDrilldownSchemaVersion,
			Window:        window,
			Source:        opts.Source,
			Note:          "multiple candidates matched; narrow with -match or an identity flag (-query-id / -normalized-query-hash)",
			Candidates:    make([]analysis.QueryCandidate, 0, len(candidates)),
			Warnings:      warnings,
		}
		for i := range candidates {
			list.Candidates = append(list.Candidates, candidateForSource(candidates[i], opts.Source, opts.RedactDimensions))
		}
		return traceSubjectSelection{list: list}, nil
	}
}

// searchTraceCandidates dispatches the candidate search on the resolved source:
// `recent` searches finished query_log events, `current` searches running
// queries in system.processes. Both return query-log-shaped rows so the shared
// selection switch (single -> drill, multiple -> bounded list, zero -> list)
// stays source-agnostic. `other` never reaches here (an explicit identity
// short-circuits selection before any search).
func searchTraceCandidates(ctx context.Context, src traceDrilldownSource, opts analyzeTraceOptions, window analysis.AnalysisWindow) ([]model.QueryLog, []string, error) {
	if opts.Source == "current" {
		return searchCurrentCandidates(ctx, src, opts)
	}
	return searchRecentCandidates(ctx, src, opts, window)
}

// searchCurrentCandidates runs the `current` candidate search over
// system.processes. The -match needle is pushed into the SQL WHERE (so the
// LIMIT bounds the matched rows) over the running-query metadata plus the
// normalized preview when supported; there is no -normalized-query-hash path
// (system.processes has no normalized_query_hash) and no window (a running query
// has no event time). When normalized previews are unavailable, -match falls
// back to metadata only with a warning, matching the recent posture.
func searchCurrentCandidates(ctx context.Context, src traceDrilldownSource, opts analyzeTraceOptions) ([]model.QueryLog, []string, error) {
	var warnings []string
	if opts.Match != "" && !src.QueryLogNormalizedSupported() {
		warnings = append(warnings,
			"normalized query previews are unavailable on this ClickHouse; -match falls back to candidate metadata only")
	}

	rows, err := src.FetchCurrentQueryCandidates(ctx, clickhouse.CurrentQueryCandidateOptions{
		Match: opts.Match,
		Limit: opts.CandidateLimit,
	})
	if err != nil {
		// The current-query search is the required query for the current source:
		// without it no candidate (and therefore no report) can be produced.
		return nil, nil, fmt.Errorf("current candidate search failed: %w", err)
	}
	return rows, warnings, nil
}

// searchRecentCandidates runs the recent query_log candidate search. The -match
// and -normalized-query-hash predicates are pushed into the SQL WHERE (before
// ORDER BY query_duration_ms DESC ... LIMIT) so the LIMIT bounds the *matched*
// rows, not a cost-truncated top-N that filtering would then shrink. The window,
// user filter, duration/length posture, ordering, and limit are likewise
// enforced in the SQL builder.
//
//   - -match is a case-insensitive substring OR-group over the candidate
//     metadata (query_id, user, client_name, client_hostname, tables,
//     databases) plus the normalized preview when normalized support is present.
//   - -normalized-query-hash is an explicit identity lookup. The column only
//     exists when normalized support is present: when it is missing, the search
//     short-circuits to an empty candidate list plus an "unsupported" warning
//     (exit 0, consistent with the no-candidate-found degradation) rather than
//     running a query against a column that does not exist.
func searchRecentCandidates(ctx context.Context, src traceDrilldownSource, opts analyzeTraceOptions, window analysis.AnalysisWindow) ([]model.QueryLog, []string, error) {
	var warnings []string

	chOpts := clickhouse.RecentQueryCandidateOptions{
		StartTime:      window.Start,
		EndTime:        window.End,
		MinDurationMs:  0,
		MaxDurationMs:  0,
		MaxQueryLength: 0,
		Match:          opts.Match,
		Limit:          opts.CandidateLimit,
	}

	// Resolve an exact hash filter up front so an unparseable value is a clear
	// usage signal rather than a silent no-match.
	if opts.NormalizedQueryHash != "" {
		h, perr := strconv.ParseUint(opts.NormalizedQueryHash, 10, 64)
		if perr != nil {
			return nil, nil, fmt.Errorf("invalid -normalized-query-hash %q: must be an unsigned integer", opts.NormalizedQueryHash)
		}
		if !src.QueryLogNormalizedSupported() {
			// The hash predicate would reference normalized_query_hash, which does
			// not exist here. Degrade to an empty candidate list with a warning
			// instead of emitting a broken query — exit 0, same shape as the
			// no-candidate-found path.
			return nil, []string{
				"no candidate (normalized_query_hash unsupported on this ClickHouse)",
			}, nil
		}
		chOpts.NormalizedQueryHash = h
		chOpts.HasHash = true
	} else if opts.Match != "" && !src.QueryLogNormalizedSupported() {
		warnings = append(warnings,
			"normalized query previews are unavailable on this ClickHouse; -match falls back to candidate metadata only")
	}

	rows, err := src.FetchRecentQueryCandidates(ctx, chOpts)
	if err != nil {
		// The candidate search is the required query for the recent source:
		// without it no candidate (and therefore no report) can be produced.
		return nil, nil, fmt.Errorf("recent candidate search failed: %w", err)
	}

	// The SQL builder now enforces the -match / -normalized-query-hash predicates
	// in the WHERE clause, so the returned rows are already the matched,
	// cost-ordered, LIMIT-bounded set. No Go-side post-filtering remains: doing it
	// here would reintroduce the cost-truncation defect for long normalized
	// queries (the SQL matches the full normalized text; a bounded Go preview
	// could not).
	return rows, warnings, nil
}

// candidateForSource builds the bounded QueryCandidate for the candidate list,
// choosing the mapping that matches the source: `current` rows come from
// system.processes (elapsed runtime, no finished stats or hash), everything
// else comes from query_log.
func candidateForSource(ql model.QueryLog, source string, redact bool) analysis.QueryCandidate {
	if source == "current" {
		return candidateFromCurrentQuery(ql, redact)
	}
	return candidateFromQueryLog(ql, source, redact)
}

// candidateFromQueryLog builds the bounded QueryCandidate shown in the
// candidate list. It carries identity, timing, and bounded normalized preview
// only — never raw query text — and honors dimension redaction.
func candidateFromQueryLog(ql model.QueryLog, source string, redact bool) analysis.QueryCandidate {
	c := analysis.QueryCandidate{Source: source, QueryID: ql.QueryID}
	applyQueryLogToCandidate(&c, ql, redact)
	return c
}

// candidateFromCurrentQuery builds the bounded QueryCandidate for a `current`
// (system.processes) row. A running query has no finished query-log stats,
// event time, or normalized_query_hash, so only the running-query metadata is
// carried: query_id, elapsed runtime, bounded normalized preview (when
// supported), and the dimension values (redaction honored). Raw query text is
// never carried.
func candidateFromCurrentQuery(ql model.QueryLog, redact bool) analysis.QueryCandidate {
	c := analysis.QueryCandidate{
		Source:          "current",
		QueryID:         ql.QueryID,
		ElapsedMs:       ql.ElapsedMs,
		NormalizedQuery: boundedPreview(ql.NormalizedQuery, traceNormalizedQueryPreviewLength),
	}
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
	return c
}
