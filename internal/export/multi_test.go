package export

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/coltconsulting/click-dog/internal/clicklog"
	"github.com/coltconsulting/click-dog/internal/model"
)

// mockExporter implements model.SpanExporter for testing.
type mockExporter struct {
	exportSpansFunc  func(ctx context.Context, spans []model.OpenTelemetrySpan) ([]model.SpanKey, error)
	exportQueryFunc  func(ctx context.Context, log model.QueryLog) error
	closeFunc        func(ctx context.Context) error
	exportSpansCalls int
	exportQueryCalls int
	closeCalls       int
}

func (m *mockExporter) ExportSpans(ctx context.Context, spans []model.OpenTelemetrySpan) (model.ExportResult, error) {
	m.exportSpansCalls++
	if m.exportSpansFunc != nil {
		keys, err := m.exportSpansFunc(ctx, spans)
		return model.ExportResult{
			Accepted:      keys,
			TotalSent:     len(spans),
			TotalAccepted: len(keys),
		}, err
	}
	keys := make([]model.SpanKey, len(spans))
	for i, s := range spans {
		keys[i] = model.KeyOf(s)
	}
	return model.ExportResult{
		Accepted:      keys,
		TotalSent:     len(spans),
		TotalAccepted: len(keys),
	}, nil
}

func (m *mockExporter) ExportQuery(ctx context.Context, log model.QueryLog) (model.ExportResult, error) {
	m.exportQueryCalls++
	if m.exportQueryFunc != nil {
		err := m.exportQueryFunc(ctx, log)
		result := model.ExportResult{TotalSent: 1}
		if err == nil {
			result.TotalAccepted = 1
		}
		return result, err
	}
	return model.ExportResult{TotalSent: 1, TotalAccepted: 1}, nil
}

func (m *mockExporter) Close(ctx context.Context) error {
	m.closeCalls++
	if m.closeFunc != nil {
		return m.closeFunc(ctx)
	}
	return nil
}

func multiTestSpans() []model.OpenTelemetrySpan {
	return []model.OpenTelemetrySpan{
		{SpanID: 100, TraceID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), OperationName: "op1"},
		{SpanID: 200, TraceID: uuid.MustParse("22222222-2222-2222-2222-222222222222"), OperationName: "op2"},
		{SpanID: 300, TraceID: uuid.MustParse("33333333-3333-3333-3333-333333333333"), OperationName: "op3"},
	}
}

func TestMultiExporter_ExportSpans(t *testing.T) {
	otelDown := errors.New("otel down")
	splunkDown := errors.New("splunk down")

	span1 := multiTestSpans()[0]
	span2 := multiTestSpans()[1]
	span3 := multiTestSpans()[2]
	key1 := model.KeyOf(span1)
	key2 := model.KeyOf(span2)
	key3 := model.KeyOf(span3)

	tests := []struct {
		name        string
		funcs       []func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error)
		wantKeys    []model.SpanKey
		wantErr     bool
		wantErrIs   []error  // errors.Is targets that must match
		wantErrSubs []string // substrings that must appear in err.Error()
	}{
		{
			name:     "all succeed returns intersection",
			funcs:    []func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error){nil, nil},
			wantKeys: []model.SpanKey{key1, key2, key3},
		},
		{
			name: "one fails returns error and nil keys",
			funcs: []func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error){
				nil,
				func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
					return nil, splunkDown
				},
			},
			wantKeys:    nil,
			wantErr:     true,
			wantErrIs:   []error{splunkDown},
			wantErrSubs: []string{"1/2 sinks", "splunk", "splunk down"},
		},
		{
			name: "all fail returns error joining every sink",
			funcs: []func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error){
				func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
					return nil, otelDown
				},
				func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
					return nil, splunkDown
				},
			},
			wantKeys:    nil,
			wantErr:     true,
			wantErrIs:   []error{otelDown, splunkDown},
			wantErrSubs: []string{"2/2 sinks", "otel", "splunk"},
		},
		{
			name: "intersection of successful sinks when all succeed",
			funcs: []func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error){
				func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
					return []model.SpanKey{key1, key2}, nil
				},
				func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
					return []model.SpanKey{key2, key3}, nil
				},
			},
			wantKeys: []model.SpanKey{key2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exps := make([]model.SpanExporter, len(tt.funcs))
			for i, fn := range tt.funcs {
				exps[i] = &mockExporter{exportSpansFunc: fn}
			}
			multi := NewMultiExporter(exps, []string{"otel", "splunk"})

			result, err := multi.ExportSpans(context.Background(), multiTestSpans())
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if err != nil {
				for _, target := range tt.wantErrIs {
					if !errors.Is(err, target) {
						t.Errorf("errors.Is(err, %v) = false; err = %v", target, err)
					}
				}
				for _, sub := range tt.wantErrSubs {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("err = %q does not contain %q", err.Error(), sub)
					}
				}
			} else if len(tt.wantErrIs) > 0 || len(tt.wantErrSubs) > 0 {
				t.Errorf("wantErrIs/wantErrSubs set but err is nil")
			}
			if tt.wantErr {
				if result.Accepted != nil {
					t.Errorf("expected nil keys on error, got %v", result.Accepted)
				}
				return
			}
			if !sameSpanKeySet(result.Accepted, tt.wantKeys) {
				t.Errorf("keys = %v, want %v (order-independent)", result.Accepted, tt.wantKeys)
			}
			if result.TotalSent != len(multiTestSpans()) {
				t.Errorf("TotalSent = %d, want %d", result.TotalSent, len(multiTestSpans()))
			}
			if result.TotalAccepted != len(result.Accepted) {
				t.Errorf("TotalAccepted = %d, want len(Accepted) %d", result.TotalAccepted, len(result.Accepted))
			}
			if len(result.Sinks) != 2 {
				t.Fatalf("len(Sinks) = %d, want 2", len(result.Sinks))
			}
			for _, sink := range result.Sinks {
				if sink.Sent != len(multiTestSpans()) {
					t.Errorf("%s Sent = %d, want %d", sink.Name, sink.Sent, len(multiTestSpans()))
				}
				if sink.Accepted == 0 {
					t.Errorf("%s Accepted = 0, want successful sink count", sink.Name)
				}
				if sink.Error != nil {
					t.Errorf("%s Error = %v, want nil", sink.Name, sink.Error)
				}
			}
		})
	}
}

func TestMultiExporter_ExportSpans_NormalizesMissingNames(t *testing.T) {
	multi := NewMultiExporter(
		[]model.SpanExporter{&mockExporter{}, &mockExporter{}},
		[]string{"otel"},
	)

	result, err := multi.ExportSpans(context.Background(), multiTestSpans())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := result.Sinks[0].Name, "otel"; got != want {
		t.Errorf("Sinks[0].Name = %q, want %q", got, want)
	}
	if got, want := result.Sinks[1].Name, "exporter_2"; got != want {
		t.Errorf("Sinks[1].Name = %q, want %q", got, want)
	}
}

func TestMultiExporter_ExportSpans_InvalidPolicyFallsBackToAllRequired(t *testing.T) {
	buf := captureMultiExporterWarns(t)
	splunkDown := errors.New("splunk down")

	multi := NewMultiExporter(
		[]model.SpanExporter{
			&mockExporter{},
			&mockExporter{
				exportSpansFunc: func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
					return nil, splunkDown
				},
			},
		},
		[]string{"otel", "splunk"},
		WithMultiExporterPolicy(MultiExporterPolicy(99)),
	)

	if !strings.Contains(buf.String(), "invalid span policy 99") {
		t.Fatalf("warning log = %q, want invalid policy warning", buf.String())
	}

	result, err := multi.ExportSpans(context.Background(), multiTestSpans())
	if err == nil {
		t.Fatal("expected all-required fallback to fail on partial sink error")
	}
	if !errors.Is(err, splunkDown) {
		t.Errorf("err = %v, want splunkDown", err)
	}
	if result.Accepted != nil {
		t.Errorf("Accepted = %v, want nil under all-required fallback", result.Accepted)
	}
}

func TestMultiExporter_ExportSpans_AllSinksAttemptedOnFailure(t *testing.T) {
	// Even when the first sink fails, every subsequent sink must still
	// be called — partial delivery to healthy sinks is preferred over
	// silently dropping every export when one sink is down.
	exp1 := &mockExporter{
		exportSpansFunc: func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
			return nil, errors.New("otel down")
		},
	}
	exp2 := &mockExporter{}
	exp3 := &mockExporter{}
	multi := NewMultiExporter(
		[]model.SpanExporter{exp1, exp2, exp3},
		[]string{"otel", "splunk", "tail"},
	)

	_, err := multi.ExportSpans(context.Background(), multiTestSpans())
	if err == nil {
		t.Fatal("expected error from partial failure")
	}
	if exp1.exportSpansCalls != 1 || exp2.exportSpansCalls != 1 || exp3.exportSpansCalls != 1 {
		t.Errorf("expected each sink called once, got %d/%d/%d",
			exp1.exportSpansCalls, exp2.exportSpansCalls, exp3.exportSpansCalls)
	}
}

func TestMultiExporter_ExportSpans_PolicyAnySuccessReturnsAcceptedUnionOnPartialFailure(t *testing.T) {
	splunkDown := errors.New("splunk down")

	span1 := multiTestSpans()[0]
	span2 := multiTestSpans()[1]
	span3 := multiTestSpans()[2]
	key1 := model.KeyOf(span1)
	key2 := model.KeyOf(span2)
	key3 := model.KeyOf(span3)

	exp1 := &mockExporter{
		exportSpansFunc: func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
			return []model.SpanKey{key1, key2}, nil
		},
	}
	exp2 := &mockExporter{
		exportSpansFunc: func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
			return nil, splunkDown
		},
	}
	exp3 := &mockExporter{
		exportSpansFunc: func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
			return []model.SpanKey{key2, key3}, nil
		},
	}
	multi := NewMultiExporter(
		[]model.SpanExporter{exp1, exp2, exp3},
		[]string{"otel", "splunk", "tail"},
		WithMultiExporterPolicy(PolicyAnySuccess),
	)

	result, err := multi.ExportSpans(context.Background(), multiTestSpans())
	if err != nil {
		t.Fatalf("unexpected error under PolicyAnySuccess: %v", err)
	}
	if !sameSpanKeySet(result.Accepted, []model.SpanKey{key1, key2, key3}) {
		t.Errorf("Accepted = %v, want union of successful sinks", result.Accepted)
	}
	if result.TotalAccepted != 3 {
		t.Errorf("TotalAccepted = %d, want 3", result.TotalAccepted)
	}
	if len(result.Sinks) != 3 {
		t.Fatalf("len(Sinks) = %d, want 3", len(result.Sinks))
	}
	if result.Sinks[0].Accepted != 2 || result.Sinks[0].Error != nil {
		t.Errorf("otel status = %+v, want accepted=2 err=nil", result.Sinks[0])
	}
	if result.Sinks[1].Accepted != 0 || !errors.Is(result.Sinks[1].Error, splunkDown) {
		t.Errorf("splunk status = %+v, want accepted=0 err=splunkDown", result.Sinks[1])
	}
	if result.Sinks[2].Accepted != 2 || result.Sinks[2].Error != nil {
		t.Errorf("tail status = %+v, want accepted=2 err=nil", result.Sinks[2])
	}
}

func TestMultiExporter_ExportSpans_PolicyAnySuccessAllSucceedEmptyAccepted(t *testing.T) {
	multi := NewMultiExporter(
		[]model.SpanExporter{
			&mockExporter{
				exportSpansFunc: func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
					return nil, nil
				},
			},
			&mockExporter{
				exportSpansFunc: func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
					return []model.SpanKey{}, nil
				},
			},
		},
		[]string{"otel", "splunk"},
		WithMultiExporterPolicy(PolicyAnySuccess),
	)

	result, err := multi.ExportSpans(context.Background(), multiTestSpans())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Accepted == nil {
		t.Fatal("Accepted = nil, want non-nil empty slice")
	}
	if len(result.Accepted) != 0 {
		t.Errorf("len(Accepted) = %d, want 0", len(result.Accepted))
	}
	if result.TotalAccepted != 0 {
		t.Errorf("TotalAccepted = %d, want 0", result.TotalAccepted)
	}
}

func TestMultiExporter_ExportSpans_PolicyAnySuccessAllFailReturnsError(t *testing.T) {
	otelDown := errors.New("otel down")
	splunkDown := errors.New("splunk down")

	multi := NewMultiExporter(
		[]model.SpanExporter{
			&mockExporter{
				exportSpansFunc: func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
					return nil, otelDown
				},
			},
			&mockExporter{
				exportSpansFunc: func(context.Context, []model.OpenTelemetrySpan) ([]model.SpanKey, error) {
					return nil, splunkDown
				},
			},
		},
		[]string{"otel", "splunk"},
		WithMultiExporterPolicy(PolicyAnySuccess),
	)

	result, err := multi.ExportSpans(context.Background(), multiTestSpans())
	if err == nil {
		t.Fatal("expected error when every sink fails under PolicyAnySuccess")
	}
	if !errors.Is(err, otelDown) || !errors.Is(err, splunkDown) {
		t.Errorf("err = %v, want joined sink errors", err)
	}
	if result.Accepted != nil {
		t.Errorf("Accepted = %v, want nil", result.Accepted)
	}
	if result.TotalAccepted != 0 {
		t.Errorf("TotalAccepted = %d, want 0", result.TotalAccepted)
	}
}

func TestMultiExporter_ExportSpans_EmptyInput(t *testing.T) {
	exp1 := &mockExporter{}
	multi := NewMultiExporter([]model.SpanExporter{exp1}, []string{"otel"})

	result, err := multi.ExportSpans(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Accepted != nil {
		t.Errorf("expected nil keys for empty input, got %v", result.Accepted)
	}
	if len(result.Sinks) != 0 {
		t.Errorf("expected no sink statuses for empty input, got %v", result.Sinks)
	}
	if exp1.exportSpansCalls != 0 {
		t.Errorf("exporter should not be called for empty spans")
	}
}

func TestMultiExporter_ExportQuery(t *testing.T) {
	otelDown := errors.New("otel down")
	splunkDown := errors.New("splunk down")

	tests := []struct {
		name         string
		funcs        []func(context.Context, model.QueryLog) error
		wantErr      bool
		wantErrIs    []error
		wantErrSubs  []string
		wantAccepted []int
		wantSinkErr  []bool
	}{
		{
			name:         "all succeed",
			funcs:        []func(context.Context, model.QueryLog) error{nil, nil},
			wantAccepted: []int{1, 1},
			wantSinkErr:  []bool{false, false},
		},
		{
			name: "one fails returns error",
			funcs: []func(context.Context, model.QueryLog) error{
				nil,
				func(context.Context, model.QueryLog) error { return splunkDown },
			},
			wantErr:      true,
			wantErrIs:    []error{splunkDown},
			wantErrSubs:  []string{"1/2 sinks", "splunk"},
			wantAccepted: []int{1, 0},
			wantSinkErr:  []bool{false, true},
		},
		{
			name: "all fail returns error joining every sink",
			funcs: []func(context.Context, model.QueryLog) error{
				func(context.Context, model.QueryLog) error { return otelDown },
				func(context.Context, model.QueryLog) error { return splunkDown },
			},
			wantErr:      true,
			wantErrIs:    []error{otelDown, splunkDown},
			wantErrSubs:  []string{"2/2 sinks", "otel", "splunk"},
			wantAccepted: []int{0, 0},
			wantSinkErr:  []bool{true, true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exps := make([]model.SpanExporter, len(tt.funcs))
			for i, fn := range tt.funcs {
				exps[i] = &mockExporter{exportQueryFunc: fn}
			}
			multi := NewMultiExporter(exps, []string{"otel", "splunk"})

			result, err := multi.ExportQuery(context.Background(), model.QueryLog{QueryID: "q1"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if err != nil {
				for _, target := range tt.wantErrIs {
					if !errors.Is(err, target) {
						t.Errorf("errors.Is(err, %v) = false; err = %v", target, err)
					}
				}
				for _, sub := range tt.wantErrSubs {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("err = %q does not contain %q", err.Error(), sub)
					}
				}
			} else if len(tt.wantErrIs) > 0 || len(tt.wantErrSubs) > 0 {
				t.Errorf("wantErrIs/wantErrSubs set but err is nil")
			}
			if result.TotalSent != 1 {
				t.Errorf("TotalSent = %d, want 1", result.TotalSent)
			}
			if len(result.Sinks) != 2 {
				t.Fatalf("len(Sinks) = %d, want 2", len(result.Sinks))
			}
			for i, sink := range result.Sinks {
				if sink.Accepted != tt.wantAccepted[i] {
					t.Errorf("Sinks[%d].Accepted = %d, want %d", i, sink.Accepted, tt.wantAccepted[i])
				}
				if (sink.Error != nil) != tt.wantSinkErr[i] {
					t.Errorf("Sinks[%d].Error = %v, wantErr %v", i, sink.Error, tt.wantSinkErr[i])
				}
			}
		})
	}
}

func TestMultiExporter_ExportQuery_StaysAllRequiredWithAnySuccessSpanPolicy(t *testing.T) {
	splunkDown := errors.New("splunk down")

	multi := NewMultiExporter(
		[]model.SpanExporter{
			&mockExporter{},
			&mockExporter{
				exportQueryFunc: func(context.Context, model.QueryLog) error {
					return splunkDown
				},
			},
		},
		[]string{"otel", "splunk"},
		WithMultiExporterPolicy(PolicyAnySuccess),
	)

	result, err := multi.ExportQuery(context.Background(), model.QueryLog{QueryID: "q1"})
	if err == nil {
		t.Fatal("expected query export error despite PolicyAnySuccess span policy")
	}
	if !errors.Is(err, splunkDown) {
		t.Errorf("err = %v, want splunkDown", err)
	}
	if result.TotalAccepted != 0 {
		t.Errorf("TotalAccepted = %d, want 0 on all-required query failure", result.TotalAccepted)
	}
	if len(result.Sinks) != 2 {
		t.Fatalf("len(Sinks) = %d, want 2", len(result.Sinks))
	}
	if result.Sinks[0].Accepted != 1 || result.Sinks[0].Error != nil {
		t.Errorf("otel status = %+v, want accepted=1 err=nil", result.Sinks[0])
	}
	if result.Sinks[1].Accepted != 0 || !errors.Is(result.Sinks[1].Error, splunkDown) {
		t.Errorf("splunk status = %+v, want accepted=0 err=splunkDown", result.Sinks[1])
	}
}

func TestMultiExporter_ExportQuery_AllSinksAttemptedOnFailure(t *testing.T) {
	exp1 := &mockExporter{
		exportQueryFunc: func(context.Context, model.QueryLog) error {
			return errors.New("otel down")
		},
	}
	exp2 := &mockExporter{}
	exp3 := &mockExporter{}
	multi := NewMultiExporter(
		[]model.SpanExporter{exp1, exp2, exp3},
		[]string{"otel", "splunk", "tail"},
	)

	if _, err := multi.ExportQuery(context.Background(), model.QueryLog{QueryID: "q1"}); err == nil {
		t.Fatal("expected error from partial failure")
	}
	if exp1.exportQueryCalls != 1 || exp2.exportQueryCalls != 1 || exp3.exportQueryCalls != 1 {
		t.Errorf("expected each sink called once, got %d/%d/%d",
			exp1.exportQueryCalls, exp2.exportQueryCalls, exp3.exportQueryCalls)
	}
}

func TestMultiExporter_Close(t *testing.T) {
	exp1 := &mockExporter{}
	exp2 := &mockExporter{
		closeFunc: func(ctx context.Context) error {
			return errors.New("close failed")
		},
	}
	multi := NewMultiExporter([]model.SpanExporter{exp1, exp2}, []string{"otel", "splunk"})

	err := multi.Close(context.Background())
	if err == nil {
		t.Fatal("expected error when one Close fails")
	}
	// Both should have been called despite the error
	if exp1.closeCalls != 1 || exp2.closeCalls != 1 {
		t.Errorf("expected both Close called, got %d and %d", exp1.closeCalls, exp2.closeCalls)
	}
}

func TestMultiExporter_Close_AllSucceed(t *testing.T) {
	exp1 := &mockExporter{}
	exp2 := &mockExporter{}
	multi := NewMultiExporter([]model.SpanExporter{exp1, exp2}, []string{"otel", "splunk"})

	err := multi.Close(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func captureMultiExporterWarns(t *testing.T) *bytes.Buffer {
	t.Helper()
	// This helper mutates clicklog/stdlog process globals; keep callers
	// non-parallel and restore the exact prior stdlog state on cleanup.
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	prevPrefix := log.Prefix()
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	})

	if err := clicklog.InitLogger("warn", "", "text", 0, 0); err != nil {
		t.Fatalf("InitLogger failed: %v", err)
	}

	var buf bytes.Buffer
	log.SetOutput(&buf)

	clicklog.Warn("multi-exporter warning capture canary")
	if !strings.Contains(buf.String(), "multi-exporter warning capture canary") {
		t.Fatalf("log capture broken: got %q", buf.String())
	}
	buf.Reset()
	return &buf
}

// sameSpanKeySet reports whether a and b contain the same SpanKeys
// with the same multiplicities, regardless of order. Used so assertions
// don't depend on map-iteration order (intersection results) or
// fixture ordering. A count map handles the duplicate-element case so
// e.g. {k1, k1} and {k1, k2} are not falsely considered equal.
func sameSpanKeySet(a, b []model.SpanKey) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[model.SpanKey]int, len(a))
	for _, k := range a {
		counts[k]++
	}
	for _, k := range b {
		counts[k]--
		if counts[k] < 0 {
			return false
		}
	}
	return true
}
