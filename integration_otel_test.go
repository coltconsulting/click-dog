//go:build integration

package main

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/model"
)

func TestIntegration_ExportSpansToRealCollector(t *testing.T) {
	waitForOTELCollector(t, 30*time.Second)

	marker := newIntegrationFixtureMarker()
	serviceName := "integration-otel-" + marker
	exporter := newOTELExporter(t, serviceName)
	defer exporter.Close(context.Background())

	now := time.Now()
	spans := []model.OpenTelemetrySpan{
		{
			Hostname:      "test-host",
			TraceID:       uuid.New(),
			SpanID:        newIntegrationSpanID(),
			OperationName: "integration-test-span",
			Kind:          "INTERNAL",
			StartTimeUs:   uint64(now.Add(-2 * time.Second).UnixMicro()),
			FinishTimeUs:  uint64(now.UnixMicro()),
			FinishDate:    now,
			Attributes:    map[string]string{"test.marker": "true"},
		},
	}

	ctx := context.Background()
	if _, err := exporter.ExportSpans(ctx, spans); err != nil {
		t.Fatalf("ExportSpans failed: %v", err)
	}
	observations := waitForOTELSpanKeys(t, serviceName, spans, 10*time.Second)
	matches := observationsWithAttribute(observations, serviceName, "test.marker", "true")
	if len(matches) != 1 || matches[0].Name != "integration-test-span" {
		t.Fatalf("exact exported span metadata = %+v, want one integration-test-span", matches)
	}
}

func TestIntegration_ExportBatchToRealCollector(t *testing.T) {
	waitForOTELCollector(t, 30*time.Second)

	marker := newIntegrationFixtureMarker()
	serviceName := "integration-batch-" + marker
	exporter := newOTELExporter(t, serviceName)
	defer exporter.Close(context.Background())

	now := time.Now()
	traceID := uuid.New()
	firstSpanID := newIntegrationSpanID()
	spans := make([]model.OpenTelemetrySpan, 10)
	for i := range spans {
		spans[i] = model.OpenTelemetrySpan{
			Hostname:      "test-host",
			TraceID:       traceID,
			SpanID:        firstSpanID + uint64(i),
			OperationName: "batch-span",
			Kind:          "INTERNAL",
			StartTimeUs:   uint64(now.Add(-time.Duration(i+1) * time.Second).UnixMicro()),
			FinishTimeUs:  uint64(now.UnixMicro()),
			FinishDate:    now,
			Attributes:    map[string]string{"batch.index": strconv.Itoa(i)},
		}
	}

	ctx := context.Background()
	if _, err := exporter.ExportSpans(ctx, spans); err != nil {
		t.Fatalf("ExportSpans (batch) failed: %v", err)
	}
	observations := waitForOTELSpanKeys(t, serviceName, spans, 10*time.Second)
	for i := range spans {
		matches := observationsWithAttribute(observations, serviceName, "batch.index", strconv.Itoa(i))
		if len(matches) != 1 || matches[0].Name != "batch-span" {
			t.Errorf("batch index %d observations = %+v, want one exact batch-span", i, matches)
		}
	}
}

func TestIntegration_ExportQueryToRealCollector(t *testing.T) {
	waitForOTELCollector(t, 30*time.Second)

	marker := newIntegrationFixtureMarker()
	serviceName := "integration-query-" + marker
	exporter := newOTELExporter(t, serviceName)
	defer exporter.Close(context.Background())

	query := model.QueryLog{
		QueryID:         integrationFixtureQueryID(marker, 0),
		QueryKind:       "QueryFinish",
		EventTime:       time.Now(),
		QueryDurationMs: 2500,
		Query:           "SELECT * FROM users WHERE id = 42",
		User:            "test-user",
		ClientAddress:   "192.168.1.1",
		ReadRows:        100,
	}

	ctx := context.Background()
	if _, err := exporter.ExportQuery(ctx, query); err != nil {
		t.Fatalf("ExportQuery failed: %v", err)
	}
	observations := waitForOTELFileObservations(t, 10*time.Second, func(observations []otelSpanObservation) bool {
		return len(observationsWithAttribute(observations, serviceName, "db.query_id", query.QueryID)) == 1
	}, fmt.Sprintf("expected exact query ID %q", query.QueryID))
	matches := observationsWithAttribute(observations, serviceName, "db.query_id", query.QueryID)
	if matches[0].Name != "clickhouse.query" || matches[0].Attributes["click_dog.source"] != "query_log" {
		t.Fatalf("query observation = %+v, want clickhouse.query from query_log", matches[0])
	}
}

func TestIntegration_CollectorHandlesMultipleExporters(t *testing.T) {
	waitForOTELCollector(t, 30*time.Second)

	ctx := context.Background()
	now := time.Now()
	marker := newIntegrationFixtureMarker()
	service1 := "instance-1-" + marker
	service2 := "instance-2-" + marker

	// Create two separate exporters (simulating multiple click-dog instances)
	exp1 := newOTELExporter(t, service1)
	defer exp1.Close(ctx)

	exp2 := newOTELExporter(t, service2)
	defer exp2.Close(ctx)

	// Export from both
	span1 := []model.OpenTelemetrySpan{{
		Hostname:      "host-1",
		TraceID:       uuid.New(),
		SpanID:        newIntegrationSpanID(),
		OperationName: "from-instance-1",
		Kind:          "INTERNAL",
		StartTimeUs:   uint64(now.Add(-1 * time.Second).UnixMicro()),
		FinishTimeUs:  uint64(now.UnixMicro()),
		FinishDate:    now,
		Attributes:    map[string]string{},
	}}
	span2 := []model.OpenTelemetrySpan{{
		Hostname:      "host-2",
		TraceID:       uuid.New(),
		SpanID:        newIntegrationSpanID(),
		OperationName: "from-instance-2",
		Kind:          "INTERNAL",
		StartTimeUs:   uint64(now.Add(-1 * time.Second).UnixMicro()),
		FinishTimeUs:  uint64(now.UnixMicro()),
		FinishDate:    now,
		Attributes:    map[string]string{},
	}}

	if _, err := exp1.ExportSpans(ctx, span1); err != nil {
		t.Fatalf("exp1.ExportSpans failed: %v", err)
	}
	if _, err := exp2.ExportSpans(ctx, span2); err != nil {
		t.Fatalf("exp2.ExportSpans failed: %v", err)
	}

	waitForOTELSpanKeys(t, service1, span1, 10*time.Second)
	waitForOTELSpanKeys(t, service2, span2, 10*time.Second)
}
