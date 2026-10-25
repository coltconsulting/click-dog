//go:build integration

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/processor"
)

// Real SQL is essential here: mocked scan rows cannot detect ClickHouse's
// global alias substitution of query_kind with the unrelated lifecycle type.
func TestIntegration_QueryOperationSurvivesBothReadersAndExport(t *testing.T) {
	ctx := context.Background()
	conn := waitForClickHouse(t, 1, 30*time.Second)
	t.Cleanup(func() { _ = conn.Close() })
	waitForOTELCollector(t, 30*time.Second)
	table := "default.operation_" + newIntegrationFixtureMarker()
	if err := conn.Exec(ctx, "CREATE TABLE "+table+" (value UInt64) ENGINE=Memory"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Exec(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
			t.Errorf("remove fixture table: %v", err)
		}
	})
	reader := newCHReader(t, 1)
	defer func() { _ = reader.Close() }()

	for _, test := range []struct{ operation, access, sql string }{
		{"Select", "read", "SELECT sleep(0.01)"},
		{"Insert", "write", "INSERT INTO " + table + " SELECT number FROM numbers(3)"},
	} {
		t.Run(test.operation, func(t *testing.T) {
			identity, err := generateTracingIdentity()
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now().Add(-clockSkewMargin)
			queryCtx := clickhouse.Context(ctx, clickhouse.WithQueryID(identity.QueryID), clickhouse.WithSpan(identity.SpanContext))
			if err := conn.Exec(queryCtx, test.sql); err != nil {
				t.Fatal(err)
			}
			if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
				t.Fatal(err)
			}
			waitForRowCount(t, conn, "operation fixture", fmt.Sprintf("SELECT count() FROM system.query_log WHERE query_id = '%s' AND type = 'QueryFinish'", identity.QueryID), 1, 5*time.Second)
			var sourceHash uint64
			if err := conn.QueryRow(ctx, "SELECT normalized_query_hash FROM system.query_log WHERE query_id = ? AND type = 'QueryFinish'", identity.QueryID).Scan(&sourceHash); err != nil {
				t.Fatal(err)
			}
			historical, err := reader.FetchSlowQueriesInRange(ctx, 0, 0, 100000, start, time.Now().Add(clockSkewMargin), 10000)
			if err != nil {
				t.Fatal(err)
			}
			live, err := reader.FetchQueryLogByQueryIDs(ctx, []string{identity.QueryID}, 1)
			if err != nil {
				t.Fatal(err)
			}
			rows := map[string]model.QueryLog{"live": live[identity.QueryID]}
			for _, row := range historical {
				if row.QueryID == identity.QueryID {
					rows["historical"] = row
				}
			}
			spans, err := reader.FetchSpansForTraceIDs(ctx, []string{identity.TraceID.String()}, 1, 1000, nil)
			if err != nil || len(spans) == 0 {
				t.Fatalf("native spans: count=%d error=%v", len(spans), err)
			}
			for _, path := range []string{"historical", "live"} {
				t.Run(path, func(t *testing.T) {
					row := rows[path]
					if row.QueryID != identity.QueryID || row.QueryKind != "QueryFinish" || row.QueryOperation != test.operation {
						t.Errorf("query identity/lifecycle/operation = %q/%q/%q, want %q/QueryFinish/%s", row.QueryID, row.QueryKind, row.QueryOperation, identity.QueryID, test.operation)
					}
					if sourceHash == 0 || row.NormalizedQueryHash != sourceHash {
						t.Errorf("normalized hash = %d, want source %d", row.NormalizedQueryHash, sourceHash)
					}
					span := spans[0]
					processor.EnrichSpanFromQueryLog(&span, row, 100000)
					service := "operation-" + path + "-" + identity.QueryID
					exporter := newOTELExporter(t, service)
					defer func() { _ = exporter.Close(ctx) }()
					if _, err := exporter.ExportSpans(ctx, []model.OpenTelemetrySpan{span}); err != nil {
						t.Fatal(err)
					}
					observations := waitForOTELSpanKeys(t, service, []model.OpenTelemetrySpan{span}, 15*time.Second)
					for _, got := range observations {
						if got.ServiceName != service || got.TraceID != span.TraceID || got.SpanID != span.SpanID {
							continue
						}
						for key, want := range map[string]string{
							"query_log.query_id":    identity.QueryID,
							"query_log.operation":   strings.ToLower(test.operation),
							"query_log.access_type": test.access,
						} {
							if got.Attributes[key] != want {
								t.Errorf("receiver %s = %v, want %s", key, got.Attributes[key], want)
							}
						}
					}
				})
			}
		})
	}
}
