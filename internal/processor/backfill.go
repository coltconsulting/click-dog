package processor

import (
	"context"
	"fmt"
	"time"

	"github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/webhook"
)

// BatchResult summarises a query export batch. Exported+Filtered+Failed
// always equals len(input). FirstErr is set iff Failed > 0.
type BatchResult struct {
	Exported int
	Filtered int
	Failed   int
	FirstErr error
}

// HasFailures reports whether any export attempt failed.
func (r BatchResult) HasFailures() bool { return r.Failed > 0 }

// AllFailed reports whether every export *attempt* failed (filtered queries
// don't count — they were never attempted).
func (r BatchResult) AllFailed() bool { return r.Failed > 0 && r.Exported == 0 }

// ProcessQueriesBatch runs queries through filter+export. Per-query export
// errors are counted and the batch continues — backfill is best-effort and
// one bad export shouldn't lose the rest of the historical range.
func ProcessQueriesBatch(
	ctx context.Context,
	queries []model.QueryLog,
	exporter model.SpanExporter,
	f *filter.QueryFilter,
	cfg *config.Config,
	m *metrics.Metrics,
) BatchResult {
	var result BatchResult

	// Determine batch size (0 = process all at once)
	batchSize := cfg.Monitor.BatchSize
	if batchSize <= 0 {
		batchSize = len(queries)
	}

	// Process queries in batches
	for batchStart := 0; batchStart < len(queries); batchStart += batchSize {
		batchEnd := batchStart + batchSize
		if batchEnd > len(queries) {
			batchEnd = len(queries)
		}

		batch := queries[batchStart:batchEnd]
		clicklog.Debug("Processing batch %d-%d of %d queries", batchStart+1, batchEnd, len(queries))

		for _, query := range batch {
			// The IP whitelist judges the originating client and the user
			// lists the originating and interserver accounts, so a
			// distributed query's secondary rows follow their client rather
			// than the initiating server. The operation filters describe
			// native span-log operations and do not apply to query_log rows.
			if f.ShouldFilterQueryLog(query, OriginatingAddress(query)) {
				result.Filtered++
				continue
			}

			// The final export boundary applies query_text_mode to this local copy
			// after filtering has inspected the raw query.
			exportResult, err := ExportQueryWithDeadline(ctx, cfg, exporter, f, query)
			RecordExportObservability(m, exportResult)
			if err != nil {
				clicklog.Error("Error exporting query %s: %v", query.QueryID, err)
				result.Failed++
				if result.FirstErr == nil {
					result.FirstErr = err
				}
				continue
			}

			result.Exported++
		}

		// Add delay between batches if configured and there are more batches.
		// Respect ctx so Ctrl-C doesn't have to wait out the full delay × remaining batches.
		if cfg.Monitor.BatchDelayMs > 0 && batchEnd < len(queries) {
			select {
			case <-ctx.Done():
				if result.FirstErr == nil {
					result.FirstErr = ctx.Err()
				}
				return result
			case <-time.After(time.Duration(cfg.Monitor.BatchDelayMs) * time.Millisecond):
			}
		}
	}

	if result.Exported > 0 || result.Filtered > 0 || result.Failed > 0 {
		clicklog.Info("Exported: %d, Filtered: %d, Failed: %d", result.Exported, result.Filtered, result.Failed)
	} else {
		clicklog.Debug("Exported: 0, Filtered: 0, Failed: 0")
	}
	return result
}

// BackfillReader reads one page of a backfill range from query_log.
// *clickhouse.ClickHouseReader is the production implementation.
type BackfillReader interface {
	FetchSlowQueriesInRangePage(ctx context.Context, minDurationMs int, maxDurationMs int, maxQueryLength int, startTime, endTime time.Time, cursor *clickhouse.QueryLogCursor, limit int) ([]model.QueryLog, error)
}

// ExportBackfillRange exports every qualifying query in [start, end]. It reads
// query_log in time order, in pages of at most monitor.max_spans_per_cycle
// rows, and exports each page before it reads the next, so memory holds one
// page however large the range is. batch_delay_ms also separates pages. A read
// that fails, or a canceled ctx, stops the run with an error and the counts so
// far, which ReportBackfillStopped reports; the queries already exported stay
// exported.
func ExportBackfillRange(
	ctx context.Context,
	reader BackfillReader,
	exporter model.SpanExporter,
	f *filter.QueryFilter,
	cfg *config.Config,
	start, end time.Time,
	m *metrics.Metrics,
) (BatchResult, error) {
	var total BatchResult
	var cursor *clickhouse.QueryLogCursor
	for page := 1; ; page++ {
		queries, more, err := readBackfillPage(ctx, reader, cfg, start, end, cursor)
		if err != nil {
			return total, fmt.Errorf("reading query_log page %d: %w", page, err)
		}
		if len(queries) > 0 {
			clicklog.Info("Backfill page %d: %d queries through %s",
				page, len(queries), queries[len(queries)-1].EventTime.UTC().Format(time.RFC3339))
			total = total.add(ProcessQueriesBatch(ctx, queries, exporter, f, cfg, m))
		}
		// A canceled run stops part-way through a page, so it is not complete.
		if err := ctx.Err(); err != nil {
			return total, fmt.Errorf("canceled during page %d: %w", page, err)
		}
		if !more {
			return total, nil
		}
		cursor = clickhouse.CursorAfter(queries[len(queries)-1])
		if err := sleepCtx(ctx, time.Duration(cfg.Monitor.BatchDelayMs)*time.Millisecond); err != nil {
			return total, fmt.Errorf("canceled after page %d: %w", page, err)
		}
	}
}

// readBackfillPage reads the page after cursor: max_spans_per_cycle rows, and
// more reports whether rows follow it. It reads one row past the page to tell.
// The cursor cannot tell apart rows that share an event time and a query ID,
// as two quick queries from a client that reuses one query ID can, so a page
// never ends inside such a group: the read widens by another page until the
// row after the page differs. Without a page size the one read returns every
// row.
func readBackfillPage(ctx context.Context, reader BackfillReader, cfg *config.Config, start, end time.Time, cursor *clickhouse.QueryLogCursor) ([]model.QueryLog, bool, error) {
	pageSize := cfg.Monitor.MaxSpansPerCycle
	fetch := func(limit int) ([]model.QueryLog, error) {
		return reader.FetchSlowQueriesInRangePage(ctx,
			cfg.Monitor.MinTraceDurationMs,
			cfg.Monitor.MaxTraceDurationMs,
			cfg.Monitor.MaxQueryLength,
			start, end, cursor, limit)
	}
	if pageSize <= 0 {
		rows, err := fetch(0)
		return rows, false, err
	}
	for limit := pageSize; ; limit += pageSize {
		rows, err := fetch(limit + 1)
		if err != nil {
			return nil, false, err
		}
		if len(rows) <= limit {
			return rows, false, nil
		}
		last, next := rows[limit-1], rows[limit]
		if !last.EventTime.Equal(next.EventTime) || last.QueryID != next.QueryID {
			return rows[:limit], true, nil
		}
	}
}

// add returns the sum of r and o, keeping the first error seen.
func (r BatchResult) add(o BatchResult) BatchResult {
	r.Exported += o.Exported
	r.Filtered += o.Filtered
	r.Failed += o.Failed
	if r.FirstErr == nil {
		r.FirstErr = o.FirstErr
	}
	return r
}

// sleepCtx waits d, returning early with ctx's error when it is canceled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// ReportBackfillOutcome routes a BatchResult to log+webhook+error. Both
// branches emit a structured summary line (Info on success, Error on
// failure) so operators get a consistent log shape independent of how the
// caller surfaces the returned error.
//
// queries=N in the summary is computed from the result counts — by
// construction in ProcessQueriesBatch every input is exactly one of
// exported/filtered/failed, so passing len(queries) separately would only
// risk drift if that invariant ever broke.
func ReportBackfillOutcome(startStr, endStr string, result BatchResult, wh *webhook.WebhookNotifier) error {
	summary := backfillSummary(startStr, endStr, result)

	switch {
	case result.AllFailed():
		msg := "Backfill failed: all export attempts failed: " + summary
		clicklog.Error("%s", msg)
		wh.NotifySync(context.Background(), webhook.EventBackfillFailed, msg)
		return fmt.Errorf("%s: %w", msg, result.FirstErr)
	case result.HasFailures():
		msg := "Backfill partial failure: " + summary
		clicklog.Error("%s", msg)
		wh.NotifySync(context.Background(), webhook.EventBackfillFailed, msg)
		return fmt.Errorf("%s: %w", msg, result.FirstErr)
	default:
		clicklog.Info("Backfill complete: %s", summary)
		wh.NotifySync(context.Background(), webhook.EventBackfillComplete, "Backfill complete: "+summary)
		return nil
	}
}

// ReportBackfillStopped reports a run that stopped before the end of its range
// because a query_log read failed or the run was canceled. Pages read before
// the stop may already be exported, so like any failed run it logs and sends
// backfill_failed with the counts so far, and returns an error that exits
// non-zero. The webhook text leaves out err, which can carry driver detail.
func ReportBackfillStopped(startStr, endStr string, result BatchResult, err error, wh *webhook.WebhookNotifier) error {
	msg := "Backfill failed: stopped before the end of the range: " + backfillSummary(startStr, endStr, result)
	clicklog.Error("%s: %v", msg, err)
	wh.NotifySync(context.Background(), webhook.EventBackfillFailed, msg)
	return fmt.Errorf("%s: %w", msg, err)
}

// backfillSummary renders a run's range and counts for its final log line and
// webhook.
func backfillSummary(startStr, endStr string, result BatchResult) string {
	totalQueries := result.Exported + result.Filtered + result.Failed
	return fmt.Sprintf("%s to %s (queries=%d exported=%d filtered=%d failed=%d)",
		startStr, endStr, totalQueries, result.Exported, result.Filtered, result.Failed)
}
