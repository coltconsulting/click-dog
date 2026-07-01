package processor

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/resilience"
	"github.com/coltconsulting/click-dog/internal/webhook"
)

// enrichFailCount tracks consecutive query_log enrichment failures.
// The first failure is always logged; subsequent failures are suppressed
// until a threshold is reached to avoid flooding logs when running against
// ClickHouse < v21.12, while still surfacing persistent real problems
// (network errors, permission issues) periodically.
var enrichFailCount atomic.Int64

// handleCircuitOpen handles the case when the circuit breaker is open.
// If canary is enabled, it runs a canary query; otherwise it skips the cycle.
// Precondition: circuitBreaker is non-nil (guaranteed by the call site guard).
func handleCircuitOpen(
	ctx context.Context,
	exporter model.SpanExporter,
	cfg *config.Config,
	circuitBreaker *resilience.CircuitBreaker,
	hb *Heartbeat,
	canaryQuerier model.CanaryQuerier,
	m *metrics.Metrics,
	wh *webhook.WebhookNotifier,
	cycleStart time.Time,
) error {
	if cfg.Monitor.Canary.Enabled && canaryQuerier != nil {
		prevState := circuitBreaker.State()
		err := RunCanaryAndExport(ctx, canaryQuerier, exporter, cfg, circuitBreaker)
		notifyOnRecovery(prevState, circuitBreaker, m, wh, "Circuit breaker closed — canary recovery successful")
		recordCanaryCycle(hb, m, err, cycleStart)
		return err
	}
	clicklog.Debug("Circuit breaker is open, skipping this cycle")
	// Record as a skipped cycle (not a zero-work success) so /status can
	// distinguish "breaker was open, nothing ran" from "cycle ran and found
	// nothing". Without this flag clickhouse_healthy would go green on the
	// next /status read after the breaker closes, even though the most
	// recent recorded "cycle" never touched ClickHouse.
	m.RecordSkippedCycleWithReason(time.Since(cycleStart).Milliseconds(), metrics.SkipReasonCircuitOpen)
	return ErrCircuitOpen
}

// handleElevatedBackoff checks if backoff is elevated (> 2x base) and canary is enabled.
// If so, it runs a canary query instead of the full span fetch.
// Returns (true, err) if canary ran, or (false, nil) if the caller should proceed normally.
//
// Hardcoded 2x threshold: with default BackoffFactor 2.0, canary triggers
// after a single failure. This is intentional — canary is cheap and we want
// early degraded-mode visibility. A future config knob could override this.
func handleElevatedBackoff(
	ctx context.Context,
	exporter model.SpanExporter,
	cfg *config.Config,
	circuitBreaker *resilience.CircuitBreaker,
	hb *Heartbeat,
	poller *resilience.AdaptivePoller,
	canaryQuerier model.CanaryQuerier,
	m *metrics.Metrics,
	wh *webhook.WebhookNotifier,
	cycleStart time.Time,
) (bool, error) {
	if poller == nil || !cfg.Monitor.Canary.Enabled || canaryQuerier == nil {
		return false, nil
	}
	baseInterval := cfg.GetCheckInterval()
	currentInterval := poller.CurrentInterval()
	if currentInterval <= 2*baseInterval {
		return false, nil
	}

	clicklog.Debug("Backoff elevated (%v > 2x base), running canary", currentInterval)
	var prevState resilience.CircuitState
	if circuitBreaker != nil {
		prevState = circuitBreaker.State()
	}
	err := RunCanaryAndExport(ctx, canaryQuerier, exporter, cfg, circuitBreaker)
	if circuitBreaker != nil {
		notifyOnRecovery(prevState, circuitBreaker, m, wh, "Circuit breaker closed — canary recovery successful")
	}
	recordCanaryCycle(hb, m, err, cycleStart)
	return true, err
}

// handleHealthCheckFailure handles a failed ClickHouse health check by recording
// a circuit breaker failure and returning ErrHealthCheck.
func handleHealthCheckFailure(circuitBreaker *resilience.CircuitBreaker, m *metrics.Metrics, wh *webhook.WebhookNotifier, cycleStart time.Time) error {
	clicklog.Debug("ClickHouse connection unhealthy, skipping this cycle")
	if circuitBreaker != nil {
		prevState := circuitBreaker.State()
		circuitBreaker.RecordFailure()
		if prevState != resilience.CircuitOpen && circuitBreaker.State() == resilience.CircuitOpen {
			m.SetCircuitBreakerState("open")
			wh.Notify(webhook.EventCircuitBreakerOpened, "Circuit breaker opened — ClickHouse health check failed")
			wh.Notify(webhook.EventErrorSpike, fmt.Sprintf("Error spike: %d consecutive failures triggered circuit breaker", circuitBreaker.Failures()))
		}
	}
	m.RecordCycle(0, 0, 0, time.Since(cycleStart).Milliseconds(), ErrHealthCheck)
	return ErrHealthCheck
}

// updateCircuitBreakerState records success or failure on the circuit breaker
// and fires appropriate webhook/metrics notifications on state transitions.
func updateCircuitBreakerState(circuitBreaker *resilience.CircuitBreaker, err error, m *metrics.Metrics, wh *webhook.WebhookNotifier) {
	if circuitBreaker == nil {
		return
	}
	prevState := circuitBreaker.State()
	if err != nil {
		circuitBreaker.RecordFailure()
		if prevState != resilience.CircuitOpen && circuitBreaker.State() == resilience.CircuitOpen {
			m.SetCircuitBreakerState("open")
			wh.Notify(webhook.EventCircuitBreakerOpened, fmt.Sprintf("Circuit breaker opened after %d failures", circuitBreaker.Failures()))
			wh.Notify(webhook.EventErrorSpike, fmt.Sprintf("Error spike: %d consecutive failures triggered circuit breaker", circuitBreaker.Failures()))
		}
	} else {
		circuitBreaker.RecordSuccess()
		notifyOnRecovery(prevState, circuitBreaker, m, wh, "Circuit breaker closed — recovery successful")
	}
}

// notifyOnRecovery checks if the circuit breaker transitioned from open/half-open
// to closed, and if so, updates metrics and sends a webhook notification.
func notifyOnRecovery(prevState resilience.CircuitState, cb *resilience.CircuitBreaker, m *metrics.Metrics, wh *webhook.WebhookNotifier, message string) {
	newState := cb.State()
	if (prevState == resilience.CircuitOpen || prevState == resilience.CircuitHalfOpen) && newState == resilience.CircuitClosed {
		m.SetCircuitBreakerState("closed")
		wh.Notify(webhook.EventCircuitBreakerClosed, message)
	}
}

// recordCanaryCycle records heartbeat and metrics for a canary cycle.
// ErrCanaryRan is a sentinel, not a real error — it is not counted as a failure.
func recordCanaryCycle(hb *Heartbeat, m *metrics.Metrics, err error, cycleStart time.Time) {
	realErr := err
	if errors.Is(err, ErrCanaryRan) {
		realErr = nil
	}
	if hb != nil {
		hb.Record(0, 0, 0, realErr)
	}
	m.RecordCycle(0, 0, 0, time.Since(cycleStart).Milliseconds(), realErr)
}

// recordSpanLogObservation pushes the per-cycle ClickHouse data-plane
// health signals into Metrics (#183). It is the single place that walks
// the raw spans slice to derive:
//
//   - last_poll_timestamp: advanced every successful fetch, including
//     zero-row fetches, so the operator can tell click-dog is alive
//     even when ClickHouse is quiet;
//   - newest_row_age: the timestamp of the freshest span seen. Empty
//     fetches pass time.Time{} and leave the gauge untouched — that
//     preservation is the staleness signal, do not "reset to now";
//   - spans_with_query_id_ratio: fraction of the raw (pre-filter) span
//     set carrying clickhouse.query_id, so dashboards reflect what
//     ClickHouse is producing rather than what click-dog chooses to
//     export.
//
// Newest-row time is derived from FinishTimeUs (microseconds since
// epoch), NOT from FinishDate. system.opentelemetry_span_log.finish_date
// is a ClickHouse Date partition column truncated to midnight, so
// comparing scrape-time-now against finish_date would inflate every
// span's apparent age by up to 24h and make a perfectly healthy
// span_log read as stale for most of the day (#200 review).
func recordSpanLogObservation(m *metrics.Metrics, spans []model.OpenTelemetrySpan) {
	var newestRowUs uint64
	withQueryID := 0
	for i := range spans {
		if spans[i].FinishTimeUs > newestRowUs {
			newestRowUs = spans[i].FinishTimeUs
		}
		if spans[i].Attributes["clickhouse.query_id"] != "" {
			withQueryID++
		}
	}
	var newestRow time.Time
	if newestRowUs > 0 {
		// Convert via int64; FinishTimeUs is uint64 but ClickHouse
		// finish_time_us is bounded by toUnixTimestamp64Micro(now64())
		// which fits well within int64 for the foreseeable future.
		newestRow = time.UnixMicro(int64(newestRowUs))
	}
	m.RecordSpanLogPoll(len(spans), newestRow)
	m.RecordSpansWithQueryIDRatio(withQueryID, len(spans))
}

// fetchAndProcessSpans fetches spans from ClickHouse and processes them.
// Returns (exported, filtered, duplicates, error).
func fetchAndProcessSpans(
	ctx context.Context,
	chReader *clickhouse.ClickHouseReader,
	exporter model.SpanExporter,
	f *filter.QueryFilter,
	cfg *config.Config,
	seenSpans *lru.Cache[model.SpanKey, bool],
	hb *Heartbeat,
	m *metrics.Metrics,
) (int, int, int, error) {
	// Fetch OpenTelemetry spans (filtered by SQL)
	spans, err := chReader.FetchOpenTelemetrySpansWithOpts(
		ctx,
		cfg.Monitor.MinTraceDurationMs,
		cfg.GetLookback(),
		cfg.Monitor.MaxSpansPerCycle,
		clickhouse.FetchOpts{
			MaxTraceDurationMs:  cfg.Monitor.MaxTraceDurationMs,
			MinSpanDurationMs:   cfg.Monitor.MinSpanDurationMs,
			MaxSpanDurationMs:   cfg.Monitor.MaxSpanDurationMs,
			BlacklistOperations: cfg.Filters.BlacklistOperations,
		},
	)
	if err != nil {
		clicklog.Error("Error fetching spans: %v", err)
		if hb != nil {
			hb.Record(0, 0, 0, err)
		}
		// Deliberately NOT recording RecordSpanLogPoll on error — a failed
		// fetch tells us nothing about span_log freshness or row counts, and
		// leaving the previous gauges intact lets the operator see "last
		// successful poll was N seconds ago" without conflating it with this
		// error cycle.
		return 0, 0, 0, err
	}

	clicklog.Debug("Found %d spans", len(spans))

	// Record span-log freshness signals (#183) — newest-row age,
	// last-poll timestamp, raw row count, query_id coverage ratio.
	// Extracted so a unit test can drive it without a real reader.
	recordSpanLogObservation(m, spans)

	// Enrich spans with query_log metadata (user, client, tables, stats).
	var queryLogMap map[string]model.QueryLog
	if cfg.Monitor.ShouldEnrichFromQueryLog() && len(spans) > 0 {
		queryIDs := UniqueQueryIDs(spans)
		if len(queryIDs) > 0 {
			lookbackDays := int(cfg.GetLookback().Hours()/24) + 1
			if lookbackDays > 7 {
				lookbackDays = 7 // cap enrichment scan; older data unlikely to match current spans
			}
			var enrichErr error
			queryLogMap, enrichErr = chReader.FetchQueryLogByQueryIDs(ctx, queryIDs, lookbackDays)
			// Record dashboard-facing enrichment metrics (#183). The attempt
			// counter increments on both branches; matched/total seeds the
			// last-cycle match-ratio gauge on success. queryIDs is non-empty
			// here (guarded above), so RecordQueryLogEnrichmentCycle won't
			// no-op on us — but the guard inside it is the canonical contract.
			m.RecordQueryLogEnrichmentCycle(len(queryLogMap), len(queryIDs), enrichErr)
			if enrichErr != nil {
				n := enrichFailCount.Add(1)
				// Log at 1, 10, 100, 1000, ... (exponential backoff on warnings)
				if n == 1 || (n >= 10 && n%(ipow10(ilog10(n))) == 0) {
					clicklog.Warn("query_log enrichment failed (%d consecutive): %v", n, enrichErr)
				}
				// Non-fatal — continue without enrichment
				queryLogMap = nil
			} else {
				enrichFailCount.Store(0) // reset on success
				if len(queryLogMap) > 0 {
					clicklog.Debug("Enriched %d/%d queries with query_log metadata", len(queryLogMap), len(queryIDs))
				}
			}
		}
		// No reset when queryIDs is empty — absence of work is not evidence of
		// success, and resetting would restart exponential backoff prematurely.
	}

	exported := 0
	filtered := 0
	duplicate := 0
	seenThisCycle := make(map[model.SpanKey]struct{}, len(spans))

	// Determine batch size (0 = process all at once)
	batchSize := cfg.Monitor.BatchSize
	if batchSize <= 0 {
		batchSize = len(spans)
	}

	// Process spans in batches
	for batchStart := 0; batchStart < len(spans); batchStart += batchSize {
		batchEnd := batchStart + batchSize
		if batchEnd > len(spans) {
			batchEnd = len(spans)
		}

		batch := spans[batchStart:batchEnd]
		clicklog.Debug("Processing batch %d-%d of %d spans", batchStart+1, batchEnd, len(spans))

		// First pass: filter, redact, and collect spans to export
		var toExport []model.OpenTelemetrySpan
		for _, span := range batch {
			// Composite key: span_id alone is not globally unique across traces.
			key := model.KeyOf(span)
			if seenSpans.Contains(key) {
				duplicate++
				continue
			}
			if _, ok := seenThisCycle[key]; ok {
				duplicate++
				continue
			}
			// First occurrence wins for this cycle, even if later filters drop it.
			// Later rows with the same span identity should not get a second pass.
			seenThisCycle[key] = struct{}{}

			// Apply filters
			queryText := span.Attributes["db.statement"]
			clientAddr := span.Attributes["client.address"]
			if f.ShouldFilter(span.OperationName, queryText, clientAddr) {
				filtered++
				continue
			}

			// User-based filter. The SQL-level filter on queryLogEnrichSelectSQL
			// has already removed enrichment rows for filtered users; this
			// per-span check enforces the same policy on the spans themselves
			// so an enriched-out user's data can't slip through.
			if ShouldFilterSpanByUser(span, f, queryLogMap) {
				filtered++
				continue
			}

			// Apply SQL redaction before export
			redacted := f.RedactQuery(queryText)
			if redacted != queryText {
				newAttrs := make(map[string]string, len(span.Attributes))
				for k, v := range span.Attributes {
					newAttrs[k] = v
				}
				newAttrs["db.statement"] = redacted
				span.Attributes = newAttrs
			}

			// Extract log_comment JSON from URI attributes
			if cfg.Monitor.ShouldExtractLogComment() {
				ExtractLogComment(&span)
			}

			// Enrich with query_log metadata (lookup by span's query_id)
			if qid := span.Attributes["clickhouse.query_id"]; qid != "" {
				if ql, ok := queryLogMap[qid]; ok {
					EnrichSpanFromQueryLog(&span, ql, cfg.Monitor.MaxQueryLength)
				}
			}

			toExport = append(toExport, span)
		}

		// Export batch to OTEL in a single gRPC call
		if len(toExport) > 0 {
			exportResult, err := ExportSpansWithDeadline(ctx, cfg, exporter, toExport)
			RecordExportObservability(m, exportResult)
			if err != nil {
				clicklog.Error("Error exporting batch of %d spans: %v", len(toExport), err)
				if hb != nil {
					hb.Record(exported, filtered, duplicate, err)
				}
				// Return error to trigger circuit breaker/backoff - spans will be retried next cycle
				return exported, filtered, duplicate, err
			}
			// Mark only successfully exported spans as seen
			for _, key := range exportResult.Accepted {
				seenSpans.Add(key, true)
			}
			exported += len(exportResult.Accepted)
		}

		// Add delay between batches if configured and there are more batches.
		// Respect ctx so SIGTERM doesn't have to wait out the full delay × remaining batches.
		if cfg.Monitor.BatchDelayMs > 0 && batchEnd < len(spans) {
			select {
			case <-ctx.Done():
				return exported, filtered, duplicate, ctx.Err()
			case <-time.After(time.Duration(cfg.Monitor.BatchDelayMs) * time.Millisecond):
			}
		}
	}

	if exported > 0 || filtered > 0 {
		clicklog.Info("Exported: %d, Filtered: %d, Duplicates: %d", exported, filtered, duplicate)
	} else {
		clicklog.Debug("Exported: 0, Filtered: 0, Duplicates: %d", duplicate)
	}
	total := exported + filtered + duplicate
	if total > 0 && duplicate > total/2 {
		clicklog.Info("High duplicate ratio: %d/%d spans (%d%%) — consider increasing dedup_cache_size if this persists",
			duplicate, total, duplicate*100/total)
	}
	if hb != nil {
		hb.Record(exported, filtered, duplicate, nil)
	}
	return exported, filtered, duplicate, nil
}

// UpdatePollerState updates the adaptive poller and ticker based on the result
func UpdatePollerState(poller *resilience.AdaptivePoller, err error, ticker *time.Ticker, baseInterval time.Duration, m *metrics.Metrics) {
	if poller == nil {
		return
	}

	// When the circuit breaker blocked the cycle, a canary ran, or a cluster
	// standby skipped, don't touch the poller — neither penalise (RecordFailure)
	// nor reward (RecordSuccess). None of these did real fetch/export work, so
	// the existing backoff state must carry over unchanged.
	if errors.Is(err, ErrCircuitOpen) || errors.Is(err, ErrCanaryRan) || errors.Is(err, ErrLeaderStandby) {
		return
	}

	if err != nil {
		poller.RecordFailure()
	} else {
		poller.RecordSuccess()
	}

	// Adjust ticker interval if backed off
	currentInterval := poller.CurrentInterval()
	if currentInterval != baseInterval {
		ticker.Reset(currentInterval)
	} else if !poller.IsBackedOff() {
		// Reset ticker to base interval if we recovered
		ticker.Reset(baseInterval)
	}

	// UpdatePollerState is called from many unit tests with a nil metrics
	// argument (they care about poller/ticker behavior, not gauges), so the
	// guard here is real — not dead code as in the cycle-recording path.
	if m != nil {
		m.SetBackoffInterval(currentInterval)
	}
}
