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
	f *filter.QueryFilter,
	circuitBreaker *resilience.CircuitBreaker,
	hb *Heartbeat,
	canaryQuerier model.CanaryQuerier,
	m *metrics.Metrics,
	wh *webhook.WebhookNotifier,
	cycleStart time.Time,
) error {
	if cfg.Monitor.Canary.Enabled && canaryQuerier != nil {
		err := RunCanaryAndExport(ctx, canaryQuerier, exporter, cfg, f, circuitBreaker, m, wh)
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
// If so, it runs a canary query instead of the full span fetch, alternating
// with real fetches: a cycle after a canary always runs the real fetch, since
// only a real fetch can show recovery and reset the backoff.
// Returns (true, err) if canary ran, or (false, nil) if the caller should proceed normally.
//
// Hardcoded 2x threshold: with default BackoffFactor 2.0, canary triggers
// after two consecutive failures. A future config knob could override this.
func (p *Pipeline) handleElevatedBackoff(
	ctx context.Context,
	exporter model.SpanExporter,
	cfg *config.Config,
	f *filter.QueryFilter,
	circuitBreaker *resilience.CircuitBreaker,
	hb *Heartbeat,
	poller *resilience.AdaptivePoller,
	canaryQuerier model.CanaryQuerier,
	m *metrics.Metrics,
	cycleStart time.Time,
) (bool, error) {
	canaryRanLast := p.lastCycleCanary
	p.lastCycleCanary = false
	if poller == nil || !cfg.Monitor.Canary.Enabled || canaryQuerier == nil {
		return false, nil
	}
	baseInterval := cfg.GetCheckInterval()
	currentInterval := poller.CurrentInterval()
	if currentInterval <= 2*baseInterval {
		return false, nil
	}
	if canaryRanLast {
		clicklog.Debug("Backoff elevated (%v > 2x base), running a real fetch after the canary", currentInterval)
		return false, nil
	}

	// The canary exports before this helper returns. Clear the cached version
	// now so the first degraded span cannot carry the previous healthy stamp.
	p.serverVersion.Clear(exporter, clearReasonCanary)

	clicklog.Debug("Backoff elevated (%v > 2x base), running canary", currentInterval)
	err := RunCanaryAndExport(ctx, canaryQuerier, exporter, cfg, f, circuitBreaker, m, p.Webhook)
	p.lastCycleCanary = true
	recordCanaryCycle(hb, m, err, cycleStart)
	return true, err
}

// handleHealthCheckFailure handles a failed ClickHouse health check by recording
// a circuit breaker failure and returning ErrHealthCheck.
func handleHealthCheckFailure(circuitBreaker *resilience.CircuitBreaker, m *metrics.Metrics, wh *webhook.WebhookNotifier, cycleStart time.Time) error {
	clicklog.Debug("ClickHouse connection unhealthy, skipping this cycle")
	recordBreakerFailure(circuitBreaker, m, wh, "ClickHouse health check failed")
	m.RecordCycle(0, 0, 0, time.Since(cycleStart).Milliseconds(), ErrHealthCheck)
	return ErrHealthCheck
}

// updateCircuitBreakerState records a cycle's outcome on the circuit breaker.
func updateCircuitBreakerState(circuitBreaker *resilience.CircuitBreaker, err error, m *metrics.Metrics, wh *webhook.WebhookNotifier) {
	if err != nil {
		recordBreakerFailure(circuitBreaker, m, wh, "cycle failed")
		return
	}
	recordBreakerSuccess(circuitBreaker, m, wh)
}

// recordBreakerFailure records a failure on the circuit breaker. When that
// failure opens the breaker, it publishes the change: the state gauge that
// /readyz, /status, and /metrics read, and the circuit_breaker_opened and
// error_spike webhooks. Every path that records a breaker failure calls this,
// so none of them can open the breaker while health and metrics still say
// closed. cause names the failing step in the webhook text.
func recordBreakerFailure(cb *resilience.CircuitBreaker, m *metrics.Metrics, wh *webhook.WebhookNotifier, cause string) {
	if cb == nil {
		return
	}
	prev := cb.State()
	cb.RecordFailure()
	if prev == resilience.CircuitOpen || cb.State() != resilience.CircuitOpen {
		return
	}
	if m != nil {
		m.SetCircuitBreakerState("open")
	}
	failures := cb.Failures()
	wh.Notify(webhook.EventCircuitBreakerOpened, fmt.Sprintf("Circuit breaker opened after %d failures (%s)", failures, cause))
	wh.Notify(webhook.EventErrorSpike, fmt.Sprintf("Error spike: %d consecutive failures triggered circuit breaker", failures))
}

// recordBreakerSuccess records a success on the circuit breaker and, when that
// success closes it, publishes the change the same way recordBreakerFailure
// publishes an opening.
func recordBreakerSuccess(cb *resilience.CircuitBreaker, m *metrics.Metrics, wh *webhook.WebhookNotifier) {
	if cb == nil {
		return
	}
	prev := cb.State()
	cb.RecordSuccess()
	if prev == resilience.CircuitClosed || cb.State() != resilience.CircuitClosed {
		return
	}
	if m != nil {
		m.SetCircuitBreakerState("closed")
	}
	wh.Notify(webhook.EventCircuitBreakerClosed, "Circuit breaker closed — recovery successful")
}

// recordCanaryCycle records heartbeat and metrics for a canary cycle.
//
// A failed canary is an error cycle. A successful one (the ErrCanaryRan
// sentinel) is neither a failure nor evidence of health: it counts as a
// skipped cycle, leaves the last cycle as the cycle recorded before it, and
// does not advance last success. Recording it as a successful cycle would make
// /status report ClickHouse healthy, and drop the fetch's error, on every
// canary while real fetches keep failing.
func recordCanaryCycle(hb *Heartbeat, m *metrics.Metrics, err error, cycleStart time.Time) {
	if errors.Is(err, ErrCanaryRan) {
		if hb != nil {
			hb.Record(0, 0, 0, nil)
		}
		m.RecordCanaryCycle()
		return
	}
	if hb != nil {
		hb.Record(0, 0, 0, err)
	}
	m.RecordCycle(0, 0, 0, time.Since(cycleStart).Milliseconds(), err)
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
	chReader LiveReader,
	exporter model.SpanExporter,
	f *filter.QueryFilter,
	cfg *config.Config,
	seenSpans *lru.Cache[model.SpanKey, bool],
	hb *Heartbeat,
	m *metrics.Metrics,
) (int, int, int, error) {
	// Fetch OpenTelemetry spans (filtered by SQL)
	spans, err := chReader.FetchLiveSpans(
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
	lookbackDays := int(cfg.GetLookback().Hours()/24) + 1
	if lookbackDays > 7 {
		lookbackDays = 7 // cap enrichment scan; older data unlikely to match current spans
	}
	if cfg.Monitor.ShouldEnrichFromQueryLog() && len(spans) > 0 {
		queryIDs := UniqueQueryIDs(spans)
		if len(queryIDs) > 0 {
			var enrichErr error
			queryLogMap, enrichErr = chReader.FetchQueryLogByQueryIDs(ctx, queryIDs, lookbackDays, EarliestQueryStartUs(spans))
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
				// Non-fatal — continue without enrichment. No filter decides
				// on these rows; they only add metadata to exported spans.
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

	// The filters that need a query's client, user, or statement are decided
	// per trace across the whole cycle, before batching: only a trace's query
	// spans carry those, and they may sit in another page. A failed lookup
	// fails the cycle before anything is exported, so the page is read again
	// and the breaker and backoff see a persistent failure.
	var traceDecision TraceDecision
	if len(spans) > 0 {
		var traceErr error
		traceDecision, traceErr = FilteredTraces(ctx, chReader, spans, f, lookbackDays)
		if traceErr != nil {
			err := fmt.Errorf("trace filters could not resolve this page's queries, withholding it: %w", traceErr)
			clicklog.Error("%v", err)
			if hb != nil {
				hb.Record(0, 0, 0, err)
			}
			return 0, 0, 0, err
		}
	}

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

		// First pass: filter, enrich, and collect spans to export. Query-text
		// shaping happens at the final boundary in ExportSpansWithDeadline so
		// exporters and fan-out sinks never receive the raw representation for
		// a privacy-restricting mode.
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

			if traceDecision.Withholds(&span) || !f.AllowsOperation(span.OperationName) {
				filtered++
				continue
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
			exportResult, err := ExportSpansWithDeadline(ctx, cfg, exporter, f, toExport)
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
	// No duplicate-ratio warning: every duplicate here is a span the walk
	// read again on purpose (the late-flush rescan, a retried page, or a
	// trace read again after its query span landed) and the dedup cache
	// caught. A cache too small for that overlap shows up as spans exported
	// twice, never as duplicates, so a high ratio is a healthy cycle.
	//
	// Every export in the page succeeded, so the walk may move past it. An
	// early return above leaves it uncommitted, and the next cycle re-reads
	// the page; spans a sink already accepted are then dedup-cache hits.
	chReader.CommitLiveSpans()
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

	// When the circuit breaker blocked the cycle, a cluster standby skipped,
	// or a canary ran in place of the fetch, don't touch the poller — neither
	// penalise (RecordFailure) nor reward (RecordSuccess). None did real
	// fetch/export work, so the existing backoff state must carry over
	// unchanged; the cycle after a canary runs the real fetch that decides it.
	if errors.Is(err, ErrCircuitOpen) || errors.Is(err, ErrLeaderStandby) || errors.Is(err, ErrCanaryRan) {
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
