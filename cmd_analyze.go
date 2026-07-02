package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"time"

	"github.com/coltconsulting/click-dog/internal/analysis"
	"github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/processor"
)

// defaultAnalysisSpanSampleLimit is the auto span sample size, capped down to
// monitor.max_spans_per_cycle so the default analysis cost stays aligned with
// the operator's existing span-ingestion posture.
const defaultAnalysisSpanSampleLimit = 1000

const analyzeUsage = `click-dog analyze — local, read-only query analysis reports

Usage:
  click-dog analyze <subcommand> [flags]

Subcommands:
  queries  Analyze recent query families and emit findings
  trace    Drill down from a query ID to its native trace spans

Run "click-dog analyze queries --help" or "click-dog analyze trace --help" for flags.
`

// runAnalyze dispatches the `analyze` subcommand family and returns the process
// exit code instead of exiting, so usage and dispatch behavior stay
// unit-testable. The subcommand registry in main.go performs the single os.Exit.
func runAnalyze(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(errOut, analyzeUsage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(out, analyzeUsage)
		return 0
	case "queries":
		return runAnalyzeQueries(args[1:], out, errOut)
	case "trace":
		return runAnalyzeTrace(args[1:], out, errOut)
	}
	_, _ = fmt.Fprintf(errOut, "click-dog analyze: unknown command %q\n\n", args[0])
	_, _ = fmt.Fprint(errOut, analyzeUsage)
	return 2
}

// analyzeQueriesOptions carries the parsed `analyze queries` flags into the
// input bridge.
type analyzeQueriesOptions struct {
	Lookback           time.Duration
	Timeout            time.Duration
	RedactDimensions   bool
	MinExecutions      uint64
	FamilyLimit        int
	SpanSampleLimit    int
	QueryPreviewLength int
}

func runAnalyzeQueries(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("analyze queries", flag.ContinueOnError)
	fs.SetOutput(errOut)
	configPath := fs.String("config", config.DefaultConfigFlag, "Path to configuration file")
	lookback := fs.Duration("lookback", time.Hour, "Analysis window ending at command start (e.g. 1h, 30m)")
	timeout := fs.Duration("timeout", 2*time.Minute, "Command-level timeout for the full analysis run")
	format := fs.String("format", "table", "Output format: table or json")
	output := fs.String("output", "", "Write the report to this path instead of stdout")
	redact := fs.Bool("redact-dimensions", false, "Redact user/client/host dimension values in reports")
	minExecutions := fs.Uint64("min-executions", 3, "Minimum executions for exact query-family groups")
	familyLimit := fs.Int("family-limit", 200, "Exact normalized groups fetched before rollup")
	spanSampleLimit := fs.Int("span-sample-limit", 0, "Max spans sampled for attribution and coverage (0 = auto)")
	previewLength := fs.Int("query-preview-length", 500, "Max normalized-query preview length")

	fs.Usage = func() {
		_, _ = fmt.Fprintf(errOut, `click-dog analyze queries — deterministic local query analysis

Usage:
  click-dog analyze queries [flags]

Builds a bounded, read-only analysis report from system.query_log and
system.opentelemetry_span_log: query-family resource outliers, log_comment
attribution gaps, user/client/host skew, and coverage prerequisites. No
exporter connections are opened and nothing is written to ClickHouse. Config
loading still expands env vars and reads configured *_file secret fields.

JSON reports are operational artifacts: normalized SQL and dimension values
can reveal schema and ownership shape, so treat reports like logs/traces.

Flags:
`)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		// -h/--help is a deliberate request: flag already printed usage, so
		// exit 0 like the ExitOnError subcommands do. Other parse errors are
		// bad CLI usage.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *format != "table" && *format != "json" {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze queries: invalid -format %q (want table or json)\n", *format)
		return 2
	}
	if *lookback <= 0 {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze queries: -lookback must be positive\n")
		return 2
	}
	if *timeout <= 0 {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze queries: -timeout must be positive\n")
		return 2
	}

	resolvedPath, err := config.ResolveConfigPath(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze queries: %v\n", err)
		return 1
	}
	cfg, err := config.LoadConfig(resolvedPath)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze queries: %v\n", err)
		return 1
	}

	// The reader is the only connection the command opens: no exporters,
	// metrics/health servers, leader election, or circuit breaker.
	reader, err := clickhouse.NewClickHouseReader(cfg.ClickHouse, cfg.Filters)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze queries: %v\n", err)
		return 1
	}
	defer func() { _ = reader.Close() }()

	opts := analyzeQueriesOptions{
		Lookback:           *lookback,
		Timeout:            *timeout,
		RedactDimensions:   *redact,
		MinExecutions:      *minExecutions,
		FamilyLimit:        *familyLimit,
		SpanSampleLimit:    *spanSampleLimit,
		QueryPreviewLength: *previewLength,
	}
	return analyzeQueriesWithSource(reader, cfg, resolvedPath, opts, *format, *output, out, errOut)
}

// analyzeQueriesWithSource runs the report against any queryAnalysisSource.
// Split from runAnalyzeQueries so tests can drive the full report path with a
// fake source instead of a live ClickHouse.
func analyzeQueriesWithSource(src queryAnalysisSource, cfg *config.Config, resolvedPath string, opts analyzeQueriesOptions, format, outputPath string, out, errOut io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
	defer cancel()

	end := time.Now().UTC().Truncate(time.Second)
	window := analysis.AnalysisWindow{Start: end.Add(-opts.Lookback), End: end}

	input, err := buildAnalysisInput(ctx, src, cfg, resolvedPath, opts, window)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze queries: %v\n", err)
		return 1
	}

	warnings := append(append(append([]string{}, cfg.DeprecationWarnings...), cfg.EnvWarnings...), cfg.ValidationWarnings...)
	report := analysis.BuildReport(ctx, analysis.NewRegistry(), input, end, warnings)

	rendered, err := renderAnalysisReport(report, format)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze queries: render report: %v\n", err)
		return 1
	}

	// Env/deprecation warnings print above human table output; JSON already
	// carries them in report metadata. With -output, stdout stays quiet, so
	// the warnings go to stderr instead.
	if format == "table" && len(report.Warnings) > 0 {
		if outputPath == "" {
			for _, w := range report.Warnings {
				_, _ = fmt.Fprintf(out, "warning: %s\n", w)
			}
			_, _ = fmt.Fprintln(out)
		} else {
			for _, w := range report.Warnings {
				_, _ = fmt.Fprintf(errOut, "warning: %s\n", w)
			}
		}
	}

	if err := writeAnalysisOutput(rendered, outputPath, out); err != nil {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze queries: write report: %v\n", err)
		return 1
	}
	return 0
}

// queryAnalysisSource is the implementation seam between the command/input
// bridge and *clickhouse.ClickHouseReader. It is intentionally not part of
// the internal/analysis API.
type queryAnalysisSource interface {
	QueryLogNormalizedSupported() bool
	FetchQueryFamilyRollups(ctx context.Context, opts clickhouse.QueryFamilyRollupOptions) ([]model.QueryFamilyRollup, error)
	FetchOpenTelemetrySpansWithOpts(ctx context.Context, minTraceDurationMs int, lookback time.Duration, limit int, opts clickhouse.FetchOpts) ([]model.OpenTelemetrySpan, error)
	FetchQueryLogByQueryIDs(ctx context.Context, queryIDs []string, lookbackDays int) (map[string]model.QueryLog, error)
	FetchQueryFamilyDimensionCounts(ctx context.Context, opts clickhouse.QueryFamilyDimensionCountOptions) ([]clickhouse.QueryFamilyDimensionCount, error)
}

// buildAnalysisInput translates concrete reader calls into the pure
// analysis.AnalysisInput. Rollups failing with ErrQueryFamilyRollupsUnsupported
// and span sampling failures degrade to coverage findings/warnings; other
// query errors fail the command.
func buildAnalysisInput(ctx context.Context, src queryAnalysisSource, cfg *config.Config, resolvedPath string, opts analyzeQueriesOptions, window analysis.AnalysisWindow) (analysis.AnalysisInput, error) {
	input := analysis.AnalysisInput{
		Window: window,
		Config: buildAnalysisReportConfig(cfg, resolvedPath, opts),
	}
	cov := analysis.CoverageSummary{
		NormalizedQuerySupported:    src.QueryLogNormalizedSupported(),
		QueryFamilyRollupsSupported: true,
	}

	families, err := src.FetchQueryFamilyRollups(ctx, clickhouse.QueryFamilyRollupOptions{
		StartTime:         window.Start,
		EndTime:           window.End,
		MinExecutionCount: opts.MinExecutions,
		ExactGroupLimit:   opts.FamilyLimit,
		MaxPreviewLength:  opts.QueryPreviewLength,
	})
	switch {
	case errors.Is(err, clickhouse.ErrQueryFamilyRollupsUnsupported):
		// Not a command failure: the report continues with whatever
		// lower-fidelity inputs are available.
		cov.QueryFamilyRollupsSupported = false
	case err != nil:
		return input, fmt.Errorf("query family rollups failed: %w", err)
	}
	input.Families = families
	cov.QueryFamilyCount = len(families)

	spanLimit := resolveAnalysisSpanSampleLimit(opts.SpanSampleLimit, cfg.Monitor.MaxSpansPerCycle)
	input.Config.SpanSampleLimit = spanLimit
	lookbackDays := analysisQueryLogLookbackDays(opts.Lookback)
	cov.QueryLogByIDLookbackDays = lookbackDays

	spans, err := src.FetchOpenTelemetrySpansWithOpts(ctx, cfg.Monitor.MinTraceDurationMs, opts.Lookback, spanLimit, clickhouse.FetchOpts{
		MaxTraceDurationMs:  cfg.Monitor.MaxTraceDurationMs,
		MinSpanDurationMs:   cfg.Monitor.MinSpanDurationMs,
		MaxSpanDurationMs:   cfg.Monitor.MaxSpanDurationMs,
		BlacklistOperations: cfg.Filters.BlacklistOperations,
	})
	if err != nil {
		// The core family/resource report remains useful without span
		// samples; record the gap and skip attribution.
		cov.Warnings = append(cov.Warnings, fmt.Sprintf("span sampling unavailable: %v", err))
	} else {
		cov.SpanSampleAvailable = true
		cov.SpanSampleSize = len(spans)
		for i := range spans {
			if spans[i].Attributes["clickhouse.query_id"] != "" {
				cov.SpansWithQueryID++
			}
		}
		if cov.SpanSampleSize > 0 {
			cov.QueryIDRatio = float64(cov.SpansWithQueryID) / float64(cov.SpanSampleSize)
		}

		attribution, attrWarning := buildAttributionCoverage(ctx, src, spans, families, lookbackDays)
		if attrWarning != "" {
			cov.Warnings = append(cov.Warnings, attrWarning)
		}
		input.AttributionByFamily = attribution
		cov.AttributionFamilyCount = len(attribution)
	}

	if cov.QueryFamilyRollupsSupported && len(families) > 0 {
		counts, err := src.FetchQueryFamilyDimensionCounts(ctx, clickhouse.QueryFamilyDimensionCountOptions{
			StartTime:         window.Start,
			EndTime:           window.End,
			MinExecutionCount: opts.MinExecutions,
			ExactGroupLimit:   opts.FamilyLimit,
		})
		if err != nil {
			return input, fmt.Errorf("query family dimension counts failed: %w", err)
		}
		input.DimensionsByFamily = buildDimensionBreakdowns(families, counts)
	}

	if opts.RedactDimensions {
		redactDimensionBreakdowns(input.DimensionsByFamily)
	}

	input.Coverage = cov
	return input, nil
}

func buildAnalysisReportConfig(cfg *config.Config, resolvedPath string, opts analyzeQueriesOptions) analysis.ReportConfig {
	return analysis.ReportConfig{
		ConfigPath:         resolvedPath,
		ClickHouseHost:     cfg.ClickHouse.Host,
		ClickHouseCluster:  cfg.ClickHouse.Cluster,
		UseClusterQueries:  cfg.ClickHouse.UseClusterQueries,
		Lookback:           opts.Lookback.String(),
		Timeout:            opts.Timeout.String(),
		RedactDimensions:   opts.RedactDimensions,
		MinTraceDurationMs: cfg.Monitor.MinTraceDurationMs,
		MaxTraceDurationMs: cfg.Monitor.MaxTraceDurationMs,
		MinSpanDurationMs:  cfg.Monitor.MinSpanDurationMs,
		MaxSpanDurationMs:  cfg.Monitor.MaxSpanDurationMs,
		MinExecutions:      opts.MinExecutions,
		FamilyLimit:        opts.FamilyLimit,
		SpanSampleLimit:    opts.SpanSampleLimit,
		QueryPreviewLength: opts.QueryPreviewLength,
	}
}

// resolveAnalysisSpanSampleLimit returns the effective span sample size: an
// explicit flag wins; otherwise the auto limit, capped down to
// monitor.max_spans_per_cycle when that is positive and lower.
func resolveAnalysisSpanSampleLimit(flagLimit, maxSpansPerCycle int) int {
	if flagLimit > 0 {
		return flagLimit
	}
	limit := defaultAnalysisSpanSampleLimit
	if maxSpansPerCycle > 0 && maxSpansPerCycle < limit {
		limit = maxSpansPerCycle
	}
	return limit
}

// analysisQueryLogLookbackDays rounds the command lookback up to whole days,
// floored at 1, for FetchQueryLogByQueryIDs partition pruning. This can
// over-read query-log partitions for a one-hour report, but the query-id set
// came from the bounded span sample, so the analysis window is unchanged.
func analysisQueryLogLookbackDays(lookback time.Duration) int {
	days := int(math.Ceil(lookback.Hours() / 24))
	if days < 1 {
		return 1
	}
	return days
}

// buildAttributionCoverage maps sampled spans back to query families via
// clickhouse.query_id -> query_log normalized hash and counts
// log_comment.app / log_comment.query_name attribution per family. A
// non-empty warning means enrichment failed and attribution was skipped.
func buildAttributionCoverage(ctx context.Context, src queryAnalysisSource, spans []model.OpenTelemetrySpan, families []model.QueryFamilyRollup, lookbackDays int) (map[string]analysis.FamilyAttributionCoverage, string) {
	if len(families) == 0 || len(spans) == 0 {
		return nil, ""
	}
	queryIDs := processor.UniqueQueryIDs(spans)
	if len(queryIDs) == 0 {
		return nil, ""
	}
	queryLogMap, err := src.FetchQueryLogByQueryIDs(ctx, queryIDs, lookbackDays)
	if err != nil {
		return nil, fmt.Sprintf("query_log enrichment unavailable, attribution analysis skipped: %v", err)
	}

	hashToFamily := familyByMemberHash(families)
	type counters struct{ sampled, withApp, withName int }
	byFamily := make(map[string]*counters)

	for i := range spans {
		qid := spans[i].Attributes["clickhouse.query_id"]
		if qid == "" {
			continue
		}
		ql, ok := queryLogMap[qid]
		if !ok || ql.NormalizedQueryHash == 0 {
			continue
		}
		familyID, ok := hashToFamily[ql.NormalizedQueryHash]
		if !ok {
			continue
		}
		// Copy the span so log-comment promotion never mutates the sampled
		// slice; ExtractLogComment keeps parsing in lockstep with scheduled
		// export behavior.
		span := spans[i]
		processor.ExtractLogComment(&span)

		c := byFamily[familyID]
		if c == nil {
			c = &counters{}
			byFamily[familyID] = c
		}
		c.sampled++
		if span.Attributes["log_comment.app"] != "" {
			c.withApp++
		}
		if span.Attributes["log_comment.query_name"] != "" {
			c.withName++
		}
	}

	if len(byFamily) == 0 {
		return nil, ""
	}
	out := make(map[string]analysis.FamilyAttributionCoverage, len(byFamily))
	for familyID, c := range byFamily {
		out[familyID] = analysis.FamilyAttributionCoverage{
			FamilyID:                familyID,
			SampledSpans:            c.sampled,
			WithLogCommentApp:       c.withApp,
			WithLogCommentQueryName: c.withName,
			AppRatio:                float64(c.withApp) / float64(c.sampled),
			QueryNameRatio:          float64(c.withName) / float64(c.sampled),
		}
	}
	return out, ""
}

func familyByMemberHash(families []model.QueryFamilyRollup) map[uint64]string {
	out := make(map[uint64]string)
	for i := range families {
		for _, h := range families[i].MemberHashesSorted {
			out[h] = families[i].FamilyID
		}
	}
	return out
}

// buildDimensionBreakdowns joins dimension counts to rollup families by
// member hash, summing counts across member hashes. Ratios are computed
// against the rollup family execution count and stored clamped to [0, 1]
// because the two queries are separate reads that can race near the window
// edge.
func buildDimensionBreakdowns(families []model.QueryFamilyRollup, counts []clickhouse.QueryFamilyDimensionCount) map[string]analysis.FamilyDimensionBreakdown {
	hashToFamily := familyByMemberHash(families)
	familyExecutions := make(map[string]uint64, len(families))
	for i := range families {
		familyExecutions[families[i].FamilyID] = families[i].Stats.ExecutionCount
	}

	// family -> dimension -> value -> summed execution count
	agg := make(map[string]map[string]map[string]uint64)
	for _, count := range counts {
		familyID, ok := hashToFamily[count.NormalizedQueryHash]
		if !ok {
			continue
		}
		dims := agg[familyID]
		if dims == nil {
			dims = make(map[string]map[string]uint64)
			agg[familyID] = dims
		}
		byValue := dims[string(count.Dimension)]
		if byValue == nil {
			byValue = make(map[string]uint64)
			dims[string(count.Dimension)] = byValue
		}
		byValue[count.Value] += count.ExecutionCount
	}

	out := make(map[string]analysis.FamilyDimensionBreakdown, len(agg))
	for familyID, dims := range agg {
		values := make(map[string][]analysis.DimensionHit, len(dims))
		for dim, byValue := range dims {
			hits := make([]analysis.DimensionHit, 0, len(byValue))
			for value, executions := range byValue {
				ratio := 0.0
				if total := familyExecutions[familyID]; total > 0 {
					ratio = float64(executions) / float64(total)
				}
				if ratio > 1 {
					ratio = 1
				}
				hits = append(hits, analysis.DimensionHit{
					Value:          value,
					ExecutionCount: executions,
					Ratio:          ratio,
				})
			}
			sortDimensionHits(hits)
			values[dim] = hits
		}
		out[familyID] = analysis.FamilyDimensionBreakdown{FamilyID: familyID, Values: values}
	}
	return out
}

func sortDimensionHits(hits []analysis.DimensionHit) {
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].ExecutionCount != hits[j].ExecutionCount {
			return hits[i].ExecutionCount > hits[j].ExecutionCount
		}
		return hits[i].Value < hits[j].Value
	})
}

// redactDimensionBreakdowns replaces raw user/client/host values with
// report-local stable labels such as user_1, assigned per dimension in the
// same sorted order the report would otherwise use (execution count
// descending, value ascending, aggregated across families). Counts and
// ratios remain; raw values are omitted.
func redactDimensionBreakdowns(byFamily map[string]analysis.FamilyDimensionBreakdown) {
	if len(byFamily) == 0 {
		return
	}

	totals := make(map[string]map[string]uint64) // dimension -> raw value -> total executions
	for _, breakdown := range byFamily {
		for dim, hits := range breakdown.Values {
			byValue := totals[dim]
			if byValue == nil {
				byValue = make(map[string]uint64)
				totals[dim] = byValue
			}
			for _, hit := range hits {
				byValue[hit.Value] += hit.ExecutionCount
			}
		}
	}

	labels := make(map[string]map[string]string, len(totals))
	for dim, byValue := range totals {
		type valueCount struct {
			value string
			count uint64
		}
		ordered := make([]valueCount, 0, len(byValue))
		for value, count := range byValue {
			ordered = append(ordered, valueCount{value: value, count: count})
		}
		sort.Slice(ordered, func(i, j int) bool {
			if ordered[i].count != ordered[j].count {
				return ordered[i].count > ordered[j].count
			}
			return ordered[i].value < ordered[j].value
		})
		dimLabels := make(map[string]string, len(ordered))
		for i, vc := range ordered {
			dimLabels[vc.value] = fmt.Sprintf("%s_%d", dim, i+1)
		}
		labels[dim] = dimLabels
	}

	for familyID, breakdown := range byFamily {
		for dim, hits := range breakdown.Values {
			for i := range hits {
				hits[i].Value = labels[dim][hits[i].Value]
			}
			// Relabeling can perturb the value tie-break, so restore the
			// stored sort invariant.
			sortDimensionHits(hits)
			breakdown.Values[dim] = hits
		}
		byFamily[familyID] = breakdown
	}
}

func renderAnalysisReport(report analysis.AnalysisReport, format string) (string, error) {
	if format == "json" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return "", err
		}
		return string(data) + "\n", nil
	}
	return analysis.RenderTable(report), nil
}

// writeAnalysisOutput writes the rendered report to stdout, or to outputPath
// with 0600 permissions on create.
func writeAnalysisOutput(rendered, outputPath string, out io.Writer) error {
	if outputPath == "" {
		_, err := io.WriteString(out, rendered)
		return err
	}
	return os.WriteFile(outputPath, []byte(rendered), 0o600)
}
