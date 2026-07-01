//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Live guards for CountDistinctClusterReaders (internal/clickhouse/reader.go).
// The unit test (internal/clickhouse/cluster_readers_test.go) pins only the
// LOCAL SQL string; it cannot see how ClickHouse rewrites and re-serializes the
// probe per replica through clusterAllReplicas — which is exactly where both
// failure modes from issue #242 live. These convert the two comment-level
// warnings ("cluster( match fragility", the buildTableRef coupling NOTE) into
// executable checks against the real 3-node cluster, with no compose changes.

// TestIntegration_ClusterReaders_SpellingCoupling pins, end to end, that the
// probe still matches the table-ref shape a real cluster span read emits. The
// probe keys off the verbatim `cluster(` + `opentelemetry_span_log` substrings
// buildTableRef produces; if a future span-read path switched to
// clusterAllReplicas(...) (whose text has no `cluster(` substring) the read would
// go invisible to the scan — a silent false negative this test catches as count 0.
func TestIntegration_ClusterReaders_SpellingCoupling(t *testing.T) {
	conns := waitForAllClickHouse(t, 30*time.Second)
	for _, c := range conns {
		defer c.Close()
	}

	// system.opentelemetry_span_log is created lazily, the first time a node emits
	// a span. Seed one traced query per node so the table exists on every shard —
	// otherwise the whole-cluster read's fan-out errors before it can log.
	for _, c := range conns {
		seedTracedQueries(t, c, 1)
	}

	reader := newClusterCHReader(t, 1, "test_cluster")
	defer reader.Close()

	ctx := context.Background()

	// One real whole-cluster span read emits cluster('test_cluster',
	// system.opentelemetry_span_log) into node 1's query_log — that read is the
	// cluster-reader footprint the probe hunts for.
	if _, err := reader.FetchOpenTelemetrySpans(ctx, 1, 0, 0, 0, 10*time.Minute, 100); err != nil {
		t.Fatalf("cluster span read failed: %v", err)
	}
	flushAllLogs(t, conns)

	const lookback, active = 15 * time.Minute, 2 * time.Minute
	var got int
	// query_log flushes asynchronously even after SYSTEM FLUSH LOGS; poll until
	// the read becomes visible to the cross-cluster scan.
	ok := pollUntil(10*time.Second, func() bool {
		n, err := reader.CountDistinctClusterReaders(ctx, lookback, active)
		if err != nil {
			t.Fatalf("CountDistinctClusterReaders failed: %v", err)
		}
		got = n
		return n >= 1
	})
	if !ok {
		t.Fatalf("probe never saw the cluster span read (count stayed %d) — the probe no longer matches the cluster( shape buildTableRef emits", got)
	}

	// Exactly one: this single test host is the only cluster reader. A count >1
	// would mean the probe is also matching the re-serialized remote copies of the
	// read's fan-out — the constant-folding false positive of #242 concern 1.
	if got != 1 {
		t.Fatalf("CountDistinctClusterReaders = %d after one cluster read from one host, want 1", got)
	}
}

// TestIntegration_ClusterReaders_NoSelfMatch pins that the probe never counts its
// own scans. CountDistinctClusterReaders reads through clusterAllReplicas(...) and
// splits its match substrings ('cluster' || '(' and 'opentelemetry_span' || '_log')
// so its own query text cannot match. Run far more often than the active window
// with no concurrent span reader: if a ClickHouse version ever constant-folds
// those splits before per-replica re-serialization, each probe would begin
// counting earlier probes and the count would climb past 1.
func TestIntegration_ClusterReaders_NoSelfMatch(t *testing.T) {
	conns := waitForAllClickHouse(t, 30*time.Second)
	for _, c := range conns {
		defer c.Close()
	}

	reader := newClusterCHReader(t, 1, "test_cluster")
	defer reader.Close()

	ctx := context.Background()

	const lookback, active = 15 * time.Minute, 2 * time.Minute
	for i := 0; i < 8; i++ {
		n, err := reader.CountDistinctClusterReaders(ctx, lookback, active)
		if err != nil {
			t.Fatalf("CountDistinctClusterReaders (iter %d) failed: %v", i, err)
		}
		// Every read in this suite originates from this one test host, so a healthy
		// probe collapses them to a single client_hostname — never more than 1. A
		// run of probes that climbs past 1 means the probe is matching its own
		// clusterAllReplicas scans.
		if n > 1 {
			t.Fatalf("probe counted %d cluster readers on iter %d from a single host — it is matching its own clusterAllReplicas scans", n, i)
		}
	}
}

// flushAllLogs forces every node's system.* logs to disk so a cross-cluster
// query_log scan can see rows written on any replica. SYSTEM FLUSH LOGS is
// node-local, so it must run on each connection.
func flushAllLogs(t *testing.T, conns []driver.Conn) {
	t.Helper()
	for i, c := range conns {
		if err := c.Exec(context.Background(), "SYSTEM FLUSH LOGS"); err != nil {
			t.Fatalf("flush logs on node %d: %v", i+1, err)
		}
	}
}
