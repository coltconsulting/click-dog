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
	"github.com/coltconsulting/click-dog/internal/model"
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

	fixture := seedSlowQueryFixture(t, conn, 3, 1500)

	reader := newCHReader(t, 1)
	defer reader.Close()

	ctx := context.Background()
	queries, err := reader.FetchSlowQueriesInRange(ctx, 1000, 0, 100000, fixture.WindowStart, fixture.WindowEnd, 100)
	if err != nil {
		t.Fatalf("FetchSlowQueriesInRange failed: %v", err)
	}

	queries = exactFixtureQueries(t, queries, fixture)
	if len(queries) != len(fixture.QueryIDs) {
		t.Fatalf("exact queries = %d, want %d", len(queries), len(fixture.QueryIDs))
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

	fixture := seedTracedQueryFixture(t, conn, 5)

	reader := newCHReader(t, 1)
	defer reader.Close()

	ctx := context.Background()
	spans, err := reader.FetchOpenTelemetrySpans(ctx, 1, 0, 0, 0, 10*time.Minute, 1000)
	if err != nil {
		t.Fatalf("FetchOpenTelemetrySpans failed: %v", err)
	}

	spans = exactFixtureSpans(t, spans, fixture)

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

	fast := seedTracedQueryFixtureWithDuration(t, conn, 2, 100*time.Millisecond)
	slow := seedTracedQueryFixtureWithDuration(t, conn, 2, 1700*time.Millisecond)

	ctx := context.Background()

	filteredReader := newCHReader(t, 1)
	defer filteredReader.Close()
	spans, err := filteredReader.FetchOpenTelemetrySpans(ctx, 1500, 0, 0, 0, 10*time.Minute, 1000)
	if err != nil {
		t.Fatalf("FetchOpenTelemetrySpans failed: %v", err)
	}
	filteredTraces := traceIDsOf(spans)
	for _, traceID := range slow.TraceIDs {
		if !filteredTraces[traceID] {
			t.Errorf("slow trace %s missing from min-duration result", traceID)
		}
	}
	for _, traceID := range fast.TraceIDs {
		if filteredTraces[traceID] {
			t.Errorf("fast trace %s incorrectly passed min-duration filter", traceID)
		}
	}

	allReader := newCHReader(t, 1)
	defer allReader.Close()
	allSpans, err := allReader.FetchOpenTelemetrySpans(ctx, 1, 0, 0, 0, 10*time.Minute, 1000)
	if err != nil {
		t.Fatalf("FetchOpenTelemetrySpans (all) failed: %v", err)
	}
	exactFixtureSpans(t, allSpans, fast, slow)
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
	fixture := seedTracedQueryFixture(t, conn, 5)

	uncappedReader := newCHReader(t, 1)
	defer uncappedReader.Close()

	ctx := context.Background()
	const spanCap = 3
	eligible, err := uncappedReader.FetchOpenTelemetrySpans(ctx, 1, 0, 0, 0, 10*time.Minute, 1000)
	if err != nil {
		t.Fatalf("uncapped FetchOpenTelemetrySpans failed: %v", err)
	}
	if got := len(exactFixtureSpans(t, eligible, fixture)); got <= spanCap {
		t.Fatalf("eligible fixture spans = %d, want more than cap %d", got, spanCap)
	}

	cappedReader := newCHReader(t, 1)
	defer cappedReader.Close()
	spans, err := cappedReader.FetchOpenTelemetrySpans(ctx, 1, 0, 0, 0, 10*time.Minute, spanCap)
	if err != nil {
		t.Fatalf("FetchOpenTelemetrySpans failed: %v", err)
	}

	if len(spans) != spanCap {
		t.Errorf("max_spans_per_cycle=%d returned %d spans, want exactly %d with excess eligible rows", spanCap, len(spans), spanCap)
	}
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
	fixture := seedTracedQueryFixture(t, conn, 3)

	reader := newCHReader(t, 1)
	defer reader.Close()

	ctx := context.Background()

	// Fetch enrichment data
	queryLogMap, err := reader.FetchQueryLogByQueryIDs(ctx, fixture.QueryIDs, 2)
	if err != nil {
		t.Fatalf("FetchQueryLogByQueryIDs failed: %v", err)
	}

	if len(queryLogMap) != len(fixture.QueryIDs) {
		t.Fatalf("enrichment rows = %d, want exact fixture count %d", len(queryLogMap), len(fixture.QueryIDs))
	}

	// Verify enrichment fields are populated
	for _, qid := range fixture.QueryIDs {
		ql, ok := queryLogMap[qid]
		if !ok {
			t.Errorf("missing exact query ID %s", qid)
			continue
		}
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

	fixture := seedTracedQueryFixture(t, conn, 2)

	reader := newCHReader(t, 1)
	defer reader.Close()

	ctx := context.Background()

	// Fetch spans
	spans, err := reader.FetchOpenTelemetrySpans(ctx, 1, 0, 0, 0, 10*time.Minute, 1000)
	if err != nil {
		t.Fatalf("FetchOpenTelemetrySpans failed: %v", err)
	}
	spans = exactFixtureSpans(t, spans, fixture)

	// Enrich
	queryLogMap, err := reader.FetchQueryLogByQueryIDs(ctx, fixture.QueryIDs, 2)
	if err != nil {
		t.Fatalf("FetchQueryLogByQueryIDs failed: %v", err)
	}

	enrichedQueryIDs := make(map[string]bool, len(fixture.QueryIDs))
	for i := range spans {
		if qid := spans[i].Attributes["clickhouse.query_id"]; qid != "" {
			if ql, ok := queryLogMap[qid]; ok {
				processor.EnrichSpanFromQueryLog(&spans[i], ql, 100000)
				enrichedQueryIDs[qid] = true
				if spans[i].Attributes["query_log.query_id"] != qid {
					t.Errorf("query %s: enriched query_log.query_id = %q", qid, spans[i].Attributes["query_log.query_id"])
				}
				if spans[i].Attributes["query_log.user"] == "" {
					t.Errorf("query %s: query_log.user should not be empty", qid)
				}
				if spans[i].Attributes["query_log.memory_usage"] == "" {
					t.Errorf("query %s: query_log.memory_usage should be set", qid)
				}
			}
		}
	}

	for _, queryID := range fixture.QueryIDs {
		if !enrichedQueryIDs[queryID] {
			t.Errorf("fixture query %s had no enriched span", queryID)
		}
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
	fixtures := seedSlowQueryFixturesOnAllNodes(t, conns, 2, 1500)

	// Create a cluster-aware reader connected to node 1
	reader := newClusterCHReader(t, 1, "test_cluster")
	defer reader.Close()

	ctx := context.Background()
	startTime, endTime := fixtureBounds(fixtures)

	queries, err := reader.FetchSlowQueriesInRange(ctx, 1000, 0, 100000, startTime, endTime, 100)
	if err != nil {
		t.Fatalf("Cluster FetchSlowQueriesInRange failed: %v", err)
	}

	want := queryIDsFromFixtures(fixtures)
	got := queryIDSet(t, queries, want)
	if len(got) != len(want) {
		t.Errorf("cluster query IDs = %v, want every fixture ID %v", got, want)
	}
}

func TestIntegration_ClusterQueries_FetchSpans(t *testing.T) {
	conns := waitForAllClickHouse(t, 30*time.Second)
	for _, c := range conns {
		defer c.Close()
	}

	fixtures := make([]tracedQueryFixture, 0, len(conns))
	for _, conn := range conns {
		fixtures = append(fixtures, seedTracedQueryFixture(t, conn, 3))
	}

	reader := newClusterCHReader(t, 1, "test_cluster")
	defer reader.Close()

	ctx := context.Background()
	spans, err := reader.FetchOpenTelemetrySpans(ctx, 1, 0, 0, 0, 10*time.Minute, 1000)
	if err != nil {
		t.Fatalf("Cluster FetchOpenTelemetrySpans failed: %v", err)
	}

	spans = exactFixtureSpans(t, spans, fixtures...)

	// Verify spans come from multiple hostnames
	hostnames := make(map[string]int)
	for _, span := range spans {
		hostnames[span.Hostname]++
	}
	t.Logf("Spans by hostname: %v", hostnames)

	if len(hostnames) != len(conns) {
		t.Errorf("fixture spans came from %d hostnames, want all %d nodes: %v", len(hostnames), len(conns), hostnames)
	}
}

func TestIntegration_ClusterQueries_VsLocalQueries(t *testing.T) {
	conns := waitForAllClickHouse(t, 30*time.Second)
	for _, c := range conns {
		defer c.Close()
	}

	// Seed on all nodes
	fixtures := seedSlowQueryFixturesOnAllNodes(t, conns, 2, 1500)

	// Local reader (node 1 only)
	localReader := newCHReader(t, 1)
	defer localReader.Close()

	// Cluster reader (queries all shards)
	clusterReader := newClusterCHReader(t, 1, "test_cluster")
	defer clusterReader.Close()

	ctx := context.Background()
	startTime, endTime := fixtureBounds(fixtures)

	localQueries, err := localReader.FetchSlowQueriesInRange(ctx, 1000, 0, 100000, startTime, endTime, 100)
	if err != nil {
		t.Fatalf("Local FetchSlowQueriesInRange failed: %v", err)
	}

	clusterQueries, err := clusterReader.FetchSlowQueriesInRange(ctx, 1000, 0, 100000, startTime, endTime, 100)
	if err != nil {
		t.Fatalf("Cluster FetchSlowQueriesInRange failed: %v", err)
	}

	wantLocal := stringSet(fixtures[0].QueryIDs)
	wantCluster := queryIDsFromFixtures(fixtures)
	gotLocal := queryIDSet(t, localQueries, wantCluster)
	gotCluster := queryIDSet(t, clusterQueries, wantCluster)
	if !sameStringSet(gotLocal, wantLocal) {
		t.Errorf("local fixture query IDs = %v, want %v", gotLocal, wantLocal)
	}
	if !sameStringSet(gotCluster, wantCluster) {
		t.Errorf("cluster fixture query IDs = %v, want %v", gotCluster, wantCluster)
	}
}

func TestIntegration_EachNodeHasOwnQueryLog(t *testing.T) {
	conns := waitForAllClickHouse(t, 30*time.Second)
	for _, c := range conns {
		defer c.Close()
	}

	// Seed on only node 2
	fixture := seedSlowQueryFixture(t, conns[1], 3, 1500)

	// Query node 2 locally -- should find them
	reader2 := newCHReader(t, 2)
	defer reader2.Close()

	ctx := context.Background()
	node2Queries, err := reader2.FetchSlowQueriesInRange(ctx, 1000, 0, 100000, fixture.WindowStart, fixture.WindowEnd, 100)
	if err != nil {
		t.Fatalf("Node 2 FetchSlowQueriesInRange failed: %v", err)
	}

	got := queryIDSet(t, node2Queries, stringSet(fixture.QueryIDs))
	if len(got) != len(fixture.QueryIDs) {
		t.Errorf("node 2 fixture query IDs = %v, want %v", got, fixture.QueryIDs)
	}
}

func traceIDsOf(spans []model.OpenTelemetrySpan) map[uuid.UUID]bool {
	result := make(map[uuid.UUID]bool)
	for _, span := range spans {
		result[span.TraceID] = true
	}
	return result
}

func fixtureBounds(fixtures []slowQueryFixture) (time.Time, time.Time) {
	start := fixtures[0].WindowStart
	end := fixtures[0].WindowEnd
	for _, fixture := range fixtures[1:] {
		if fixture.WindowStart.Before(start) {
			start = fixture.WindowStart
		}
		if fixture.WindowEnd.After(end) {
			end = fixture.WindowEnd
		}
	}
	return start, end
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func queryIDsFromFixtures(fixtures []slowQueryFixture) map[string]struct{} {
	result := make(map[string]struct{})
	for _, fixture := range fixtures {
		for _, queryID := range fixture.QueryIDs {
			result[queryID] = struct{}{}
		}
	}
	return result
}

func queryIDSet(t *testing.T, queries []model.QueryLog, allowed map[string]struct{}) map[string]struct{} {
	t.Helper()
	result := make(map[string]struct{})
	for _, query := range queries {
		if _, ok := allowed[query.QueryID]; ok {
			if _, duplicate := result[query.QueryID]; duplicate {
				t.Errorf("reader returned fixture query %s more than once", query.QueryID)
			}
			result[query.QueryID] = struct{}{}
		}
	}
	return result
}

func sameStringSet(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for value := range left {
		if _, ok := right[value]; !ok {
			return false
		}
	}
	return true
}
