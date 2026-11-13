//go:build integration

package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	chreader "github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/processor"
)

// The row filters are SQL, so only a real ClickHouse proves that match(),
// isIPAddressInRange(), and the originating-address expression behave as the
// Go filters do. Each run seeds three queries of a family shape unique to the
// run, tagged <marker>-0..2, then reads them back through readers carrying
// each filter: as rows (backfill, trace-drilldown candidates) and as the
// family rollup and dimension counts analyze reports and notifies from.
func TestIntegration_RowFilters_MatchGoFilters(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()

	marker := newIntegrationFixtureMarker()
	shape := normalizedShapeExpression(marker)
	windowStart := time.Now().Add(-clockSkewMargin)
	var queryIDs []string
	for i := 0; i < 3; i++ {
		queryID := integrationFixtureQueryID(marker, i)
		query := fmt.Sprintf("SELECT sleep(0.02), %s AS family_fixture, '%s-%d' AS tag SETTINGS max_execution_time=30", shape, marker, i)
		if err := conn.Exec(clickhouse.Context(context.Background(), clickhouse.WithQueryID(queryID)), query); err != nil {
			t.Fatalf("seed query %d: %v", i, err)
		}
		queryIDs = append(queryIDs, queryID)
	}
	if err := conn.Exec(context.Background(), "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("flush logs: %v", err)
	}
	waitForRowCount(t, conn, "row-filter fixture rows", fmt.Sprintf(
		"SELECT count() FROM system.query_log WHERE query_id IN (%s) AND type = 'QueryFinish'", sqlStringList(queryIDs),
	), 3, 5*time.Second)
	windowEnd := time.Now().Add(clockSkewMargin)

	ctx := context.Background()
	// The fixture's own originating address, as the Go filter resolves it.
	reader := newCHReader(t, 1)
	logs, err := reader.FetchQueryLogByQueryIDs(ctx, queryIDs[:1], 1, 0)
	_ = reader.Close()
	if err != nil || logs[queryIDs[0]].NormalizedQueryHash == 0 {
		t.Fatalf("FetchQueryLogByQueryIDs: %v (%d rows)", err, len(logs))
	}
	origin := processor.OriginatingAddress(logs[queryIDs[0]])
	prefix, err := config.ClientPrefix(origin)
	if err != nil {
		t.Fatalf("fixture origin %q: %v", origin, err)
	}
	t.Logf("fixture originating address %q, prefix %s", origin, prefix)
	familyHash := logs[queryIDs[0]].NormalizedQueryHash

	// fixtureRows returns which fixture tags a reader's row and aggregate
	// paths admit; every path must agree.
	fixtureRows := func(t *testing.T, filters config.FiltersConfig) []string {
		t.Helper()
		reader := newFilteredCHReader(t, 1, filters)
		defer reader.Close()

		rows, err := reader.FetchSlowQueriesInRangePage(ctx, 0, 0, 0, windowStart, windowEnd, nil, 100000)
		if err != nil {
			t.Fatalf("FetchSlowQueriesInRangePage: %v", err)
		}
		var tags []string
		for _, row := range rows {
			for i, id := range queryIDs {
				if row.QueryID == id {
					tags = append(tags, fmt.Sprintf("%d", i))
				}
			}
		}
		sort.Strings(tags)

		candidates, err := reader.FetchRecentQueryCandidates(ctx, chreader.RecentQueryCandidateOptions{
			StartTime: windowStart, EndTime: windowEnd, Match: marker, Limit: 100,
		})
		if err != nil {
			t.Fatalf("FetchRecentQueryCandidates: %v", err)
		}
		var candidateCount int
		for _, c := range candidates {
			for _, id := range queryIDs {
				if c.QueryID == id {
					candidateCount++
				}
			}
		}
		if candidateCount != len(tags) {
			t.Errorf("recent candidates admitted %d fixture queries, rows admitted %v", candidateCount, tags)
		}

		// No fixture is running, but the candidate query itself is, so this
		// proves the system.processes SQL is valid with the filters attached.
		if _, err := reader.FetchCurrentQueryCandidates(ctx, chreader.CurrentQueryCandidateOptions{Limit: 10}); err != nil {
			t.Fatalf("FetchCurrentQueryCandidates: %v", err)
		}

		var executions uint64
		rollups, err := reader.FetchQueryFamilyRollups(ctx, chreader.QueryFamilyRollupOptions{
			StartTime: windowStart, EndTime: windowEnd, MinExecutionCount: 1, ExactGroupLimit: 1000,
			SimilarityThreshold: 1, MaxPreviewLength: 4000,
		})
		if err != nil {
			t.Fatalf("FetchQueryFamilyRollups: %v", err)
		}
		for _, rollup := range rollups {
			for _, member := range rollup.Members {
				if member.NormalizedQueryHash == familyHash {
					executions += member.ExecutionCount
				}
			}
		}
		if executions != uint64(len(tags)) {
			t.Errorf("family rollup counted %d fixture executions, rows admitted %v", executions, tags)
		}

		counts, err := reader.FetchQueryFamilyDimensionCounts(ctx, chreader.QueryFamilyDimensionCountOptions{
			StartTime: windowStart, EndTime: windowEnd, MinExecutionCount: 1, ExactGroupLimit: 1000,
		})
		if err != nil {
			t.Fatalf("FetchQueryFamilyDimensionCounts: %v", err)
		}
		var userCount uint64
		for _, c := range counts {
			if c.NormalizedQueryHash == familyHash && c.Dimension == chreader.QueryFamilyDimensionUser {
				userCount += c.ExecutionCount
			}
		}
		if userCount != uint64(len(tags)) {
			t.Errorf("dimension counts covered %d fixture executions, rows admitted %v", userCount, tags)
		}
		return tags
	}

	all := fixtureRows(t, config.FiltersConfig{})
	if strings.Join(all, ",") != "0,1,2" {
		t.Fatalf("unfiltered reader admitted %v, want all three fixtures", all)
	}

	tests := []struct {
		name    string
		filters config.FiltersConfig
		want    string
	}{
		{"blacklist_queries drops the matching query", config.FiltersConfig{BlacklistQueries: []string{marker + "-1"}}, "0,2"},
		{"blacklist_queries honors RE2 flags", config.FiltersConfig{BlacklistQueries: []string{"(?i)" + strings.ToUpper(marker) + "-[02]"}}, "1"},
		{"whitelist_ips admits the client as ClickHouse spells it", config.FiltersConfig{WhitelistIPs: []string{origin}}, "0,1,2"},
		{"whitelist_ips admits the client's prefix", config.FiltersConfig{WhitelistIPs: []string{prefix.String()}}, "0,1,2"},
		{"whitelist_ips excludes another network", config.FiltersConfig{WhitelistIPs: []string{"192.0.2.0/24", "2001:db8::/32"}}, ""},
		{"blacklist_queries and whitelist_ips together", config.FiltersConfig{
			BlacklistQueries: []string{marker + "-0"},
			WhitelistIPs:     []string{"192.0.2.1", prefix.String()},
		}, "1,2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := strings.Join(fixtureRows(t, tt.filters), ","); got != tt.want {
				t.Errorf("admitted fixtures %q, want %q", got, tt.want)
			}
		})
	}
}

// Two cases the single-shape fixture above cannot reach. ClickHouse's match()
// lets . span a line break, so the Go filter must too, or live export would
// ship a statement analyze hides. And a distributed query's secondary rows
// carry the shard's rewritten text, not the client's, so blacklist_queries
// must reach them through their initial query. prefer_localhost_replica = 0
// makes remote() to this node run as a logged secondary query.
func TestIntegration_RowFilters_BlacklistAcrossLinesAndShards(t *testing.T) {
	conn := waitForClickHouse(t, 1, 30*time.Second)
	defer conn.Close()

	marker := newIntegrationFixtureMarker()
	windowStart := time.Now().Add(-clockSkewMargin)
	multiLineID := integrationFixtureQueryID(marker, 0)
	distributedID := integrationFixtureQueryID(marker, 1)
	seeds := map[string]string{
		multiLineID:   fmt.Sprintf("SELECT 1 AS first,\n  '%s-ml' AS tag SETTINGS max_execution_time=30", marker),
		distributedID: fmt.Sprintf("SELECT '%s-rm' AS tag FROM remote('127.0.0.1', system.one) SETTINGS prefer_localhost_replica = 0, max_execution_time=30", marker),
	}
	for id, query := range seeds {
		if err := conn.Exec(clickhouse.Context(context.Background(), clickhouse.WithQueryID(id)), query); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	if err := conn.Exec(context.Background(), "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("flush logs: %v", err)
	}
	waitForRowCount(t, conn, "secondary query row", fmt.Sprintf(
		"SELECT count() FROM system.query_log WHERE initial_query_id = '%s' AND NOT is_initial_query AND type = 'QueryFinish'", distributedID,
	), 1, 5*time.Second)
	waitForRowCount(t, conn, "multi-line query row", fmt.Sprintf(
		"SELECT count() FROM system.query_log WHERE query_id = '%s' AND type = 'QueryFinish'", multiLineID,
	), 1, 5*time.Second)
	var secondaryID, secondaryText string
	if err := conn.QueryRow(context.Background(), fmt.Sprintf(
		"SELECT query_id, query FROM system.query_log WHERE initial_query_id = '%s' AND NOT is_initial_query AND type = 'QueryFinish' LIMIT 1", distributedID,
	)).Scan(&secondaryID, &secondaryText); err != nil {
		t.Fatalf("read secondary query: %v", err)
	}
	windowEnd := time.Now().Add(clockSkewMargin)

	ctx := context.Background()
	admitted := func(t *testing.T, filters config.FiltersConfig) map[string]bool {
		t.Helper()
		reader := newFilteredCHReader(t, 1, filters)
		defer reader.Close()
		rows, err := reader.FetchSlowQueriesInRangePage(ctx, 0, 0, 0, windowStart, windowEnd, nil, 100000)
		if err != nil {
			t.Fatalf("FetchSlowQueriesInRangePage: %v", err)
		}
		got := make(map[string]bool)
		for _, row := range rows {
			switch row.QueryID {
			case multiLineID, distributedID, secondaryID:
				got[row.QueryID] = true
			}
		}
		return got
	}

	if got := admitted(t, config.FiltersConfig{}); !got[multiLineID] || !got[distributedID] || !got[secondaryID] {
		t.Fatalf("unfiltered reader admitted %v, want the multi-line, initial, and secondary rows", got)
	}

	t.Run("a pattern spanning a line break", func(t *testing.T) {
		filters := config.FiltersConfig{BlacklistQueries: []string{"AS first,.*" + marker + "-ml"}}
		f, err := filter.NewQueryFilter(filters)
		if err != nil {
			t.Fatal(err)
		}
		if !f.IsBlacklistedQuery(seeds[multiLineID]) {
			t.Error("the Go filter admits the multi-line statement")
		}
		if admitted(t, filters)[multiLineID] {
			t.Error("ClickHouse admitted the multi-line statement")
		}
	})

	t.Run("a secondary query follows its initial query", func(t *testing.T) {
		filters := config.FiltersConfig{BlacklistQueries: []string{marker + `-rm' AS tag FROM remote\(`}}
		f, err := filter.NewQueryFilter(filters)
		if err != nil {
			t.Fatal(err)
		}
		if f.IsBlacklistedQuery(secondaryText) {
			t.Fatalf("the pattern matches the secondary's own text %q, so this case proves nothing", secondaryText)
		}
		got := admitted(t, filters)
		if got[distributedID] || got[secondaryID] {
			t.Errorf("admitted %v, want neither the initial nor the secondary row", got)
		}
		if !got[multiLineID] {
			t.Error("an unrelated query was dropped")
		}
	})

	// The secondary row's own address is this server (127.0.0.1); its
	// initial_address is the test's client, which whitelist_ips must judge.
	t.Run("whitelist_ips judges a secondary query by its client", func(t *testing.T) {
		reader := newCHReader(t, 1)
		logs, err := reader.FetchQueryLogByQueryIDs(ctx, []string{secondaryID}, 1, 0)
		_ = reader.Close()
		if err != nil || logs[secondaryID].QueryID == "" {
			t.Fatalf("FetchQueryLogByQueryIDs: %v (%d rows)", err, len(logs))
		}
		row := logs[secondaryID]
		origin := processor.OriginatingAddress(row)
		if strings.HasSuffix(origin, "127.0.0.1") {
			t.Skipf("the client is loopback (%q), so own and initial addresses cannot be told apart", origin)
		}
		t.Logf("secondary row: address %q, initial_address %q", row.ClientAddress, row.InitialAddress)
		if got := admitted(t, config.FiltersConfig{WhitelistIPs: []string{origin}}); !got[secondaryID] {
			t.Errorf("whitelisting the client %q dropped its secondary query", origin)
		}
		if got := admitted(t, config.FiltersConfig{WhitelistIPs: []string{"127.0.0.1"}}); got[secondaryID] {
			t.Error("whitelisting the initiating server admitted a secondary query from another client")
		}
	})
}
