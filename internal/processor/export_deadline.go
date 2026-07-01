package processor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
)

func exportDeadline(cfg *config.Config) (time.Duration, bool) {
	// cfg is intentionally required; production callers pass the resolved
	// LoadConfig result, and tests that construct Config directly should make
	// the desired timeout policy explicit.
	// LoadConfig defaults omitted export_timeout_s to DefaultExportTimeoutS;
	// a zero value here means the user explicitly disabled the deadline or a
	// test constructed Config directly.
	if cfg.Monitor.ExportTimeoutS == 0 {
		return 0, false
	}
	return time.Duration(cfg.Monitor.ExportTimeoutS) * time.Second, true
}

func wrapExportDeadlineError(timeout time.Duration, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("export deadline (%v) exceeded: %w", timeout, err)
	}
	return err
}

func ExportSpansWithDeadline(ctx context.Context, cfg *config.Config, exporter model.SpanExporter, spans []model.OpenTelemetrySpan) (model.ExportResult, error) {
	timeout, ok := exportDeadline(cfg)
	if !ok {
		return exporter.ExportSpans(ctx, spans)
	}
	exportCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := exporter.ExportSpans(exportCtx, spans)
	if err != nil {
		// Treat any exporter error as a full-batch failure. The next cycle's
		// lookback overlap will retry the batch, and only successful exports
		// are added to the dedup cache by the caller.
		return result, wrapExportDeadlineError(timeout, err)
	}
	return result, nil
}

func ExportQueryWithDeadline(ctx context.Context, cfg *config.Config, exporter model.SpanExporter, query model.QueryLog) (model.ExportResult, error) {
	timeout, ok := exportDeadline(cfg)
	if !ok {
		return exporter.ExportQuery(ctx, query)
	}
	exportCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := exporter.ExportQuery(exportCtx, query)
	if err != nil {
		return result, wrapExportDeadlineError(timeout, err)
	}
	return result, nil
}
