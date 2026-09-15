package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
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
	// returns or lists. It mirrors the spec's --candidate-limit default.
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

// traceWaitPollInterval is the fixed cadence at which the --wait loop re-checks
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

// analyzeTraceIsTTY decides whether the --wizard gate sees an interactive
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

// fanoutSet is the parsed --fanout selection. trace and stats are the default
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
// queryAnalysisSource adds are only invoked when --fanout findings is requested.
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
	wait := fs.Duration("wait", 0, "Bounded wait for a selected current query's trace to materialize (e.g. 30s); 0 = no polling. Capped by --timeout.")
	fanout := fs.String("fanout", "trace,stats", "Comma-separated fan-out views: trace, stats, similar, findings")
	redact := fs.Bool("redact-dimensions", false, "Redact user/client/host dimension values in the report")
	wizard := fs.Bool("wizard", false, "Run the TTY-only guided drilldown (source -> candidate search -> selection -> fan-out)")

	fs.Usage = func() {
		_, _ = fmt.Fprintf(errOut, `click-dog analyze trace — local, read-only query trace drilldown

Usage:
  click-dog analyze trace --wizard [flags]
  click-dog analyze trace --source recent --match <substr> [flags]
  click-dog analyze trace --source current --match <substr> [--wait 30s] [flags]
  click-dog analyze trace --query-id <id> [flags]
  click-dog analyze trace --trace-id <id> [flags]
  click-dog analyze trace --normalized-query-hash <hash> [flags]

Finds a query, then the native trace IDs that carry it via the
clickhouse.query_id span attribute, fetches the trace's spans, and fans out to
query-log stats and the matching query-family rollup. With --source recent the
command searches system.query_log inside the lookback window; --source current
searches the running queries in system.processes. --query-id and --trace-id are
direct drills that skip candidate search; --normalized-query-hash does an exact
recent query-log lookup and drills in only when exactly one candidate matches.
The command opens only the ClickHouse reader: no exporter connections are
opened and nothing is written to ClickHouse. Config loading still expands env
vars and reads configured *_file secret fields.

A --source current candidate may not have finished yet: its query-log row and
normalized-query hash may not exist and its trace spans may not be flushed. Use
--wait to poll (bounded by both --wait and --timeout) for the trace to materialize
before reporting it unavailable; missing pieces become warnings and the command
still exits 0.

With --wizard the command runs a TTY-only guided flow (choose source, search and
select a candidate, choose fan-out views) and then runs the same drilldown as
the equivalent flags. --wizard requires an interactive terminal: with a
non-interactive stdin it errors and exits 2 rather than blocking on input.

In non-interactive mode the command never blocks on input: if a search returns
exactly one candidate it drills in; otherwise it prints the bounded candidate
list and exits 0 with a note to narrow the search.

The report is an operational artifact: it never emits raw query text, only
bounded normalized previews and metadata. Treat reports like logs/traces.

Flags:
`)
		printFlagDefaults(errOut, fs)
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
		_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: invalid --format %q (want table or json)\n", *format)
		return 2
	}
	if *lookback <= 0 {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: --lookback must be positive\n")
		return 2
	}
	if *timeout <= 0 {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: --timeout must be positive\n")
		return 2
	}
	if *spanLimit <= 0 {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: --span-limit must be positive\n")
		return 2
	}
	if *candidateLimit <= 0 {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: --candidate-limit must be positive\n")
		return 2
	}
	// --wait 0s means no polling (one shot). A negative wait is meaningless and a
	// clear usage error rather than a silent no-op.
	if *wait < 0 {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: --wait must not be negative\n")
		return 2
	}

	// --wizard is TTY-only. A non-interactive stdin would make the guided prompts
	// block (or, with the EOF guard, fail mid-flow), so a headless --wizard is a
	// usage error: error clearly and exit 2 rather than hanging. Any run WITHOUT
	// --wizard never reaches this gate and behaves exactly as Phases 1-2. The gate
	// runs before config load so a headless caller opens no connection.
	// analyzeTraceIsTTY is a seam so the gate can be exercised deterministically
	// without depending on the test harness's stdin device.
	if *wizard && !analyzeTraceIsTTY() {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: --wizard requires an interactive terminal; use the --source/-query-id/-trace-id/-normalized-query-hash flags for non-interactive runs\n")
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

// resolveTraceSource validates --source and reconciles it with the identity
// flags. An explicit identity flag implies the `other` source even when --source
// is left at its `recent` default (or set to `current`). A supplied --query-id or
// --trace-id is a direct drill that bypasses candidate search, while a supplied
// --normalized-query-hash is resolved by an exact recent query-log lookup over
// candidate search; --source other with no identity is a usage error because
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
			return "", errors.New("--source other requires an identity flag (--trace-id, --query-id, or --normalized-query-hash)")
		}
		return "other", nil
	default:
		return "", fmt.Errorf("invalid --source %q (want recent, current, or other)", source)
	}
}

// parseFanout parses the comma-separated --fanout list. trace, stats, similar,
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
			return fanoutSet{}, fmt.Errorf("invalid --fanout view %q (accepted: trace, stats, similar, findings)", name)
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
