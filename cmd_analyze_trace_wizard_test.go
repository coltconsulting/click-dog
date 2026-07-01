package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/analysis"
	"github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/model"
)

// wizardBaseOptions are the non-interactive defaults the wizard folds its
// collected choices onto — the same field set runAnalyzeTrace builds before
// dispatching to the wizard. Source/Match/identity/Fanout are left zero because
// the wizard overrides them from the prompts.
func wizardBaseOptions() analyzeTraceOptions {
	return analyzeTraceOptions{
		Lookback:       time.Hour,
		Timeout:        time.Minute,
		CandidateLimit: defaultTraceCandidateLimit,
		SpanLimit:      1000,
	}
}

// ---------------------------------------------------------------------------
// Transcript tests: scripted stdin drives the prompts; assert prompt sequence
// and the resulting selection/report. No real TTY required (injected reader).
// ---------------------------------------------------------------------------

// TestAnalyzeTraceWizard_RecentTranscript walks the full recent flow:
// source=recent, a match string, the candidate list, a pick, and the fan-out
// answers. It asserts the prompt sequence and that the picked candidate is the
// one drilled into the report.
func TestAnalyzeTraceWizard_RecentTranscript(t *testing.T) {
	traceID := uuid.New()
	src := &fakeTraceSource{
		normalized: true,
		candidates: []model.QueryLog{
			{QueryID: "q-a", QueryDurationMs: 9000, NormalizedQueryHash: 1, NormalizedQuery: "SELECT ? FROM events a", User: "u1"},
			{QueryID: "q-b", QueryDurationMs: 8000, NormalizedQueryHash: 2, NormalizedQuery: "SELECT ? FROM events b", User: "u2"},
		},
		traceIDs: []string{traceID.String()},
		spans:    []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog: map[string]model.QueryLog{
			"q-b": {QueryID: "q-b", QueryDurationMs: 8000, NormalizedQueryHash: 2, NormalizedQuery: "SELECT ? FROM events b", User: "u2"},
		},
	}

	// source=recent, match=events, pick candidate 2 (q-b), stats=default(yes),
	// similar=no, findings=no.
	in := strings.NewReader(strings.Join([]string{
		"recent", // source
		"events", // match
		"2",      // pick candidate 2 -> q-b
		"",       // stats fan-out: default yes
		"n",      // similar fan-out: no
		"n",      // findings fan-out: no
	}, "\n") + "\n")

	var out, errOut bytes.Buffer
	code := runAnalyzeTraceWizard(in, &out, &errOut, src, testTraceConfig(), "", wizardBaseOptions(), "json", "")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}

	// Prompt sequence: the operator should have been asked for source, match,
	// shown a candidate list, asked to select, then asked the fan-out questions.
	body := out.String()
	for _, want := range []string{
		"Candidate source (recent/current/other)",
		"Match (case-insensitive substring",
		"2 candidate(s):",
		"[1] query_id=q-a",
		"[2] query_id=q-b",
		"Select a candidate (1-2)",
		"Include query-log stats fan-out?",
		"Include similar query-family fan-out?",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("prompt transcript missing %q:\n%s", want, body)
		}
	}

	// The JSON report follows the prompts on the same stream; extract and verify
	// the selected candidate is the picked one.
	report := extractWizardReport(t, body)
	if report.SelectedCandidate == nil || report.SelectedCandidate.QueryID != "q-b" {
		t.Fatalf("wizard should drill into the picked candidate q-b, got %+v", report.SelectedCandidate)
	}
	// The drill must have run the query-id -> trace-id lookup for the picked
	// candidate, exactly as a -query-id flag run would.
	if src.traceIDQueryID != "q-b" {
		t.Errorf("trace lookup should use the picked query_id q-b, got %q", src.traceIDQueryID)
	}
	// The source recorded in the report reflects the path taken.
	if report.Source != "recent" {
		t.Errorf("source = %q, want recent", report.Source)
	}
	// similar was declined, so no family fan-out.
	if report.Family != nil {
		t.Errorf("similar declined; family should be nil, got %+v", report.Family)
	}
}

// TestAnalyzeTraceWizard_OtherIdentityTranscript walks the `other` source flow
// with an explicit query-id, the guided equivalent of
// `analyze trace -source other -query-id q-123`.
func TestAnalyzeTraceWizard_OtherIdentityTranscript(t *testing.T) {
	traceID := uuid.New()
	src := &fakeTraceSource{
		traceIDs: []string{traceID.String()},
		spans:    []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog: map[string]model.QueryLog{
			"q-123": {QueryID: "q-123", QueryDurationMs: 4200, User: "etl_user"},
		},
	}

	in := strings.NewReader(strings.Join([]string{
		"other",    // source
		"query-id", // identity kind
		"q-123",    // identity value
		"y",        // stats fan-out
		"n",        // similar fan-out
		"n",        // findings fan-out
	}, "\n") + "\n")

	var out, errOut bytes.Buffer
	code := runAnalyzeTraceWizard(in, &out, &errOut, src, testTraceConfig(), "", wizardBaseOptions(), "json", "")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}

	body := out.String()
	for _, want := range []string{
		"Candidate source (recent/current/other)",
		"Identity kind (query-id/trace-id/normalized-query-hash)",
		"query-id:",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("other-flow transcript missing %q:\n%s", want, body)
		}
	}

	// The candidate search must NOT have run on the explicit-identity path.
	if !src.candidateOpts.StartTime.IsZero() {
		t.Errorf("explicit identity must skip the recent candidate search, but it ran: %+v", src.candidateOpts)
	}

	report := extractWizardReport(t, body)
	if report.SelectedCandidate == nil || report.SelectedCandidate.QueryID != "q-123" {
		t.Fatalf("other/query-id flow should drill q-123, got %+v", report.SelectedCandidate)
	}
	if report.Source != "other" {
		t.Errorf("source = %q, want other", report.Source)
	}
	if report.SuppliedIdentity == nil || report.SuppliedIdentity.QueryID != "q-123" {
		t.Errorf("supplied identity should record the typed query-id, got %+v", report.SuppliedIdentity)
	}
}

// TestAnalyzeTraceWizard_SimilarFanoutSelected proves the fan-out answers reach
// the report path: answering yes to similar runs the query-family fan-out.
func TestAnalyzeTraceWizard_SimilarFanoutSelected(t *testing.T) {
	traceID := uuid.New()
	src := &fakeTraceSource{
		normalized: true,
		traceIDs:   []string{traceID.String()},
		spans:      []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog: map[string]model.QueryLog{
			"q-123": {QueryID: "q-123", NormalizedQueryHash: 777, NormalizedQuery: "SELECT ? FROM events"},
		},
		families: []model.QueryFamilyRollup{
			{FamilyID: "fam-1", RepresentativeQuery: "SELECT ? FROM events", MemberHashesSorted: []uint64{555, 777}},
		},
	}

	in := strings.NewReader(strings.Join([]string{
		"other",    // source
		"query-id", // kind
		"q-123",    // value
		"y",        // stats
		"y",        // similar -> on
		"n",        // findings -> off
	}, "\n") + "\n")

	var out, errOut bytes.Buffer
	code := runAnalyzeTraceWizard(in, &out, &errOut, src, testTraceConfig(), "", wizardBaseOptions(), "json", "")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	report := extractWizardReport(t, out.String())
	if report.Family == nil || report.Family.FamilyID != "fam-1" {
		t.Fatalf("similar=yes should map hash 777 to fam-1, got %+v", report.Family)
	}
}

// TestAnalyzeTraceWizard_CurrentTranscript walks the current-source flow:
// source=current, a match string, the system.processes candidate list, a pick,
// and the fan-out answers. It asserts the source option is offered and that the
// picked running query is drilled, carrying its elapsed metadata even though no
// query-log row exists yet.
func TestAnalyzeTraceWizard_CurrentTranscript(t *testing.T) {
	traceID := uuid.New()
	src := &fakeTraceSource{
		normalized: true,
		currentCandidates: []model.QueryLog{
			{QueryID: "run-a", ElapsedMs: 9000, NormalizedQuery: "SELECT ? FROM events a", User: "u1"},
			{QueryID: "run-b", ElapsedMs: 8000, NormalizedQuery: "SELECT ? FROM events b", User: "u2"},
		},
		traceIDs: []string{traceID.String()},
		spans:    []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog: map[string]model.QueryLog{}, // running: no query-log row yet
	}

	// source=current, match=events, pick candidate 2 (run-b), stats default(yes),
	// similar no, findings no.
	in := strings.NewReader(strings.Join([]string{
		"current", // source
		"events",  // match
		"2",       // pick candidate 2 -> run-b
		"",        // stats fan-out: default yes
		"n",       // similar fan-out: no
		"n",       // findings fan-out: no
	}, "\n") + "\n")

	var out, errOut bytes.Buffer
	code := runAnalyzeTraceWizard(in, &out, &errOut, src, testTraceConfig(), "", wizardBaseOptions(), "json", "")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}

	body := out.String()
	for _, want := range []string{
		"Candidate source (recent/current/other)",
		"2 candidate(s):",
		"[1] query_id=run-a",
		"[2] query_id=run-b",
		"elapsed_ms=8000",
		"Select a candidate (1-2)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("current transcript missing %q:\n%s", want, body)
		}
	}

	report := extractWizardReport(t, body)
	if report.Source != "current" {
		t.Errorf("source = %q, want current", report.Source)
	}
	if report.SelectedCandidate == nil || report.SelectedCandidate.QueryID != "run-b" {
		t.Fatalf("wizard should drill the picked running query run-b, got %+v", report.SelectedCandidate)
	}
	// The picked running query's elapsed metadata must survive into the report.
	if report.SelectedCandidate.ElapsedMs != 8000 {
		t.Errorf("picked candidate elapsed_ms = %d, want 8000", report.SelectedCandidate.ElapsedMs)
	}
	if src.traceIDQueryID != "run-b" {
		t.Errorf("trace lookup should use the picked query_id run-b, got %q", src.traceIDQueryID)
	}
	// The recent search must not have run.
	if !src.candidateOpts.StartTime.IsZero() {
		t.Errorf("current wizard must not run the recent search, but it did: %+v", src.candidateOpts)
	}
}

// TestAnalyzeTraceWizard_FindingsFanoutOffered proves the fan-out prompt now
// offers findings (Phase 5) and that answering yes drives the findings fan-out
// into the report, filtered to the selected family. It uses a skew-tripping
// family so the real registry emits a finding for the selected query.
func TestAnalyzeTraceWizard_FindingsFanoutOffered(t *testing.T) {
	traceID := uuid.New()
	const selHash = 777
	src := &fakeTraceSource{
		normalized: true,
		traceIDs:   []string{traceID.String()},
		spans:      []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog: map[string]model.QueryLog{
			"q-123": {QueryID: "q-123", NormalizedQueryHash: selHash, NormalizedQuery: "SELECT ? FROM events"},
		},
		families: []model.QueryFamilyRollup{
			skewFamily("fam-sel", selHash, "SELECT ? FROM events"),
			skewFamily("fam-other", 111, "SELECT ? FROM other"),
		},
		dimensionCounts: []clickhouse.QueryFamilyDimensionCount{
			skewDimCount(selHash, "etl_user"),
			skewDimCount(111, "other_user"),
		},
	}

	// source=other, query-id=q-123, stats yes, similar no, findings YES.
	in := strings.NewReader(strings.Join([]string{
		"other",    // source
		"query-id", // kind
		"q-123",    // value
		"y",        // stats
		"n",        // similar
		"y",        // findings -> on
	}, "\n") + "\n")

	var out, errOut bytes.Buffer
	code := runAnalyzeTraceWizard(in, &out, &errOut, src, testTraceConfig(), "", wizardBaseOptions(), "json", "")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}

	body := out.String()
	if !strings.Contains(body, "Include analysis findings fan-out") {
		t.Errorf("fan-out prompt should offer findings:\n%s", body)
	}

	report := extractWizardReport(t, body)
	if len(report.Findings) == 0 {
		t.Fatalf("findings=yes should populate findings; warnings: %v", report.Warnings)
	}
	for _, f := range report.Findings {
		if f.FamilyID != "fam-sel" {
			t.Errorf("findings should be filtered to the selected family, leaked %s (family %s)", f.ID, f.FamilyID)
		}
	}
}

// ---------------------------------------------------------------------------
// Parity: a wizard run {source=recent, match=X, pick candidate N} produces the
// same selection/report as the equivalent flag run that resolves to the same
// query-id.
// ---------------------------------------------------------------------------

// TestAnalyzeTraceWizard_ParityWithFlags asserts that a guided pick of
// candidate N produces the identical report (modulo wall-clock fields) as the
// scriptable `-source recent -match X` run where the same candidate was the
// sole/selected one — because both resolve to the same query-id and run the
// same analyzeTraceWithSource path.
func TestAnalyzeTraceWizard_ParityWithFlags(t *testing.T) {
	traceID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	makeSrc := func() *fakeTraceSource {
		return &fakeTraceSource{
			normalized: true,
			candidates: []model.QueryLog{
				{QueryID: "q-pick", QueryDurationMs: 8000, NormalizedQueryHash: 2, NormalizedQuery: "SELECT ? FROM events", User: "u2"},
			},
			traceIDs: []string{traceID.String()},
			spans:    []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
			queryLog: map[string]model.QueryLog{
				"q-pick": {QueryID: "q-pick", QueryDurationMs: 8000, NormalizedQueryHash: 2, NormalizedQuery: "SELECT ? FROM events", User: "u2"},
			},
		}
	}

	// (a) Wizard run: recent, match=events, pick candidate 1 (the only one),
	// stats yes, similar no, findings no.
	wizardSrc := makeSrc()
	in := strings.NewReader(strings.Join([]string{"recent", "events", "1", "y", "n", "n"}, "\n") + "\n")
	var wizOut, wizErr bytes.Buffer
	if code := runAnalyzeTraceWizard(in, &wizOut, &wizErr, wizardSrc, testTraceConfig(), "", wizardBaseOptions(), "json", ""); code != 0 {
		t.Fatalf("wizard exit = %d, want 0; stderr:\n%s", code, wizErr.String())
	}
	wizReport := extractWizardReport(t, wizOut.String())

	// (b) Flag run: the equivalent non-interactive recent search that auto-
	// selects the single candidate (-source recent -match events). With one
	// candidate the non-interactive path drills in directly.
	flagSrc := makeSrc()
	flagOpts := wizardBaseOptions()
	flagOpts.Source = "recent"
	flagOpts.Match = "events"
	flagOpts.Fanout = fanoutSet{Trace: true, Stats: true}
	var flagOut, flagErr bytes.Buffer
	if code := analyzeTraceWithSource(flagSrc, testTraceConfig(), "", flagOpts, "json", "", &flagOut, &flagErr); code != 0 {
		t.Fatalf("flag run exit = %d, want 0; stderr:\n%s", code, flagErr.String())
	}
	var flagReport analysis.TraceDrilldownReport
	if err := json.Unmarshal(flagOut.Bytes(), &flagReport); err != nil {
		t.Fatalf("flag run output is not a report: %v\n%s", err, flagOut.String())
	}

	// The two must agree on the load-bearing selection and report shape. The
	// wall-clock fields (GeneratedAt/Window) differ by run instant, so normalize
	// them before comparing.
	normalizeReportClock(&wizReport)
	normalizeReportClock(&flagReport)

	wizJSON, _ := json.MarshalIndent(wizReport, "", "  ")
	flagJSON, _ := json.MarshalIndent(flagReport, "", "  ")
	if string(wizJSON) != string(flagJSON) {
		t.Errorf("wizard and flag runs diverged:\n--- wizard ---\n%s\n--- flag ---\n%s", wizJSON, flagJSON)
	}
	// And both must have driven the same query-id into the trace lookup.
	if wizardSrc.traceIDQueryID != flagSrc.traceIDQueryID || wizardSrc.traceIDQueryID != "q-pick" {
		t.Errorf("both runs should drill q-pick; wizard=%q flag=%q", wizardSrc.traceIDQueryID, flagSrc.traceIDQueryID)
	}
}

// ---------------------------------------------------------------------------
// Headless / TTY-gating: -wizard without a TTY must not block.
// ---------------------------------------------------------------------------

// TestAnalyzeTrace_WizardWithoutTTYExits2 pins the non-TTY posture: running
// `analyze trace -wizard` without an interactive terminal must error and exit 2
// rather than block. The TTY probe is pinned to false via the analyzeTraceIsTTY
// seam so the assertion is deterministic regardless of the test harness's stdin
// device (which is often /dev/null, itself a character device).
func TestAnalyzeTrace_WizardWithoutTTYExits2(t *testing.T) {
	prev := analyzeTraceIsTTY
	analyzeTraceIsTTY = func() bool { return false }
	defer func() { analyzeTraceIsTTY = prev }()

	var out, errOut bytes.Buffer
	code := runAnalyzeTrace([]string{"-wizard"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 for -wizard without a TTY", code)
	}
	if !strings.Contains(errOut.String(), "-wizard requires an interactive terminal") {
		t.Errorf("stderr missing TTY-required message:\n%s", errOut.String())
	}
	// The gate must fire before any prompt or config load: stdout stays empty.
	if out.Len() != 0 {
		t.Errorf("headless -wizard should not print prompts, got:\n%s", out.String())
	}
}

// ---------------------------------------------------------------------------
// EOF / no-candidate handling: never block, sane exit codes.
// ---------------------------------------------------------------------------

// TestAnalyzeTraceWizard_TruncatedStdinExits1 confirms a stdin that closes
// mid-wizard does not block: the EOF guard fails the run (exit 1) rather than
// drilling on silent defaults.
func TestAnalyzeTraceWizard_TruncatedStdinExits1(t *testing.T) {
	src := &fakeTraceSource{normalized: true}
	// Only the source answer, then EOF before the match/identity prompt.
	in := strings.NewReader("recent\n")
	var out, errOut bytes.Buffer
	code := runAnalyzeTraceWizard(in, &out, &errOut, src, testTraceConfig(), "", wizardBaseOptions(), "json", "")
	if code != 1 {
		t.Fatalf("truncated stdin should exit 1, got %d; stderr:\n%s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "stdin closed before the wizard finished") {
		t.Errorf("stderr missing truncated-stdin message:\n%s", errOut.String())
	}
	// Must not have drilled into any trace.
	if src.traceIDQueryID != "" {
		t.Errorf("truncated wizard must not drill, drilled %q", src.traceIDQueryID)
	}
}

// TestAnalyzeTraceWizard_NoCandidatesCleanStop confirms an empty candidate list
// is a clean stop (exit 0) with narrowing guidance, not a failure — the wizard
// can't select what isn't there.
func TestAnalyzeTraceWizard_NoCandidatesCleanStop(t *testing.T) {
	src := &fakeTraceSource{normalized: true, candidates: nil}
	in := strings.NewReader(strings.Join([]string{"recent", "nomatch"}, "\n") + "\n")
	var out, errOut bytes.Buffer
	code := runAnalyzeTraceWizard(in, &out, &errOut, src, testTraceConfig(), "", wizardBaseOptions(), "json", "")
	if code != 0 {
		t.Fatalf("no-candidate wizard should exit 0 (clean stop), got %d; stderr:\n%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "No candidates matched") {
		t.Errorf("expected a narrowing note, got:\n%s", out.String())
	}
	if src.traceIDQueryID != "" {
		t.Errorf("no-candidate wizard must not drill, drilled %q", src.traceIDQueryID)
	}
}

// TestAnalyzeTraceWizard_BadSelectionReprompts confirms an out-of-range pick
// re-prompts rather than aborting, preserving the candidate list the operator
// already saw.
func TestAnalyzeTraceWizard_BadSelectionReprompts(t *testing.T) {
	traceID := uuid.New()
	src := &fakeTraceSource{
		normalized: true,
		candidates: []model.QueryLog{
			{QueryID: "q-a", NormalizedQuery: "SELECT ? FROM a"},
			{QueryID: "q-b", NormalizedQuery: "SELECT ? FROM b"},
		},
		traceIDs: []string{traceID.String()},
		spans:    []model.OpenTelemetrySpan{makeSpan(traceID, 1, 0, "Query", 1_000_000, 2_000_000)},
		queryLog: map[string]model.QueryLog{"q-b": {QueryID: "q-b"}},
	}
	// Bad pick "5" (out of range), then "2".
	in := strings.NewReader(strings.Join([]string{"recent", "", "5", "2", "y", "n", "n"}, "\n") + "\n")
	var out, errOut bytes.Buffer
	code := runAnalyzeTraceWizard(in, &out, &errOut, src, testTraceConfig(), "", wizardBaseOptions(), "json", "")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "invalid selection") {
		t.Errorf("expected an invalid-selection re-prompt error, got:\n%s", errOut.String())
	}
	report := extractWizardReport(t, out.String())
	if report.SelectedCandidate == nil || report.SelectedCandidate.QueryID != "q-b" {
		t.Fatalf("re-prompt should land on q-b, got %+v", report.SelectedCandidate)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// extractWizardReport pulls the JSON report off the wizard's combined stdout
// (prompts + report share the stream). The report starts at the first '{'.
func extractWizardReport(t *testing.T, body string) analysis.TraceDrilldownReport {
	t.Helper()
	i := strings.Index(body, "{")
	if i < 0 {
		t.Fatalf("no JSON report found in wizard output:\n%s", body)
	}
	var report analysis.TraceDrilldownReport
	if err := json.Unmarshal([]byte(body[i:]), &report); err != nil {
		t.Fatalf("wizard report is not valid JSON: %v\n%s", err, body[i:])
	}
	return report
}

// normalizeReportClock zeroes the wall-clock fields so two reports produced at
// different instants compare equal on their load-bearing content.
func normalizeReportClock(r *analysis.TraceDrilldownReport) {
	r.GeneratedAt = time.Time{}
	r.Window = analysis.AnalysisWindow{}
}
