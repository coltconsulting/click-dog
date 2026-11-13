//go:build integration

package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/processor"
)

// TestIntegration_RunningQuerySpansWaitForTheirQuery reproduces the leak the
// span attribution rule closes, on a real server. An application sends two
// queries under one traceparent: a short allowed one, then a long one that
// blacklist_queries matches. While the long one runs, ClickHouse has already
// written some of its spans but not its query span, so the only query the span
// log holds for the trace is the allowed one. The trace filters must withhold
// the running query's spans, and drop the trace once its query span lands.
func TestIntegration_RunningQuerySpansWaitForTheirQuery(t *testing.T) {
	conn := waitForClickHouse(t, 1, 60*time.Second)
	defer func() { _ = conn.Close() }()
	reader := newCHReader(t, 1)
	defer func() { _ = reader.Close() }()

	marker := newIntegrationFixtureMarker()
	traceID := uuid.New()
	traced := func(queryID string) context.Context {
		var spanID oteltrace.SpanID
		binary.BigEndian.PutUint64(spanID[:], newIntegrationSpanID())
		return clickhouse.Context(context.Background(),
			clickhouse.WithQueryID(queryID),
			clickhouse.WithSpan(oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
				TraceID: oteltrace.TraceID(traceID), SpanID: spanID, TraceFlags: oteltrace.FlagsSampled,
			})))
	}
	since := time.Now().Add(-clockSkewMargin)
	allowedID := integrationFixtureQueryID(marker, 0)
	runningID := integrationFixtureQueryID(marker, 1)

	if err := conn.Exec(traced(allowedID), "SELECT 1"); err != nil {
		t.Fatalf("allowed query: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		query := fmt.Sprintf("SELECT sleepEachRow(1), 'secret-report-%s' FROM numbers(10) SETTINGS max_block_size = 1, max_execution_time = 60", marker)
		if err := conn.Exec(traced(runningID), query); err != nil {
			t.Errorf("running query: %v", err)
		}
	}()

	qf, err := filter.NewQueryFilter(config.FiltersConfig{BlacklistQueries: []string{"secret-report"}})
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}
	decide := func() ([]model.OpenTelemetrySpan, processor.TraceDecision) {
		if err := conn.Exec(context.Background(), "SYSTEM FLUSH LOGS"); err != nil {
			t.Fatalf("flush logs: %v", err)
		}
		spans, err := reader.FetchSpansForTraceIDs(context.Background(), []string{traceID.String()}, 1, since, 1000, nil)
		if err != nil {
			t.Fatalf("FetchSpansForTraceIDs: %v", err)
		}
		decision, err := processor.FilteredTraces(context.Background(), reader, spans, qf, 1)
		if err != nil {
			t.Fatalf("FilteredTraces: %v", err)
		}
		return spans, decision
	}

	// Mid-flight: the allowed query's spans pass; every other span belongs
	// to the running query, which has no query span yet.
	time.Sleep(4 * time.Second)
	spans, decision := decide()
	allowedTree := map[uint64]bool{}
	for _, s := range spans {
		if s.OperationName == "query" && s.Attributes["clickhouse.query_id"] == allowedID {
			allowedTree[s.SpanID], allowedTree[s.ParentSpanID] = true, true
		}
		if s.Attributes["clickhouse.query_id"] == runningID && s.OperationName == "query" {
			t.Fatalf("the running query's span was written before it finished; the test needs it running")
		}
	}
	if len(allowedTree) == 0 {
		t.Fatalf("no query span for the allowed query among %d spans", len(spans))
	}
	for changed := true; changed; {
		changed = false
		for _, s := range spans {
			if allowedTree[s.ParentSpanID] && !allowedTree[s.SpanID] {
				allowedTree[s.SpanID], changed = true, true
			}
		}
	}
	running := 0
	for i := range spans {
		s := &spans[i]
		inAllowed := allowedTree[s.SpanID]
		if !inAllowed {
			running++
		}
		if withheld := decision.Withholds(s); withheld == inAllowed {
			t.Errorf("span %d %s: withheld = %t, want %t", s.SpanID, s.OperationName, withheld, !inAllowed)
		}
	}
	if running == 0 {
		t.Fatalf("ClickHouse wrote none of the running query's spans while it ran; nothing to withhold among %d spans", len(spans))
	}

	// Once the running query's span lands, its statement drops the trace.
	wg.Wait()
	if _, decision = decide(); !decision.Dropped[traceID] {
		t.Errorf("trace kept after its blacklisted query finished: %+v", decision)
	}

	// Under a filter neither query matches, every span is attributed once both
	// queries finished, connection spans included.
	qf, err = filter.NewQueryFilter(config.FiltersConfig{BlacklistQueries: []string{"no-such-query-" + marker}})
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}
	spans, decision = decide()
	for i := range spans {
		if decision.Withholds(&spans[i]) {
			t.Errorf("finished allowed span %d %s withheld: %+v", spans[i].SpanID, spans[i].OperationName, decision)
		}
	}
}
