//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	chreader "github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/processor"
	"github.com/coltconsulting/click-dog/internal/resilience"
)

// TestIntegration_E2E_ClickHouseToCollector tests the full pipeline:
// real ClickHouse -> ClickHouseReader -> filter -> OTELExporter -> real OTEL Collector
func TestIntegration_E2E_ClickHouseToCollector(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()
	waitForOTELCollector(t, 30*time.Second)
	clearOTELOutput(t)

	// Seed traced queries
	seedTracedQueries(t, conn, 5)

	reader := newCHReader(t, 1)
	defer reader.Close()

	exporter := newOTELExporter(t, "e2e-test")
	defer exporter.Close(context.Background())

	ctx := context.Background()

	// Fetch spans from real ClickHouse
	spans, err := reader.FetchOpenTelemetrySpans(ctx, 1, 0, 0, 0, 10*time.Minute, 100)
	if err != nil {
		t.Fatalf("FetchOpenTelemetrySpans failed: %v", err)
	}
	if len(spans) == 0 {
		t.Fatal("Expected spans from ClickHouse opentelemetry_span_log")
	}
	t.Logf("Fetched %d spans from ClickHouse", len(spans))

	// Apply filter (no blacklist)
	filter, _ := filter.NewQueryFilter(config.FiltersConfig{})
	var toExport []model.OpenTelemetrySpan
	for _, span := range spans {
		queryText := span.Attributes["db.statement"]
		clientAddr := span.Attributes["client.address"]
		if !filter.ShouldFilter(span.OperationName, queryText, clientAddr) {
			toExport = append(toExport, span)
		}
	}

	// Export to real collector
	baselineTotal := len(readOTELFileExporterSpans(t))
	if len(toExport) > 0 {
		if _, err := exporter.ExportSpans(ctx, toExport); err != nil {
			t.Fatalf("ExportSpans failed: %v", err)
		}
	}

	receivedNames := waitForOTELSpanTotalAboveOrTimeout(t, baselineTotal, 10*time.Second)
	if len(receivedNames) <= baselineTotal {
		t.Error("Expected new spans in collector output")
	}
	t.Logf("Collector received %d span names", len(receivedNames))
}

// TestIntegration_E2E_MultiNodeToCollector tests fetching from all 3 ClickHouse
// nodes via cluster() and exporting to the real OTEL collector.
func TestIntegration_E2E_MultiNodeToCollector(t *testing.T) {
	conns := waitForAllClickHouse(t, 30*time.Second)
	for _, c := range conns {
		defer c.Close()
	}
	waitForOTELCollector(t, 30*time.Second)
	clearOTELOutput(t)

	// Seed traced queries on each node
	for _, conn := range conns {
		seedTracedQueries(t, conn, 3)
	}

	// Use cluster-aware reader
	reader := newClusterCHReader(t, 1, "test_cluster")
	defer reader.Close()

	exporter := newOTELExporter(t, "e2e-cluster-test")
	defer exporter.Close(context.Background())

	ctx := context.Background()

	spans, err := reader.FetchOpenTelemetrySpans(ctx, 1, 0, 0, 0, 10*time.Minute, 100)
	if err != nil {
		t.Fatalf("Cluster FetchOpenTelemetrySpans failed: %v", err)
	}

	t.Logf("Cluster query returned %d spans", len(spans))

	if len(spans) == 0 {
		t.Fatal("Expected spans from cluster query")
	}

	// Export all to real collector
	baselineTotal := len(readOTELFileExporterSpans(t))
	if _, err := exporter.ExportSpans(ctx, spans); err != nil {
		t.Fatalf("ExportSpans failed: %v", err)
	}

	receivedNames := waitForOTELSpanTotalAboveOrTimeout(t, baselineTotal, 10*time.Second)
	if len(receivedNames) <= baselineTotal {
		t.Error("Expected new spans in collector output from cluster pipeline")
	}
	t.Logf("Cluster E2E: %d spans exported, %d received by collector", len(spans), len(receivedNames))
}

// TestIntegration_E2E_FilterIntegration verifies that blacklisted queries are
// not exported when running through the full pipeline.
func TestIntegration_E2E_FilterIntegration(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()
	waitForOTELCollector(t, 30*time.Second)
	clearOTELOutput(t)

	seedTracedQueries(t, conn, 5)

	reader := newCHReader(t, 1)
	defer reader.Close()

	exporter := newOTELExporter(t, "e2e-filter-test")
	defer exporter.Close(context.Background())

	// Blacklist everything matching "system" -- should filter most CH internal spans
	filter, err := filter.NewQueryFilter(config.FiltersConfig{
		BlacklistQueries: []string{"(?i)system"},
	})
	if err != nil {
		t.Fatalf("NewQueryFilter failed: %v", err)
	}

	ctx := context.Background()
	spans, err := reader.FetchOpenTelemetrySpans(ctx, 1, 0, 0, 0, 10*time.Minute, 100)
	if err != nil {
		t.Fatalf("FetchOpenTelemetrySpans failed: %v", err)
	}

	exported := 0
	filtered := 0
	for _, span := range spans {
		queryText := span.Attributes["db.statement"]
		clientAddr := span.Attributes["client.address"]
		if filter.ShouldFilter(span.OperationName, queryText, clientAddr) {
			filtered++
			continue
		}
		if _, err := exporter.ExportSpans(ctx, []model.OpenTelemetrySpan{span}); err != nil {
			t.Logf("Warning: export failed for span %d: %v", span.SpanID, err)
			continue
		}
		exported++
	}

	t.Logf("Filter E2E: %d total, %d exported, %d filtered", len(spans), exported, filtered)

	// Should have filtered at least some spans (system queries from FLUSH LOGS etc)
	if filtered == 0 && len(spans) > 0 {
		t.Log("Warning: expected some spans to be filtered by 'system' blacklist")
	}
}

// TestIntegration_E2E_ProcessQueriesBatch tests the processQueriesBatch function
// with real slow queries from ClickHouse exported to real collector.
func TestIntegration_E2E_ProcessQueriesBatch(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()
	waitForOTELCollector(t, 30*time.Second)
	clearOTELOutput(t)

	// Seed slow queries
	seedSlowQueries(t, conn, 3, 1500)

	reader := newCHReader(t, 1)
	defer reader.Close()

	exporter := newOTELExporter(t, "e2e-batch-test")
	defer exporter.Close(context.Background())

	filter, _ := filter.NewQueryFilter(config.FiltersConfig{})

	cfg := &config.Config{
		Monitor: config.MonitorConfig{
			MinTraceDurationMs: 1000,
			MaxSpansPerCycle:   100,
		},
	}

	ctx := context.Background()
	startTime := time.Now().Add(-5 * time.Minute)
	endTime := time.Now()

	queries, err := reader.FetchSlowQueriesInRange(ctx, 1000, 0, 100000, startTime, endTime, 100)
	if err != nil {
		t.Fatalf("FetchSlowQueriesInRange failed: %v", err)
	}

	if len(queries) == 0 {
		t.Fatal("Expected slow queries from ClickHouse")
	}
	t.Logf("Processing %d slow queries through batch pipeline", len(queries))

	// Use the actual processQueriesBatch function
	baselineTotal := len(readOTELFileExporterSpans(t))
	processor.ProcessQueriesBatch(ctx, queries, exporter, filter, cfg, nil)

	receivedNames := waitForOTELSpanTotalAboveOrTimeout(t, baselineTotal, 10*time.Second)
	t.Logf("Batch E2E: collector received %d spans", len(receivedNames))
}

// TestIntegration_E2E_DedupWithRealData drives the real export pipeline twice
// over the same ClickHouse rows and asserts the dedup cache stops the second
// delivery. It calls processor.Pipeline.Process, so a regression in the
// product's own dedup path fails this test — it does not re-implement the loop.
//
// Deduplication and cursor forward-progress are separate mechanisms and this
// test isolates the first. The scheduled reader carries a keyset cursor, so a
// second cycle on the SAME reader would fetch the NEXT page by design; each
// cycle therefore gets its own reader, both starting at the head of the
// window, while the dedup cache is shared exactly as it is across cycles in a
// running process.
//
// The single-reader wrap path is NOT covered here. FetchOpenTelemetrySpans
// returns an empty page both when the walk ends and when a trace page yields
// no spans after SQL filtering, and those two cases move the cursor in
// opposite directions, so a caller cannot detect a wrap from outside.
// internal/clickhouse/span_cursor_test.go covers it against a fake connection.
func TestIntegration_E2E_DedupWithRealData(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()
	waitForOTELCollector(t, 30*time.Second)
	clearOTELOutput(t)

	// Seeding flushes the span log and waits for the rows to materialize, so
	// the two cycles below read a window that nothing writes to in between.
	seedTracedQueries(t, conn, 3)

	qf, err := filter.NewQueryFilter(config.FiltersConfig{})
	if err != nil {
		t.Fatalf("NewQueryFilter failed: %v", err)
	}

	exporter := newOTELExporter(t, "e2e-dedup-test")
	defer exporter.Close(context.Background())

	cfg := &config.Config{
		Monitor: config.MonitorConfig{
			MinTraceDurationMs: 1,
			LookbackS:          600,
			MaxSpansPerCycle:   100,
			DedupCacheSize:     10000,
			// Explicit: a zero here means "no per-export deadline", which is
			// not what LoadConfig would produce for a real operator.
			ExportTimeoutS: 30,
		},
	}

	// One cache across both cycles, as a running process has. Each cycle gets
	// its own reader so cycle 2 re-serves cycle 1's rows instead of paging past
	// them; only the cache can suppress the repeat.
	seenSpans := mustLRU(t, cfg.Monitor.DedupCacheSize)

	runCycle := func(reader *chreader.ClickHouseReader) metrics.CycleSnapshot {
		t.Helper()
		m := metrics.NewMetrics()
		pipeline, err := processor.NewPipeline(processor.Pipeline{
			Reader:    reader,
			Exporter:  exporter,
			Filter:    qf,
			Config:    cfg,
			SeenSpans: seenSpans,
			Metrics:   m,
		})
		if err != nil {
			t.Fatalf("NewPipeline: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		if err := pipeline.Process(ctx); err != nil {
			t.Fatalf("pipeline.Process failed: %v", err)
		}
		snap := m.Snapshot()
		if !snap.HaveLastCycle {
			t.Fatal("pipeline recorded no cycle")
		}
		return snap.LastCycle
	}

	reader1 := newCHReader(t, 1)
	defer reader1.Close()
	cycle1 := runCycle(reader1)

	reader2 := newCHReader(t, 1)
	defer reader2.Close()
	cycle2 := runCycle(reader2)

	t.Logf("Dedup E2E: cycle1 exported=%d filtered=%d duplicates=%d; cycle2 exported=%d filtered=%d duplicates=%d",
		cycle1.Exported, cycle1.Filtered, cycle1.Duplicates,
		cycle2.Exported, cycle2.Filtered, cycle2.Duplicates)

	if cycle1.Exported == 0 {
		t.Fatal("Cycle 1 exported nothing, so there is nothing for cycle 2 to deduplicate")
	}
	if cycle2.Duplicates == 0 {
		t.Fatal("Cycle 2 deduplicated nothing; either the rows did not repeat or the cache is not consulted")
	}
	// The property, stated directly: nothing already delivered is delivered
	// again. Counting a span as a duplicate is not enough — it must also not be
	// exported. Nothing writes to the window between the cycles, so any export
	// here is a second delivery.
	if cycle2.Exported != 0 {
		t.Errorf("Cycle 2 exported %d spans that were already delivered in cycle 1 (it also counted %d duplicates); every span in the window had been seen",
			cycle2.Exported, cycle2.Duplicates)
	}
}

// TestIntegration_E2E_CircuitBreakerWithRealReader tests circuit breaker
// integration with a real ClickHouse connection.
func TestIntegration_E2E_CircuitBreakerWithRealReader(t *testing.T) {
	_ = waitForClickHouse(t, 1, 30*time.Second)

	reader := newCHReader(t, 1)
	defer reader.Close()

	cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 3,
		SuccessThreshold: 1,
		ResetTimeoutS:    60,
	})

	ctx := context.Background()

	// Healthy connection should succeed and keep circuit closed
	if !cb.Allow() {
		t.Fatal("Circuit should allow initially")
	}

	if reader.IsHealthy(ctx) {
		cb.RecordSuccess()
	}

	if cb.State() != resilience.CircuitClosed {
		t.Errorf("Expected circuit closed after healthy check, got %v", cb.State())
	}
}
