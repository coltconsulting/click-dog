package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/model"
)

// pagedQueryLog answers paged range reads from memory: rows in (event_time,
// query_id) order after the cursor, at most limit. failOn fails that read
// (1-based); 0 never fails.
type pagedQueryLog struct {
	rows   []model.QueryLog
	failOn int
	reads  int
}

func (p *pagedQueryLog) FetchSlowQueriesInRangePage(_ context.Context, _, _, _ int, _, _ time.Time, cursor *clickhouse.QueryLogCursor, limit int) ([]model.QueryLog, error) {
	p.reads++
	if p.reads == p.failOn {
		return nil, errors.New("query_log read timed out")
	}
	var out []model.QueryLog
	for _, r := range p.rows {
		if cursor != nil && (r.EventTime.Before(cursor.EventTime) ||
			(r.EventTime.Equal(cursor.EventTime) && r.QueryID <= cursor.QueryID)) {
			continue
		}
		out = append(out, r)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

// newPagedQueryLog returns n rows, one a second, already in read order.
func newPagedQueryLog(n int) *pagedQueryLog {
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	p := &pagedQueryLog{}
	for i := 0; i < n; i++ {
		p.rows = append(p.rows, model.QueryLog{
			QueryID:   fmt.Sprintf("q%02d", i),
			Query:     "SELECT 1",
			EventTime: base.Add(time.Duration(i) * time.Second),
		})
	}
	return p
}

func runBackfillWiring(t *testing.T, log *pagedQueryLog, exp *DryRunExporter) error {
	t.Helper()
	cfg := &config.Config{Monitor: config.MonitorConfig{MaxSpansPerCycle: 5}}
	f, err := filter.NewQueryFilter(cfg.Filters)
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}
	return runBackfillMode(context.Background(), log, exp, f, cfg,
		"2026-11-01T10:00:00Z", "2026-11-01T11:00:00Z", nil, nil)
}

// TestRunBackfillMode_ExportsTheWholeRange pins the command's wiring to the
// paged read: release testing saw 12 qualifying queries with
// max_spans_per_cycle 5 export 5 and exit 0. A single capped read here fails
// this test.
func TestRunBackfillMode_ExportsTheWholeRange(t *testing.T) {
	log := newPagedQueryLog(12)
	exp := NewDryRunExporter()

	if err := runBackfillWiring(t, log, exp); err != nil {
		t.Fatalf("runBackfillMode: %v", err)
	}
	if exp.queryCount != 12 || len(exp.traceIDs) != 12 {
		t.Errorf("exported %d queries (%d distinct), want all 12", exp.queryCount, len(exp.traceIDs))
	}
	if log.reads < 3 {
		t.Errorf("page reads = %d, want the range read in pages of 5", log.reads)
	}
}

// TestRunBackfillMode_ReadFailureFailsTheRun pins that a page read that fails
// after a page was exported exits non-zero and reports the run as failed with
// the counts so far.
func TestRunBackfillMode_ReadFailureFailsTheRun(t *testing.T) {
	log := newPagedQueryLog(12)
	log.failOn = 2
	exp := NewDryRunExporter()

	err := runBackfillWiring(t, log, exp)
	if err == nil {
		t.Fatal("runBackfillMode succeeded despite a failed page read")
	}
	if !strings.Contains(err.Error(), "stopped before the end of the range") || !strings.Contains(err.Error(), "queries=5 exported=5") {
		t.Errorf("error = %q, want a stopped run with the first page's counts", err)
	}
}
