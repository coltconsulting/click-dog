package processor

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
)

func runLiveDedupCycle(
	t *testing.T,
	reader *mockLiveReader,
	seenSpans *lru.Cache[model.SpanKey, bool],
	exporter model.SpanExporter,
	spans []model.OpenTelemetrySpan,
) (metrics.CycleSnapshot, error) {
	t.Helper()

	reader.spans = spans
	m := metrics.NewMetrics()
	pipeline := mustLivePipeline(t, reader, exporter, &config.Config{}, seenSpans, m)

	err := pipeline.Process(context.Background())
	return m.Snapshot().LastCycle, err
}

func mustDedupCache(t *testing.T) *lru.Cache[model.SpanKey, bool] {
	t.Helper()
	c, err := lru.New[model.SpanKey, bool](1024)
	if err != nil {
		t.Fatalf("lru.New: %v", err)
	}
	return c
}

// Same span_id under different trace_ids must both be exported. OTLP span
// IDs are only unique within a trace, so a span-id-only cache would
// silently drop the second arrival.
func TestDedup_SameSpanIDDifferentTraces_BothExported(t *testing.T) {
	traceA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	traceB := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	const collidingSpanID uint64 = 42

	spans := []model.OpenTelemetrySpan{
		{TraceID: traceA, SpanID: collidingSpanID, OperationName: "trace-a-op"},
		{TraceID: traceB, SpanID: collidingSpanID, OperationName: "trace-b-op"},
	}

	exp := &mockExporter{}
	cache := mustDedupCache(t)
	reader := &mockLiveReader{healthy: true}

	cycle, err := runLiveDedupCycle(t, reader, cache, exp, spans)
	if err != nil {
		t.Fatalf("Pipeline.Process returned err: %v", err)
	}
	if cycle.Exported != 2 {
		t.Errorf("exported = %d, want 2 (both spans must be exported despite shared span_id)", cycle.Exported)
	}
	if cycle.Duplicates != 0 {
		t.Errorf("duplicates = %d, want 0 (different trace_ids are not duplicates)", cycle.Duplicates)
	}
	if !cache.Contains(model.SpanKey{TraceID: traceA, SpanID: collidingSpanID}) {
		t.Error("cache missing key for trace A")
	}
	if !cache.Contains(model.SpanKey{TraceID: traceB, SpanID: collidingSpanID}) {
		t.Error("cache missing key for trace B")
	}
}

// Replaying the exact same (trace_id, span_id) after a successful export
// must be suppressed.
func TestDedup_SameTraceAndSpanIDReplay_SuppressedAfterExport(t *testing.T) {
	trace := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	span := model.OpenTelemetrySpan{TraceID: trace, SpanID: 7, OperationName: "op"}

	exp := &mockExporter{}
	cache := mustDedupCache(t)
	reader := &mockLiveReader{healthy: true}

	cycle1, err := runLiveDedupCycle(t, reader, cache, exp, []model.OpenTelemetrySpan{span})
	if err != nil {
		t.Fatalf("first cycle err: %v", err)
	}
	if cycle1.Exported != 1 || cycle1.Duplicates != 0 {
		t.Fatalf("first cycle: exported=%d duplicates=%d, want 1/0", cycle1.Exported, cycle1.Duplicates)
	}

	cycle2, err := runLiveDedupCycle(t, reader, cache, exp, []model.OpenTelemetrySpan{span})
	if err != nil {
		t.Fatalf("second cycle err: %v", err)
	}
	if cycle2.Exported != 0 {
		t.Errorf("second cycle exported = %d, want 0 (replay must be suppressed)", cycle2.Exported)
	}
	if cycle2.Duplicates != 1 {
		t.Errorf("second cycle duplicates = %d, want 1", cycle2.Duplicates)
	}
	if exp.exportSpansCalls != 1 {
		t.Errorf("ExportSpans calls = %d, want 1 (no second export)", exp.exportSpansCalls)
	}
}

// Repeating the same (trace_id, span_id) inside a single fetched cycle must
// be suppressed before export, even though the cross-cycle cache is only
// populated after the exporter reports accepted keys.
func TestDedup_SameCycleDuplicate_SuppressedBeforeExport(t *testing.T) {
	trace := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	first := model.OpenTelemetrySpan{TraceID: trace, SpanID: 9, OperationName: "first"}
	second := model.OpenTelemetrySpan{TraceID: trace, SpanID: 9, OperationName: "second"}

	var sent []model.OpenTelemetrySpan
	exp := &mockExporter{
		exportSpansFunc: func(_ context.Context, in []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
			sent = append(sent, in...)
			keys := make([]model.SpanKey, len(in))
			for i, s := range in {
				keys[i] = model.KeyOf(s)
			}
			return keys, nil
		},
	}
	cache := mustDedupCache(t)
	reader := &mockLiveReader{healthy: true}

	cycle, err := runLiveDedupCycle(t, reader, cache, exp, []model.OpenTelemetrySpan{first, second})
	if err != nil {
		t.Fatalf("Pipeline.Process returned err: %v", err)
	}
	if cycle.Exported != 1 {
		t.Errorf("exported = %d, want 1", cycle.Exported)
	}
	if cycle.Duplicates != 1 {
		t.Errorf("duplicates = %d, want 1", cycle.Duplicates)
	}
	if len(sent) != 1 {
		t.Fatalf("exporter received %d spans, want 1", len(sent))
	}
	if sent[0].OperationName != "first" {
		t.Errorf("exported operation = %q, want first occurrence", sent[0].OperationName)
	}
	if !cache.Contains(model.KeyOf(first)) {
		t.Error("accepted first occurrence must be marked seen")
	}
}

// A failed export must not mark any of the unexported spans as seen so
// the next cycle re-attempts them. Mirrors the all-fail contract from
// the MultiExporter all-required design.
func TestDedup_ExportFailure_DoesNotMarkSeen(t *testing.T) {
	trace := uuid.MustParse("45454545-4545-4545-4545-454545454545")
	span := model.OpenTelemetrySpan{TraceID: trace, SpanID: 9, OperationName: "op"}

	exp := &mockExporter{
		exportSpansFunc: func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
			return nil, errors.New("collector down")
		},
	}
	cache := mustDedupCache(t)
	reader := &mockLiveReader{healthy: true}

	if _, err := runLiveDedupCycle(t, reader, cache, exp, []model.OpenTelemetrySpan{span}); err == nil {
		t.Fatal("expected error from failing exporter")
	}
	if cache.Contains(model.KeyOf(span)) {
		t.Error("failed export must not mark the span as seen")
	}
}

// A partial-success export — only some keys returned by the exporter —
// must mark exactly the returned keys as seen and leave the rest
// available for retry. This matches MultiExporter's intersection
// contract: callers must trust the returned key slice as ground truth.
func TestDedup_PartialSuccess_OnlyReturnedKeysMarkedSeen(t *testing.T) {
	traceA := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	traceB := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	traceC := uuid.MustParse("77777777-7777-7777-7777-777777777777")

	spans := []model.OpenTelemetrySpan{
		{TraceID: traceA, SpanID: 1},
		{TraceID: traceB, SpanID: 2},
		{TraceID: traceC, SpanID: 3},
	}

	// Exporter returns a strict subset — span B is *not* in the
	// returned slice, simulating an exporter that accepted A and C
	// but silently rejected B (e.g. the MultiExporter intersection
	// pruned a key that one sink failed to acknowledge).
	exp := &mockExporter{
		exportSpansFunc: func(_ context.Context, in []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
			out := make([]model.SpanKey, 0, len(in))
			for _, s := range in {
				if s.TraceID == traceB {
					continue
				}
				out = append(out, model.KeyOf(s))
			}
			return out, nil
		},
	}
	cache := mustDedupCache(t)
	reader := &mockLiveReader{healthy: true}

	cycle, err := runLiveDedupCycle(t, reader, cache, exp, spans)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cycle.Exported != 2 {
		t.Errorf("exported = %d, want 2 accepted spans", cycle.Exported)
	}

	if !cache.Contains(model.SpanKey{TraceID: traceA, SpanID: 1}) {
		t.Error("trace A span 1 must be marked seen (returned by exporter)")
	}
	if cache.Contains(model.SpanKey{TraceID: traceB, SpanID: 2}) {
		t.Error("trace B span 2 must NOT be marked seen (omitted from exporter result)")
	}
	if !cache.Contains(model.SpanKey{TraceID: traceC, SpanID: 3}) {
		t.Error("trace C span 3 must be marked seen (returned by exporter)")
	}
}

// Same-cycle duplicate suppression must not poison the cross-cycle cache
// for a key omitted from the exporter's accepted set. The duplicate copy is
// dropped within the cycle, but the key remains retryable on the next cycle.
func TestDedup_SameCycleDuplicatePartialSuccess_UnacceptedKeyRetriesNextCycle(t *testing.T) {
	traceA := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	traceB := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	traceC := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

	spanA := model.OpenTelemetrySpan{TraceID: traceA, SpanID: 1, OperationName: "accepted-a"}
	spanB := model.OpenTelemetrySpan{TraceID: traceB, SpanID: 2, OperationName: "retry-me"}
	spanBDuplicate := model.OpenTelemetrySpan{TraceID: traceB, SpanID: 2, OperationName: "retry-me-duplicate"}
	spanC := model.OpenTelemetrySpan{TraceID: traceC, SpanID: 3, OperationName: "accepted-c"}

	var calls [][]model.OpenTelemetrySpan
	exp := &mockExporter{
		exportSpansFunc: func(_ context.Context, in []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
			copied := append([]model.OpenTelemetrySpan(nil), in...)
			calls = append(calls, copied)

			keys := make([]model.SpanKey, 0, len(in))
			for _, s := range in {
				if len(calls) == 1 && s.TraceID == traceB {
					continue
				}
				keys = append(keys, model.KeyOf(s))
			}
			return keys, nil
		},
	}
	cache := mustDedupCache(t)
	reader := &mockLiveReader{healthy: true}

	cycle1, err := runLiveDedupCycle(t, reader, cache, exp, []model.OpenTelemetrySpan{
		spanA,
		spanB,
		spanBDuplicate,
		spanC,
	})
	if err != nil {
		t.Fatalf("first cycle err: %v", err)
	}
	if cycle1.Exported != 2 {
		t.Errorf("first cycle exported = %d, want 2", cycle1.Exported)
	}
	if cycle1.Duplicates != 1 {
		t.Errorf("first cycle duplicates = %d, want 1", cycle1.Duplicates)
	}
	if len(calls) != 1 {
		t.Fatalf("export calls after first cycle = %d, want 1", len(calls))
	}
	if got := len(calls[0]); got != 3 {
		t.Fatalf("first export received %d spans, want 3", got)
	}
	for _, s := range calls[0] {
		if s.OperationName == spanBDuplicate.OperationName {
			t.Fatal("same-cycle duplicate was sent to exporter")
		}
	}
	if cache.Contains(model.KeyOf(spanB)) {
		t.Error("unaccepted span B must not be marked seen after first cycle")
	}

	cycle2, err := runLiveDedupCycle(t, reader, cache, exp, []model.OpenTelemetrySpan{spanB})
	if err != nil {
		t.Fatalf("second cycle err: %v", err)
	}
	if cycle2.Exported != 1 {
		t.Errorf("second cycle exported = %d, want 1", cycle2.Exported)
	}
	if cycle2.Duplicates != 0 {
		t.Errorf("second cycle duplicates = %d, want 0", cycle2.Duplicates)
	}
	if len(calls) != 2 {
		t.Fatalf("export calls after second cycle = %d, want 2", len(calls))
	}
	if len(calls[1]) != 1 || model.KeyOf(calls[1][0]) != model.KeyOf(spanB) {
		t.Fatalf("second export = %+v, want only span B", calls[1])
	}
	if !cache.Contains(model.KeyOf(spanB)) {
		t.Error("retry-accepted span B must be marked seen")
	}
}
