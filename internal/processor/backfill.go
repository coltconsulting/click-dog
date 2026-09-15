package processor

import (
	"context"
	"fmt"
	"time"

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
			// Apply filters (query log doesn't have operation names)
			// The whitelist judges the originating client, so a distributed
			// query's secondary rows follow the same decision as its initial
			// row rather than the initiating server's address.
			if f.ShouldFilter("", query.Query, OriginatingAddress(query)) {
				result.Filtered++
				continue
			}

			if f.HasUserFilter() && f.ShouldFilterUser(query.User) {
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
	totalQueries := result.Exported + result.Filtered + result.Failed
	summary := fmt.Sprintf("%s to %s (queries=%d exported=%d filtered=%d failed=%d)",
		startStr, endStr, totalQueries, result.Exported, result.Filtered, result.Failed)

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
