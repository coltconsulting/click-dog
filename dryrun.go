package main

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/coltconsulting/click-dog/internal/model"
)

// Compile-time check that *DryRunExporter satisfies model.SpanExporter.
var _ model.SpanExporter = (*DryRunExporter)(nil)

// DryRunExporter captures export calls without sending data anywhere.
// Used with --dry-run to preview what would be exported.
type DryRunExporter struct {
	mu         sync.Mutex
	spanCount  int
	queryCount int
	traceIDs   map[string]bool
	operations map[string]int
}

func NewDryRunExporter() *DryRunExporter {
	return &DryRunExporter{
		traceIDs:   make(map[string]bool),
		operations: make(map[string]int),
	}
}

func (d *DryRunExporter) ExportSpans(_ context.Context, spans []model.OpenTelemetrySpan) (model.ExportResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	keys := make([]model.SpanKey, len(spans))
	for i, span := range spans {
		d.spanCount++
		d.traceIDs[span.TraceID.String()] = true
		d.operations[span.OperationName]++
		keys[i] = model.KeyOf(span)
	}
	return model.ExportResult{
		Accepted:      keys,
		TotalSent:     len(spans),
		TotalAccepted: len(keys),
		Sinks: []model.ExportSinkStatus{{
			Name:     "dry_run",
			Sent:     len(spans),
			Accepted: len(keys),
		}},
	}, nil
}

func (d *DryRunExporter) ExportQuery(_ context.Context, q model.QueryLog) (model.ExportResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.queryCount++
	d.traceIDs[q.QueryID] = true
	return model.ExportResult{
		TotalSent:     1,
		TotalAccepted: 1,
		Sinks: []model.ExportSinkStatus{{
			Name:     "dry_run",
			Sent:     1,
			Accepted: 1,
		}},
	}, nil
}

func (d *DryRunExporter) Close(_ context.Context) error {
	return nil
}

func (d *DryRunExporter) PrintSummary() {
	d.mu.Lock()
	defer d.mu.Unlock()

	fmt.Println("\n--- Dry Run Summary ---")
	fmt.Printf("Traces:     %d\n", len(d.traceIDs))
	fmt.Printf("Spans:      %d\n", d.spanCount)
	fmt.Printf("Queries:    %d\n", d.queryCount)

	if len(d.operations) > 0 {
		fmt.Println("\nTop operations:")

		type opCount struct {
			name  string
			count int
		}
		ops := make([]opCount, 0, len(d.operations))
		for name, count := range d.operations {
			ops = append(ops, opCount{name, count})
		}
		sort.Slice(ops, func(i, j int) bool { return ops[i].count > ops[j].count })

		limit := 10
		if len(ops) < limit {
			limit = len(ops)
		}
		for _, op := range ops[:limit] {
			fmt.Printf("  %5d  %s\n", op.count, op.name)
		}
	}
}
