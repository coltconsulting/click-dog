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

// Scheduled path: Pipeline.Process must surface a partial multi-sink failure
// and must not mark any span as seen.
func TestPipeline_MultiExporterPartialFailureSurfacesError(t *testing.T) {
	multi, good, bad := newMultiExporterPartialFailure(t)

	spans := []model.OpenTelemetrySpan{
		{SpanID: 1, TraceID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), OperationName: "op1"},
		{SpanID: 2, TraceID: uuid.MustParse("22222222-2222-2222-2222-222222222222"), OperationName: "op2"},
	}

	reader := &mockLiveReader{healthy: true, spans: spans}
	m := metrics.NewMetrics()
	pipeline := mustLivePipeline(t, reader, multi, &config.Config{}, nil, m)
	err := pipeline.Process(context.Background())
	if err == nil {
		t.Fatal("Pipeline.Process returned nil error on partial multi-sink failure")
	}
	for _, span := range spans {
		if pipeline.SeenSpans.Contains(model.KeyOf(span)) {
			t.Errorf("failed span %v was marked seen", model.KeyOf(span))
		}
	}
	if good.exportSpansCalls != 1 || bad.exportSpansCalls != 1 {
		t.Errorf("expected each sink called once, got good=%d bad=%d",
			good.exportSpansCalls, bad.exportSpansCalls)
	}
	if m.Snapshot().LastCycle.Err == "" {
		t.Error("partial failure did not reach the processor cycle error")
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

// Repeated partial failures must flow through the live cycle's metrics and
// circuit-breaker updates exactly like all-sink failures.
func TestPartialMultiExporterFailure_TriggersBreakerAndMetrics(t *testing.T) {
	multi, good, bad := newMultiExporterPartialFailure(t)
	span := model.OpenTelemetrySpan{
		SpanID:  1,
		TraceID: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
	}
	reader := &mockLiveReader{healthy: true, spans: []model.OpenTelemetrySpan{span}}

	m := metrics.NewMetrics()
	cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 2,
		SuccessThreshold: 1,
		ResetTimeoutS:    60,
	})
	pipeline := mustLivePipeline(t, reader, multi, &config.Config{}, nil, m)
	pipeline.CircuitBreaker = cb

	// Two consecutive partial failures should trip the breaker, exactly
	// like two all-fail cycles would.
	for cycle := 1; cycle <= 2; cycle++ {
		if err := pipeline.Process(context.Background()); err == nil {
			t.Fatalf("cycle %d returned nil error", cycle)
		}
	}

	if cb.State() != resilience.CircuitOpen {
		t.Errorf("circuit breaker state = %v, want CircuitOpen after 2 partial-failure cycles", cb.State())
	}

	snap := m.Snapshot()
	if snap.LastCycle.Err == "" {
		t.Error("metrics last cycle Err is empty; partial failure should land in /status as a real error")
	}
	if good.exportSpansCalls != 2 || bad.exportSpansCalls != 2 {
		t.Errorf("each sink should be retried twice, got good=%d bad=%d", good.exportSpansCalls, bad.exportSpansCalls)
	}
	if pipeline.SeenSpans.Contains(model.KeyOf(span)) {
		t.Error("span was marked seen after repeated partial failures")
	}
}
