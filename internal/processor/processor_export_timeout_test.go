package processor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/resilience"
)

func TestExportSpansWithDeadline_TimesOutBlockedExporter(t *testing.T) {
	cfg := &config.Config{
		Monitor: config.MonitorConfig{ExportTimeoutS: 1},
	}
	exporter := &mockExporter{
		exportSpansFunc: func(ctx context.Context, _ []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	start := time.Now()
	_, err := ExportSpansWithDeadline(context.Background(), cfg, exporter, []model.OpenTelemetrySpan{
		{SpanID: 1},
	})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	if !strings.Contains(err.Error(), "export deadline (1s) exceeded") {
		t.Fatalf("error = %q, want self-describing export deadline", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("export took %v, want bounded by configured timeout", elapsed)
	}
}

func TestExportSpansWithDeadline_ZeroDisablesTimeout(t *testing.T) {
	cfg := &config.Config{
		Monitor: config.MonitorConfig{ExportTimeoutS: 0},
	}
	exporter := &mockExporter{
		exportSpansFunc: func(ctx context.Context, spans []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
			if _, ok := ctx.Deadline(); ok {
				t.Fatal("export context should not have a deadline when export_timeout_s is 0")
			}
			keys := make([]model.SpanKey, len(spans))
			return keys, nil
		},
	}

	if _, err := ExportSpansWithDeadline(context.Background(), cfg, exporter, []model.OpenTelemetrySpan{{SpanID: 1}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProcessQueriesBatch_ExportTimeout(t *testing.T) {
	cfg := &config.Config{
		Monitor: config.MonitorConfig{ExportTimeoutS: 1},
	}
	exporter := &mockExporter{
		exportQueryFunc: func(ctx context.Context, _ model.QueryLog) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	qf, err := filter.NewQueryFilter(config.FiltersConfig{})
	if err != nil {
		t.Fatalf("query filter: %v", err)
	}

	result := ProcessQueriesBatch(context.Background(), []model.QueryLog{
		{QueryID: "q-timeout", Query: "SELECT 1"},
	}, exporter, qf, cfg, nil)

	if result.Exported != 0 {
		t.Errorf("exported = %d, want 0", result.Exported)
	}
	if result.Failed != 1 {
		t.Errorf("failed = %d, want 1", result.Failed)
	}
	if !errors.Is(result.FirstErr, context.DeadlineExceeded) {
		t.Fatalf("first error = %v, want deadline exceeded", result.FirstErr)
	}
	if !strings.Contains(result.FirstErr.Error(), "export deadline (1s) exceeded") {
		t.Fatalf("first error = %q, want self-describing export deadline", result.FirstErr)
	}
}

func TestExportQueryWithDeadline_ZeroDisablesTimeout(t *testing.T) {
	cfg := &config.Config{
		Monitor: config.MonitorConfig{ExportTimeoutS: 0},
	}
	exporter := &mockExporter{
		exportQueryFunc: func(ctx context.Context, _ model.QueryLog) error {
			if _, ok := ctx.Deadline(); ok {
				t.Fatal("export query context should not have a deadline when export_timeout_s is 0")
			}
			return nil
		},
	}

	if _, err := ExportQueryWithDeadline(context.Background(), cfg, exporter, model.QueryLog{QueryID: "q-1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunCanaryAndExport_ExportTimeoutRecordsFailure(t *testing.T) {
	cfg := &config.Config{
		Monitor: config.MonitorConfig{
			ExportTimeoutS: 1,
			Canary:         config.CanaryConfig{Enabled: true, ThresholdDurationMs: 60000},
		},
	}
	querier := &mockCanaryQuerier{
		result: model.CanaryResult{Count: 1, LongQueriesExist: true},
	}
	exporter := &mockExporter{
		exportSpansFunc: func(ctx context.Context, _ []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	cb := resilience.NewCircuitBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 3,
		SuccessThreshold: 1,
		ResetTimeoutS:    60,
	})

	err := RunCanaryAndExport(context.Background(), querier, exporter, cfg, cb)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	if !strings.Contains(err.Error(), "export deadline (1s) exceeded") {
		t.Fatalf("error = %q, want self-describing export deadline", err)
	}
	if cb.Failures() != 1 {
		t.Errorf("expected 1 CB failure, got %d", cb.Failures())
	}
}
