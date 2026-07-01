//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	oteltrace "go.opentelemetry.io/otel/trace"

	chreader "github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/export"
	"github.com/coltconsulting/click-dog/internal/leader"
)

// Default integration cluster addresses (matching docker-compose.integration.yml).
// Override via environment variables for non-standard setups.

func integrationCHAddr(node int) string {
	envKey := fmt.Sprintf("CLICKHOUSE%d_ADDR", node)
	if addr := os.Getenv(envKey); addr != "" {
		return addr
	}
	// Ports 19000, 19001, 19002 for nodes 1, 2, 3
	return fmt.Sprintf("localhost:%d", 18999+node)
}

func integrationCHHost(node int) string {
	host, _, _ := strings.Cut(integrationCHAddr(node), ":")
	return host
}

func integrationCHPort(node int) int {
	_, portStr, _ := strings.Cut(integrationCHAddr(node), ":")
	port := 19000
	fmt.Sscanf(portStr, "%d", &port)
	return port
}

func integrationOTELAddr() string {
	if addr := os.Getenv("OTEL_ADDR"); addr != "" {
		return addr
	}
	return "localhost:14317"
}

func integrationKeeperAddr(node int) string {
	envKey := fmt.Sprintf("KEEPER%d_ADDR", node)
	if addr := os.Getenv(envKey); addr != "" {
		return addr
	}
	// Ports 19181, 19182, 19183 for nodes 1, 2, 3
	return fmt.Sprintf("localhost:%d", 19180+node)
}

func integrationAllKeeperAddrs() []string {
	return []string{
		integrationKeeperAddr(1),
		integrationKeeperAddr(2),
		integrationKeeperAddr(3),
	}
}

// -------------------------------------------------------------------
// Service readiness helpers
// -------------------------------------------------------------------

func waitForClickHouse(t *testing.T, node int, timeout time.Duration) driver.Conn {
	t.Helper()
	deadline := time.Now().Add(timeout)
	addr := integrationCHAddr(node)

	for time.Now().Before(deadline) {
		conn, err := clickhouse.Open(&clickhouse.Options{
			Addr: []string{addr},
			Auth: clickhouse.Auth{
				Database: "default",
				Username: "default",
			},
			DialTimeout: 2 * time.Second,
		})
		if err == nil {
			if err := conn.Ping(context.Background()); err == nil {
				return conn
			}
			conn.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("ClickHouse node %d not ready after %v at %s", node, timeout, addr)
	return nil
}

func waitForAllClickHouse(t *testing.T, timeout time.Duration) []driver.Conn {
	t.Helper()
	conns := make([]driver.Conn, 3)
	for i := 1; i <= 3; i++ {
		conns[i-1] = waitForClickHouse(t, i, timeout)
	}
	return conns
}

func waitForKeeper(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	addrs := integrationAllKeeperAddrs()

	for time.Now().Before(deadline) {
		le, err := leader.NewLeaderElection(
			config.LeaderElectionConfig{
				Hosts:          addrs,
				SessionTimeout: 5,
				BasePath:       "/click-dog/health-check",
			},
			func() {},
			func() {},
		)
		if err == nil {
			le.Resign()
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("ClickHouse Keeper not ready after %v at %v", timeout, addrs)
}

func waitForOTELCollector(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	addr := integrationOTELAddr()

	for time.Now().Before(deadline) {
		exporter, err := export.NewOTELExporter(config.OTELConfig{
			CollectorAddress: addr,
			ServiceName:      "health-check",
		})
		if err == nil {
			exporter.Close(context.Background())
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("OTEL collector not ready after %v at %s", timeout, addr)
}

// -------------------------------------------------------------------
// Data seeding helpers
// -------------------------------------------------------------------

// seedSlowQueries runs N queries with sleep() on a specific node so they appear
// in that node's system.query_log with measurable duration.
func seedSlowQueries(t *testing.T, conn driver.Conn, count int, durationMs int) {
	t.Helper()
	ctx := context.Background()
	sleepSec := float64(durationMs) / 1000.0

	// Snapshot before issuing seed queries so the post-flush count filter can
	// require event_time_microseconds >= seedStart. Without this lower bound
	// a leftover row from a prior `make integration` run with the same marker
	// (the marker is derived from t.Name) would let the poll return instantly
	// against stale data and the test would silently skip the real wait.
	//
	// The -clockSkewMargin offset absorbs small client/server clock drift —
	// if the test process's clock runs ahead of the ClickHouse server's, a
	// strict bound would exclude the seeded rows and the poll would time
	// out despite a successful seed. Margin still excludes any row older
	// than a couple seconds before the seed call.
	seedStartUs := time.Now().Add(-clockSkewMargin).UnixMicro()

	marker := queryLogMarker(t)
	for i := 0; i < count; i++ {
		query := fmt.Sprintf(
			"SELECT sleep(%f), '%s-%d' AS test_marker SETTINGS max_execution_time=30",
			sleepSec, marker, i,
		)
		if err := conn.Exec(ctx, query); err != nil {
			t.Fatalf("Failed to seed slow query %d: %v", i, err)
		}
	}

	// Flush logs so they appear in system tables immediately, then poll until
	// the seeded rows are visible. Replaces a fixed time.Sleep that would race
	// against ClickHouse's async log buffer flush on slow CI hosts.
	if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("Failed to flush logs: %v", err)
	}
	// event_time_microseconds (not event_time) so this matches the µs
	// resolution used by seedTracedQueries — the two helpers now agree on
	// what "newer than the seed call" means.
	countQuery := fmt.Sprintf(
		"SELECT count() FROM system.query_log "+
			"WHERE position(query, '%s-') > 0 "+
			"AND type = 'QueryFinish' "+
			"AND event_time_microseconds >= fromUnixTimestamp64Micro(%d)",
		marker,
		seedStartUs,
	)
	waitForRowCount(t, conn, "seeded query_log rows", countQuery, uint64(count), 5*time.Second)
}

// seedTracedQueries runs queries that generate entries in system.opentelemetry_span_log.
// Each query carries an OpenTelemetry trace context so ClickHouse records spans.
//
// All seeded trace IDs share the prefix tracedQueryTraceIDPrefix (in unhyphenated
// form); the post-flush poll matches on that prefix to count rows produced by
// this seed call.
const tracedQueryTraceIDPrefix = "550e8400e29b41d4a716"

// clockSkewMargin is the slack the seed helpers subtract from time.Now() before
// embedding it as the lower bound in their post-flush count queries. The bound
// is compared against ClickHouse-server timestamps (event_time_microseconds /
// finish_time_us); if the test process's clock runs ahead of the server's
// (CI runner skew, virtualised clocks), a strict bound would exclude the
// seeded rows. Two seconds is generous enough for any reasonable drift on
// localhost Docker and still strictly narrower than the prior `time.Sleep(1s)`
// window the gate replaces, so stale-row protection is preserved.
const clockSkewMargin = 2 * time.Second

func seedTracedQueries(t *testing.T, conn driver.Conn, count int) {
	t.Helper()

	// Snapshot in microseconds so the post-flush count filter can require
	// finish_time_us >= seedStartUs. Same rationale as seedSlowQueries: the
	// trace-ID prefix is shared across runs, so a leftover row from a prior
	// run would otherwise let the poll return instantly against stale data.
	// -clockSkewMargin absorbs small client/server drift.
	seedStartUs := time.Now().Add(-clockSkewMargin).UnixMicro()

	for i := 0; i < count; i++ {
		// Create a trace context so ClickHouse populates opentelemetry_span_log.
		// clickhouse-go requires clickhouse.Context + WithSpan (not just Go context).
		traceID, _ := oteltrace.TraceIDFromHex(fmt.Sprintf("%s%012d", tracedQueryTraceIDPrefix, i))
		spanID, _ := oteltrace.SpanIDFromHex(fmt.Sprintf("00f067aa%08x", i+1))
		spanCtx := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
			TraceID:    traceID,
			SpanID:     spanID,
			TraceFlags: oteltrace.FlagsSampled,
		})
		ctx := clickhouse.Context(context.Background(), clickhouse.WithSpan(spanCtx))

		query := fmt.Sprintf(
			"SELECT number, sleep(0.05) FROM system.numbers LIMIT %d SETTINGS max_execution_time=30",
			10+i,
		)
		if err := conn.Exec(ctx, query); err != nil {
			t.Logf("Warning: seeded query %d failed: %v", i, err)
		}
	}

	if err := conn.Exec(context.Background(), "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("Failed to flush logs: %v", err)
	}
	// Each seeded query usually produces multiple spans (a small tree), so poll
	// for at least one matching row — that's enough to confirm FLUSH LOGS has
	// propagated. Callers that need the full tree read it themselves.
	countQuery := fmt.Sprintf(
		"SELECT count() FROM system.opentelemetry_span_log "+
			"WHERE startsWith(replaceAll(toString(trace_id), '-', ''), '%s') "+
			"AND finish_time_us >= %d",
		tracedQueryTraceIDPrefix,
		seedStartUs,
	)
	waitForRowCount(t, conn, "seeded opentelemetry_span_log rows", countQuery, 1, 5*time.Second)
}

// queryLogMarker returns a SQL-safe identifier derived from t.Name that the
// seed helpers embed as a literal in seeded queries. ClickHouse stores the
// query verbatim in system.query_log, so the marker lets waitForRowCount
// distinguish this test's seeded rows from any other tenant traffic on the
// shared cluster.
func queryLogMarker(t *testing.T) string {
	t.Helper()
	// t.Name on subtests contains '/'; replace so the marker stays a single
	// position()-matchable token without needing escapes.
	return strings.ReplaceAll(t.Name(), "/", "_")
}

// waitForRowCount polls a ClickHouse count() expression until it returns at
// least minCount or the timeout expires. Used after SYSTEM FLUSH LOGS to wait
// for seeded rows to materialise in system tables, replacing fixed sleeps
// that were both slow on fast CI hosts and flaky on slow ones.
func waitForRowCount(t *testing.T, conn driver.Conn, label, countQuery string, minCount uint64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got uint64
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := conn.QueryRow(ctx, countQuery).Scan(&got)
		cancel()
		if err == nil && got >= minCount {
			return
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("%s: count query never reached %d within %v (last count=%d, last err=%v): %s", label, minCount, timeout, got, lastErr, countQuery)
	}
	t.Fatalf("%s: count query never reached %d within %v (last count=%d): %s", label, minCount, timeout, got, countQuery)
}

// seedSlowQueriesOnAllNodes seeds queries across all 3 cluster nodes.
func seedSlowQueriesOnAllNodes(t *testing.T, conns []driver.Conn, countPerNode int, durationMs int) {
	t.Helper()
	for i, conn := range conns {
		t.Logf("Seeding %d slow queries on node %d", countPerNode, i+1)
		seedSlowQueries(t, conn, countPerNode, durationMs)
	}
}

// -------------------------------------------------------------------
// OTEL file exporter verification helpers
// -------------------------------------------------------------------

func otelOutputDir() string {
	if dir := os.Getenv("OTEL_OUTPUT_DIR"); dir != "" {
		return dir
	}
	_, filename, _, _ := runtime.Caller(0)
	projectRoot := filepath.Dir(filename)
	return filepath.Join(projectRoot, "testing", "otel-output")
}

func clearOTELOutput(t *testing.T) {
	t.Helper()
	// No-op: the OTEL collector holds an open file descriptor to traces.jsonl.
	// Removing the file causes writes to go to the deleted inode (invisible
	// from the filesystem). Truncating risks sparse files if the collector
	// doesn't use O_APPEND. Tests use unique span names, so accumulated
	// data across tests doesn't cause interference.
}

// readOTELFileExporterSpans reads the JSONL output from the OTEL collector's
// file exporter and returns all span names found.
func readOTELFileExporterSpans(t *testing.T) []string {
	t.Helper()
	filePath := filepath.Join(otelOutputDir(), "traces.jsonl")

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Logf("OTEL output file does not exist: %s", filePath)
			return nil
		}
		t.Fatalf("Failed to read OTEL output file: %v", err)
	}

	t.Logf("OTEL output file size: %d bytes", len(data))
	if len(data) > 0 && len(data) < 2000 {
		t.Logf("OTEL output file contents: %s", string(data))
	}

	var spanNames []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			t.Logf("OTEL output: failed to parse JSON line (%d bytes): %v", len(line), err)
			continue
		}
		spanNames = append(spanNames, extractSpanNamesFromOTLP(payload)...)
	}
	return spanNames
}

func waitForOTELFileSpans(t *testing.T, timeout time.Duration, condition func([]string) bool, failure string) []string {
	t.Helper()
	spanNames, ok := pollOTELFileSpans(t, timeout, condition)
	if ok {
		return spanNames
	}

	t.Fatalf("%s after %v; received span names: %v", failure, timeout, spanNames)
	return nil
}

func waitForOTELSpanNameCount(t *testing.T, spanName string, minCount int, timeout time.Duration) []string {
	t.Helper()
	return waitForOTELFileSpans(t, timeout, func(spanNames []string) bool {
		return countOTELSpanNames(spanNames, spanName) >= minCount
	}, fmt.Sprintf("expected at least %d spans named %q", minCount, spanName))
}

func waitForOTELSpanTotalAbove(t *testing.T, baseline int, timeout time.Duration) []string {
	t.Helper()
	return waitForOTELFileSpans(t, timeout, func(spanNames []string) bool {
		return len(spanNames) > baseline
	}, fmt.Sprintf("expected more than %d spans", baseline))
}

func waitForOTELSpanTotalAboveOrTimeout(t *testing.T, baseline int, timeout time.Duration) []string {
	t.Helper()
	spanNames, _ := pollOTELFileSpans(t, timeout, func(spanNames []string) bool {
		return len(spanNames) > baseline
	})
	return spanNames
}

func pollOTELFileSpans(t *testing.T, timeout time.Duration, condition func([]string) bool) ([]string, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var spanNames []string

	for time.Now().Before(deadline) {
		spanNames = readOTELFileExporterSpans(t)
		if condition(spanNames) {
			return spanNames, true
		}
		time.Sleep(500 * time.Millisecond)
	}

	return spanNames, false
}

func countOTELSpanNames(spanNames []string, target string) int {
	count := 0
	for _, spanName := range spanNames {
		if spanName == target {
			count++
		}
	}
	return count
}

func extractSpanNamesFromOTLP(payload map[string]interface{}) []string {
	var names []string
	resourceSpans, ok := payload["resourceSpans"].([]interface{})
	if !ok {
		return names
	}
	for _, rs := range resourceSpans {
		rsMap, ok := rs.(map[string]interface{})
		if !ok {
			continue
		}
		scopeSpans, ok := rsMap["scopeSpans"].([]interface{})
		if !ok {
			continue
		}
		for _, ss := range scopeSpans {
			ssMap, ok := ss.(map[string]interface{})
			if !ok {
				continue
			}
			spans, ok := ssMap["spans"].([]interface{})
			if !ok {
				continue
			}
			for _, s := range spans {
				sMap, ok := s.(map[string]interface{})
				if !ok {
					continue
				}
				if name, ok := sMap["name"].(string); ok {
					names = append(names, name)
				}
			}
		}
	}
	return names
}

// -------------------------------------------------------------------
// Test isolation helpers
// -------------------------------------------------------------------

// uniqueBasePath returns a unique Keeper base path per test to avoid interference.
func uniqueBasePath(t *testing.T) string {
	t.Helper()
	name := strings.ReplaceAll(t.Name(), "/", "-")
	return fmt.Sprintf("/click-dog/test-%s-%d", name, time.Now().UnixNano())
}

// newCHReader creates a ClickHouseReader connected to the given node.
func newCHReader(t *testing.T, node int) *chreader.ClickHouseReader {
	t.Helper()
	reader, err := chreader.NewClickHouseReader(config.ClickHouseConfig{
		Host:           integrationCHHost(node),
		Port:           integrationCHPort(node),
		Database:       "default",
		Username:       "default",
		Password:       "",
		MaxOpenConns:   2,
		MaxIdleConns:   1,
		QueryTimeoutS:  30,
		MaxMemoryUsage: 104857600,
	}, config.FiltersConfig{})
	if err != nil {
		t.Fatalf("Failed to create ClickHouseReader for node %d: %v", node, err)
	}
	return reader
}

// newClusterCHReader creates a ClickHouseReader with cluster() queries enabled,
// connected to the specified node.
func newClusterCHReader(t *testing.T, node int, clusterName string) *chreader.ClickHouseReader {
	t.Helper()
	reader, err := chreader.NewClickHouseReader(config.ClickHouseConfig{
		Host:              integrationCHHost(node),
		Port:              integrationCHPort(node),
		Database:          "default",
		Username:          "default",
		Password:          "",
		Cluster:           clusterName,
		UseClusterQueries: true,
		MaxOpenConns:      2,
		MaxIdleConns:      1,
		QueryTimeoutS:     30,
		MaxMemoryUsage:    104857600,
	}, config.FiltersConfig{})
	if err != nil {
		t.Fatalf("Failed to create cluster ClickHouseReader for node %d: %v", node, err)
	}
	return reader
}

// newOTELExporter creates an OTELExporter connected to the integration collector.
func newOTELExporter(t *testing.T, serviceName string) *export.OTELExporter {
	t.Helper()
	exporter, err := export.NewOTELExporter(config.OTELConfig{
		CollectorAddress: integrationOTELAddr(),
		ServiceName:      serviceName,
	})
	if err != nil {
		t.Fatalf("Failed to create OTELExporter: %v", err)
	}
	return exporter
}
