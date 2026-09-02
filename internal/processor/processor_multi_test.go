package processor

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/export"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/resilience"
)

// These tests prove that a partial multi-sink failure (one sink fails,
// another succeeds) is surfaced as a real export error to every
// downstream consumer that already treats err != nil as a failed
// cycle: backfill BatchResult accounting, the cycle metrics counter,
// and the circuit breaker. Before #92 this path silently returned
// nil, hiding the dead sink from operators.

func newMultiExporterPartialFailure(t *testing.T) (*export.MultiExporter, *mockExporter, *mockExporter) {
	t.Helper()
	good := &mockExporter{}
	bad := &mockExporter{
		exportSpansFunc: func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
			return nil, errors.New("splunk down")
		},
		exportQueryFunc: func(context.Context, model.QueryLog) error {
			return errors.New("splunk down")
		},
	}
	multi := export.NewMultiExporter(
		[]model.SpanExporter{good, bad},
		[]string{"otel", "splunk"},
	)
	return multi, good, bad
}

// Backfill path: ProcessQueriesBatch must classify a partial multi-sink
// failure as a per-query Failed and surface it via FirstErr so
// ReportBackfillOutcome routes the run to the failed branch.
func TestProcessQueriesBatch_MultiExporter_PartialFailure(t *testing.T) {
	multi, good, bad := newMultiExporterPartialFailure(t)

	qf, err := filter.NewQueryFilter(config.FiltersConfig{})
	if err != nil {
		t.Fatalf("NewQueryFilter failed: %v", err)
	}

	queries := []model.QueryLog{
		{QueryID: "q1", Query: "SELECT 1"},
		{QueryID: "q2", Query: "SELECT 2"},
	}

	got := ProcessQueriesBatch(context.Background(), queries, multi, qf, &config.Config{}, nil)

	if got.Exported != 0 {
		t.Errorf("Exported = %d, want 0 (partial multi-sink failure must not count as success)", got.Exported)
	}
	if got.Failed != len(queries) {
		t.Errorf("Failed = %d, want %d", got.Failed, len(queries))
	}
	if !got.HasFailures() {
		t.Error("HasFailures() = false; partial multi-sink failure must surface as a batch failure")
	}
	if got.FirstErr == nil {
		t.Fatal("FirstErr is nil; expected wrapped sink error")
	}
	if good.exportQueryCalls != len(queries) || bad.exportQueryCalls != len(queries) {
		t.Errorf("expected each sink called once per query, got good=%d bad=%d",
			good.exportQueryCalls, bad.exportQueryCalls)
	}
}

// Scheduled path: the inner export call inside fetchAndProcessSpans is
// `exporter.ExportSpans(...)`. We replicate that call here to prove
// MultiExporter's partial-failure error reaches the processor's err
// branch and that the processor would *not* mark any span as seen.
func TestExportSpans_MultiExporter_PartialFailureSurfacesError(t *testing.T) {
	multi, good, bad := newMultiExporterPartialFailure(t)

	spans := []model.OpenTelemetrySpan{
		{SpanID: 1, TraceID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), OperationName: "op1"},
		{SpanID: 2, TraceID: uuid.MustParse("22222222-2222-2222-2222-222222222222"), OperationName: "op2"},
	}

	result, err := multi.ExportSpans(context.Background(), spans)
	if err == nil {
		t.Fatal("ExportSpans returned nil error on partial multi-sink failure")
	}
	if result.Accepted != nil {
		t.Errorf("expected nil keys on error so processor cannot mark any span as seen, got %v", result.Accepted)
	}
	if len(result.Sinks) != 2 {
		t.Fatalf("expected two sink statuses, got %d", len(result.Sinks))
	}
	if result.Sinks[0].Name != "otel" || result.Sinks[0].Accepted != len(spans) || result.Sinks[0].Error != nil {
		t.Errorf("good sink status = %+v, want otel accepted=%d with nil error", result.Sinks[0], len(spans))
	}
	if result.Sinks[1].Name != "splunk" || result.Sinks[1].Accepted != 0 || result.Sinks[1].Error == nil {
		t.Errorf("bad sink status = %+v, want splunk accepted=0 with error", result.Sinks[1])
	}
	if good.exportSpansCalls != 1 || bad.exportSpansCalls != 1 {
		t.Errorf("expected each sink called once, got good=%d bad=%d",
			good.exportSpansCalls, bad.exportSpansCalls)
	}
}

func TestExportSpansWithDeadline_ShapesBeforeMultiExporterFanOut(t *testing.T) {
	assertSafe := func(_ context.Context, spans []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
		if len(spans) != 1 {
			t.Fatalf("received %d spans, want 1", len(spans))
		}
		if _, ok := spans[0].Attributes["db.statement"]; ok {
			t.Fatal("sink received raw db.statement before fan-out")
		}
		if spans[0].Attributes["query_log.normalized_query"] != "SELECT ?" {
			t.Fatal("sink did not receive the safe normalized representation")
		}
		return []model.SpanKey{model.KeyOf(spans[0])}, nil
	}
	left := &mockExporter{exportSpansFunc: assertSafe}
	right := &mockExporter{exportSpansFunc: assertSafe}
	multi := export.NewMultiExporter([]model.SpanExporter{left, right}, []string{"otel", "splunk"})
	cfg := &config.Config{Filters: config.FiltersConfig{QueryTextMode: config.QueryTextModeNormalizedOnly}}
	qf, err := filter.NewQueryFilter(cfg.Filters)
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}
	span := model.OpenTelemetrySpan{
		TraceID: uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		SpanID:  3,
		Attributes: map[string]string{
			"db.statement":               "SELECT 123",
			"query_log.normalized_query": "SELECT ?",
		},
	}
	result, err := ExportSpansWithDeadline(context.Background(), cfg, multi, qf, []model.OpenTelemetrySpan{span})
	if err != nil {
		t.Fatalf("ExportSpansWithDeadline: %v", err)
	}
	if result.TotalAccepted != 1 || left.exportSpansCalls != 1 || right.exportSpansCalls != 1 {
		t.Fatalf("fan-out result = %+v, calls=%d/%d", result, left.exportSpansCalls, right.exportSpansCalls)
	}
	if span.Attributes["db.statement"] == "" {
		t.Fatal("privacy boundary mutated the processor's input span")
	}
}

// The cycle metrics + circuit breaker logic in processor.go is keyed
// purely off `err != nil`; this test proves the composition by feeding
// the partial-failure error from MultiExporter into the same helpers
// the live cycle uses, asserting both error counter and breaker state
// transition correctly.
func TestPartialMultiExporterFailure_TriggersBreakerAndMetrics(t *testing.T) {
	multi, _, _ := newMultiExporterPartialFailure(t)

	_, exportErr := multi.ExportSpans(context.Background(), []model.OpenTelemetrySpan{
		{SpanID: 1, TraceID: uuid.MustParse("11111111-1111-1111-1111-111111111111")},
	})
	if exportErr == nil {
		t.Fatal("expected partial failure to return an error")
	}

	m := metrics.NewMetrics()
	cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 2,
		SuccessThreshold: 1,
		ResetTimeoutS:    60,
	})

	// Two consecutive partial failures should trip the breaker, exactly
	// like two all-fail cycles would.
	for range 2 {
		m.RecordCycle(0, 0, 0, 1, exportErr)
		updateCircuitBreakerState(cb, exportErr, m, nil)
	}

	if cb.State() != resilience.CircuitOpen {
		t.Errorf("circuit breaker state = %v, want CircuitOpen after 2 partial-failure cycles", cb.State())
	}

	snap := m.Snapshot()
	if snap.LastCycle.Err == "" {
		t.Error("metrics last cycle Err is empty; partial failure should land in /status as a real error")
	}
}
