//go:build integration

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	chreader "github.com/coltconsulting/click-dog/internal/clickhouse"
)

// normalizedShapeExpression turns a random hex marker into a distinct sequence
// of string functions. ClickHouse normalizes literal values and even aliases,
// but it retains function structure in normalized_query_hash. Encoding every
// marker bit as lower() or upper() therefore isolates repeated test runs without
// relying on query_log's second-resolution time bounds.
func normalizedShapeExpression(marker string) string {
	expr := "'fixture'"
	for _, digit := range marker {
		var nibble int
		switch {
		case digit >= '0' && digit <= '9':
			nibble = int(digit - '0')
		case digit >= 'a' && digit <= 'f':
			nibble = int(digit-'a') + 10
		}
		for bit := 0; bit < 4; bit++ {
			function := "lower"
			if nibble&(1<<bit) != 0 {
				function = "upper"
			}
			expr = function + "(" + expr + ")"
		}
	}
	return expr
}

// seedClusterQueryFamilyFixture gives each test run a unique normalized SQL
// shape on every cluster node and waits for every exact query ID to materialize.
func seedClusterQueryFamilyFixture(t *testing.T, conns []driver.Conn, countPerNode, durationMs int) slowQueryFixture {
	t.Helper()
	if countPerNode <= 0 {
		t.Fatalf("query-family fixture count must be positive, got %d", countPerNode)
	}

	marker := newIntegrationFixtureMarker()
	windowStart := time.Now().Add(-clockSkewMargin)
	seedStartUs := windowStart.UnixMicro()
	sleepSeconds := float64(durationMs) / 1000
	shapeExpression := normalizedShapeExpression(marker)
	queryIDs := make([]string, 0, len(conns)*countPerNode)

	for node, conn := range conns {
		nodeQueryIDs := make([]string, 0, countPerNode)
		for i := 0; i < countPerNode; i++ {
			queryID := integrationFixtureQueryID(marker, node*countPerNode+i)
			query := fmt.Sprintf(
				"SELECT sleep(%f), %s AS family_fixture SETTINGS max_execution_time=30",
				sleepSeconds, shapeExpression,
			)
			ctx := clickhouse.Context(context.Background(), clickhouse.WithQueryID(queryID))
			if err := conn.Exec(ctx, query); err != nil {
				t.Fatalf("seed query-family fixture on node %d: %v", node+1, err)
			}
			nodeQueryIDs = append(nodeQueryIDs, queryID)
			queryIDs = append(queryIDs, queryID)
		}
		if err := conn.Exec(context.Background(), "SYSTEM FLUSH LOGS"); err != nil {
			t.Fatalf("flush query-family fixture logs on node %d: %v", node+1, err)
		}
		countQuery := fmt.Sprintf(
			"SELECT count() FROM system.query_log WHERE query_id IN (%s) "+
				"AND type = 'QueryFinish' AND event_time_microseconds >= fromUnixTimestamp64Micro(%d)",
			sqlStringList(nodeQueryIDs), seedStartUs,
		)
		waitForRowCount(t, conn, fmt.Sprintf("query-family fixture rows on node %d", node+1), countQuery, uint64(countPerNode), 5*time.Second)
	}

	return slowQueryFixture{
		Marker:      marker,
		QueryIDs:    queryIDs,
		DurationMs:  durationMs,
		WindowStart: windowStart,
		WindowEnd:   time.Now().Add(clockSkewMargin),
	}
}

func TestIntegration_QueryFamilies_AggregateExactClusterFixtures(t *testing.T) {
	conns := waitForAllClickHouse(t, 30*time.Second)
	for _, conn := range conns {
		defer conn.Close()
	}

	const queriesPerNode = 2
	fixture := seedClusterQueryFamilyFixture(t, conns, queriesPerNode, 20)
	queryIDs := fixture.QueryIDs
	wantExecutions := uint64(len(queryIDs))

	reader := newClusterCHReader(t, 1, "test_cluster")
	defer reader.Close()

	ctx := context.Background()
	logs, err := reader.FetchQueryLogByQueryIDs(ctx, queryIDs, 1)
	if err != nil {
		t.Fatalf("FetchQueryLogByQueryIDs: %v", err)
	}
	if len(logs) != len(queryIDs) {
		t.Fatalf("cluster enrichment returned %d query IDs, want %d exact fixtures", len(logs), len(queryIDs))
	}

	var wantHash uint64
	for _, queryID := range queryIDs {
		log, ok := logs[queryID]
		if !ok {
			t.Fatalf("cluster enrichment omitted fixture query %q", queryID)
		}
		if log.NormalizedQueryHash == 0 {
			t.Fatalf("fixture query %q has zero normalized hash", queryID)
		}
		if wantHash == 0 {
			wantHash = log.NormalizedQueryHash
		}
		if log.NormalizedQueryHash != wantHash {
			t.Fatalf("fixture query %q hash = %d, want shared shape hash %d", queryID, log.NormalizedQueryHash, wantHash)
		}
	}

	rollups, err := reader.FetchQueryFamilyRollups(ctx, chreader.QueryFamilyRollupOptions{
		StartTime:           fixture.WindowStart,
		EndTime:             fixture.WindowEnd,
		MinExecutionCount:   wantExecutions,
		ExactGroupLimit:     10,
		SimilarityThreshold: 1,
		MaxPreviewLength:    200,
	})
	if err != nil {
		t.Fatalf("FetchQueryFamilyRollups: %v", err)
	}
	var fixtureRollups []int
	for i, rollup := range rollups {
		for _, hash := range rollup.MemberHashesSorted {
			if hash == wantHash {
				fixtureRollups = append(fixtureRollups, i)
			}
		}
	}
	if len(fixtureRollups) != 1 {
		t.Fatalf("fixture hash %d appeared in %d cluster rollups, want exactly one: %+v", wantHash, len(fixtureRollups), rollups)
	}
	rollup := rollups[fixtureRollups[0]]
	if len(rollup.MemberHashesSorted) != 1 || rollup.MemberHashesSorted[0] != wantHash {
		t.Errorf("member hashes = %v, want [%d]", rollup.MemberHashesSorted, wantHash)
	}
	if len(rollup.Members) != 1 || rollup.Members[0].ExecutionCount != wantExecutions {
		t.Errorf("members = %+v, want one member with %d executions", rollup.Members, wantExecutions)
	}
	if rollup.Stats.ExecutionCount != wantExecutions || rollup.Stats.SuccessfulCount != wantExecutions || rollup.Stats.FailedCount != 0 {
		t.Errorf("rollup stats = %+v, want %d successful executions and no failures", rollup.Stats, wantExecutions)
	}
	if rollup.FamilyID == "" {
		t.Error("rollup has empty stable family ID")
	}
	if !strings.Contains(strings.ToLower(rollup.RepresentativeQuery), "sleep") {
		t.Errorf("representative query = %q, want normalized fixture shape", rollup.RepresentativeQuery)
	}

	counts, err := reader.FetchQueryFamilyDimensionCounts(ctx, chreader.QueryFamilyDimensionCountOptions{
		StartTime:         fixture.WindowStart,
		EndTime:           fixture.WindowEnd,
		MinExecutionCount: wantExecutions,
		ExactGroupLimit:   10,
		TopK:              5,
	})
	if err != nil {
		t.Fatalf("FetchQueryFamilyDimensionCounts: %v", err)
	}
	wantDimensions := map[chreader.QueryFamilyDimension]bool{
		chreader.QueryFamilyDimensionUser:   false,
		chreader.QueryFamilyDimensionClient: false,
		chreader.QueryFamilyDimensionHost:   false,
	}
	var fixtureCounts []chreader.QueryFamilyDimensionCount
	for _, count := range counts {
		if count.NormalizedQueryHash != wantHash {
			continue
		}
		fixtureCounts = append(fixtureCounts, count)
	}
	if len(fixtureCounts) != len(wantDimensions) {
		t.Fatalf("fixture dimension counts = %+v, want one non-empty value for each of %d dimensions", fixtureCounts, len(wantDimensions))
	}
	for _, count := range fixtureCounts {
		if _, ok := wantDimensions[count.Dimension]; !ok {
			t.Errorf("unexpected dimension %q", count.Dimension)
			continue
		}
		wantDimensions[count.Dimension] = true
		if count.Value == "" {
			t.Errorf("%s has an empty dimension value", count.Dimension)
		}
		if count.ExecutionCount != wantExecutions {
			t.Errorf("%s count = %d, want %d", count.Dimension, count.ExecutionCount, wantExecutions)
		}
	}
	for dimension, found := range wantDimensions {
		if !found {
			t.Errorf("missing %s dimension count", dimension)
		}
	}
}
