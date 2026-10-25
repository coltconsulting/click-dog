package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/coltconsulting/click-dog/internal/config"
	"github.com/coltconsulting/click-dog/internal/model"
)

func TestExecuteTracingTest_WaitsForFirstSpanLog(t *testing.T) {
	identity := fixedTracingIdentity()
	missingTable := fmt.Errorf("fetch: %w", &clickhouse.Exception{Code: 60, Message: "Unknown table system.opentelemetry_span_log"})
	reader := &fakeTracingTestReader{
		fetchErrors:    []error{missingTable, missingTable},
		fetchResponses: [][]model.OpenTelemetrySpan{nil, nil, nil, {nativeTracingChild(identity)}},
	}
	exporter := &testCommandExporter{acceptedCount: -1}
	deps := tracingTestDeps(reader, []builtExporter{{Label: "test", Exporter: exporter}})
	var delays []time.Duration
	var out bytes.Buffer
	deps.wait = func(_ context.Context, delay time.Duration) error {
		if strings.Count(out.String(), "waiting for system.opentelemetry_span_log") != 1 {
			t.Errorf("expected exactly one notice before waiting:\n%s", out.String())
		}
		delays = append(delays, delay)
		return nil
	}
	if code := executeTracingTest(context.Background(), &config.Config{}, identity, deps, &out); code != 0 {
		t.Fatalf("exit = %d:\n%s", code, out.String())
	}
	if reader.queryCalls != 1 || reader.fetchCalls != 4 || len(exporter.spans) != 2 {
		t.Fatalf("queries/fetches/exported = %d/%d/%d, want 1/4/2", reader.queryCalls, reader.fetchCalls, len(exporter.spans))
	}
	if fmt.Sprint(delays) != "[100ms 200ms 400ms]" {
		t.Errorf("poll delays = %v", delays)
	}
	if exporter.spans[1].ParentSpanID != identity.SpanID || exporter.spans[1].TraceID != identity.TraceID {
		t.Error("native propagation changed while waiting for materialization")
	}
	if strings.Count(out.String(), "waiting for system.opentelemetry_span_log") != 1 || !strings.Contains(out.String(), "Result: PASS") {
		t.Errorf("expected one notice followed by success:\n%s", out.String())
	}
}

func TestPollTracingSpans_MissingTableRemainsBounded(t *testing.T) {
	for _, stop := range []error{context.DeadlineExceeded, context.Canceled} {
		t.Run(stop.Error(), func(t *testing.T) {
			reader := &fakeTracingTestReader{fetchErr: &clickhouse.Exception{Code: 60, Message: "Unknown table system.opentelemetry_span_log"}}
			var delays []time.Duration
			_, err := pollTracingSpans(context.Background(), reader, fixedTracingIdentity().TraceID, func(_ context.Context, delay time.Duration) error {
				delays = append(delays, delay)
				if len(delays) == 7 {
					return stop
				}
				return nil
			}, io.Discard)
			if !errors.Is(err, stop) || !strings.Contains(err.Error(), "system.opentelemetry_span_log") {
				t.Fatalf("error = %v, want table diagnostic and %v", err, stop)
			}
			if fmt.Sprint(delays) != "[100ms 200ms 400ms 800ms 1s 1s 1s]" {
				t.Errorf("poll delays = %v", delays)
			}
		})
	}
}

func TestPollTracingSpans_DoesNotRetryOtherErrors(t *testing.T) {
	for _, fetchErr := range []error{
		&clickhouse.Exception{Code: 497, Message: "ACCESS_DENIED"},
		&clickhouse.Exception{Code: 47, Message: "UNKNOWN_IDENTIFIER"},
		errors.New("UNKNOWN_TABLE code 60"),
	} {
		t.Run(fetchErr.Error(), func(t *testing.T) {
			reader := &fakeTracingTestReader{fetchErr: fetchErr}
			_, err := pollTracingSpans(context.Background(), reader, fixedTracingIdentity().TraceID, func(context.Context, time.Duration) error {
				t.Fatal("unrelated errors must fail immediately")
				return nil
			}, io.Discard)
			if !errors.Is(err, fetchErr) || reader.fetchCalls != 1 {
				t.Errorf("fetches/error = %d/%v, want 1/%v", reader.fetchCalls, err, fetchErr)
			}
		})
	}
}

func TestExecuteTracingTest_MaterializationNoticeOnFailure(t *testing.T) {
	for _, test := range []struct {
		name        string
		fetchErr    error
		wantNotices int
		wantError   string
	}{
		{"missing table", &clickhouse.Exception{Code: 60, Message: "UNKNOWN_TABLE"}, 1, "context deadline exceeded"},
		{"empty table", nil, 0, "context deadline exceeded"},
		{"permission denied", &clickhouse.Exception{Code: 497, Message: "ACCESS_DENIED"}, 0, "ACCESS_DENIED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &fakeTracingTestReader{fetchErr: test.fetchErr}
			deps := tracingTestDeps(reader, nil)
			waits := 0
			deps.wait = func(context.Context, time.Duration) error {
				waits++
				if waits == 3 {
					return context.DeadlineExceeded
				}
				return nil
			}
			var out bytes.Buffer
			if code := executeTracingTest(context.Background(), &config.Config{}, fixedTracingIdentity(), deps, &out); code != 1 {
				t.Fatalf("exit = %d, want 1:\n%s", code, out.String())
			}
			if got := strings.Count(out.String(), "waiting for system.opentelemetry_span_log to be created"); got != test.wantNotices {
				t.Errorf("notices = %d, want %d:\n%s", got, test.wantNotices, out.String())
			}
			if !strings.Contains(out.String(), test.wantError) || !strings.Contains(out.String(), "Result: FAIL") {
				t.Errorf("expected final failure with %q:\n%s", test.wantError, out.String())
			}
		})
	}
}
