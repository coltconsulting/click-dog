package processor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/model"
)

// fakeBackfillLog serves query_log pages the way the paged range query does:
// rows in (event_time, query_id) order, after the cursor, at most limit.
type fakeBackfillLog struct {
	rows    []model.QueryLog
	failOn  int // fail the read of this page number (1-based); 0 never fails
	reads   int
	limits  []int
	cursors []*clickhouse.QueryLogCursor
}

func (f *fakeBackfillLog) FetchSlowQueriesInRangePage(_ context.Context, _, _, _ int, _, _ time.Time, cursor *clickhouse.QueryLogCursor, limit int) ([]model.QueryLog, error) {
	f.reads++
	f.limits = append(f.limits, limit)
	f.cursors = append(f.cursors, cursor)
	if f.reads == f.failOn {
		return nil, errors.New("query_log read timed out")
	}
	rows := append([]model.QueryLog(nil), f.rows...)
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].EventTime.Equal(rows[j].EventTime) {
			return rows[i].EventTime.Before(rows[j].EventTime)
		}
		return rows[i].QueryID < rows[j].QueryID
	})
	var out []model.QueryLog
	for _, r := range rows {
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

// backfillRows returns n query_log rows, three to a second, so pages split
// rows that share an event time.
func backfillRows(n int) []model.QueryLog {
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	rows := make([]model.QueryLog, n)
	for i := range rows {
		rows[i] = model.QueryLog{
			QueryID:   fmt.Sprintf("q%02d", i),
			Query:     "SELECT 1",
			EventTime: base.Add(time.Duration(i/3) * time.Second),
		}
	}
	return rows
}

func runBackfillRange(t *testing.T, log *fakeBackfillLog, cfg *config.Config, exp *mockExporter) (BatchResult, error) {
	t.Helper()
	qf, err := filter.NewQueryFilter(cfg.Filters)
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}
	start := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	return ExportBackfillRange(context.Background(), log, exp, qf, cfg, start, start.Add(time.Hour), nil)
}

// TestExportBackfillRange_PagesThroughTheWholeRange is the release-test case:
// 12 qualifying rows with max_spans_per_cycle 5 exported 5 and reported
// success. Every row in the range must be exported, in pages of at most 5.
func TestExportBackfillRange_PagesThroughTheWholeRange(t *testing.T) {
	log := &fakeBackfillLog{rows: backfillRows(12)}
	exp := &mockExporter{}
	cfg := &config.Config{Monitor: config.MonitorConfig{MaxSpansPerCycle: 5}}

	result, err := runBackfillRange(t, log, cfg, exp)
	if err != nil {
		t.Fatalf("ExportBackfillRange: %v", err)
	}
	if result.Exported != 12 || result.Failed != 0 || result.Filtered != 0 {
		t.Fatalf("result = %+v, want all 12 rows exported", result)
	}
	if exp.exportQueryCalls != 12 {
		t.Errorf("export calls = %d, want 12", exp.exportQueryCalls)
	}
	if log.reads != 3 {
		t.Errorf("page reads = %d, want 3 (5, 5, then a short page of 2)", log.reads)
	}
	for i, limit := range log.limits {
		if limit != 6 {
			t.Errorf("page %d limit = %d, want max_spans_per_cycle plus the look-ahead row (6)", i+1, limit)
		}
	}
	if log.cursors[0] != nil {
		t.Errorf("first page cursor = %+v, want none", log.cursors[0])
	}
	// The first page ends on q04, part-way through the second that holds
	// q03-q05; the next page resumes after it rather than after the second.
	if c := log.cursors[1]; c == nil || c.QueryID != "q04" || !c.EventTime.Equal(log.rows[4].EventTime) {
		t.Errorf("second page cursor = %+v, want after q04", c)
	}
	if err := ReportBackfillOutcome("start", "end", result, nil); err != nil {
		t.Errorf("ReportBackfillOutcome = %v, want success", err)
	}
}

// TestExportBackfillRange_ExactMultipleNeedsNoEmptyRead pins the end of a
// range whose size is a multiple of the page size: the look-ahead row shows
// the last full page ends the range.
func TestExportBackfillRange_ExactMultipleNeedsNoEmptyRead(t *testing.T) {
	log := &fakeBackfillLog{rows: backfillRows(10)}
	exp := &mockExporter{}
	cfg := &config.Config{Monitor: config.MonitorConfig{MaxSpansPerCycle: 5}}

	result, err := runBackfillRange(t, log, cfg, exp)
	if err != nil {
		t.Fatalf("ExportBackfillRange: %v", err)
	}
	if result.Exported != 10 || log.reads != 2 {
		t.Fatalf("exported = %d over %d reads, want 10 over 2", result.Exported, log.reads)
	}
}

// TestExportBackfillRange_PageNeverSplitsARepeatedQueryID pins rows the cursor
// cannot tell apart: a client that reuses one query ID can finish two queries
// in the same second. If a page ended between them, the next page's cursor
// would skip the second, so the page widens until the row after it differs.
func TestExportBackfillRange_PageNeverSplitsARepeatedQueryID(t *testing.T) {
	rows := backfillRows(8)
	// Rows 4 and 5 straddle the first page boundary with one key.
	rows[5].EventTime, rows[5].QueryID = rows[4].EventTime, rows[4].QueryID
	log := &fakeBackfillLog{rows: rows}
	var exported []string
	exp := &mockExporter{exportQueryFunc: func(_ context.Context, q model.QueryLog) error {
		exported = append(exported, q.QueryID)
		return nil
	}}
	cfg := &config.Config{Monitor: config.MonitorConfig{MaxSpansPerCycle: 5}}

	result, err := runBackfillRange(t, log, cfg, exp)
	if err != nil {
		t.Fatalf("ExportBackfillRange: %v", err)
	}
	if result.Exported != 8 {
		t.Fatalf("exported %d of 8 rows: %v", result.Exported, exported)
	}
	if got := strings.Count(strings.Join(exported, ","), "q04"); got != 2 {
		t.Errorf("rows exported with the repeated key = %d, want both: %v", got, exported)
	}
	if want := []int{6, 11}; len(log.limits) < 2 || log.limits[0] != want[0] || log.limits[1] != want[1] {
		t.Errorf("read limits = %v, want the first page widened from %v", log.limits, want)
	}
}

// TestExportBackfillRange_NoPageSizeReadsOnce pins that without a page size
// the single read returns every row and no further page is read.
func TestExportBackfillRange_NoPageSizeReadsOnce(t *testing.T) {
	log := &fakeBackfillLog{rows: backfillRows(7)}
	exp := &mockExporter{}

	result, err := runBackfillRange(t, log, &config.Config{}, exp)
	if err != nil {
		t.Fatalf("ExportBackfillRange: %v", err)
	}
	if result.Exported != 7 || log.reads != 1 {
		t.Fatalf("exported = %d over %d reads, want 7 over 1", result.Exported, log.reads)
	}
}

// TestExportBackfillRange_ReadFailureStopsWithCounts pins that a page read
// that fails part-way through the range fails the run instead of reporting a
// short range as complete. The first page is already exported, so the run is
// reported like any failed one: backfill_failed with the counts so far, and
// an error that exits non-zero. The webhook text leaves out the driver error.
func TestExportBackfillRange_ReadFailureStopsWithCounts(t *testing.T) {
	log := &fakeBackfillLog{rows: backfillRows(12), failOn: 2}
	exp := &mockExporter{}
	cfg := &config.Config{Monitor: config.MonitorConfig{MaxSpansPerCycle: 5}}

	result, err := runBackfillRange(t, log, cfg, exp)
	if err == nil {
		t.Fatal("ExportBackfillRange succeeded despite a failed page read")
	}
	if result.Exported != 5 {
		t.Errorf("exported = %d, want the first page's 5", result.Exported)
	}
	for _, want := range []string{"page 2", "query_log read timed out"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}

	rec, wh := newWebhookRecorder(t)
	reported := ReportBackfillStopped("start", "end", result, err, wh)
	const want = "Backfill failed: stopped before the end of the range: start to end (queries=5 exported=5 filtered=0 failed=0)"
	if reported == nil || !strings.Contains(reported.Error(), want) || !strings.Contains(reported.Error(), "query_log read timed out") {
		t.Errorf("ReportBackfillStopped = %v, want %q and the cause", reported, want)
	}
	if texts := rec.wait(t, 1); texts[0] != "[click-dog] "+want {
		t.Errorf("backfill_failed webhook = %q, want %q", texts[0], "[click-dog] "+want)
	}
}

// TestExportBackfillRange_CanceledRunIsNotComplete pins that a run canceled
// between pages stops with an error rather than reporting the range as done.
func TestExportBackfillRange_CanceledRunIsNotComplete(t *testing.T) {
	log := &fakeBackfillLog{rows: backfillRows(12)}
	ctx, cancel := context.WithCancel(context.Background())
	exp := &mockExporter{exportQueryFunc: func(_ context.Context, q model.QueryLog) error {
		if q.QueryID == "q04" {
			cancel() // the last query of the first page
		}
		return nil
	}}
	cfg := &config.Config{Monitor: config.MonitorConfig{MaxSpansPerCycle: 5}}
	qf, _ := filter.NewQueryFilter(cfg.Filters)
	start := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

	result, err := ExportBackfillRange(ctx, log, exp, qf, cfg, start, start.Add(time.Hour), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ExportBackfillRange = %v, want a cancellation error", err)
	}
	if result.Exported != 5 || log.reads != 1 {
		t.Errorf("exported %d over %d reads, want the first page only", result.Exported, log.reads)
	}
}

// TestExportBackfillRange_ExportFailuresAcrossPagesAreReported pins that the
// per-page results add up, so a failure in any page fails the run.
func TestExportBackfillRange_ExportFailuresAcrossPagesAreReported(t *testing.T) {
	log := &fakeBackfillLog{rows: backfillRows(12)}
	exportErr := errors.New("collector unavailable")
	exp := &mockExporter{exportQueryFunc: func(_ context.Context, q model.QueryLog) error {
		if q.QueryID == "q11" {
			return exportErr
		}
		return nil
	}}
	cfg := &config.Config{Monitor: config.MonitorConfig{MaxSpansPerCycle: 5}}

	result, err := runBackfillRange(t, log, cfg, exp)
	if err != nil {
		t.Fatalf("ExportBackfillRange: %v", err)
	}
	if result.Exported != 11 || result.Failed != 1 || !errors.Is(result.FirstErr, exportErr) {
		t.Fatalf("result = %+v, want 11 exported and the last page's failure", result)
	}
	if err := ReportBackfillOutcome("start", "end", result, nil); err == nil || !strings.Contains(err.Error(), "queries=12 exported=11") {
		t.Errorf("ReportBackfillOutcome = %v, want a partial failure over all 12 queries", err)
	}
}
