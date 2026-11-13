//go:build integration

package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/processor"
)

// versionLookupMarker is the comment the reader tags its version query with.
// The query_log checks below find the reader's lookups by it.
const versionLookupMarker = "/* click-dog server version */"

// adminServerVersion reads version() over an unrestricted admin connection
// rather than through the reader under test, so a reader that queried the
// wrong thing cannot supply its own expected value.
func adminServerVersion(t *testing.T, admin driver.Conn) string {
	t.Helper()
	var version string
	if err := admin.QueryRow(context.Background(), "SELECT version()").Scan(&version); err != nil {
		t.Fatalf("admin SELECT version(): %v", err)
	}
	if version == "" {
		t.Fatal("admin SELECT version() returned an empty string")
	}
	return version
}

// serverNowUs reads the server's clock. The query_log checks are bounded by it
// so they see only lookups made after it, whatever this host's clock says.
func serverNowUs(t *testing.T, admin driver.Conn) int64 {
	t.Helper()
	var us int64
	if err := admin.QueryRow(context.Background(), "SELECT toUnixTimestamp64Micro(now64(6))").Scan(&us); err != nil {
		t.Fatalf("reading server time: %v", err)
	}
	return us
}

// versionLookupsWhere selects a node's query_log rows for the reader's version
// lookups since sinceUs. The query_log exclusion keeps the checks' own text,
// which contains the marker, from matching itself.
func versionLookupsWhere(sinceUs int64) string {
	return fmt.Sprintf("is_initial_query"+
		" AND toUnixTimestamp64Micro(event_time_microseconds) >= %d"+
		" AND position(query, '%s') > 0"+
		" AND position(query, 'query_log') = 0", sinceUs, versionLookupMarker)
}

// TestIntegration_ServerVersionUnderReadonly2 pins the one claim the unit tests
// cannot make: that `SELECT version()` is actually permitted on a real server
// under the enforced readonly=2 connection mode, and returns that server's
// version.
//
// It asserts loudly on purpose. Production deliberately degrades a failed
// lookup to an absent attribute and a single debug line, so a lookup that never
// worked at all would be invisible in an ordinary integration run.
func TestIntegration_ServerVersionUnderReadonly2(t *testing.T) {
	admin := waitForClickHouse(t, 1, 30*time.Second)
	defer admin.Close()
	since := serverNowUs(t, admin)

	reader := newCHReader(t, 1)
	defer reader.Close()

	got, err := reader.ServerVersion(context.Background())
	if err != nil {
		t.Fatalf("ServerVersion under readonly=2: %v", err)
	}
	if want := adminServerVersion(t, admin); got != want {
		t.Fatalf("ServerVersion = %q, server reports %q", got, want)
	}

	// Succeeding is not enough: the lookup must have run under readonly=2.
	// query_log records the settings each query ran with.
	if err := admin.Exec(context.Background(), "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("SYSTEM FLUSH LOGS: %v", err)
	}
	waitForRowCount(t, admin, "version lookup finished under readonly=2",
		"SELECT count() FROM system.query_log WHERE type = 'QueryFinish'"+
			" AND Settings['readonly'] = '2' AND "+versionLookupsWhere(since),
		1, 10*time.Second)
	t.Logf("resolved ClickHouse server version under readonly=2: %q", got)
}

// The version lookup must not inherit the cluster fan-out. It describes the
// connected server, so a cluster-configured reader answers for the node it is
// connected to: the lookup runs there alone, with no remote part on any other
// replica.
func TestIntegration_ServerVersionWithClusterQueries(t *testing.T) {
	admins := waitForAllClickHouse(t, 30*time.Second)
	defer func() {
		for _, admin := range admins {
			admin.Close()
		}
	}()
	since := serverNowUs(t, admins[0])

	reader := newClusterCHReader(t, 1, "test_cluster")
	defer reader.Close()

	ctx := context.Background()
	got, err := reader.ServerVersion(ctx)
	if err != nil {
		t.Fatalf("ServerVersion with cluster queries enabled: %v", err)
	}
	if want := adminServerVersion(t, admins[0]); got != want {
		t.Fatalf("ServerVersion = %q, node 1 reports %q", got, want)
	}

	// A fan-out would leave rows on the other replicas whose initial_query_id
	// is the lookup's ID. query_log can lag SYSTEM FLUSH LOGS, so finding none
	// proves nothing alone: a barrier query that does fan out runs after the
	// lookup, and once its remote rows are visible on a replica, any remote row
	// the lookup left there is visible too.
	barrierID := "click-dog-int-version-barrier-" + newIntegrationFixtureMarker()
	var replicas uint64
	barrierCtx := clickhouse.Context(ctx, clickhouse.WithQueryID(barrierID))
	if err := admins[0].QueryRow(barrierCtx, "SELECT count() FROM clusterAllReplicas('test_cluster', system.one)").Scan(&replicas); err != nil {
		t.Fatalf("fan-out barrier: %v", err)
	}
	flushAllLogs(t, admins)

	where := versionLookupsWhere(since)
	waitForRowCount(t, admins[0], "cluster reader's version lookup on node 1",
		"SELECT count() FROM system.query_log WHERE "+where, 1, 10*time.Second)
	rows, err := admins[0].Query(ctx, "SELECT DISTINCT query_id FROM system.query_log WHERE "+where)
	if err != nil {
		t.Fatalf("reading version lookup query IDs: %v", err)
	}
	var lookupIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scanning version lookup query ID: %v", err)
		}
		lookupIDs = append(lookupIDs, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating version lookup query IDs: %v", err)
	}
	rows.Close()

	for i, replica := range admins[1:] {
		node := i + 2
		waitForRowCount(t, replica, fmt.Sprintf("fan-out barrier on node %d", node),
			fmt.Sprintf("SELECT count() FROM system.query_log WHERE NOT is_initial_query AND initial_query_id = '%s'", barrierID),
			1, 10*time.Second)
		var remote uint64
		if err := replica.QueryRow(ctx, fmt.Sprintf(
			"SELECT count() FROM system.query_log WHERE NOT is_initial_query AND initial_query_id IN (%s)",
			sqlStringList(lookupIDs))).Scan(&remote); err != nil {
			t.Fatalf("counting remote version lookups on node %d: %v", node, err)
		}
		if remote != 0 {
			t.Fatalf("version lookup fanned out: node %d logged %d remote rows for it", node, remote)
		}
	}
	t.Logf("resolved ClickHouse server version (cluster reader, node 1 only): %q", got)
}

// stampCapturingExporter records every version the pipeline stamps on it and,
// for each export, the version stamped when those spans went out.
type stampCapturingExporter struct {
	current       string
	stamped       []string
	exportedUnder []string
}

func (e *stampCapturingExporter) ExportSpans(_ context.Context, spans []model.OpenTelemetrySpan) (model.ExportResult, error) {
	if len(spans) > 0 {
		e.exportedUnder = append(e.exportedUnder, e.current)
	}
	return model.ExportResult{}, nil
}

func (e *stampCapturingExporter) ExportQuery(context.Context, model.QueryLog) (model.ExportResult, error) {
	return model.ExportResult{}, nil
}

func (e *stampCapturingExporter) Close(context.Context) error { return nil }

func (e *stampCapturingExporter) SetServerVersion(version string) {
	e.current = version
	e.stamped = append(e.stamped, version)
}

// TestIntegration_PipelineStampsRealServerVersion closes the gap every unit
// test leaves open: that a real Pipeline, driving a real ClickHouseReader
// against a real server, actually reaches the refresh and stamps the server's
// version before its first export. The unit tests prove the logic with fakes
// on both sides; only this proves the production wiring is connected.
func TestIntegration_PipelineStampsRealServerVersion(t *testing.T) {
	admin := waitForClickHouse(t, 1, 30*time.Second)
	defer admin.Close()
	// The cycle needs spans to export, since the claim is about the version
	// they go out with, so seed a traced query and let it settle into the live
	// walk. Seeding also creates the span log, which ClickHouse creates lazily
	// on the first traced query; without it a healthy cycle fails its fetch.
	seedTracedQueryFixture(t, admin, 1)
	waitForLiveSettle()
	want := adminServerVersion(t, admin)

	reader := newCHReader(t, 1)
	defer reader.Close()

	qf, err := filter.NewQueryFilter(config.FiltersConfig{})
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}
	exporter := &stampCapturingExporter{}
	pipeline, err := processor.NewPipeline(processor.Pipeline{
		Reader:   reader,
		Exporter: exporter,
		Filter:   qf,
		Config: &config.Config{Monitor: config.MonitorConfig{
			MinTraceDurationMs: 1,
			LookbackS:          600,
			MaxSpansPerCycle:   100,
			ExportTimeoutS:     30,
		}},
		SeenSpans: mustLRU(t, 1024),
		Metrics:   metrics.NewMetrics(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// Stamping happens before the fetch, so assert it independently: a fetch
	// failure must not mask whether the wiring ran.
	processErr := pipeline.Process(ctx)

	if len(exporter.stamped) == 0 {
		t.Fatal("pipeline never stamped a server version on the exporter")
	}
	if got := exporter.stamped[len(exporter.stamped)-1]; got != want {
		t.Fatalf("pipeline stamped %q, server reports %q", got, want)
	}
	if processErr != nil {
		t.Fatalf("Process: %v", processErr)
	}

	// The stamp must land before the cycle's first export, or those spans go
	// out without the version.
	if len(exporter.exportedUnder) == 0 {
		t.Fatal("cycle exported no spans, so nothing shows the stamp preceded an export")
	}
	if first := exporter.exportedUnder[0]; first != want {
		t.Fatalf("first export went out stamped %q, server reports %q", first, want)
	}
	t.Logf("pipeline stamped ClickHouse server version %q before its first export", want)
}
