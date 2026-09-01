package processor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/export"
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
	qf, _ := filter.NewQueryFilter(cfg.Filters)
	_, err := ExportSpansWithDeadline(context.Background(), cfg, exporter, qf, []model.OpenTelemetrySpan{
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

func TestExportSpansWithDeadline_MultiExporterStartsEverySinkWithFullBudget(t *testing.T) {
	cfg := &config.Config{Monitor: config.MonitorConfig{ExportTimeoutS: 1}}
	started := make(chan string, 2)
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()

	blockingExporter := func(name string) *mockExporter {
		return &mockExporter{exportSpansFunc: func(ctx context.Context, _ []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
			started <- name
			select {
			case <-release:
				return nil, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}}
	}
	multi := export.NewMultiExporter(
		[]model.SpanExporter{blockingExporter("otel"), blockingExporter("splunk")},
		[]string{"otel", "splunk"},
	)
	qf, _ := filter.NewQueryFilter(cfg.Filters)

	done := make(chan error, 1)
	go func() {
		_, err := ExportSpansWithDeadline(context.Background(), cfg, multi, qf, []model.OpenTelemetrySpan{{SpanID: 1}})
		done <- err
	}()

	gotStarted := make(map[string]bool, 2)
	for range 2 {
		select {
		case name := <-started:
			gotStarted[name] = true
		case <-time.After(500 * time.Millisecond):
			t.Fatal("not every span sink started with the original export deadline")
		}
	}
	if !gotStarted["otel"] || !gotStarted["splunk"] {
		t.Fatalf("started sinks = %v, want otel and splunk", gotStarted)
	}
	close(release)
	released = true
	if err := <-done; err != nil {
		t.Fatalf("ExportSpansWithDeadline: %v", err)
	}
}

func TestExportBoundary_MissingFilterFailsClosed(t *testing.T) {
	exporter := &mockExporter{}
	cfg := &config.Config{}
	if _, err := ExportSpansWithDeadline(context.Background(), cfg, exporter, nil, []model.OpenTelemetrySpan{{
		Attributes: map[string]string{"db.statement": "SELECT secret"},
	}}); err == nil || !strings.Contains(err.Error(), "boundary") {
		t.Fatalf("span export error = %v, want missing-boundary failure", err)
	}
	if _, err := ExportQueryWithDeadline(context.Background(), cfg, exporter, nil, model.QueryLog{Query: "SELECT secret"}); err == nil || !strings.Contains(err.Error(), "boundary") {
		t.Fatalf("query export error = %v, want missing-boundary failure", err)
	}
	if exporter.exportSpansCalls != 0 || exporter.exportQueryCalls != 0 {
		t.Fatal("exporter was called without the query-text boundary")
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

	qf, _ := filter.NewQueryFilter(cfg.Filters)
	if _, err := ExportSpansWithDeadline(context.Background(), cfg, exporter, qf, []model.OpenTelemetrySpan{{SpanID: 1}}); err != nil {
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

	qf, _ := filter.NewQueryFilter(cfg.Filters)
	if _, err := ExportQueryWithDeadline(context.Background(), cfg, exporter, qf, model.QueryLog{QueryID: "q-1"}); err != nil {
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

	qf, _ := filter.NewQueryFilter(cfg.Filters)
	err := RunCanaryAndExport(context.Background(), querier, exporter, cfg, qf, cb)

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
