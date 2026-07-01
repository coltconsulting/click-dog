package processor

import (
	"context"
	"encoding/binary"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/resilience"
)

// RunCanaryAndExport executes a lightweight canary query during degraded mode
// and exports a synthetic span with the results. On success it records a
// circuit breaker success (helping recovery) and returns ErrCanaryRan.
//
// Note: the canary queries ClickHouse only. If the circuit breaker was tripped
// by OTEL export failures (not ClickHouse), a successful canary will still
// call RecordSuccess — this is acceptable because the canary export also
// exercises the export path, confirming both sides are healthy.
func RunCanaryAndExport(
	ctx context.Context,
	querier model.CanaryQuerier,
	exporter model.SpanExporter,
	cfg *config.Config,
	circuitBreaker *resilience.CircuitBreaker,
) error {
	canaryStart := time.Now()
	result, err := querier.RunCanaryQuery(ctx, cfg.Monitor.Canary.ThresholdDurationMs)
	if err != nil {
		clicklog.Warn("Canary query failed: %v", err)
		if circuitBreaker != nil {
			circuitBreaker.RecordFailure()
		}
		return fmt.Errorf("canary query failed: %w", err) // real error — triggers backoff
	}

	clicklog.Info("Degraded mode: canary detected %d queries > %dms (took %dms)",
		result.Count, cfg.Monitor.Canary.ThresholdDurationMs, time.Since(canaryStart).Milliseconds())

	canarySpan := BuildCanarySpan(result, cfg.Monitor.Canary.ThresholdDurationMs)

	_, exportErr := ExportSpansWithDeadline(ctx, cfg, exporter, []model.OpenTelemetrySpan{canarySpan})
	if exportErr != nil {
		clicklog.Warn("Canary span export failed: %v", exportErr)
		if circuitBreaker != nil {
			circuitBreaker.RecordFailure()
		}
		return fmt.Errorf("canary span export failed: %w", exportErr) // real error — triggers backoff
	}

	if circuitBreaker != nil {
		circuitBreaker.RecordSuccess()
	}
	return ErrCanaryRan // sentinel — suppresses backoff adjustment
}

// BuildCanarySpan creates a synthetic OpenTelemetrySpan representing a canary
// query result. The span has a unique nanosecond-based span ID and 1ms duration.
// TraceID packs the low 48 bits of UnixNano into the last segment of an
// otherwise-zero UUID, preserving the prior display form and giving each
// canary its own trace on the wire.
func BuildCanarySpan(result model.CanaryResult, thresholdMs int) model.OpenTelemetrySpan {
	now := time.Now()
	startUs := uint64(now.UnixMicro())
	finishUs := startUs + 1000 // 1ms synthetic duration

	spanID := uint64(now.UnixNano())

	var traceID uuid.UUID
	// PutUint64 at offset 8 writes bytes 8-15; the high 2 bytes (8-9) stay
	// zero because the 48-bit mask clears them, matching the old %012x tail.
	binary.BigEndian.PutUint64(traceID[8:], uint64(now.UnixNano())&0xffffffffffff)

	return model.OpenTelemetrySpan{
		TraceID:       traceID,
		SpanID:        spanID,
		OperationName: CanarySpanName,
		Kind:          "INTERNAL",
		StartTimeUs:   startUs,
		FinishTimeUs:  finishUs,
		FinishDate:    now,
		Attributes: map[string]string{
			"click_dog.canary":             "true",
			"click_dog.degraded":           "true",
			"click_dog.long_queries_exist": strconv.FormatBool(result.LongQueriesExist),
			"click_dog.canary_count":       strconv.FormatInt(result.Count, 10),
			"click_dog.threshold_ms":       strconv.Itoa(thresholdMs),
		},
	}
}
