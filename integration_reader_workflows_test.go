//go:build integration

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	chreader "github.com/coltconsulting/click-dog/internal/clickhouse"
)

func TestIntegration_TraceDrilldown_RoundTripsAcrossCluster(t *testing.T) {
	conns := waitForAllClickHouse(t, 30*time.Second)
	for _, conn := range conns {
		defer conn.Close()
	}

	fixtures := make([]tracedQueryFixture, 0, len(conns))
	for _, conn := range conns {
		fixtures = append(fixtures, seedTracedQueryFixture(t, conn, 1))
	}

	reader := newClusterCHReader(t, 1, "test_cluster")
	defer reader.Close()

	ctx := context.Background()
	traceIDs := make([]string, 0, len(fixtures))
	for _, fixture := range fixtures {
		got, err := reader.FetchTraceIDsByQueryID(ctx, fixture.QueryIDs[0], 1, 10)
		if err != nil {
			t.Fatalf("FetchTraceIDsByQueryID(%q): %v", fixture.QueryIDs[0], err)
		}
		want := fixture.TraceIDs[0].String()
		if len(got) != 1 || got[0] != want {
			t.Fatalf("trace IDs for query %q = %v, want [%s]", fixture.QueryIDs[0], got, want)
		}
		traceIDs = append(traceIDs, got[0])
	}

	spans, err := reader.FetchSpansForTraceIDs(ctx, traceIDs, 1, 100, nil)
	if err != nil {
		t.Fatalf("FetchSpansForTraceIDs: %v", err)
	}
	spans = exactFixtureSpans(t, spans, fixtures...)

	hostnames := make(map[string]struct{}, len(conns))
	queryIDs := make(map[string]struct{}, len(fixtures))
	for _, span := range spans {
		hostnames[span.Hostname] = struct{}{}
		if queryID := span.Attributes["clickhouse.query_id"]; queryID != "" {
			queryIDs[queryID] = struct{}{}
		}
	}
	if len(hostnames) != len(conns) {
		t.Errorf("drilldown returned spans from %d hosts, want all %d: %v", len(hostnames), len(conns), hostnames)
	}
	for _, fixture := range fixtures {
		if _, ok := queryIDs[fixture.QueryIDs[0]]; !ok {
			t.Errorf("drilldown spans do not carry seeded query ID %q", fixture.QueryIDs[0])
		}
	}
}

func TestIntegration_RecentQueryCandidates_MatchAndHashExactFixtures(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()

	fixture := seedSlowQueryFixture(t, conn, 3, 20)
	reader := newCHReader(t, 1)
	defer reader.Close()

	ctx := context.Background()
	candidates, err := reader.FetchRecentQueryCandidates(ctx, chreader.RecentQueryCandidateOptions{
		StartTime: fixture.WindowStart,
		EndTime:   fixture.WindowEnd,
		Match:     fixture.Marker,
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("FetchRecentQueryCandidates(match): %v", err)
	}
	if len(candidates) != len(fixture.QueryIDs) {
		t.Fatalf("matched candidates = %d, want only %d fixtures", len(candidates), len(fixture.QueryIDs))
	}
	candidates = exactFixtureQueries(t, candidates, fixture)

	hash := candidates[0].NormalizedQueryHash
	if hash == 0 {
		t.Fatal("seeded candidates have zero normalized_query_hash")
	}
	for _, candidate := range candidates {
		if candidate.NormalizedQueryHash != hash {
			t.Fatalf("fixture query %q hash = %d, want shared normalized hash %d", candidate.QueryID, candidate.NormalizedQueryHash, hash)
		}
		if candidate.Query != "" {
			t.Fatalf("recent candidate %q exposed raw query text", candidate.QueryID)
		}
	}

	byHash, err := reader.FetchRecentQueryCandidates(ctx, chreader.RecentQueryCandidateOptions{
		StartTime:           fixture.WindowStart,
		EndTime:             fixture.WindowEnd,
		Match:               fixture.Marker,
		NormalizedQueryHash: hash,
		HasHash:             true,
		Limit:               10,
	})
	if err != nil {
		t.Fatalf("FetchRecentQueryCandidates(hash): %v", err)
	}
	if len(byHash) != len(fixture.QueryIDs) {
		t.Fatalf("hash candidates = %d, want only %d fixtures", len(byHash), len(fixture.QueryIDs))
	}
	byHash = exactFixtureQueries(t, byHash, fixture)
}

func TestIntegration_CurrentQueryCandidates_FindsExactRunningQuery(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()

	reader := newCHReader(t, 1)
	defer reader.Close()

	marker := newIntegrationFixtureMarker()
	queryID := integrationFixtureQueryID(marker, 0)
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		ctx := clickhouse.Context(runCtx, clickhouse.WithQueryID(queryID))
		// sleep() is the workload under observation, not a synchronization
		// delay: the test discovers it with pollUntil and cancels it immediately.
		done <- conn.Exec(ctx, "SELECT sleep(3) SETTINGS max_execution_time=10")
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("running query %q did not stop after cancellation", queryID)
		}
	})

	var candidateElapsed uint64
	var candidatePreview string
	if !pollUntil(5*time.Second, func() bool {
		candidates, err := reader.FetchCurrentQueryCandidates(context.Background(), chreader.CurrentQueryCandidateOptions{
			Match: queryID,
			Limit: 5,
		})
		if err != nil {
			return false
		}
		for _, candidate := range candidates {
			if candidate.QueryID != queryID {
				continue
			}
			if candidate.Query != "" {
				t.Errorf("current candidate exposed raw query text")
			}
			if candidate.ElapsedMs == 0 {
				return false
			}
			candidateElapsed = candidate.ElapsedMs
			candidatePreview = candidate.NormalizedQuery
			return true
		}
		return false
	}) {
		t.Fatalf("running query %q never appeared in current candidates", queryID)
	}
	if candidateElapsed == 0 {
		t.Errorf("running query %q reported zero elapsed milliseconds", queryID)
	}
	if candidatePreview == "" || !strings.Contains(candidatePreview, "sleep") {
		t.Errorf("normalized preview = %q, want a bounded sleep query preview", candidatePreview)
	}

	cancel()
}

func TestIntegration_ReadinessAndCanary_UseLiveDataPlane(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()

	reader := newCHReader(t, 1)
	defer reader.Close()

	ctx := context.Background()
	const thresholdMs = 20
	before, err := reader.RunCanaryQuery(ctx, thresholdMs)
	if err != nil {
		t.Fatalf("RunCanaryQuery(before): %v", err)
	}

	fixture := seedTracedQueryFixtureWithDuration(t, conn, 2, 50*time.Millisecond)
	fixtureTraceIDs := make([]string, len(fixture.TraceIDs))
	for i, traceID := range fixture.TraceIDs {
		fixtureTraceIDs[i] = traceID.String()
	}
	spans, err := reader.FetchSpansForTraceIDs(ctx, fixtureTraceIDs, 1, 100, nil)
	if err != nil {
		t.Fatalf("FetchSpansForTraceIDs: %v", err)
	}
	spans = exactFixtureSpans(t, spans, fixture)
	var qualifying int64
	for _, span := range spans {
		if span.FinishTimeUs-span.StartTimeUs >= thresholdMs*1000 {
			qualifying++
		}
	}
	if qualifying == 0 {
		t.Fatal("seeded trace produced no canary-qualifying spans")
	}

	after, err := reader.RunCanaryQuery(ctx, thresholdMs)
	if err != nil {
		t.Fatalf("RunCanaryQuery(after): %v", err)
	}
	if after.Count != before.Count+qualifying {
		t.Errorf("canary count = %d, want baseline %d + fixture spans %d", after.Count, before.Count, qualifying)
	}
	if !after.LongQueriesExist {
		t.Error("canary did not report long queries after qualifying fixture")
	}

	hasRows, err := reader.HasRecentQueryLogRows(ctx, time.Minute)
	if err != nil {
		t.Fatalf("HasRecentQueryLogRows: %v", err)
	}
	if !hasRows {
		t.Fatal("query_log sanity probe did not see the freshly seeded queries")
	}

	checks := reader.RunReadinessChecks(ctx, chreader.ReadinessOptions{
		Lookback:           time.Minute,
		MinTraceDurationMs: thresholdMs,
		EnrichmentNeeded:   true,
		SampleLimit:        len(fixture.QueryIDs),
	})
	if len(checks) == 0 {
		t.Fatal("RunReadinessChecks returned no checks")
	}
	for _, check := range checks {
		if check.Status != chreader.ReadinessPass {
			t.Errorf("readiness check %q = %s (%s), want PASS", check.Name, check.Status, check.Detail)
		}
	}
	if t.Failed() {
		t.Logf("readiness report: %s", formatReadinessChecks(checks))
	}
}

func formatReadinessChecks(checks []chreader.ReadinessCheck) string {
	parts := make([]string, 0, len(checks))
	for _, check := range checks {
		parts = append(parts, fmt.Sprintf("%s=%s (%s)", check.Name, check.Status, check.Detail))
	}
	return strings.Join(parts, "; ")
}
