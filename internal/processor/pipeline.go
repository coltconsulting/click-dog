package processor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/coltconsulting/click-dog/internal/clickhouse"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/resilience"
	"github.com/coltconsulting/click-dog/internal/webhook"
)

// LiveReader is the ClickHouse read surface required by a scheduled cycle.
// *clickhouse.ClickHouseReader is the production implementation.
type LiveReader interface {
	IsHealthy(ctx context.Context) bool
	// FetchLiveSpans returns the next page of Live Span Selection and holds its
	// progress pending; CommitLiveSpans records it once the cycle has handled
	// every span. A cycle that does not commit is read again by the next one.
	FetchLiveSpans(ctx context.Context, minTraceDurationMs int, lookback time.Duration, limit int, opts clickhouse.FetchOpts) ([]model.OpenTelemetrySpan, error)
	CommitLiveSpans()
	FetchQueryLogByQueryIDs(ctx context.Context, queryIDs []string, lookbackDays int) (map[string]model.QueryLog, error)
	// FetchTraceQueryIDs returns the clickhouse.query_id values recorded in the
	// span log for each trace. The IP whitelist uses it when a span page holds
	// a trace but none of its query spans, so resolution does not depend on
	// which page the root landed in.
	FetchTraceQueryIDs(ctx context.Context, traceIDs []uuid.UUID, lookbackDays int) (map[uuid.UUID][]string, error)
}

// Pipeline owns the live fetch → filter → enrich → export path plus its
// protection hooks. Callers wire dependencies once and then drive cycles
// through Process.
type Pipeline struct {
	Reader         LiveReader
	Exporter       model.SpanExporter
	Filter         *filter.QueryFilter
	Config         *config.Config
	SeenSpans      *lru.Cache[model.SpanKey, bool]
	CircuitBreaker *resilience.CircuitBreaker
	Heartbeat      *Heartbeat
	Poller         *resilience.AdaptivePoller
	CanaryQuerier  model.CanaryQuerier
	Metrics        *metrics.Metrics
	Webhook        *webhook.WebhookNotifier

	// LeaderGate, when non-nil, must return true for the cycle to run. It is
	// the cluster-mode leadership gate: a standby (non-leader) returns false
	// and the cycle is skipped without touching ClickHouse. nil means "always
	// run" — the sidecar / single-node case (no election) and every existing
	// call site, so the gate is a no-op unless explicitly wired in main.go.
	LeaderGate func() bool

	// serverVersion keeps the exporter's clickhouse.version attribute in step
	// with the server being read. Written and read on the single cycle
	// goroutine.
	serverVersion serverVersionStamp
}

func NewPipeline(p Pipeline) (*Pipeline, error) {
	var missing []string
	if p.Config == nil {
		missing = append(missing, "Config")
	}
	if p.Reader == nil {
		missing = append(missing, "Reader")
	}
	if p.Exporter == nil {
		missing = append(missing, "Exporter")
	}
	if p.Filter == nil {
		missing = append(missing, "Filter")
	}
	if p.SeenSpans == nil {
		missing = append(missing, "SeenSpans")
	}
	if p.Metrics == nil {
		missing = append(missing, "Metrics")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("processor pipeline missing required dependencies: %s", strings.Join(missing, ", "))
	}
	return &p, nil
}

func (p *Pipeline) Process(ctx context.Context) error {
	cycleStart := time.Now()

	// Cluster-mode leader gate. A non-leader standby runs no fetch/export so
	// only the leader's spans reach the backend (no steady-state duplication).
	// Recorded as a skipped cycle — not a zero-work success — so /status shows
	// the instance alive in standby without advancing last-success. Returns the
	// ErrLeaderStandby sentinel (not nil) so UpdatePollerState leaves the
	// adaptive backoff untouched: a standby skip did no real work and must not
	// reset a previously elevated interval. nil gate (sidecar / single) falls
	// straight through.
	if p.LeaderGate != nil {
		if !p.LeaderGate() {
			p.Metrics.SetLeader(false)
			p.Metrics.RecordSkippedCycleWithReason(time.Since(cycleStart).Milliseconds(), metrics.SkipReasonLeaderStandby)
			return ErrLeaderStandby
		}
		p.Metrics.SetLeader(true)
	}

	// Check circuit breaker before proceeding
	// A degraded cycle still exports (the canary span goes out while the
	// breaker is open) but cannot confirm the server version — and a
	// ClickHouse upgrade is the likeliest cause of the outage. Clear the stamp
	// rather than let those spans carry the pre-upgrade version.
	if p.CircuitBreaker != nil && !p.CircuitBreaker.Allow() {
		p.serverVersion.Clear(p.Exporter)
		return handleCircuitOpen(ctx, p.Exporter, p.Config, p.Filter, p.CircuitBreaker, p.Heartbeat, p.CanaryQuerier, p.Metrics, p.Webhook, cycleStart)
	}

	// If Allow() transitioned the circuit breaker to half-open, reflect that in metrics.
	if p.CircuitBreaker != nil && p.CircuitBreaker.State() == resilience.CircuitHalfOpen {
		p.Metrics.SetCircuitBreakerState("half_open")
	}

	// Check if backoff is elevated and canary should run instead of full fetch.
	if ran, canaryErr := p.handleElevatedBackoff(ctx, p.Exporter, p.Config, p.Filter, p.CircuitBreaker, p.Heartbeat, p.Poller, p.CanaryQuerier, p.Metrics, p.Webhook, cycleStart); ran {
		return canaryErr
	}

	// Health check before running queries - helps detect stale connections early
	if !p.Reader.IsHealthy(ctx) {
		p.serverVersion.Clear(p.Exporter)
		return handleHealthCheckFailure(p.CircuitBreaker, p.Metrics, p.Webhook, cycleStart)
	}

	// Stamp the exporter with the version of the server these spans came from,
	// after the health check so a known-bad connection is not queried again.
	p.serverVersion.Refresh(ctx, p.Reader, p.Exporter, time.Now())

	// Run the actual processing and capture any errors
	exported, filtered, dupes, err := fetchAndProcessSpans(ctx, p.Reader, p.Exporter, p.Filter, p.Config, p.SeenSpans, p.Heartbeat, p.Metrics)

	p.Metrics.RecordCycle(exported, filtered, dupes, time.Since(cycleStart).Milliseconds(), err)

	// Update circuit breaker state based on result
	updateCircuitBreakerState(p.CircuitBreaker, err, p.Metrics, p.Webhook)

	return err
}
