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

// TestExecuteTracingTest_WaitsPastPartialFlush is the release-test false
// failure: a flush landed between the query's inner spans and its outermost
// span, so the first non-empty read had no span continuing the supplied
// parent. The command must keep polling until that span arrives, then export
// the whole trace.
func TestExecuteTracingTest_WaitsPastPartialFlush(t *testing.T) {
	identity := fixedTracingIdentity()
	root := nativeTracingChild(identity)
	inner := nativeTracingChild(identity)
	inner.SpanID = 9002
	inner.ParentSpanID = root.SpanID
	inner.OperationName = "Planner"
	reader := &fakeTracingTestReader{fetchResponses: [][]model.OpenTelemetrySpan{nil, {inner}, {inner}, {inner, root}}}
	exporter := &testCommandExporter{acceptedCount: -1}
	deps := tracingTestDeps(reader, []builtExporter{{Label: "test", Exporter: exporter}})
	var out bytes.Buffer
	if code := executeTracingTest(context.Background(), &config.Config{}, identity, deps, &out); code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", code, out.String())
	}
	if reader.fetchCalls != 4 {
		t.Errorf("fetches = %d, want 4 (empty, two partial, complete)", reader.fetchCalls)
	}
	if len(exporter.spans) != 3 {
		t.Errorf("exported %d spans, want the local parent and both native spans", len(exporter.spans))
	}
	if !strings.Contains(out.String(), "PASS (2 spans") || !strings.Contains(out.String(), "Result: PASS") {
		t.Errorf("expected both spans and a pass:\n%s", out.String())
	}
}

// TestPollTracingSpans_PartialReadAtDeadlineIsJudged pins what a deadline does
// after a partial read: the spans read so far come back without an error and
// marked as timed out, so a server that never continues the parent still gets
// the propagation verdict rather than a materialization timeout, and the
// output says that time ran out.
func TestPollTracingSpans_PartialReadAtDeadlineIsJudged(t *testing.T) {
	identity := fixedTracingIdentity()
	orphan := nativeTracingChild(identity)
	orphan.ParentSpanID = 999
	reader := &fakeTracingTestReader{fetchResponses: [][]model.OpenTelemetrySpan{{orphan}}}
	waits := 0
	spans, timedOut, err := pollTracingSpans(context.Background(), reader, identity.TraceID, identity.SpanID, time.Time{}, func(context.Context, time.Duration) error {
		waits++
		if waits == 3 {
			return context.DeadlineExceeded
		}
		return nil
	}, io.Discard)
	if err != nil {
		t.Fatalf("err = %v, want the partial read returned for judging", err)
	}
	if !timedOut {
		t.Error("timedOut = false, want true after the deadline")
	}
	if len(spans) != 1 || spans[0].SpanID != orphan.SpanID {
		t.Errorf("spans = %+v, want the partial read", spans)
	}
	if reader.fetchCalls != 3 {
		t.Errorf("fetches = %d, want polling to continue until the deadline", reader.fetchCalls)
	}
}

// TestExecuteTracingTest_DeadlineAfterPartialReadNamesTimeout pins the
// verdict's wording: when the deadline ends polling after a partial read, the
// output says the parent span was not seen within --timeout and suggests
// raising it, instead of only reporting a propagation failure.
func TestExecuteTracingTest_DeadlineAfterPartialReadNamesTimeout(t *testing.T) {
	identity := fixedTracingIdentity()
	orphan := nativeTracingChild(identity)
	orphan.ParentSpanID = 999
	deps := tracingTestDeps(&fakeTracingTestReader{fetchResponses: [][]model.OpenTelemetrySpan{{orphan}}}, nil)
	deps.wait = func(context.Context, time.Duration) error { return context.DeadlineExceeded }
	var out bytes.Buffer
	if code := executeTracingTest(context.Background(), &config.Config{}, identity, deps, &out); code != 1 {
		t.Fatalf("exit = %d, want 1:\n%s", code, out.String())
	}
	for _, want := range []string{
		"FAIL (no native span referenced parent_span_id=42 within --timeout)",
		"not seen within --timeout. Increase --timeout",
		"did not continue the parent context",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

// TestPollTracingSpans_ParentFoundIsNotTimedOut pins that a complete read is
// not reported as timed out.
func TestPollTracingSpans_ParentFoundIsNotTimedOut(t *testing.T) {
	identity := fixedTracingIdentity()
	reader := &fakeTracingTestReader{fetchResponses: [][]model.OpenTelemetrySpan{{nativeTracingChild(identity)}}}
	spans, timedOut, err := pollTracingSpans(context.Background(), reader, identity.TraceID, identity.SpanID, time.Time{}, func(context.Context, time.Duration) error {
		t.Fatal("a complete first read must not wait")
		return nil
	}, io.Discard)
	if err != nil || timedOut || len(spans) != 1 {
		t.Errorf("spans/timedOut/err = %d/%v/%v, want 1/false/nil", len(spans), timedOut, err)
	}
}

func TestPollTracingSpans_MissingTableRemainsBounded(t *testing.T) {
	for _, stop := range []error{context.DeadlineExceeded, context.Canceled} {
		t.Run(stop.Error(), func(t *testing.T) {
			reader := &fakeTracingTestReader{fetchErr: &clickhouse.Exception{Code: 60, Message: "Unknown table system.opentelemetry_span_log"}}
			var delays []time.Duration
			_, _, err := pollTracingSpans(context.Background(), reader, fixedTracingIdentity().TraceID, fixedTracingIdentity().SpanID, time.Time{}, func(_ context.Context, delay time.Duration) error {
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
			_, _, err := pollTracingSpans(context.Background(), reader, fixedTracingIdentity().TraceID, fixedTracingIdentity().SpanID, time.Time{}, func(context.Context, time.Duration) error {
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
