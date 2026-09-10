//go:build integration

package main

import (
	"context"
	"fmt"
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

func exactFixtureSpans(t *testing.T, spans []model.OpenTelemetrySpan, fixtures ...tracedQueryFixture) []model.OpenTelemetrySpan {
	t.Helper()
	wanted := make(map[string]struct{})
	for _, fixture := range fixtures {
		for _, traceID := range fixture.TraceIDs {
			wanted[traceID.String()] = struct{}{}
		}
	}
	found := make(map[string]struct{}, len(wanted))
	var exact []model.OpenTelemetrySpan
	for _, span := range spans {
		traceID := span.TraceID.String()
		if _, ok := wanted[traceID]; !ok {
			continue
		}
		exact = append(exact, span)
		found[traceID] = struct{}{}
	}
	for traceID := range wanted {
		if _, ok := found[traceID]; !ok {
			t.Errorf("reader did not return seeded trace %s", traceID)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	return exact
}

func exactFixtureQueries(t *testing.T, queries []model.QueryLog, fixture slowQueryFixture) []model.QueryLog {
	t.Helper()
	wanted := make(map[string]struct{}, len(fixture.QueryIDs))
	for _, queryID := range fixture.QueryIDs {
		wanted[queryID] = struct{}{}
	}
	found := make(map[string]struct{}, len(wanted))
	var exact []model.QueryLog
	for _, query := range queries {
		if _, ok := wanted[query.QueryID]; !ok {
			continue
		}
		exact = append(exact, query)
		found[query.QueryID] = struct{}{}
	}
	for queryID := range wanted {
		if _, ok := found[queryID]; !ok {
			t.Errorf("reader did not return seeded query %s", queryID)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	return exact
}

// TestIntegration_E2E_ClickHouseToCollector tests the full pipeline:
// real ClickHouse -> ClickHouseReader -> filter -> OTELExporter -> real OTEL Collector
func TestIntegration_E2E_ClickHouseToCollector(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()
	waitForOTELCollector(t, 30*time.Second)

	fixture := seedTracedQueryFixture(t, conn, 5)

	reader := newCHReader(t, 1)
	defer reader.Close()

	exporter := newOTELExporter(t, "e2e-test")
	defer exporter.Close(context.Background())

	ctx := context.Background()

	// Fetch spans from real ClickHouse
	spans, err := reader.FetchOpenTelemetrySpans(ctx, 1, 0, 0, 0, 10*time.Minute, 1000)
	if err != nil {
		t.Fatalf("FetchOpenTelemetrySpans failed: %v", err)
	}
	spans = exactFixtureSpans(t, spans, fixture)

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
	if len(toExport) == 0 {
		t.Fatal("no exact fixture spans survived the no-op filter")
	}
	if _, err := exporter.ExportSpans(ctx, toExport); err != nil {
		t.Fatalf("ExportSpans failed: %v", err)
	}
	waitForOTELSpanKeys(t, "e2e-test", toExport, 10*time.Second)
}

// TestIntegration_E2E_MultiNodeToCollector tests fetching from all 3 ClickHouse
// nodes via cluster() and exporting to the real OTEL collector.
func TestIntegration_E2E_MultiNodeToCollector(t *testing.T) {
	conns := waitForAllClickHouse(t, 30*time.Second)
	for _, c := range conns {
		defer c.Close()
	}
	waitForOTELCollector(t, 30*time.Second)

	fixtures := make([]tracedQueryFixture, 0, len(conns))
	for _, conn := range conns {
		fixtures = append(fixtures, seedTracedQueryFixture(t, conn, 3))
	}

	// Use cluster-aware reader
	reader := newClusterCHReader(t, 1, "test_cluster")
	defer reader.Close()

	exporter := newOTELExporter(t, "e2e-cluster-test")
	defer exporter.Close(context.Background())

	ctx := context.Background()

	spans, err := reader.FetchOpenTelemetrySpans(ctx, 1, 0, 0, 0, 10*time.Minute, 1000)
	if err != nil {
		t.Fatalf("Cluster FetchOpenTelemetrySpans failed: %v", err)
	}

	spans = exactFixtureSpans(t, spans, fixtures...)

	// Export all to real collector
	if _, err := exporter.ExportSpans(ctx, spans); err != nil {
		t.Fatalf("ExportSpans failed: %v", err)
	}
	waitForOTELSpanKeys(t, "e2e-cluster-test", spans, 10*time.Second)
}

// TestIntegration_E2E_FilterIntegration verifies that blacklisted queries are
// not exported when running through the full pipeline.
func TestIntegration_E2E_FilterIntegration(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()
	waitForOTELCollector(t, 30*time.Second)

	allowedFixture := seedTracedQueryFixture(t, conn, 1)
	blockedFixture := seedTracedQueryFixture(t, conn, 1)

	reader := newCHReader(t, 1)
	defer reader.Close()

	exporter := newOTELExporter(t, "e2e-filter-test")
	defer exporter.Close(context.Background())

	// Only the second fixture carries this marker in its query text.
	filter, err := filter.NewQueryFilter(config.FiltersConfig{
		BlacklistQueries: []string{blockedFixture.Marker},
	})
	if err != nil {
		t.Fatalf("NewQueryFilter failed: %v", err)
	}

	ctx := context.Background()
	spans, err := reader.FetchOpenTelemetrySpans(ctx, 1, 0, 0, 0, 10*time.Minute, 1000)
	if err != nil {
		t.Fatalf("FetchOpenTelemetrySpans failed: %v", err)
	}

	spans = exactFixtureSpans(t, spans, allowedFixture, blockedFixture)
	blockedTraceID := blockedFixture.TraceIDs[0]
	var exported []model.OpenTelemetrySpan
	var filtered []model.OpenTelemetrySpan
	for _, span := range spans {
		queryText := span.Attributes["db.statement"]
		clientAddr := span.Attributes["client.address"]
		if filter.ShouldFilter(span.OperationName, queryText, clientAddr) {
			filtered = append(filtered, span)
			continue
		}
		exported = append(exported, span)
	}
	if len(filtered) == 0 {
		t.Fatal("exact blocked fixture produced no filtered span")
	}
	if len(exported) == 0 {
		t.Fatal("exact allowed fixture produced no exportable span")
	}
	allowedExported := 0
	filteredKeys := make(map[model.SpanKey]struct{}, len(filtered))
	for _, span := range filtered {
		if span.TraceID != blockedTraceID {
			t.Fatalf("filter removed trace %s, want only blocked trace %s", span.TraceID, blockedTraceID)
		}
		filteredKeys[model.KeyOf(span)] = struct{}{}
	}
	for _, span := range exported {
		if span.TraceID == allowedFixture.TraceIDs[0] {
			allowedExported++
		}
	}
	if allowedExported == 0 {
		t.Fatal("allowed fixture produced no exported span")
	}
	if _, err := exporter.ExportSpans(ctx, exported); err != nil {
		t.Fatalf("ExportSpans failed: %v", err)
	}
	// The conditional wait returns the collector snapshot containing every
	// accepted span from this export. Inspect that same completed snapshot for
	// filtered identities instead of adding a fixed post-export sleep.
	observations := waitForOTELSpanKeys(t, "e2e-filter-test", exported, 10*time.Second)
	for _, observation := range observations {
		key := model.SpanKey{TraceID: observation.TraceID, SpanID: observation.SpanID}
		if observation.ServiceName == "e2e-filter-test" {
			if _, blocked := filteredKeys[key]; blocked {
				t.Fatalf("filtered span %v reached the collector", key)
			}
		}
	}
}

// TestIntegration_E2E_ProcessQueriesBatch tests the processQueriesBatch function
// with real slow queries from ClickHouse exported to real collector.
func TestIntegration_E2E_ProcessQueriesBatch(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()
	waitForOTELCollector(t, 30*time.Second)

	fixture := seedSlowQueryFixture(t, conn, 3, 1500)

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
	queries, err := reader.FetchSlowQueriesInRange(ctx, 1000, 0, 100000, fixture.WindowStart, fixture.WindowEnd, 100)
	if err != nil {
		t.Fatalf("FetchSlowQueriesInRange failed: %v", err)
	}

	queries = exactFixtureQueries(t, queries, fixture)

	// Use the actual processQueriesBatch function
	result := processor.ProcessQueriesBatch(ctx, queries, exporter, filter, cfg, nil)
	if result.Exported != len(fixture.QueryIDs) || result.Filtered != 0 || result.Failed != 0 || result.FirstErr != nil {
		t.Fatalf("batch result = %+v, want %d exact exports and no failures", result, len(fixture.QueryIDs))
	}
	waitForOTELFileObservations(t, 10*time.Second, func(observations []otelSpanObservation) bool {
		for _, queryID := range fixture.QueryIDs {
			if len(observationsWithAttribute(observations, "e2e-batch-test", "db.query_id", queryID)) != 1 {
				return false
			}
		}
		return true
	}, fmt.Sprintf("expected exact query IDs %v at collector", fixture.QueryIDs))
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

	if !reader.IsHealthy(ctx) {
		t.Fatal("real ClickHouse reader is unhealthy; circuit assertion has no valid precondition")
	}
	cb.RecordSuccess()

	if cb.State() != resilience.CircuitClosed {
		t.Errorf("Expected circuit closed after healthy check, got %v", cb.State())
	}
}
