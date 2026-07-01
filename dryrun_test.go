package main

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/model"
)

func TestDryRunExporter_ExportSpans(t *testing.T) {
	exp := NewDryRunExporter()

	trace1 := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	trace2 := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	spans := []model.OpenTelemetrySpan{
		{TraceID: trace1, SpanID: 100, OperationName: "op-a"},
		{TraceID: trace1, SpanID: 101, OperationName: "op-b"},
		{TraceID: trace2, SpanID: 200, OperationName: "op-a"},
	}

	result, err := exp.ExportSpans(context.Background(), spans)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Accepted) != 3 {
		t.Fatalf("expected 3 exported IDs, got %d", len(result.Accepted))
	}
	if result.TotalSent != 3 || result.TotalAccepted != 3 {
		t.Fatalf("result counts = sent %d accepted %d, want 3/3", result.TotalSent, result.TotalAccepted)
	}

	exp.mu.Lock()
	defer exp.mu.Unlock()

	if exp.spanCount != 3 {
		t.Errorf("expected 3 spans, got %d", exp.spanCount)
	}
	if len(exp.traceIDs) != 2 {
		t.Errorf("expected 2 unique traces, got %d", len(exp.traceIDs))
	}
	if exp.operations["op-a"] != 2 {
		t.Errorf("expected op-a count 2, got %d", exp.operations["op-a"])
	}
	if exp.operations["op-b"] != 1 {
		t.Errorf("expected op-b count 1, got %d", exp.operations["op-b"])
	}
}

func TestDryRunExporter_ExportQuery(t *testing.T) {
	exp := NewDryRunExporter()

	result, err := exp.ExportQuery(context.Background(), model.QueryLog{QueryID: "q-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.TotalSent != 1 || result.TotalAccepted != 1 {
		t.Fatalf("result counts = sent %d accepted %d, want 1/1", result.TotalSent, result.TotalAccepted)
	}
	_, err = exp.ExportQuery(context.Background(), model.QueryLog{QueryID: "q-2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	exp.mu.Lock()
	defer exp.mu.Unlock()

	if exp.queryCount != 2 {
		t.Errorf("expected 2 queries, got %d", exp.queryCount)
	}
}

func TestDryRunExporter_Close(t *testing.T) {
	exp := NewDryRunExporter()
	if err := exp.Close(context.Background()); err != nil {
		t.Errorf("unexpected error from Close: %v", err)
	}
}

func TestDryRunExporter_PrintSummary(t *testing.T) {
	exp := NewDryRunExporter()

	// Populate some data
	_, _ = exp.ExportSpans(context.Background(), []model.OpenTelemetrySpan{
		{TraceID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), SpanID: 1, OperationName: "op-a"},
		{TraceID: uuid.MustParse("22222222-2222-2222-2222-222222222222"), SpanID: 2, OperationName: "op-b"},
	})

	// Just ensure PrintSummary doesn't panic
	exp.PrintSummary()
}
