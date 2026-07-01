//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/model"
)

func TestIntegration_ExportSpansToRealCollector(t *testing.T) {
	waitForOTELCollector(t, 30*time.Second)
	clearOTELOutput(t)

	exporter := newOTELExporter(t, "integration-otel-test")
	defer exporter.Close(context.Background())

	now := time.Now()
	spans := []model.OpenTelemetrySpan{
		{
			Hostname:      "test-host",
			TraceID:       uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
			SpanID:        99001,
			OperationName: "integration-test-span",
			Kind:          "INTERNAL",
			StartTimeUs:   uint64(now.Add(-2 * time.Second).UnixMicro()),
			FinishTimeUs:  uint64(now.UnixMicro()),
			FinishDate:    now,
			Attributes:    map[string]string{"test.marker": "true"},
		},
	}

	ctx := context.Background()
	baselineCount := countOTELSpanNames(readOTELFileExporterSpans(t), "integration-test-span")
	if _, err := exporter.ExportSpans(ctx, spans); err != nil {
		t.Fatalf("ExportSpans failed: %v", err)
	}

	waitForOTELSpanNameCount(t, "integration-test-span", baselineCount+1, 10*time.Second)
}

func TestIntegration_ExportBatchToRealCollector(t *testing.T) {
	waitForOTELCollector(t, 30*time.Second)
	clearOTELOutput(t)

	exporter := newOTELExporter(t, "integration-batch-test")
	defer exporter.Close(context.Background())

	now := time.Now()
	spans := make([]model.OpenTelemetrySpan, 10)
	for i := range spans {
		spans[i] = model.OpenTelemetrySpan{
			Hostname:      "test-host",
			TraceID:       uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
			SpanID:        uint64(80001 + i),
			OperationName: "batch-span",
			Kind:          "INTERNAL",
			StartTimeUs:   uint64(now.Add(-time.Duration(i+1) * time.Second).UnixMicro()),
			FinishTimeUs:  uint64(now.UnixMicro()),
			FinishDate:    now,
			Attributes:    map[string]string{"batch.index": string(rune('0' + i))},
		}
	}

	ctx := context.Background()
	baselineCount := countOTELSpanNames(readOTELFileExporterSpans(t), "batch-span")
	if _, err := exporter.ExportSpans(ctx, spans); err != nil {
		t.Fatalf("ExportSpans (batch) failed: %v", err)
	}

	receivedNames := waitForOTELSpanNameCount(t, "batch-span", baselineCount+10, 10*time.Second)
	count := countOTELSpanNames(receivedNames, "batch-span") - baselineCount
	t.Logf("Received %d new 'batch-span' spans (total spans: %d)", count, len(receivedNames))
}

func TestIntegration_ExportQueryToRealCollector(t *testing.T) {
	waitForOTELCollector(t, 30*time.Second)
	clearOTELOutput(t)

	exporter := newOTELExporter(t, "integration-query-export")
	defer exporter.Close(context.Background())

	query := model.QueryLog{
		QueryID:         "test-query-export-1",
		QueryKind:       "QueryFinish",
		EventTime:       time.Now(),
		QueryDurationMs: 2500,
		Query:           "SELECT * FROM users WHERE id = 42",
		User:            "test-user",
		ClientAddress:   "192.168.1.1",
		ReadRows:        100,
	}

	ctx := context.Background()
	baselineTotal := len(readOTELFileExporterSpans(t))
	if _, err := exporter.ExportQuery(ctx, query); err != nil {
		t.Fatalf("ExportQuery failed: %v", err)
	}

	receivedNames := waitForOTELSpanTotalAbove(t, baselineTotal, 10*time.Second)
	t.Logf("Received %d spans after ExportQuery", len(receivedNames))
}

func TestIntegration_CollectorHandlesMultipleExporters(t *testing.T) {
	waitForOTELCollector(t, 30*time.Second)
	clearOTELOutput(t)

	ctx := context.Background()
	now := time.Now()

	// Create two separate exporters (simulating multiple click-dog instances)
	exp1 := newOTELExporter(t, "instance-1")
	defer exp1.Close(ctx)

	exp2 := newOTELExporter(t, "instance-2")
	defer exp2.Close(ctx)

	// Export from both
	span1 := []model.OpenTelemetrySpan{{
		Hostname:      "host-1",
		TraceID:       uuid.MustParse("aaaa0000-0000-0000-0000-000000000001"),
		SpanID:        70001,
		OperationName: "from-instance-1",
		Kind:          "INTERNAL",
		StartTimeUs:   uint64(now.Add(-1 * time.Second).UnixMicro()),
		FinishTimeUs:  uint64(now.UnixMicro()),
		FinishDate:    now,
		Attributes:    map[string]string{},
	}}
	span2 := []model.OpenTelemetrySpan{{
		Hostname:      "host-2",
		TraceID:       uuid.MustParse("bbbb0000-0000-0000-0000-000000000002"),
		SpanID:        70002,
		OperationName: "from-instance-2",
		Kind:          "INTERNAL",
		StartTimeUs:   uint64(now.Add(-1 * time.Second).UnixMicro()),
		FinishTimeUs:  uint64(now.UnixMicro()),
		FinishDate:    now,
		Attributes:    map[string]string{},
	}}

	baselineNames := readOTELFileExporterSpans(t)
	baselineInst1 := countOTELSpanNames(baselineNames, "from-instance-1")
	baselineInst2 := countOTELSpanNames(baselineNames, "from-instance-2")
	if _, err := exp1.ExportSpans(ctx, span1); err != nil {
		t.Fatalf("exp1.ExportSpans failed: %v", err)
	}
	if _, err := exp2.ExportSpans(ctx, span2); err != nil {
		t.Fatalf("exp2.ExportSpans failed: %v", err)
	}

	waitForOTELFileSpans(t, 10*time.Second, func(spanNames []string) bool {
		return countOTELSpanNames(spanNames, "from-instance-1") >= baselineInst1+1 &&
			countOTELSpanNames(spanNames, "from-instance-2") >= baselineInst2+1
	}, "expected spans from both collector instances")
}
