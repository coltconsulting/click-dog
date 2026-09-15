package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/coltconsulting/click-dog/internal/analysis"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
)

// analyzeTraceWizardInput is the seam the Guided Trace Search collects from the
// operator. It mirrors the Phase 1-2 flags one-to-one so a wizard run is exactly
// a flag run with the same inputs: there is no parallel execution path. The
// wizard fills these fields by prompting, then hands them to the same
// analyzeTraceWithSource report pipeline the flags drive.
type analyzeTraceWizardInput struct {
	Source              string
	Match               string
	QueryID             string
	TraceID             string
	NormalizedQueryHash string
	// SelectedQueryID carries a recent/current-search candidate the operator
	// picked. It is a discovered identity (not supplied): the report drills it but
	// records no supplied identity, exactly like the non-interactive
	// single-candidate auto-select. Set only on the recent/current search paths.
	SelectedQueryID string
	// SelectedCurrentCandidate carries the system.processes row for a picked
	// `current` candidate so the report keeps its running metadata (elapsed,
	// user/client/host) when no query-log row exists yet, matching the
	// non-interactive current drill. Set only on the current path.
	SelectedCurrentCandidate *model.QueryLog
	Fanout                   fanoutSet
}

// runAnalyzeTraceWizard is the TTY-gated Guided Trace Search entry point called
// from runAnalyzeTrace. It walks the operator through source selection,
// candidate search, candidate selection, and fan-out selection, then runs the
// exact same drilldown report path as the equivalent flags via
// analyzeTraceWithSource. base carries the non-interactive defaults (lookback,
// timeout, limits, redaction) the wizard does not prompt for; the wizard only
// overrides the inputs a guided choice expresses.
//
// The reader (src) is already open and read-only — the wizard never constructs
// exporters, servers, HA, breaker, or poller, and never writes to ClickHouse.
//
// in is injected (os.Stdin in production) so transcript tests can script the
// prompts without a real TTY, the same way init --wizard tests drive runWizard.
// The function never blocks on a closed stdin: every prompt loop bails on EOF
// (the wizPrompter sawEOF contract). On EOF mid-wizard it reports the truncated
// input and returns exit 1 rather than drilling on silent defaults.
func runAnalyzeTraceWizard(in io.Reader, out, errOut io.Writer, src traceDrilldownSource, cfg *config.Config, resolvedPath string, base analyzeTraceOptions, format, outputPath string) int {
	w := &wizPrompter{r: bufio.NewReader(in)}

	_, _ = fmt.Fprintln(out, "click-dog trace drilldown — answer a few questions to find a query's native trace.")
	_, _ = fmt.Fprintln(out, "Press Enter at any prompt to accept the [default] in brackets.")
	_, _ = fmt.Fprintln(out)

	collected, ok := collectTraceWizardInput(w, out, errOut, src, base)
	if !ok {
		// Either stdin closed mid-wizard or no candidate could be selected and
		// the operator was shown why. A truncated stdin is the only path that
		// should fail the command; a "no candidate" stop already printed its
		// guidance and is a clean exit.
		if w.sawEOF {
			_, _ = fmt.Fprintln(errOut, "click-dog analyze trace: stdin closed before the wizard finished; not running the drilldown")
			return 1
		}
		return 0
	}

	// Flag parity: fold the collected choices onto the non-interactive defaults
	// and run the identical report pipeline. A guided pick resolves to a
	// query-id (exactly what --query-id would supply), so the drilldown re-reads
	// the query-log row by ID and the report path is byte-for-byte the same as
	// the scriptable equivalent.
	opts := base
	opts.Source = collected.Source
	opts.Match = collected.Match
	opts.QueryID = collected.QueryID
	opts.TraceID = collected.TraceID
	opts.NormalizedQueryHash = collected.NormalizedQueryHash
	opts.selectedQueryID = collected.SelectedQueryID
	opts.currentCandidate = collected.SelectedCurrentCandidate
	opts.Fanout = collected.Fanout

	return analyzeTraceWithSource(src, cfg, resolvedPath, opts, format, outputPath, out, errOut)
}

// collectTraceWizardInput runs the guided prompts and returns the collected
// inputs. The second return is false when the wizard could not produce a
// drillable selection: either stdin closed (w.sawEOF set) or the candidate
// search yielded nothing/was abandoned (guidance already printed). A true
// return guarantees exactly one of {QueryID, TraceID, NormalizedQueryHash} or a
// resolved QueryID from a picked candidate is set.
func collectTraceWizardInput(w *wizPrompter, out, errOut io.Writer, src traceDrilldownSource, base analyzeTraceOptions) (analyzeTraceWizardInput, bool) {
	source := promptTraceSource(w, out, errOut)
	if w.sawEOF && source == "" {
		return analyzeTraceWizardInput{}, false
	}

	switch source {
	case "other":
		return collectTraceWizardOther(w, out, errOut)
	case "current":
		return collectTraceWizardSearch(w, out, errOut, src, base, "current")
	default: // "recent"
		return collectTraceWizardSearch(w, out, errOut, src, base, "recent")
	}
}

// promptTraceSource asks for the candidate source: recent (finished query_log
// events), current (running system.processes queries), or other (an explicit
// identity). The prompt mirrors the init-wizard topology prompt's
// re-prompt-on-bad-answer, bail-on-EOF posture.
func promptTraceSource(w *wizPrompter, out, errOut io.Writer) string {
	for {
		_, _ = fmt.Fprint(out, "Candidate source (recent/current/other) [recent]: ")
		switch strings.ToLower(w.readLine()) {
		case "":
			return "recent"
		case "recent":
			return "recent"
		case "current":
			return "current"
		case "other":
			return "other"
		default:
			_, _ = fmt.Fprintln(errOut, "please answer recent, current, or other")
			if w.sawEOF {
				return ""
			}
		}
	}
}

// collectTraceWizardOther collects an explicit identity for the `other` source:
// query-id, trace-id, or normalized-query-hash. Exactly one identity is
// required; the prompt re-asks until one is given (or stdin closes). This is the
// guided equivalent of `analyze trace --source other -<identity>`.
func collectTraceWizardOther(w *wizPrompter, out, errOut io.Writer) (analyzeTraceWizardInput, bool) {
	for {
		_, _ = fmt.Fprint(out, "Identity kind (query-id/trace-id/normalized-query-hash) [query-id]: ")
		kind := strings.ToLower(w.readLine())
		if kind == "" {
			kind = "query-id"
		}

		var label string
		switch kind {
		case "query-id", "trace-id", "normalized-query-hash":
			label = kind
		default:
			_, _ = fmt.Fprintln(errOut, "please answer query-id, trace-id, or normalized-query-hash")
			if w.sawEOF {
				return analyzeTraceWizardInput{}, false
			}
			continue
		}

		value := promptTraceNonEmpty(w, out, errOut, label)
		if value == "" {
			// Empty + EOF: stdin closed. Empty without EOF cannot happen because
			// promptTraceNonEmpty re-prompts on a blank line.
			return analyzeTraceWizardInput{}, false
		}

		in := analyzeTraceWizardInput{Source: "other", Fanout: fanoutSet{Trace: true}}
		switch kind {
		case "query-id":
			in.QueryID = value
		case "trace-id":
			in.TraceID = value
		case "normalized-query-hash":
			in.NormalizedQueryHash = value
		}
		fanout, ok := promptTraceFanout(w, out, errOut)
		if !ok {
			return analyzeTraceWizardInput{}, false
		}
		in.Fanout = fanout
		return in, true
	}
}

// collectTraceWizardSearch collects a --match string, runs the candidate search
// for the chosen source (recent over query_log, or current over
// system.processes), shows the bounded list, and lets the operator select one.
// The selected candidate's query-id is carried as the resolved QueryID so the
// drilldown runs the identical path as a direct --query-id. Returns false when
// the search yields no candidates (guidance printed) or stdin closes.
func collectTraceWizardSearch(w *wizPrompter, out, errOut io.Writer, src traceDrilldownSource, base analyzeTraceOptions, source string) (analyzeTraceWizardInput, bool) {
	matchPrompt := "Match (case-insensitive substring; empty lists recent)"
	if source == "current" {
		matchPrompt = "Match (case-insensitive substring; empty lists running queries)"
	}
	match := promptString(w, out, matchPrompt, "")
	if w.sawEOF && match == "" {
		return analyzeTraceWizardInput{}, false
	}

	// Run the same bounded search the non-interactive path uses. The window /
	// timeout / candidate-limit come from the base options so the wizard honors
	// the same --lookback / --timeout / --candidate-limit the flags expose.
	ctx, cancel := context.WithTimeout(context.Background(), base.Timeout)
	defer cancel()
	end := time.Now().UTC().Truncate(time.Second)
	window := analysis.AnalysisWindow{Start: end.Add(-base.Lookback), End: end}

	searchOpts := base
	searchOpts.Source = source
	searchOpts.Match = match

	rows, warnings, err := searchTraceCandidates(ctx, src, searchOpts, window)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "click-dog analyze trace: %s candidate search failed: %v\n", source, err)
		return analyzeTraceWizardInput{}, false
	}
	for _, warn := range warnings {
		_, _ = fmt.Fprintf(out, "warning: %s\n", warn)
	}

	// Map the rows into bounded candidates the same way the non-interactive
	// candidate list does (identity + bounded normalized preview, redaction
	// honored), so the wizard shows the identical shape.
	candidates := make([]analysis.QueryCandidate, 0, len(rows))
	for i := range rows {
		candidates = append(candidates, candidateForSource(rows[i], source, base.RedactDimensions))
	}

	candidate, ok := promptTraceCandidatePick(w, out, errOut, candidates)
	if !ok {
		return analyzeTraceWizardInput{}, false
	}

	// Flag parity: a picked candidate is a search *discovered* identity, not a
	// supplied one — so it goes in SelectedQueryID. The drilldown then records no
	// supplied identity and keeps the chosen source, byte-identical to the
	// non-interactive search path that discovered the same query_id.
	in := analyzeTraceWizardInput{Source: source, SelectedQueryID: candidate.QueryID}
	if source == "current" {
		// Carry the picked system.processes row so the report keeps its running
		// metadata when no query-log row exists yet, matching the non-interactive
		// current drill. Match by query_id (the candidate list preserves order).
		for i := range rows {
			if rows[i].QueryID == candidate.QueryID {
				row := rows[i]
				in.SelectedCurrentCandidate = &row
				break
			}
		}
	}
	fanout, ok := promptTraceFanout(w, out, errOut)
	if !ok {
		return analyzeTraceWizardInput{}, false
	}
	in.Fanout = fanout
	return in, true
}

// promptTraceCandidatePick renders the bounded candidate list and asks the
// operator to choose one by its 1-based index. An empty list prints a narrowing
// note and returns false (a clean stop, not a failure). Invalid indices
// re-prompt; EOF bails. The returned candidate carries at least the query-id
// needed to drill in.
func promptTraceCandidatePick(w *wizPrompter, out, errOut io.Writer, candidates []analysis.QueryCandidate) (analysis.QueryCandidate, bool) {
	if len(candidates) == 0 {
		_, _ = fmt.Fprintln(out, "No candidates matched; widen --lookback or adjust the match string, then re-run.")
		return analysis.QueryCandidate{}, false
	}

	_, _ = fmt.Fprintf(out, "\n%d candidate(s):\n", len(candidates))
	for i, c := range candidates {
		_, _ = fmt.Fprintf(out, "  [%d] query_id=%s", i+1, c.QueryID)
		if c.QueryDurationMs > 0 {
			_, _ = fmt.Fprintf(out, " duration_ms=%d", c.QueryDurationMs)
		}
		if c.ElapsedMs > 0 {
			_, _ = fmt.Fprintf(out, " elapsed_ms=%d", c.ElapsedMs)
		}
		if !c.EventTime.IsZero() {
			_, _ = fmt.Fprintf(out, " event_time=%s", c.EventTime.UTC().Format(time.RFC3339))
		}
		if c.NormalizedQueryHash != 0 {
			_, _ = fmt.Fprintf(out, " normalized_query_hash=%d", c.NormalizedQueryHash)
		}
		if c.User != "" {
			_, _ = fmt.Fprintf(out, " user=%s", c.User)
		}
		_, _ = fmt.Fprintln(out)
		if c.NormalizedQuery != "" {
			_, _ = fmt.Fprintf(out, "      %s\n", truncateOp(c.NormalizedQuery, 100))
		}
	}
	_, _ = fmt.Fprintln(out)

	for {
		_, _ = fmt.Fprintf(out, "Select a candidate (1-%d) [1]: ", len(candidates))
		line := w.readLine()
		idx := 1
		if line != "" {
			n, err := strconv.Atoi(line)
			if err != nil || n < 1 || n > len(candidates) {
				_, _ = fmt.Fprintf(errOut, "invalid selection %q; enter a number between 1 and %d\n", line, len(candidates))
				if w.sawEOF {
					return analysis.QueryCandidate{}, false
				}
				continue
			}
			idx = n
		}
		return candidates[idx-1], true
	}
}

// promptTraceFanout asks which fan-out views to include. trace is always
// implied (the report exists to produce it); stats and similar default on/off
// to match the non-interactive --fanout default (trace,stats); findings defaults
// off because it runs the full analysis registry inline. Returns false only on
// a closed stdin.
func promptTraceFanout(w *wizPrompter, out, errOut io.Writer) (fanoutSet, bool) {
	set := fanoutSet{Trace: true}
	set.Stats = promptYesNo(w, out, errOut, "Include query-log stats fan-out?", true)
	if w.sawEOF {
		return fanoutSet{}, false
	}
	set.Similar = promptYesNo(w, out, errOut, "Include similar query-family fan-out?", false)
	if w.sawEOF {
		return fanoutSet{}, false
	}
	set.Findings = promptYesNo(w, out, errOut, "Include analysis findings fan-out (filtered to the selected family)?", false)
	if w.sawEOF {
		return fanoutSet{}, false
	}
	return set, true
}

// promptTraceNonEmpty prompts for a required free-text value, re-prompting on a
// blank line. Bails on EOF (returns ""), so a closed stdin doesn't spin the
// loop. Used for the explicit-identity values, which have no sensible default.
func promptTraceNonEmpty(w *wizPrompter, out, errOut io.Writer, label string) string {
	for {
		_, _ = fmt.Fprintf(out, "%s: ", label)
		line := w.readLine()
		if line != "" {
			return line
		}
		if w.sawEOF {
			return ""
		}
		_, _ = fmt.Fprintf(errOut, "%s is required\n", label)
	}
}
