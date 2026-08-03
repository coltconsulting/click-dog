package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coltconsulting/click-dog/internal/analysis"
	"github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
)

const (
	// defaultTraceSpanLimit bounds how many spans are fetched for the selected
	// trace IDs. It mirrors the analyze-queries span sample default.
	defaultTraceSpanLimit = 1000

	// defaultTraceCandidateLimit bounds how many candidates the recent search
	// returns or lists. It mirrors the spec's -candidate-limit default.
	defaultTraceCandidateLimit = 20

	// traceSlowestSpanLimit caps the slowest-span list in the trace summary so
	// table and JSON output stay bounded for wide traces.
	traceSlowestSpanLimit = 10

	// traceNormalizedQueryPreviewLength bounds the normalized-query preview
	// echoed in the report. Raw query text is never emitted.
	traceNormalizedQueryPreviewLength = 500

	// traceFindingsTableLimit caps the findings list in the compact table view
	// so a noisy family stays bounded; JSON always carries the full set.
	traceFindingsTableLimit = 20
)

// traceWaitPollInterval is the fixed cadence at which the -wait loop re-checks
// whether a selected current query's trace spans have materialized. It is
// intentionally coarse: span-log/query-log flushes are not instantaneous, so
// polling tighter than this only adds load without surfacing the data any
// sooner. It is a var (not a const) so tests can shrink it for deterministic,
// fast polling assertions; production never reassigns it.
var traceWaitPollInterval = 2 * time.Second

// traceNowFunc supplies the command-start instant used to stamp the report
// window. It is a var so the findings-fan-out golden can pin the clock: the
// analysis registry hashes the window start into each finding's stable ID, so a
// deterministic golden needs a deterministic window. Production never reassigns
// it. Only buildTraceDrilldownReport reads it; the candidate-selection clock
// (buildTraceDrilldown) is independent and does not feed finding IDs.
var traceNowFunc = func() time.Time { return time.Now().UTC().Truncate(time.Second) }

// analyzeTraceIsTTY decides whether the -wizard gate sees an interactive
// terminal. It defaults to the shared isTTY probe over os.Stdin and is a var so
// tests can pin the non-TTY posture without depending on the test harness's
// stdin device (which is often /dev/null, itself a character device).
var analyzeTraceIsTTY = isTTY

// analyzeTraceOptions carries the parsed `analyze trace` flags into the report
// builder.
type analyzeTraceOptions struct {
	Lookback            time.Duration
	Timeout             time.Duration
	Source              string
	Match               string
	QueryID             string
	TraceID             string
	NormalizedQueryHash string
	CandidateLimit      int
	SpanLimit           int
	Wait                time.Duration
	Fanout              fanoutSet
	RedactDimensions    bool

	// selectedQueryID carries a query_id the recent search already resolved to
	// (e.g. the wizard's operator-picked candidate). It is a *discovered*
	// identity, not a supplied one: selectTraceSubject drills it directly
	// without re-running the search and keeps SuppliedIdentity empty, exactly as
	// the non-interactive single-candidate auto-select does. This keeps the
	// guided path on the same report pipeline as the flags rather than a parallel
	// one. It is never set from a flag.
	selectedQueryID string

	// currentCandidate carries the system.processes row for a selected `current`
	// candidate so the report can surface its running metadata (elapsed runtime,
	// user/client/host) even when no query-log row has materialized yet. It is a
	// *discovered* identity from the current search, never set from a flag, and
	// is only populated on the current source's single-candidate drill path.
	currentCandidate *model.QueryLog
}

// fanoutSet is the parsed -fanout selection. trace and stats are the default
// views; similar maps the selected query to its query-family rollup; findings
// runs the same analysis registry as `analyze queries` and filters the
// findings to the selected family.
type fanoutSet struct {
	Trace    bool
	Stats    bool
	Similar  bool
	Findings bool
}

// traceDrilldownSource is the implementation seam between the command and
// *clickhouse.ClickHouseReader. It opens only the reader and runs only read
// paths: no exporter, metrics, health, leader, or breaker methods appear here,
// which keeps the read-only guarantee testable. Phase 2 adds the `recent`
// candidate search and the `similar` fan-out reads (rollups + normalized
// support) alongside the Phase 1 trace/query-log reads.
//
// It embeds queryAnalysisSource so the `findings` fan-out (Phase 5) can reuse
// the exact same buildAnalysisInput build path and compiled-in analyzer
// registry as `analyze queries` — the trace command builds the same input over
// its own lookback window, runs analysis.NewRegistry(), and filters the
// findings to the selected family. The span-sample and dimension-count reads
// queryAnalysisSource adds are only invoked when -fanout findings is requested.
type traceDrilldownSource interface {
	queryAnalysisSource

	FetchTraceIDsByQueryID(ctx context.Context, queryID string, lookbackDays, limit int) ([]string, error)
	FetchSpansForTraceIDs(ctx context.Context, traceIDs []string, lookbackDays, limit int, blacklistOperations []string) ([]model.OpenTelemetrySpan, error)
	FetchRecentQueryCandidates(ctx context.Context, opts clickhouse.RecentQueryCandidateOptions) ([]model.QueryLog, error)
	FetchCurrentQueryCandidates(ctx context.Context, opts clickhouse.CurrentQueryCandidateOptions) ([]model.QueryLog, error)
}

func runAnalyzeTrace(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("analyze trace", flag.ContinueOnError)
	fs.SetOutput(errOut)
	configPath := fs.String("config", config.DefaultConfigFlag, "Path to configuration file")
	lookback := fs.Duration("lookback", time.Hour, "Search/report window ending at command start (e.g. 1h, 30m)")
	timeout := fs.Duration("timeout", 2*time.Minute, "Command-level timeout for the full drilldown run")
	format := fs.String("format", "table", "Output format: table or json")
	output := fs.String("output", "", "Write the report to this path instead of stdout")
	source := fs.String("source", "recent", "Candidate source: recent (query_log search), current (system.processes running queries), or other (explicit identity)")
	match := fs.String("match", "", "Case-insensitive substring over normalized query previews and candidate metadata (recent/current source)")
	queryID := fs.String("query-id", "", "Explicit ClickHouse query ID to drill down on")
	traceID := fs.String("trace-id", "", "Explicit trace ID; skips the query-id lookup")
	normalizedHash := fs.String("normalized-query-hash", "", "Explicit exact normalized query hash to drill down on")
	candidateLimit := fs.Int("candidate-limit", defaultTraceCandidateLimit, "Max candidates returned or listed by the recent/current search")
	spanLimit := fs.Int("span-limit", defaultTraceSpanLimit, "Max spans fetched for the selected trace IDs")
	wait := fs.Duration("wait", 0, "Bounded wait for a selected current query's trace to materialize (e.g. 30s); 0 = no polling. Capped by -timeout.")
	fanout := fs.String("fanout", "trace,stats", "Comma-separated fan-out views: trace, stats, similar, findings")
	redact := fs.Bool("redact-dimensions", false, "Redact user/client/host dimension values in the report")
	wizard := fs.Bool("wizard", false, "Run the TTY-only guided drilldown (source -> candidate search -> selection -> fan-out)")

	fs.Usage = func() {
		_, _ = fmt.Fprintf(errOut, `click-dog analyze trace — local, read-only query trace drilldown

Usage:
  click-dog analyze trace -wizard [flags]
  click-dog analyze trace -source recent -match <substr> [flags]
  click-dog analyze trace -source current -match <substr> [-wait 30s] [flags]
  click-dog analyze trace -query-id <id> [flags]
  click-dog analyze trace -trace-id <id> [flags]
  click-dog analyze trace -normalized-query-hash <hash> [flags]

Finds a query, then the native trace IDs that carry it via the
clickhouse.query_id span attribute, fetches the trace's spans, and fans out to
query-log stats and the matching query-family rollup. With -source recent the
command searches system.query_log inside the lookback window; -source current
searches the running queries in system.processes. -query-id and -trace-id are
direct drills that skip candidate search; -normalized-query-hash does an exact
recent query-log lookup and drills in only when exactly one candidate matches.
The command opens only the ClickHouse reader: no exporter connections are
opened and nothing is written to ClickHouse. Config loading still expands env
vars and reads configured *_file secret fields.

A -source current candidate may not have finished yet: its query-log row and
normalized-query hash may not exist and its trace spans may not be flushed. Use
-wait to poll (bounded by both -wait and -timeout) for the trace to materialize
before reporting it unavailable; missing pieces become warnings and the command
still exits 0.

With -wizard the command runs a TTY-only guided flow (choose source, search and
select a candidate, choose fan-out views) and then runs the same drilldown as
the equivalent flags. -wizard requires an interactive terminal: with a
non-interactive stdin it errors and exits 2 rather than blocking on input.

In non-interactive mode the command never blocks on input: if a search returns
exactly one candidate it drills in; otherwise it prints the bounded candidate
list and exits 0 with a note to narrow the search.

The report is an operational artifact: it never emits raw query text, only
bounded normalized previews and metadata. Treat reports like logs/traces.

Flags:
`)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		// -h/--help is a deliberate request: flag already printed usage, exit 0.
		// Other parse errors are bad CLI usage.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *format != "table" && *format != "json" {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: invalid -format %q (want table or json)\n", *format)
		return 2
	}
	if *lookback <= 0 {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: -lookback must be positive\n")
		return 2
	}
	if *timeout <= 0 {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: -timeout must be positive\n")
		return 2
	}
	if *spanLimit <= 0 {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: -span-limit must be positive\n")
		return 2
	}
	if *candidateLimit <= 0 {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: -candidate-limit must be positive\n")
		return 2
	}
	// -wait 0s means no polling (one shot). A negative wait is meaningless and a
	// clear usage error rather than a silent no-op.
	if *wait < 0 {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: -wait must not be negative\n")
		return 2
	}

	// -wizard is TTY-only. A non-interactive stdin would make the guided prompts
	// block (or, with the EOF guard, fail mid-flow), so a headless -wizard is a
	// usage error: error clearly and exit 2 rather than hanging. Any run WITHOUT
	// -wizard never reaches this gate and behaves exactly as Phases 1-2. The gate
	// runs before config load so a headless caller opens no connection.
	// analyzeTraceIsTTY is a seam so the gate can be exercised deterministically
	// without depending on the test harness's stdin device.
	if *wizard && !analyzeTraceIsTTY() {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: -wizard requires an interactive terminal; use the -source/-query-id/-trace-id/-normalized-query-hash flags for non-interactive runs\n")
		return 2
	}

	// The wizard collects source, identity, and fan-out interactively, so the
	// flag-driven resolution of those is skipped on the wizard branch. The
	// remaining flags (lookback/timeout/limits/redaction/format/output) still
	// apply and were validated above.
	var resolvedSource string
	var fanoutSet fanoutSet
	if !*wizard {
		var err error
		resolvedSource, err = resolveTraceSource(*source, *queryID, *traceID, *normalizedHash)
		if err != nil {
			_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: %v\n", err)
			return 2
		}

		fanoutSet, err = parseFanout(*fanout)
		if err != nil {
			_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: %v\n", err)
			return 2
		}
	}

	cfg, resolvedPath, err := loadConfig(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: %v\n", err)
		return 1
	}

	// The reader is the only connection the command opens: no exporters,
	// metrics/health servers, leader election, circuit breaker, or poller.
	reader, err := clickhouse.NewClickHouseReader(cfg.ClickHouse, cfg.Filters)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: %v\n", err)
		return 1
	}
	defer func() { _ = reader.Close() }()

	opts := analyzeTraceOptions{
		Lookback:            *lookback,
		Timeout:             *timeout,
		Source:              resolvedSource,
		Match:               *match,
		QueryID:             *queryID,
		TraceID:             *traceID,
		NormalizedQueryHash: *normalizedHash,
		CandidateLimit:      *candidateLimit,
		SpanLimit:           *spanLimit,
		Wait:                *wait,
		Fanout:              fanoutSet,
		RedactDimensions:    *redact,
	}
	if *wizard {
		// The wizard reads os.Stdin (the real terminal) and overrides
		// source/identity/fan-out from the operator's answers; the other opts
		// fields carry the same flag defaults the scriptable path uses.
		return runAnalyzeTraceWizard(os.Stdin, out, errOut, reader, cfg, resolvedPath, opts, *format, *output)
	}
	return analyzeTraceWithSource(reader, cfg, resolvedPath, opts, *format, *output, out, errOut)
}

// resolveTraceSource validates -source and reconciles it with the identity
// flags. An explicit identity flag implies the `other` source even when -source
// is left at its `recent` default (or set to `current`). A supplied -query-id or
// -trace-id is a direct drill that bypasses candidate search, while a supplied
// -normalized-query-hash is resolved by an exact recent query-log lookup over
// candidate search; -source other with no identity is a usage error because
// `other` means "drill an explicit identity". The returned source is the actual
// source the report should record.
func resolveTraceSource(source, queryID, traceID, normalizedHash string) (string, error) {
	hasIdentity := queryID != "" || traceID != "" || normalizedHash != ""
	switch source {
	case "recent", "current":
		// An explicit identity short-circuits the search: record it as `other`
		// so the report's source reflects the path actually taken.
		if hasIdentity {
			return "other", nil
		}
		return source, nil
	case "other":
		if !hasIdentity {
			return "", errors.New("-source other requires an identity flag (-trace-id, -query-id, or -normalized-query-hash)")
		}
		return "other", nil
	default:
		return "", fmt.Errorf("invalid -source %q (want recent, current, or other)", source)
	}
}

// parseFanout parses the comma-separated -fanout list. trace, stats, similar,
// and findings are accepted. Empty entries (from stray commas) are ignored.
// Trace is always implied so the report always carries the trace section it
// exists to produce. findings reuses the `analyze queries` analyzer registry
// (see buildTraceFindings) and filters the results to the selected family.
func parseFanout(spec string) (fanoutSet, error) {
	set := fanoutSet{Trace: true}
	for _, raw := range strings.Split(spec, ",") {
		name := strings.TrimSpace(raw)
		switch name {
		case "":
			continue
		case "trace":
			set.Trace = true
		case "stats":
			set.Stats = true
		case "similar":
			set.Similar = true
		case "findings":
			set.Findings = true
		default:
			return fanoutSet{}, fmt.Errorf("invalid -fanout view %q (accepted: trace, stats, similar, findings)", name)
		}
	}
	return set, nil
}

// analyzeTraceWithSource runs the drilldown against any traceDrilldownSource.
// Split from runAnalyzeTrace so tests can drive the full report path with a
// fake source instead of a live ClickHouse. resolvedPath is the resolved config
// path, threaded through only so the findings fan-out can build the same
// analysis ReportConfig as `analyze queries`.
func analyzeTraceWithSource(src traceDrilldownSource, cfg *config.Config, resolvedPath string, opts analyzeTraceOptions, format, outputPath string, out, errOut io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
	defer cancel()

	outcome, err := buildTraceDrilldown(ctx, src, cfg, resolvedPath, opts)
	if err != nil {
		// Only required-query failures reach here; missing data degrades to
		// warnings inside the builder.
		_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: %v\n", err)
		return 1
	}

	rendered, err := renderTraceDrilldownOutcome(outcome, format)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: render report: %v\n", err)
		return 1
	}

	if err := writeAnalysisOutput(rendered, outputPath, out); err != nil {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: write report: %v\n", err)
		return 1
	}
	return 0
}

// traceDrilldownOutcome is the result of one drilldown run. Exactly one of
// Report or CandidateList is populated: when candidate selection resolves to a
// single query the full Report is produced; when the recent search returns
// multiple candidates (and no identity selected one), CandidateList carries the
// bounded list so the operator can narrow the search. Both outcomes exit 0; a
// candidate list is a successful "here is what matched" result, not a failure.
type traceDrilldownOutcome struct {
	Report        *analysis.TraceDrilldownReport
	CandidateList *traceCandidateList
}

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
			fmt.Fprintf(&b, "  (showing %d of %d findings; use -format json for the full report)\n",
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
