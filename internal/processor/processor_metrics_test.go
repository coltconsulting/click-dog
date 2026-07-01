package processor

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/metrics"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/resilience"
)

// scrapeMetrics renders Prometheus exposition for m via httptest.NewRecorder
// — same lightweight pattern as TestMetrics_Handler in internal/metrics, no
// real listener allocated.
func scrapeMetrics(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	m.Handler()(w, req)
	return w.Body.String()
}

func mustContain(t *testing.T, body, line string) {
	t.Helper()
	if !strings.Contains(body, line) {
		t.Errorf("metrics body missing %q\n----\n%s\n----", line, body)
	}
}

func captureProcessorWarns(t *testing.T) *bytes.Buffer {
	t.Helper()
	resetExportSinkWarningCounts()
	t.Cleanup(resetExportSinkWarningCounts)
	if err := clicklog.InitLogger("warn", "", "text", 0, 0); err != nil {
		t.Fatalf("InitLogger: %v", err)
	}
	t.Cleanup(clicklog.CloseLogger)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	const canary = "__processor_warn_capture__"
	clicklog.Warn(canary)
	if !strings.Contains(buf.String(), canary) {
		t.Fatalf("log capture broken: clicklog.Warn output was not captured; got %q", buf.String())
	}
	buf.Reset()
	return &buf
}

// TestPipeline_ProcessCircuitOpenRecordsSkippedCycle — circuit breaker open, canary disabled.
// Pipeline.Process short-circuits to the skipped path; the result
// counter should land on `skipped` and the last-cycle gauges should show
// zero counts (the cycle did no ClickHouse work). Last-success timestamp
// must NOT advance — a skip is not evidence of health.
func TestPipeline_ProcessCircuitOpenRecordsSkippedCycle(t *testing.T) {
	cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 1,
		SuccessThreshold: 1,
		ResetTimeoutS:    3600,
	})
	cb.RecordFailure() // open the circuit

	m := metrics.NewMetrics()
	cfg := &config.Config{
		Monitor: config.MonitorConfig{
			Canary: config.CanaryConfig{Enabled: false},
		},
	}
	qf, _ := filter.NewQueryFilter(config.FiltersConfig{})

	err := (&Pipeline{
		Exporter:       &mockExporter{},
		Filter:         qf,
		Config:         cfg,
		CircuitBreaker: cb,
		Metrics:        m,
	}).Process(context.Background())

	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got %v", err)
	}
	if got := m.Snapshot().LastCycle.SkipReason; got != metrics.SkipReasonCircuitOpen {
		t.Fatalf("skip reason = %q, want %q", got, metrics.SkipReasonCircuitOpen)
	}

	body := scrapeMetrics(t, m)
	mustContain(t, body, `click_dog_cycle_results_total{result="skipped"} 1`)
	mustContain(t, body, `click_dog_cycle_results_total{result="success"} 0`)
	mustContain(t, body, `click_dog_cycle_results_total{result="error"} 0`)
	mustContain(t, body, "click_dog_last_cycle_exported_spans 0")
	mustContain(t, body, "click_dog_last_cycle_filtered_spans 0")
	mustContain(t, body, "click_dog_last_cycle_duplicate_spans 0")
	// Skipped cycle MUST NOT advance last-success — operators alert on this
	// gauge to detect a stalled exporter.
	mustContain(t, body, "click_dog_last_success_timestamp_seconds 0")
}

// TestProcessor_ErrorCycleOutcome — drives the health-check failure path
// (handleHealthCheckFailure) directly. That path records a real cycle with
// ErrHealthCheck, so the result counter should land on `error` and
// last-success timestamp should remain at zero.
func TestProcessor_ErrorCycleOutcome(t *testing.T) {
	cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 5,
		SuccessThreshold: 1,
		ResetTimeoutS:    60,
	})
	m := metrics.NewMetrics()

	err := handleHealthCheckFailure(cb, m, nil, time.Now())
	if !errors.Is(err, ErrHealthCheck) {
		t.Fatalf("expected ErrHealthCheck, got %v", err)
	}

	body := scrapeMetrics(t, m)
	mustContain(t, body, `click_dog_cycle_results_total{result="error"} 1`)
	mustContain(t, body, `click_dog_cycle_results_total{result="success"} 0`)
	mustContain(t, body, `click_dog_cycle_results_total{result="skipped"} 0`)
	// Error cycles must not falsely advance last-success semantics.
	mustContain(t, body, "click_dog_last_success_timestamp_seconds 0")
}

// TestProcessor_SuccessCycleUpdatesLastCycleGauges drives RecordCycle the way
// fetchAndProcessSpans drives it on a successful normal cycle: with real
// exported/filtered/duplicate counts and a non-zero duration. The
// last-cycle gauges must reflect those values, the success counter must
// increment, and the error/skipped counters must stay flat.
func TestProcessor_SuccessCycleUpdatesLastCycleGauges(t *testing.T) {
	m := metrics.NewMetrics()
	m.RecordCycle(42, 7, 3, 1234, nil) // 1.234s

	body := scrapeMetrics(t, m)
	mustContain(t, body, `click_dog_cycle_results_total{result="success"} 1`)
	mustContain(t, body, `click_dog_cycle_results_total{result="error"} 0`)
	mustContain(t, body, `click_dog_cycle_results_total{result="skipped"} 0`)
	mustContain(t, body, "click_dog_last_cycle_duration_seconds 1.234")
	mustContain(t, body, "click_dog_last_cycle_exported_spans 42")
	mustContain(t, body, "click_dog_last_cycle_filtered_spans 7")
	mustContain(t, body, "click_dog_last_cycle_duplicate_spans 3")
}

// TestProcessor_CanarySuccessCountsAsSuccess pins the canary→outcome routing.
// A canary that ran cleanly resolves the circuit-blocked cycle through
// RecordCycle with a nil error (ErrCanaryRan is filtered out by
// recordCanaryCycle), so the result lands in the success bucket. If a
// future change reclassifies canary outcomes into a separate bucket,
// update this test alongside the docs and dashboard.
func TestProcessor_CanarySuccessCountsAsSuccess(t *testing.T) {
	cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 1,
		SuccessThreshold: 1,
		ResetTimeoutS:    3600,
	})
	cb.RecordFailure() // open the circuit

	querier := &mockCanaryQuerier{
		result: model.CanaryResult{Count: 0, LongQueriesExist: false},
	}
	m := metrics.NewMetrics()
	cfg := &config.Config{
		Monitor: config.MonitorConfig{
			Canary: config.CanaryConfig{Enabled: true, ThresholdDurationMs: 60000},
		},
	}
	qf, _ := filter.NewQueryFilter(config.FiltersConfig{})

	err := (&Pipeline{
		Exporter:       &mockExporter{},
		Filter:         qf,
		Config:         cfg,
		CircuitBreaker: cb,
		CanaryQuerier:  querier,
		Metrics:        m,
	}).Process(context.Background())
	if !errors.Is(err, ErrCanaryRan) {
		t.Fatalf("expected ErrCanaryRan, got %v", err)
	}

	body := scrapeMetrics(t, m)
	// Canary success bumps the success bucket today. If a future change
	// reclassifies canary cycles into a "skipped" or "degraded" outcome,
	// update this test alongside the docs.
	mustContain(t, body, `click_dog_cycle_results_total{result="success"} 1`)
	mustContain(t, body, `click_dog_cycle_results_total{result="skipped"} 0`)
	mustContain(t, body, `click_dog_cycle_results_total{result="error"} 0`)
}

func TestProcessQueriesBatch_RecordsSingleExporterResult(t *testing.T) {
	m := metrics.NewMetrics()
	exp := &mockExporter{}
	qf, err := filter.NewQueryFilter(config.FiltersConfig{})
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}

	result := ProcessQueriesBatch(
		context.Background(),
		[]model.QueryLog{{QueryID: "q1", Query: "SELECT 1"}},
		exp,
		qf,
		&config.Config{},
		m,
	)

	if result.Exported != 1 || result.Failed != 0 || result.Filtered != 0 {
		t.Fatalf("BatchResult = %+v, want one exported query", result)
	}

	body := scrapeMetrics(t, m)
	mustContain(t, body, `click_dog_export_attempts_total{sink="mock"} 1`)
	mustContain(t, body, `click_dog_export_accepted_total{sink="mock"} 1`)
	mustContain(t, body, `click_dog_export_errors_total{sink="mock"} 0`)
}

func TestRecordExportObservability_MultiExporterPartialFailure(t *testing.T) {
	buf := captureProcessorWarns(t)
	multi, _, _ := newMultiExporterPartialFailure(t)

	result, exportErr := multi.ExportSpans(context.Background(), []model.OpenTelemetrySpan{
		{SpanID: 1},
	})
	if exportErr == nil {
		t.Fatal("expected partial multi-exporter failure")
	}

	m := metrics.NewMetrics()
	buf.Reset()
	RecordExportObservability(m, result)

	body := scrapeMetrics(t, m)
	mustContain(t, body, `click_dog_export_attempts_total{sink="otel"} 1`)
	mustContain(t, body, `click_dog_export_accepted_total{sink="otel"} 1`)
	mustContain(t, body, `click_dog_export_errors_total{sink="otel"} 0`)
	mustContain(t, body, `click_dog_export_attempts_total{sink="splunk"} 1`)
	mustContain(t, body, `click_dog_export_accepted_total{sink="splunk"} 0`)
	mustContain(t, body, `click_dog_export_errors_total{sink="splunk"} 1`)

	out := buf.String()
	if !strings.Contains(out, `Exporter sink="splunk" accepted 0/1 items before error`) {
		t.Errorf("expected sink failure summary in logs, got:\n%s", out)
	}
	if strings.Contains(out, `Exporter sink="otel"`) {
		t.Errorf("healthy sink should not emit a warning, got:\n%s", out)
	}
}

func TestRecordExportObservability_PartialWithoutErrorWarnsWithBackoff(t *testing.T) {
	buf := captureProcessorWarns(t)
	result := model.ExportResult{Sinks: []model.ExportSinkStatus{{
		Name:     "otel",
		Sent:     2,
		Accepted: 1,
	}}}

	for range 11 {
		RecordExportObservability(nil, result)
	}

	out := buf.String()
	if got := strings.Count(out, `Exporter sink="otel"`); got != 2 {
		t.Errorf("warning count = %d, want 2 (first and tenth consecutive warnings)\n%s", got, out)
	}
	if !strings.Contains(out, "without returning an error (1 consecutive)") {
		t.Errorf("missing first partial-acceptance warning:\n%s", out)
	}
	if !strings.Contains(out, "without returning an error (10 consecutive)") {
		t.Errorf("missing tenth partial-acceptance warning:\n%s", out)
	}

	buf.Reset()
	RecordExportObservability(nil, model.ExportResult{Sinks: []model.ExportSinkStatus{{
		Name:     "otel",
		Sent:     2,
		Accepted: 2,
	}}})
	RecordExportObservability(nil, result)

	if !strings.Contains(buf.String(), "without returning an error (1 consecutive)") {
		t.Errorf("successful sink status should reset warning backoff, got:\n%s", buf.String())
	}
}
