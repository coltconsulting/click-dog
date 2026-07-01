package processor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/filter"
	"github.com/coltconsulting/click-dog/internal/model"
	"github.com/coltconsulting/click-dog/internal/webhook"
)

func TestBatchResult_Predicates(t *testing.T) {
	tests := []struct {
		name      string
		r         BatchResult
		hasFailed bool
		allFailed bool
	}{
		{name: "empty", r: BatchResult{}, hasFailed: false, allFailed: false},
		{name: "all success", r: BatchResult{Exported: 3}, hasFailed: false, allFailed: false},
		{name: "all filtered", r: BatchResult{Filtered: 3}, hasFailed: false, allFailed: false},
		{name: "partial fail", r: BatchResult{Exported: 2, Failed: 1}, hasFailed: true, allFailed: false},
		{name: "all failed", r: BatchResult{Failed: 3}, hasFailed: true, allFailed: true},
		{name: "all failed with filtered", r: BatchResult{Filtered: 1, Failed: 2}, hasFailed: true, allFailed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.r.HasFailures(); got != tt.hasFailed {
				t.Errorf("HasFailures() = %v, want %v", got, tt.hasFailed)
			}
			if got := tt.r.AllFailed(); got != tt.allFailed {
				t.Errorf("AllFailed() = %v, want %v", got, tt.allFailed)
			}
		})
	}
}

func TestProcessQueriesBatch_AllSuccess(t *testing.T) {
	exp := &mockExporter{}
	qf, err := filter.NewQueryFilter(config.FiltersConfig{})
	if err != nil {
		t.Fatalf("NewQueryFilter failed: %v", err)
	}

	queries := []model.QueryLog{
		{QueryID: "q1", Query: "SELECT 1"},
		{QueryID: "q2", Query: "SELECT 2"},
		{QueryID: "q3", Query: "SELECT 3"},
	}

	got := ProcessQueriesBatch(context.Background(), queries, exp, qf, &config.Config{}, nil)

	want := BatchResult{Exported: 3}
	if got != want {
		t.Errorf("BatchResult = %+v, want %+v", got, want)
	}
	if got.HasFailures() || got.AllFailed() {
		t.Errorf("expected no failure flags, got HasFailures=%v AllFailed=%v", got.HasFailures(), got.AllFailed())
	}
	if exp.exportQueryCalls != 3 {
		t.Errorf("expected 3 export calls, got %d", exp.exportQueryCalls)
	}
}

func TestProcessQueriesBatch_PartialFailure(t *testing.T) {
	failOn := map[string]bool{"q2": true, "q4": true}
	firstErr := errors.New("collector unavailable")

	exp := &mockExporter{
		exportQueryFunc: func(_ context.Context, log model.QueryLog) error {
			if failOn[log.QueryID] {
				return fmt.Errorf("%s: %w", log.QueryID, firstErr)
			}
			return nil
		},
	}
	qf, err := filter.NewQueryFilter(config.FiltersConfig{})
	if err != nil {
		t.Fatalf("NewQueryFilter failed: %v", err)
	}

	queries := []model.QueryLog{
		{QueryID: "q1", Query: "SELECT 1"},
		{QueryID: "q2", Query: "SELECT 2"},
		{QueryID: "q3", Query: "SELECT 3"},
		{QueryID: "q4", Query: "SELECT 4"},
		{QueryID: "q5", Query: "SELECT 5"},
	}

	got := ProcessQueriesBatch(context.Background(), queries, exp, qf, &config.Config{}, nil)

	if got.Exported != 3 {
		t.Errorf("Exported = %d, want 3", got.Exported)
	}
	if got.Failed != 2 {
		t.Errorf("Failed = %d, want 2", got.Failed)
	}
	if got.Filtered != 0 {
		t.Errorf("Filtered = %d, want 0", got.Filtered)
	}
	if !got.HasFailures() {
		t.Error("HasFailures() should be true")
	}
	if got.AllFailed() {
		t.Error("AllFailed() should be false for partial failure")
	}
	if !errors.Is(got.FirstErr, firstErr) {
		t.Errorf("FirstErr = %v, want wrap of %v", got.FirstErr, firstErr)
	}
	// Continue-past-failures invariant: every query attempted once.
	if exp.exportQueryCalls != 5 {
		t.Errorf("expected 5 export calls (continue past failures), got %d", exp.exportQueryCalls)
	}
}

func TestProcessQueriesBatch_AllFailure(t *testing.T) {
	exportErr := errors.New("OTLP grpc connection refused")
	exp := &mockExporter{
		exportQueryFunc: func(_ context.Context, _ model.QueryLog) error {
			return exportErr
		},
	}
	qf, err := filter.NewQueryFilter(config.FiltersConfig{})
	if err != nil {
		t.Fatalf("NewQueryFilter failed: %v", err)
	}

	queries := []model.QueryLog{
		{QueryID: "q1", Query: "SELECT 1"},
		{QueryID: "q2", Query: "SELECT 2"},
		{QueryID: "q3", Query: "SELECT 3"},
	}

	got := ProcessQueriesBatch(context.Background(), queries, exp, qf, &config.Config{}, nil)

	if got.Exported != 0 {
		t.Errorf("Exported = %d, want 0", got.Exported)
	}
	if got.Failed != 3 {
		t.Errorf("Failed = %d, want 3", got.Failed)
	}
	if !got.AllFailed() {
		t.Error("AllFailed() should be true when every export fails")
	}
	if !errors.Is(got.FirstErr, exportErr) {
		t.Errorf("FirstErr = %v, want %v", got.FirstErr, exportErr)
	}
}

func TestProcessQueriesBatch_FilteredAndFailed(t *testing.T) {
	exp := &mockExporter{
		exportQueryFunc: func(_ context.Context, log model.QueryLog) error {
			if log.QueryID == "fail" {
				return errors.New("boom")
			}
			return nil
		},
	}
	qf, err := filter.NewQueryFilter(config.FiltersConfig{
		BlacklistQueries: []string{"^DROP"},
	})
	if err != nil {
		t.Fatalf("NewQueryFilter failed: %v", err)
	}

	queries := []model.QueryLog{
		{QueryID: "ok1", Query: "SELECT 1"},
		{QueryID: "drop", Query: "DROP TABLE t"}, // filtered
		{QueryID: "fail", Query: "SELECT 2"},     // export error
		{QueryID: "ok2", Query: "SELECT 3"},
	}

	got := ProcessQueriesBatch(context.Background(), queries, exp, qf, &config.Config{}, nil)

	if got.Exported != 2 || got.Filtered != 1 || got.Failed != 1 {
		t.Errorf("counts = (exp=%d filt=%d fail=%d), want (2,1,1)", got.Exported, got.Filtered, got.Failed)
	}
	if got.Exported+got.Filtered+got.Failed != len(queries) {
		t.Errorf("counts %d+%d+%d != %d input queries", got.Exported, got.Filtered, got.Failed, len(queries))
	}
	if !got.HasFailures() {
		t.Error("HasFailures() should be true")
	}
	if got.AllFailed() {
		t.Error("AllFailed() should be false when at least one export succeeded")
	}
}

// wh is nil throughout — Notify is nil-safe; we assert on the returned
// error since that is what makes backfill exit non-zero.
func TestReportBackfillOutcome(t *testing.T) {
	tests := []struct {
		name        string
		result      BatchResult
		wantErr     bool
		errContains string
	}{
		{
			name:    "all success",
			result:  BatchResult{Exported: 5},
			wantErr: false,
		},
		{
			name:    "all filtered routes to complete",
			result:  BatchResult{Filtered: 5},
			wantErr: false,
		},
		{
			name:        "partial failure",
			result:      BatchResult{Exported: 3, Failed: 2, FirstErr: errors.New("send failed")},
			wantErr:     true,
			errContains: "partial failure",
		},
		{
			name:        "all failed",
			result:      BatchResult{Failed: 5, FirstErr: errors.New("send failed")},
			wantErr:     true,
			errContains: "all export attempts failed",
		},
	}
	var nilNotifier *webhook.WebhookNotifier
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ReportBackfillOutcome("2024-01-01T00:00:00Z", "2024-01-01T01:00:00Z", tt.result, nilNotifier)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.errContains != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("error %q does not contain %q", err, tt.errContains)
				}
				if !errors.Is(err, tt.result.FirstErr) {
					t.Errorf("error does not wrap FirstErr: %v", err)
				}
				// queries=N is computed from the result counts; check it lines up with the input.
				totalQueries := tt.result.Exported + tt.result.Filtered + tt.result.Failed
				for _, want := range []string{
					fmt.Sprintf("queries=%d", totalQueries),
					fmt.Sprintf("exported=%d", tt.result.Exported),
					fmt.Sprintf("failed=%d", tt.result.Failed),
				} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q missing count fragment %q", err, want)
					}
				}
			}
		})
	}
}
