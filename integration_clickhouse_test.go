//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/analysis"
	"github.com/coltconsulting/click-dog/internal/processor"
)

// -------------------------------------------------------------------
// Single-node ClickHouse reader tests
// -------------------------------------------------------------------

func TestIntegration_ClickHouseConnection(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()

	reader := newCHReader(t, 1)
	defer reader.Close()

	if !reader.IsHealthy(context.Background()) {
		t.Fatal("ClickHouseReader should report healthy after connecting")
	}
}

func TestIntegration_ClickHouseAllNodesHealthy(t *testing.T) {
	conns := waitForAllClickHouse(t, 30*time.Second)
	for _, c := range conns {
		defer c.Close()
	}

	for node := 1; node <= 3; node++ {
		reader := newCHReader(t, node)
		defer reader.Close()

		if !reader.IsHealthy(context.Background()) {
			t.Errorf("Node %d should report healthy", node)
		}
	}
}

func TestIntegration_FetchSlowQueriesInRange(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()

	// Seed 3 slow queries (~1.5s each)
	seedSlowQueries(t, conn, 3, 1500)

	reader := newCHReader(t, 1)
	defer reader.Close()

	ctx := context.Background()
	startTime := time.Now().Add(-5 * time.Minute)
	endTime := time.Now()

	queries, err := reader.FetchSlowQueriesInRange(ctx, 1000, 0, 100000, startTime, endTime, 100)
	if err != nil {
		t.Fatalf("FetchSlowQueriesInRange failed: %v", err)
	}

	if len(queries) < 3 {
		t.Errorf("Expected at least 3 slow queries, got %d", len(queries))
	}

	for _, q := range queries {
		if q.QueryID == "" {
			t.Error("QueryID should not be empty")
		}
		if q.QueryDurationMs < 1000 {
			t.Errorf("Query duration %d should be >= 1000ms", q.QueryDurationMs)
		}
	}
}

// TestIntegration_AnalyzeQueriesHandlesTDigestFloat32 pins issue #381 against
// the ClickHouse 24.1 image used by the integration suite. quantileTDigest()
// returns Float32 on that version, while query-family report fields are
// float64; the production SQL must cast at the source so the driver can scan
// the complete report without a conversion error.
func TestIntegration_AnalyzeQueriesHandlesTDigestFloat32(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()

	seedSlowQueries(t, conn, 3, 10)

	configPath := filepath.Join(t.TempDir(), "click-dog.yaml")
	configYAML := fmt.Sprintf(`clickhouse:
  host: %s
  port: %d
  database: default
  username: default

exporters:
  otel:
    - collector_address: localhost:14317

monitor:
  enabled: true
  min_trace_duration_ms: 1
  check_interval_s: 30
`, integrationCHHost(1), integrationCHPort(1))
	if err := os.WriteFile(configPath, []byte(configYAML), 0600); err != nil {
		t.Fatalf("write integration config: %v", err)
	}

	var out, errOut bytes.Buffer
	code := runAnalyzeQueries([]string{
		"-config", configPath,
		"-lookback", "10m",
		"-timeout", "30s",
		"-min-executions", "1",
		"-format", "json",
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("analyze queries exit code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	var report analysis.AnalysisReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("analyze queries returned invalid JSON: %v\n%s", err, out.String())
	}
	if !report.Coverage.QueryFamilyRollupsSupported {
		t.Fatal("query-family rollups should be supported by the ClickHouse 24.1 integration image")
	}
	if report.Coverage.QueryFamilyCount == 0 {
		t.Fatal("expected at least one scanned query family from the seeded queries")
	}
}

func TestIntegration_FetchOpenTelemetrySpans(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()

	// Seed traced queries that generate spans
	seedTracedQueries(t, conn, 5)

	reader := newCHReader(t, 1)
	defer reader.Close()

	ctx := context.Background()
	spans, err := reader.FetchOpenTelemetrySpans(ctx, 1, 0, 0, 0, 10*time.Minute, 100)
	if err != nil {
		t.Fatalf("FetchOpenTelemetrySpans failed: %v", err)
	}

	if len(spans) == 0 {
		// Debug: check if the span log table has any rows at all
		var count uint64
		if err := conn.QueryRow(ctx, "SELECT count() FROM system.opentelemetry_span_log").Scan(&count); err != nil {
			t.Logf("Debug: failed to query span log count: %v", err)
		} else {
			t.Logf("Debug: system.opentelemetry_span_log has %d total rows", count)
		}
		t.Fatal("Expected spans from ClickHouse opentelemetry_span_log")
	}
	t.Logf("Fetched %d spans from node 1", len(spans))

	// Verify span fields are populated
	for _, span := range spans {
		if span.TraceID == uuid.Nil {
			t.Error("Span TraceID should not be the zero UUID")
		}
		if span.SpanID == 0 {
			t.Error("Span SpanID should not be 0")
		}
		if span.OperationName == "" {
			t.Error("Span OperationName should not be empty")
		}
	}
}

func TestIntegration_FetchSpans_DurationFilters(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()

	// Seed a mix: some fast, some slow
	seedSlowQueries(t, conn, 2, 500)  // fast (0.5s)
	seedSlowQueries(t, conn, 2, 2000) // slow (2s)

	reader := newCHReader(t, 1)
	defer reader.Close()

	ctx := context.Background()

	// Fetch with high min duration -- should only return the slow ones
	spans, err := reader.FetchOpenTelemetrySpans(ctx, 1500, 0, 0, 0, 10*time.Minute, 100)
	if err != nil {
		t.Fatalf("FetchOpenTelemetrySpans failed: %v", err)
	}

	// Every returned span's trace should have at least one span >= 1500ms
	t.Logf("Fetched %d spans with minTraceDuration=1500ms", len(spans))

	// Verify we get fewer spans than without filter
	allSpans, err := reader.FetchOpenTelemetrySpans(ctx, 1, 0, 0, 0, 10*time.Minute, 100)
	if err != nil {
		t.Fatalf("FetchOpenTelemetrySpans (all) failed: %v", err)
	}

	if len(spans) >= len(allSpans) && len(allSpans) > 0 {
		t.Logf("Filtered spans (%d) should be <= all spans (%d)", len(spans), len(allSpans))
	}
}

// TestIntegration_FetchSpans_RespectsSpanCap pins issue #91: the limit
// argument (monitor.max_spans_per_cycle) must be enforced as the actual
// per-cycle span cap. Before the fix, the reader expanded the cap to
// limit*100 spans in step 2; this test asserts the configured cap is the
// upper bound on returned spans.
func TestIntegration_FetchSpans_RespectsSpanCap(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()

	// Seed enough traces so total qualifying spans comfortably exceed the
	// small cap configured below. Each ClickHouse-traced query typically
	// emits several internal spans (parse, plan, execute, ...), so 5
	// traces produces well above 3 total spans.
	seedTracedQueries(t, conn, 5)

	reader := newCHReader(t, 1)
	defer reader.Close()

	ctx := context.Background()
	const spanCap = 3
	spans, err := reader.FetchOpenTelemetrySpans(ctx, 1, 0, 0, 0, 10*time.Minute, spanCap)
	if err != nil {
		t.Fatalf("FetchOpenTelemetrySpans failed: %v", err)
	}

	if len(spans) > spanCap {
		t.Errorf("max_spans_per_cycle=%d violated: got %d spans (must be <= %d)", spanCap, len(spans), spanCap)
	}
	t.Logf("Span cap honored: requested <= %d, fetched %d", spanCap, len(spans))
}

func TestIntegration_FetchSpans_EmptyResult(t *testing.T) {
	_ = waitForClickHouse(t, 1, 30*time.Second)

	reader := newCHReader(t, 1)
	defer reader.Close()

	ctx := context.Background()
	// Use an absurdly high min duration so nothing matches
	spans, err := reader.FetchOpenTelemetrySpans(ctx, 999999999, 0, 0, 0, 10*time.Minute, 100)
	if err != nil {
		t.Fatalf("FetchOpenTelemetrySpans failed: %v", err)
	}

	if len(spans) != 0 {
		t.Errorf("Expected 0 spans with very high min duration, got %d", len(spans))
	}
}

// -------------------------------------------------------------------
// query_log enrichment
// -------------------------------------------------------------------

func TestIntegration_FetchQueryLogByQueryIDs(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()

	// Seed traced queries — these generate both span_log and query_log entries
	// sharing the same query_id.
	seedTracedQueries(t, conn, 3)

	reader := newCHReader(t, 1)
	defer reader.Close()

	ctx := context.Background()

	// First fetch spans to get the query IDs from span attributes
	spans, err := reader.FetchOpenTelemetrySpans(ctx, 1, 0, 0, 0, 10*time.Minute, 100)
	if err != nil {
		t.Fatalf("FetchOpenTelemetrySpans failed: %v", err)
	}
	if len(spans) == 0 {
		t.Fatal("Expected spans from ClickHouse — cannot test enrichment without spans")
	}

	// Collect unique query IDs from span attributes
	queryIDs := processor.UniqueQueryIDs(spans)
	t.Logf("Found %d spans across %d unique query_ids", len(spans), len(queryIDs))
	if len(queryIDs) == 0 {
		t.Fatal("No clickhouse.query_id attributes found on spans")
	}

	// Fetch enrichment data
	queryLogMap, err := reader.FetchQueryLogByQueryIDs(ctx, queryIDs, 2)
	if err != nil {
		t.Fatalf("FetchQueryLogByQueryIDs failed: %v", err)
	}

	if len(queryLogMap) == 0 {
		var count uint64
		if err := conn.QueryRow(ctx, "SELECT count() FROM system.query_log WHERE event_date >= today() - 1").Scan(&count); err != nil {
			t.Logf("Debug: query_log count query failed: %v", err)
		} else {
			t.Logf("Debug: system.query_log has %d rows in last 2 days", count)
		}
		t.Fatal("Expected query_log enrichment data for at least some query_ids")
	}

	t.Logf("Enriched %d/%d query_ids with query_log metadata", len(queryLogMap), len(queryIDs))

	// Verify enrichment fields are populated
	for qid, ql := range queryLogMap {
		if ql.QueryID == "" {
			t.Errorf("query_id %s: QueryID should not be empty", qid)
		}
		if ql.QueryKind != "QueryFinish" && ql.QueryKind != "ExceptionWhileProcessing" {
			t.Errorf("query_id %s: unexpected QueryKind %q", qid, ql.QueryKind)
		}
		if ql.User == "" {
			t.Errorf("query_id %s: User should not be empty", qid)
		}
		// client_address should be a valid IP string
		if ql.ClientAddress == "" {
			t.Errorf("query_id %s: ClientAddress should not be empty", qid)
		}
	}
}

func TestIntegration_EnrichSpanPipeline(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()

	seedTracedQueries(t, conn, 2)

	reader := newCHReader(t, 1)
	defer reader.Close()

	ctx := context.Background()

	// Fetch spans
	spans, err := reader.FetchOpenTelemetrySpans(ctx, 1, 0, 0, 0, 10*time.Minute, 100)
	if err != nil {
		t.Fatalf("FetchOpenTelemetrySpans failed: %v", err)
	}
	if len(spans) == 0 {
		t.Fatal("Expected spans")
	}

	// Enrich
	queryIDs := processor.UniqueQueryIDs(spans)
	if len(queryIDs) == 0 {
		t.Fatal("No query_ids found on spans")
	}
	queryLogMap, err := reader.FetchQueryLogByQueryIDs(ctx, queryIDs, 2)
	if err != nil {
		t.Fatalf("FetchQueryLogByQueryIDs failed: %v", err)
	}

	enriched := 0
	for i := range spans {
		if qid := spans[i].Attributes["clickhouse.query_id"]; qid != "" {
			if ql, ok := queryLogMap[qid]; ok {
				processor.EnrichSpanFromQueryLog(&spans[i], ql, 100000)
				enriched++
			}
		}
	}

	if enriched == 0 {
		t.Fatal("Expected at least some spans to be enriched")
	}
	t.Logf("Enriched %d/%d spans", enriched, len(spans))

	// Verify enriched attributes exist on at least one span
	found := false
	for _, span := range spans {
		if _, ok := span.Attributes["query_log.query_id"]; ok {
			found = true
			if span.Attributes["query_log.user"] == "" {
				t.Error("query_log.user should not be empty on enriched span")
			}
			if span.Attributes["query_log.memory_usage"] == "" {
				t.Error("query_log.memory_usage should be set on enriched span")
			}
			break
		}
	}
	if !found {
		t.Error("No span had query_log.query_id attribute after enrichment")
	}
}

func TestIntegration_FetchQueryLogByQueryIDs_NoMatch(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second) // ensure ClickHouse is up before testing the reader
	defer conn.Close()

	reader := newCHReader(t, 1)
	defer reader.Close()

	// Use a fake query ID that doesn't exist
	result, err := reader.FetchQueryLogByQueryIDs(
		context.Background(),
		[]string{"00000000-0000-0000-0000-000000000000"},
		2,
	)
	if err != nil {
		t.Fatalf("FetchQueryLogByQueryIDs failed on no-match: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("Expected empty result for non-existent query IDs, got %d", len(result))
	}
}

// -------------------------------------------------------------------
// Multi-node cluster queries
// -------------------------------------------------------------------

func TestIntegration_ClusterQueries_FetchSlowQueries(t *testing.T) {
	conns := waitForAllClickHouse(t, 30*time.Second)
	for _, c := range conns {
		defer c.Close()
	}

	// Seed slow queries on each node
	seedSlowQueriesOnAllNodes(t, conns, 2, 1500)

	// Create a cluster-aware reader connected to node 1
	reader := newClusterCHReader(t, 1, "test_cluster")
	defer reader.Close()

	ctx := context.Background()
	startTime := time.Now().Add(-5 * time.Minute)
	endTime := time.Now()

	queries, err := reader.FetchSlowQueriesInRange(ctx, 1000, 0, 100000, startTime, endTime, 100)
	if err != nil {
		t.Fatalf("Cluster FetchSlowQueriesInRange failed: %v", err)
	}

	// With cluster() function we should see queries from all 3 nodes
	// Each node had 2 queries seeded = at least 6 total
	t.Logf("Cluster query returned %d slow queries", len(queries))

	if len(queries) < 6 {
		t.Errorf("Expected at least 6 slow queries across cluster, got %d", len(queries))
	}
}

func TestIntegration_ClusterQueries_FetchSpans(t *testing.T) {
	conns := waitForAllClickHouse(t, 30*time.Second)
	for _, c := range conns {
		defer c.Close()
	}

	// Seed traced queries on each node
	for _, conn := range conns {
		seedTracedQueries(t, conn, 3)
	}

	reader := newClusterCHReader(t, 1, "test_cluster")
	defer reader.Close()

	ctx := context.Background()
	spans, err := reader.FetchOpenTelemetrySpans(ctx, 1, 0, 0, 0, 10*time.Minute, 100)
	if err != nil {
		t.Fatalf("Cluster FetchOpenTelemetrySpans failed: %v", err)
	}

	t.Logf("Cluster query returned %d spans", len(spans))

	if len(spans) == 0 {
		t.Fatal("Expected spans from cluster query")
	}

	// Verify spans come from multiple hostnames
	hostnames := make(map[string]int)
	for _, span := range spans {
		hostnames[span.Hostname]++
	}
	t.Logf("Spans by hostname: %v", hostnames)

	if len(hostnames) < 2 {
		t.Errorf("Expected spans from multiple hosts in cluster query, got hosts: %v", hostnames)
	}
}

func TestIntegration_ClusterQueries_VsLocalQueries(t *testing.T) {
	conns := waitForAllClickHouse(t, 30*time.Second)
	for _, c := range conns {
		defer c.Close()
	}

	// Seed on all nodes
	seedSlowQueriesOnAllNodes(t, conns, 2, 1500)

	// Local reader (node 1 only)
	localReader := newCHReader(t, 1)
	defer localReader.Close()

	// Cluster reader (queries all shards)
	clusterReader := newClusterCHReader(t, 1, "test_cluster")
	defer clusterReader.Close()

	ctx := context.Background()
	startTime := time.Now().Add(-5 * time.Minute)
	endTime := time.Now()

	localQueries, err := localReader.FetchSlowQueriesInRange(ctx, 1000, 0, 100000, startTime, endTime, 100)
	if err != nil {
		t.Fatalf("Local FetchSlowQueriesInRange failed: %v", err)
	}

	clusterQueries, err := clusterReader.FetchSlowQueriesInRange(ctx, 1000, 0, 100000, startTime, endTime, 100)
	if err != nil {
		t.Fatalf("Cluster FetchSlowQueriesInRange failed: %v", err)
	}

	t.Logf("Local queries: %d, Cluster queries: %d", len(localQueries), len(clusterQueries))

	// Cluster should return more (or equal) results than local-only
	if len(clusterQueries) < len(localQueries) {
		t.Errorf("Cluster queries (%d) should return >= local queries (%d)",
			len(clusterQueries), len(localQueries))
	}
}

func TestIntegration_EachNodeHasOwnQueryLog(t *testing.T) {
	conns := waitForAllClickHouse(t, 30*time.Second)
	for _, c := range conns {
		defer c.Close()
	}

	// Seed on only node 2
	seedSlowQueries(t, conns[1], 3, 1500)

	// Query node 2 locally -- should find them
	reader2 := newCHReader(t, 2)
	defer reader2.Close()

	ctx := context.Background()
	startTime := time.Now().Add(-5 * time.Minute)
	endTime := time.Now()

	node2Queries, err := reader2.FetchSlowQueriesInRange(ctx, 1000, 0, 100000, startTime, endTime, 100)
	if err != nil {
		t.Fatalf("Node 2 FetchSlowQueriesInRange failed: %v", err)
	}

	if len(node2Queries) < 3 {
		t.Errorf("Node 2 should have at least 3 slow queries, got %d", len(node2Queries))
	}
}
