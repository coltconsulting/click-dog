package processor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/resilience"
)

func TestBuildCanarySpan(t *testing.T) {
	result := model.CanaryResult{Count: 5, LongQueriesExist: true}
	span := BuildCanarySpan(result, 60000)

	if span.OperationName != "click-dog.canary" {
		t.Errorf("operation name = %q, want click-dog.canary", span.OperationName)
	}
	if span.Kind != "INTERNAL" {
		t.Errorf("kind = %q, want INTERNAL", span.Kind)
	}
	if span.Attributes["click_dog.canary"] != "true" {
		t.Errorf("click_dog.canary = %q, want true", span.Attributes["click_dog.canary"])
	}
	if span.Attributes["click_dog.degraded"] != "true" {
		t.Errorf("click_dog.degraded = %q, want true", span.Attributes["click_dog.degraded"])
	}
	if span.Attributes["click_dog.long_queries_exist"] != "true" {
		t.Errorf("click_dog.long_queries_exist = %q, want true", span.Attributes["click_dog.long_queries_exist"])
	}
	if span.Attributes["click_dog.canary_count"] != "5" {
		t.Errorf("click_dog.canary_count = %q, want 5", span.Attributes["click_dog.canary_count"])
	}
	if span.Attributes["click_dog.threshold_ms"] != "60000" {
		t.Errorf("click_dog.threshold_ms = %q, want 60000", span.Attributes["click_dog.threshold_ms"])
	}
	if span.SpanID == 0 {
		t.Error("span ID should be non-zero")
	}
	// Duration should be 1ms (1000 microseconds)
	if span.FinishTimeUs-span.StartTimeUs != 1000 {
		t.Errorf("duration = %d us, want 1000", span.FinishTimeUs-span.StartTimeUs)
	}
}

func TestBuildCanarySpan_NoLongQueries(t *testing.T) {
	result := model.CanaryResult{Count: 0, LongQueriesExist: false}
	span := BuildCanarySpan(result, 60000)

	if span.Attributes["click_dog.long_queries_exist"] != "false" {
		t.Errorf("click_dog.long_queries_exist = %q, want false", span.Attributes["click_dog.long_queries_exist"])
	}
	if span.Attributes["click_dog.canary_count"] != "0" {
		t.Errorf("click_dog.canary_count = %q, want 0", span.Attributes["click_dog.canary_count"])
	}
}

// mockCanaryQuerier implements model.CanaryQuerier for unit testing.
type mockCanaryQuerier struct {
	result model.CanaryResult
	err    error
	calls  int
}

func (m *mockCanaryQuerier) RunCanaryQuery(_ context.Context, _ int) (model.CanaryResult, error) {
	m.calls++
	return m.result, m.err
}

func TestRunCanaryAndExport_Success(t *testing.T) {
	querier := &mockCanaryQuerier{
		result: model.CanaryResult{Count: 3, LongQueriesExist: true},
	}
	exporter := &mockExporter{}
	cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 3,
		SuccessThreshold: 1,
		ResetTimeoutS:    60,
	})

	cfg := &config.Config{
		Monitor: config.MonitorConfig{
			Canary: config.CanaryConfig{Enabled: true, ThresholdDurationMs: 60000},
		},
	}

	err := RunCanaryAndExport(context.Background(), querier, exporter, cfg, cb)

	if !errors.Is(err, ErrCanaryRan) {
		t.Errorf("expected ErrCanaryRan, got %v", err)
	}
	if querier.calls != 1 {
		t.Errorf("expected 1 canary query call, got %d", querier.calls)
	}
	if exporter.exportSpansCalls != 1 {
		t.Errorf("expected 1 ExportSpans call, got %d", exporter.exportSpansCalls)
	}
}

func TestRunCanaryAndExport_QueryFailure(t *testing.T) {
	querier := &mockCanaryQuerier{
		err: fmt.Errorf("clickhouse unreachable"),
	}
	exporter := &mockExporter{}
	cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 3,
		SuccessThreshold: 1,
		ResetTimeoutS:    60,
	})

	cfg := &config.Config{
		Monitor: config.MonitorConfig{
			Canary: config.CanaryConfig{Enabled: true, ThresholdDurationMs: 60000},
		},
	}

	err := RunCanaryAndExport(context.Background(), querier, exporter, cfg, cb)

	if err == nil || errors.Is(err, ErrCanaryRan) {
		t.Errorf("expected real error, got %v", err)
	}
	if exporter.exportSpansCalls != 0 {
		t.Error("exporter should not be called when canary query fails")
	}
	if cb.Failures() != 1 {
		t.Errorf("expected 1 CB failure, got %d", cb.Failures())
	}
}

func TestRunCanaryAndExport_ExportFailure(t *testing.T) {
	querier := &mockCanaryQuerier{
		result: model.CanaryResult{Count: 1, LongQueriesExist: true},
	}
	exporter := &mockExporter{
		exportSpansFunc: func(_ context.Context, _ []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
			return nil, fmt.Errorf("otel down")
		},
	}
	cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 3,
		SuccessThreshold: 1,
		ResetTimeoutS:    60,
	})

	cfg := &config.Config{
		Monitor: config.MonitorConfig{
			Canary: config.CanaryConfig{Enabled: true, ThresholdDurationMs: 60000},
		},
	}

	err := RunCanaryAndExport(context.Background(), querier, exporter, cfg, cb)

	if err == nil || errors.Is(err, ErrCanaryRan) {
		t.Errorf("expected export error, got %v", err)
	}
	if cb.Failures() != 1 {
		t.Errorf("expected 1 CB failure, got %d", cb.Failures())
	}
}

func TestProcessSpansWithProtection_CanaryOnCircuitOpen(t *testing.T) {
	cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 1,
		SuccessThreshold: 1,
		ResetTimeoutS:    3600,
	})
	cb.RecordFailure() // open the circuit

	querier := &mockCanaryQuerier{
		result: model.CanaryResult{Count: 2, LongQueriesExist: true},
	}
	exporter := &mockExporter{}
	cfg := &config.Config{
		Monitor: config.MonitorConfig{
			Canary: config.CanaryConfig{Enabled: true, ThresholdDurationMs: 60000},
		},
	}

	qf, _ := filter.NewQueryFilter(config.FiltersConfig{})

	err := (&Pipeline{
		Exporter:       exporter,
		Filter:         qf,
		Config:         cfg,
		CircuitBreaker: cb,
		CanaryQuerier:  querier,
		Metrics:        metrics.NewMetrics(),
	}).Process(context.Background())

	if !errors.Is(err, ErrCanaryRan) {
		t.Errorf("expected ErrCanaryRan, got %v", err)
	}
	if querier.calls != 1 {
		t.Errorf("expected canary to run, got %d calls", querier.calls)
	}
}

func TestProcessSpansWithProtection_NoCanaryWhenDisabled(t *testing.T) {
	cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 1,
		SuccessThreshold: 1,
		ResetTimeoutS:    3600,
	})
	cb.RecordFailure()

	cfg := &config.Config{
		Monitor: config.MonitorConfig{
			Canary: config.CanaryConfig{Enabled: false, ThresholdDurationMs: 60000},
		},
	}

	qf, _ := filter.NewQueryFilter(config.FiltersConfig{})

	exporter := &mockExporter{}
	err := (&Pipeline{
		Exporter:       exporter,
		Filter:         qf,
		Config:         cfg,
		CircuitBreaker: cb,
		Metrics:        metrics.NewMetrics(),
	}).Process(context.Background())

	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("expected ErrCircuitOpen, got %v", err)
	}
	if exporter.exportSpansCalls != 0 {
		t.Error("exporter should not be called when canary is disabled")
	}
}

func TestProcessSpansWithProtection_CanaryOnHighBackoff(t *testing.T) {
	baseInterval := 10 * time.Second
	poller := resilience.NewAdaptivePoller(baseInterval, config.BackoffConfig{
		MaxIntervalS:  300,
		BackoffFactor: 3.0, // one failure -> 30s, which is > 2*10s
	})
	poller.RecordFailure() // interval now 30s > 20s (2x base)

	querier := &mockCanaryQuerier{
		result: model.CanaryResult{Count: 0, LongQueriesExist: false},
	}
	exporter := &mockExporter{}
	cfg := &config.Config{
		Monitor: config.MonitorConfig{
			Canary:         config.CanaryConfig{Enabled: true, ThresholdDurationMs: 60000},
			CheckIntervalS: 10,
		},
	}

	qf, _ := filter.NewQueryFilter(config.FiltersConfig{})

	err := (&Pipeline{
		Exporter:      exporter,
		Filter:        qf,
		Config:        cfg,
		Poller:        poller,
		CanaryQuerier: querier,
		Metrics:       metrics.NewMetrics(),
	}).Process(context.Background())

	if !errors.Is(err, ErrCanaryRan) {
		t.Errorf("expected ErrCanaryRan, got %v", err)
	}
	if querier.calls != 1 {
		t.Errorf("expected canary to run on high backoff, got %d calls", querier.calls)
	}
}

func TestProcessQueriesBatch_RedactionDoesNotMutateInput(t *testing.T) {
	qf, err := filter.NewQueryFilter(config.FiltersConfig{
		RedactQueries: []config.RedactionRule{
			{Pattern: `(?i)password\s*=\s*'[^']*'`, Replacement: "password='[REDACTED]'"},
		},
	})
	if err != nil {
		t.Fatalf("Failed to create filter: %v", err)
	}

	original := "SELECT * FROM t WHERE password = 'secret123'"
	queries := []model.QueryLog{
		{QueryID: "q1", Query: original},
	}

	exporter := &mockExporter{}
	cfg := &config.Config{}

	ProcessQueriesBatch(context.Background(), queries, exporter, qf, cfg, nil)

	// The caller's slice must not be mutated
	if queries[0].Query != original {
		t.Errorf("caller's slice was mutated: got %q, want %q", queries[0].Query, original)
	}
	// But the query should have been exported (redacted)
	if exporter.exportQueryCalls != 1 {
		t.Errorf("expected 1 export call, got %d", exporter.exportQueryCalls)
	}
}

func TestUpdatePollerState_CanaryRanSentinel(t *testing.T) {
	baseInterval := 10 * time.Second
	poller := resilience.NewAdaptivePoller(baseInterval, config.BackoffConfig{
		MaxIntervalS:  300,
		BackoffFactor: 2.0,
	})
	ticker := time.NewTicker(baseInterval)
	defer ticker.Stop()

	poller.RecordFailure()
	backedOffInterval := poller.CurrentInterval()

	UpdatePollerState(poller, ErrCanaryRan, ticker, baseInterval, nil)

	if poller.CurrentInterval() != backedOffInterval {
		t.Errorf("poller interval should be unchanged after canary, got %v, want %v",
			poller.CurrentInterval(), backedOffInterval)
	}
}
